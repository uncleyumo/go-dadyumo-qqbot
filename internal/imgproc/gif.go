package imgproc

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"

	"dadyumo/internal/logx"
)

// GIF 拆帧：把动图按**总时长**均匀抽几帧，让模型看到过程而不只是开场。
//
// 为什么需要它：Compress 对 GIF 显式豁免（原样透传，见 imgproc.go 的 GIF 段落），
// 于是发出去的是完整的多帧字节，而**上游 vision 模型只看得到首帧**——
// 群里大量表情包是动图，「它在干嘛」「这是不是那个表情」只能靠猜。
// 视频早就抽帧了（brain/video.go 的 extractFrames），GIF 一直没有对应实现。
//
// 顺带解决一个更要紧的问题：GIF 豁免压缩，而 base64 编码没有任何体积上限，
// 一张 6MB 的动图会以 ~8MB 进请求体，是整条图片链路里唯一不受控的一条。
// 拆成几张 JPEG 后这个量级自然消失。

const (
	// maxGIFFramesToComposite 帧数上限，超过就整体放弃拆帧、原样透传。
	//
	// 卡的是**合成次数**不是内存：Disposal 是串行链，取第 5 帧必须一路
	// 合成过去，没法跳过中间帧直接定位。所以一张 400 帧的小图（100×100，
	// 6MB 完全可能）会白白合成 400 次只为了取 5 张。
	// 内存那边估算过了：6MB 的 GIF 解压后约 10MB 像素数据，峰值 10~20MB，
	// 百毫秒级，不构成 MaxBytes 之外的额外风险。
	maxGIFFramesToComposite = 200
)

// FetchViews 抓图并返回两个东西：给模型看的若干张 view，以及**给池子用的原始 src**。
//
// 为什么必须分开：dataURLs 同时喂给模型和表情包池（brain/collect.go 按模型
// 报的序号取图入池）。若 GIF 在这里直接炸成 5 张 JPEG，模型报「第 3 张有梗」，
// 池子里就会多一张**不会动的静帧**——2026-10-01 那次 GIF 被压成静图的事故
// 换个入口重演一遍，而且现有测试抓不到（memepool 的测试直接调 Add，
// 不经过 dataURLs）。
//
// 同一张图的多个 view 共享同一个 src，调用方按下标对齐不可能错位。
func FetchViews(url string, maxSide, maxFrames int) (views []string, src string, err error) {
	raw, ct, err := Fetch(url, maxSide)
	if err != nil {
		return nil, "", err
	}
	src = "data:" + ct + ";base64," + base64Encode(raw)

	// 判据用 SniffType 而不是 Fetch 返回的 ct，与 Compress 保持同一口径：
	// 服务端把 JPEG 谎报成 image/gif 时不该走进解码分支。
	if SniffType(raw) != "image/gif" {
		return []string{src}, src, nil
	}
	// 不拆帧的两种情况：调用方不要，或帧数太多合成不划算。
	// 都退回原样透传——「看到首帧」本来就比「什么都不给」强。
	if maxFrames <= 0 {
		return []string{src}, src, nil
	}
	frames, ok := splitGIFFrames(raw, maxSide, maxFrames)
	if !ok {
		return []string{src}, src, nil
	}
	return frames, src, nil
}

// splitGIFFrames 解码动图并按总时长均匀抽帧，逐帧压成 JPEG data URI。
//
// 第二个返回值 false 表示「没拆成」，调用方应退回原样透传。
// 任何一步失败（解码不了、画布尺寸离谱、编码出错）都走这条路——
// 丢一张图的观感比只看到首帧差得多。
func splitGIFFrames(raw []byte, maxSide, maxFrames int) ([]string, bool) {
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil || len(g.Image) == 0 {
		if err != nil {
			logx.Warn("GIF 解码失败，按原图处理", "err", err.Error())
		}
		return nil, false
	}
	if len(g.Image) > maxGIFFramesToComposite {
		logx.Info("GIF 帧数过多，放弃拆帧",
			"帧数", len(g.Image), "上限", maxGIFFramesToComposite)
		return nil, false
	}
	// 画布尺寸取自 Logical Screen Descriptor。没兜底成 Config：
	// 尺寸为 0 会让 NewRGBA 拿到空画布，后面每帧都白合成一遍。
	w, h := g.Config.Width, g.Config.Height
	if w <= 0 || h <= 0 {
		return nil, false
	}

	picks := pickGIFFrameIndexes(g)
	if len(picks) == 0 {
		return nil, false
	}

	// 合成是一趟串行扫描：Disposal 决定了每一帧都得在上一帧的结果上叠加，
	// 没法只为选中的那几个帧跳着走。选中的帧在扫描到位时立刻编码。
	want := make(map[int]bool, len(picks))
	for _, i := range picks {
		want[i] = true
	}
	dc := image.NewRGBA(image.Rect(0, 0, w, h))
	out := make([]string, 0, len(picks))
	// DisposalPrevious 要回到本帧绘制前的画面，只在真有帧用到时才复制整块画布。
	// 每帧都复制的话 200 帧就是 200 次全画布拷贝，纯浪费。
	var prev image.Image

	for i, f := range g.Image {
		disposal := byte(gif.DisposalNone)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			prev = cloneImage(dc)
		}
		draw.Draw(dc, f.Bounds(), f, f.Bounds().Min, draw.Over)

		if want[i] {
			if uri, ok := encodeJPEGDataURI(dc, maxSide); ok {
				out = append(out, uri)
			}
		}

		// Disposal 作用于**本帧显示之后**，是为下一帧准备画布。
		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(dc, f.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			if prev != nil {
				draw.Draw(dc, dc.Bounds(), prev, image.Point{}, draw.Src)
				prev = nil
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	logx.Info("GIF 已拆帧", "原帧数", len(g.Image), "给出", len(out), "长边", maxSide)
	return out, true
}

// pickGIFFrameIndexes 按**总时长**均匀选帧下标。
//
// 不能按帧数均分：每帧的 Delay 可以差很多倍（一张 60 帧的动图可能前 50 帧
// 各停 2 秒、后 10 帧各停 20 秒），按帧数取 5 个会全挤在静止的前半段。
// 正确做法是累加 delay 得总时长，再按时间的等分点反查下标。
func pickGIFFrameIndexes(g *gif.GIF) []int {
	n := len(g.Image)
	if n == 0 {
		return nil
	}
	want := 5
	if n < want {
		want = n
	}
	// 只有一帧，或 Delay 全是 0（很多 GIF 的编码器不写延时）：
	// 总时长为 0，算不出时间等分点，退回按帧数均分。
	total := 0
	for i := range g.Image {
		if i < len(g.Delay) && g.Delay[i] > 0 {
			total += g.Delay[i]
		}
	}
	if total <= 0 {
		return spreadIndexes(n, want)
	}

	seen := make(map[int]bool, want)
	out := make([]int, 0, want)
	for k := 0; k < want; k++ {
		// 第 k 个等分点落在 k/want 处。want==1 时就是首帧。
		target := total * k / want
		idx := nearestFreeFrame(g, frameAtTime(g, target), seen, n)
		if idx < 0 {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nearestFreeFrame 从 want 出发找最近的一个还没被选中的帧下标，全被占了返回 -1。
//
// 为什么需要它：延时分布极不均匀时，多个时间等分点会落进同一帧——
// 比如 delays 是 {100,100,100,10,10,10}（总时长 330，最后 3 帧只占 9%），
// 5 个等分点反查出来是 0,0,1,1,2，去重后只剩 3 帧。
//
// 那确实是「按时间均匀」的正确答案（后 3 帧在时间轴上本来就挤在一起），
// 但只给模型 3 帧不是我们要的：它要的是 5 个不同的瞬间。
// 所以时间均分选出主干的 5 个点之后，不足的部分按「就近」补上，
// 补位只在主干点附近取，不会跑到时间轴的另一端去。
func nearestFreeFrame(g *gif.GIF, want int, seen map[int]bool, n int) int {
	if want < 0 || want >= n {
		want = n - 1
	}
	if !seen[want] {
		return want
	}
	for d := 1; d < n; d++ {
		if lo := want - d; lo >= 0 && !seen[lo] {
			return lo
		}
		if hi := want + d; hi < n && !seen[hi] {
			return hi
		}
	}
	return -1
}

// frameAtTime 反查「累计时长首次达到 target 的那一帧」。
func frameAtTime(g *gif.GIF, target int) int {
	acc := 0
	for i := range g.Image {
		acc += g.Delay[i]
		if acc > target {
			return i
		}
	}
	// target 落在最后之后（延时四舍五入的边缘）：给最后一帧。
	return len(g.Image) - 1
}

// spreadIndexes 把 n 帧按 want 个等分点摊开，首尾各占一个。
func spreadIndexes(n, want int) []int {
	if want >= n {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, want)
	for k := 0; k < want; k++ {
		idx := k * (n - 1) / (want - 1)
		if k == 0 {
			idx = 0
		}
		if k == want-1 {
			idx = n - 1
		}
		if len(out) > 0 && out[len(out)-1] == idx {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// encodeJPEGDataURI 把合成好的画布压成 JPEG data URI。
// 压不动时（本来就小）返回 false，调用方保留上一版结果。
func encodeJPEGDataURI(img image.Image, maxSide int) (string, bool) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, fitForEncode(img, maxSide), &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return "", false
	}
	return "data:image/jpeg;base64," + base64Encode(buf.Bytes()), true
}

// fitForEncode 按需缩放。返回原图或缩放后的副本。
func fitForEncode(src image.Image, maxSide int) image.Image {
	if maxSide <= 0 {
		return src
	}
	b := src.Bounds()
	longest := b.Dx()
	if h := b.Dy(); h > longest {
		longest = h
	}
	if longest <= maxSide {
		return src
	}
	scale := float64(maxSide) / float64(longest)
	nw := int(float64(b.Dx()) * scale)
	nh := int(float64(b.Dy()) * scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return resizeBox(src, nw, nh)
}

func cloneImage(src *image.RGBA) image.Image {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

// base64Encode 只是让上面几行不必重复写 base64.StdEncoding。
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
