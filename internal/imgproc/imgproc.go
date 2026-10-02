// Package imgproc 是图片抓取与压缩的公共实现。
//
// 单独成包的原因：表情包池也要压图（入池前统一压到长边上限，控制 MinIO 体积和
// QQ 上传量），而池子要被 brain 用。放在 brain 里的话 memepool → brain → memepool
// 会成循环依赖。
package imgproc

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // 注册 gif 解码器：动图只取第一帧，后续帧对理解没意义
	"image/jpeg"
	_ "image/png" // 只为让 image.Decode 认识 png
	"io"
	"net/http"
	"strings"
	"time"

	"dadyumo/internal/logx"
)

// 图片抓取与压缩的硬限制。
//
// 为什么必须压缩：群里的图一半是手机截图，PNG 动辄 1~3MB，base64 之后再膨胀 33%，
// 一张图就能吃掉整轮 token 预算。而模型真正需要的只是「看得清」，
// 长边压到 1024、质量 80 的 JPEG 通常只有一两百 KB，视觉信息几乎无损。
const (
	// MaxBytes 下载上限，超过直接放弃。
	//
	// 定这个数时先确认过它**不是**图片进模型的那道墙：抓下来的图一律要过
	// Compress，长边压到 maxSide（默认 1024）、质量 80 之后基本只有一两百 KB，
	// base64 再膨胀 33% 也远不到 5MB。所以这个上限卡的是「原图」，不是「发给模型的图」。
	//
	// 6MB 是刻意放宽的：群里手机截图、摄影原图动辄好几 MB，卡太紧的话
	// 一张正常照片就被整张丢掉，模型只能收到「有人发了张图」，比图糊了糟得多。
	// 流量不值钱，而 Compress 那点 CPU 不值钱。
	//
	// 真正的硬墙在别处，见 s3.go 里单次 PUT 的 5MB——但那条路径上的数据
	// 早就压过了，同样碰不到。
	MaxBytes   = 6 << 20
	fetchWait  = 12 * time.Second
	allowedImg = "image/"

	// PassthroughBytes 小图直通阈值：体积和尺寸都达标就不重编码，省 CPU。
	// 表情包基本都走这条路——它们本来就没几 KB。
	PassthroughBytes = 256 << 10
	JPEGQuality      = 80
)

// FetchAsDataURL 把图片 URL 抓下来、按需压缩后转成 data URI，供多模态报文内联使用。
// maxSide 为长边上限（<=0 表示不压缩）。
// 失败返回错误，调用方跳过这张图即可——一张图挂了不值得让整轮决策失败。
func FetchAsDataURL(url string, maxSide int) (string, error) {
	raw, ct, err := Fetch(url, maxSide)
	if err != nil {
		return "", err
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// Fetch 抓图并按需压缩，返回原始字节与 MIME。
//
// 表情包池走这条：拿到字节自己存 MinIO，不需要 base64。
func Fetch(url string, maxSide int) ([]byte, string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, "", errors.New("空图片 URL")
	}
	// 平台给的 URL 有时是协议相对的（//multimedia...），补全
	if strings.HasPrefix(url, "//") {
		url = "https:" + url
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, "", fmt.Errorf("非 HTTP 图片 URL: %.60s", url)
	}

	client := &http.Client{Timeout: fetchWait}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (dadyumo-qqbot)")
	rsp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rsp.Body.Close() }()
	if rsp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("图片下载 HTTP %d", rsp.StatusCode)
	}
	ct := strings.ToLower(strings.TrimSpace(rsp.Header.Get("Content-Type")))
	if ct != "" && !strings.HasPrefix(ct, allowedImg) {
		return nil, "", fmt.Errorf("非图片类型: %s", ct)
	}
	raw, err := io.ReadAll(io.LimitReader(rsp.Body, MaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > MaxBytes {
		return nil, "", fmt.Errorf("图片超过 %d MB 上限", MaxBytes>>20)
	}
	if len(raw) == 0 {
		return nil, "", errors.New("图片内容为空")
	}
	// Content-Type 缺失时按魔数猜一个，OpenAI 系接口对 data URI 的类型比较敏感
	if ct == "" {
		ct = SniffType(raw)
		if ct == "" {
			return nil, "", errors.New("无法识别图片类型")
		}
	}

	// 先压再编码：base64 会再膨胀 33%，必须在膨胀前把体积降下来
	if maxSide > 0 {
		if out, outCT, ok := Compress(raw, maxSide); ok {
			// Warn 级：生产跑 Info，Debug 等于没记。而「原图多大、压成多大」
			// 是排查「图怎么变糊了」「怎么没压」的唯一依据，不能只在开发时可见。
			logx.Info("图片已压缩", "url", url[:min(48, len(url))],
				"原", len(raw), "压后", len(out), "格式", outCT)
			raw, ct = out, outCT
		}
	}
	return raw, ct, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Compress 把图片缩到长边不超过 maxSide。
//
// 返回 (压缩后字节, 新 MIME, 是否真的压了)。调用方只在 ok=true 时替换。
// 三种情况直接放弃压缩、原样返回：
//   - 解码失败（webp 之类标准库不认的格式）→ 原样放行，总比丢图强
//   - 尺寸和体积本来就达标 → 省 CPU
//   - 压完反而更大（比如已经高度优化过的小 JPEG）→ 不干亏本买卖
//
// **动图一律不碰。** Go 的 image.Decode 对 GIF 只解第一帧，
// 后面 jpeg.Encode 写出去的就是一张静图——群里那些会动的表情包
// 到这里就死了，而且死得毫无痕迹：文件还是「一张正常的 jpg」，
// 只是不动了。表情包池尤其不能踩这个坑：入池时压过一次，
// 之后发出去的就是静图，用户只会觉得「这图怎么不会动了」。
// GIF / WebP / APNG 一律原样放行，交给下游自己处理。
func Compress(raw []byte, maxSide int) ([]byte, string, bool) {
	ct := SniffType(raw)
	if ct == "image/gif" || ct == "image/webp" {
		return nil, "", false
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= 0 {
		return nil, "", false
	}
	if longest <= maxSide && len(raw) <= PassthroughBytes {
		return nil, "", false
	}

	scale := float64(maxSide) / float64(longest)
	if scale > 1 {
		scale = 1
	}
	nw := int(float64(w) * scale)
	nh := int(float64(h) * scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	dst := resizeBox(img, nw, nh)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return nil, "", false
	}
	out := buf.Bytes()
	if len(out) >= len(raw) {
		return nil, "", false
	}
	return out, "image/jpeg", true
}

// resizeBox 区域平均降采样（box filter）。
//
// 为什么自己写：标准库的 image/draw 只提供最近邻（缩放后锯齿严重、文字糊成一团），
// 高质量的 CatmullRom/ApproxBiLinear 在扩展库里，为了一个缩放器加依赖不划算。
// 降采样场景下区域平均的效果足够好，而且实现只有几十行、零依赖。
func resizeBox(src image.Image, nw, nh int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0 := b.Min.Y + y*sh/nh
		y1 := b.Min.Y + (y+1)*sh/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := b.Min.X + x*sw/nw
			x1 := b.Min.X + (x+1)*sw/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as uint64
			var n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bb, a := src.At(sx, sy).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(bb)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(rs / n >> 8)
			dst.Pix[i+1] = uint8(gs / n >> 8)
			dst.Pix[i+2] = uint8(bs / n >> 8)
			dst.Pix[i+3] = uint8(as / n >> 8)
		}
	}
	return dst
}

// SniffType 按魔数判断图片 MIME。Content-Type 缺失时用。
func SniffType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}