package imgproc

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

// makeGIF 造一张多帧动图，尺寸可调。
// 用真实的 gif.Encode 写盘，保证字节是合法 GIF 而不是拼出来的假头。
func makeGIF(w, h, frames int) []byte {
	g := &gif.GIF{LoopCount: 0} // 0 = 无限循环
	// 调色板给足 256 色，且每帧图案都不一样。
	// 规律条纹 LZW 压得极狠，做出来只有几十 KB，根本进不到
	//「体积超 PassthroughBytes」那条分支——那测的就不是真实路径了。
	pal := make(color.Palette, 256)
	for c := range pal {
		pal[c] = color.RGBA{uint8(c * 7 % 256), uint8(c * 13 % 256), uint8(c * 29 % 256), 255}
	}
	for i := 0; i < frames; i++ {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				p.SetColorIndex(x, y, uint8(x*x+y*7+i*31+((x*y)>>3)))
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func countGIFFrames(b []byte) int {
	g, err := gif.DecodeAll(bytes.NewReader(b))
	if err != nil {
		return -1
	}
	return len(g.Image)
}

// Compress 绝不能把动图压成静图。
//
// 这是 2026-10-01 生产事故：Go 的 image.Decode 对 GIF 只解第一帧，
// 后面 jpeg.Encode 写出去就是一张静图。文件还是「一张正常的 jpg」，
// 只是不动了——入池时就死了，之后每次发出去都是静图。
// 关键在于它**不报错**：肉眼扫代码完全看不出问题。
func TestCompressNeverFlattensGIF(t *testing.T) {
	// 体积必须超过 PassthroughBytes(256KB)，否则根本进不到压缩分支，
	// 测了也白测（真事故里那张 GIF 就是 300x300 但几百 KB）。
	raw := makeGIF(600, 600, 12)
	if len(raw) <= PassthroughBytes {
		t.Fatalf("测试素材要大于直通门槛才能覆盖真实路径：%d <= %d", len(raw), PassthroughBytes)
	}
	if n := countGIFFrames(raw); n != 12 {
		t.Fatalf("素材应含 12 帧，实际 %d", n)
	}

	out, ct, ok := Compress(raw, 1024)
	if ok {
		t.Fatalf("GIF 不该走 JPEG 重编码：返回了 %s / %d 字节", ct, len(out))
	}
	// 原样放行，字节必须一模一样
	if !bytes.Equal(out, nil) {
		t.Errorf("未压缩时应返回 nil 字节，实际 %v", out)
	}
	if SniffType(raw) != "image/gif" {
		t.Errorf("GIF 魔数应被认出，实际 %q", SniffType(raw))
	}
}

// WebP 同理：标准库解不了，但更不能被"压"成一张糊掉的静图
func TestCompressNeverFlattensWebP(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 4<<20)...)
	if _, _, ok := Compress(webp, 1024); ok {
		t.Error("webp 不该被 JPEG 重编码")
	}
	if SniffType(webp) != "image/webp" {
		t.Errorf("webp 魔数应被认出，实际 %q", SniffType(webp))
	}
}

// 静图该压还是要压——上面的豁免不能变成「所有图都不压了」
func TestCompressStillCompressesStaticImages(t *testing.T) {
	raw := makePNG(1080, 2400)
	out, ct, ok := Compress(raw, 1024)
	if !ok || ct != "image/jpeg" {
		t.Fatalf("静图仍应压成 jpeg：ok=%v ct=%s", ok, ct)
	}
	if len(out) >= len(raw) {
		t.Errorf("压完应更小：原 %d 压后 %d", len(raw), len(out))
	}
}

// 小尺寸的 GIF 也一样不能被动——哪怕它会走直通分支，
// 也要确保「直通」不是因为尺寸判断，而是「本来就不压动图」这条规则。
// 换一个尺寸不该改变动图的待遇。
func TestCompressKeepsSmallGIFUntouched(t *testing.T) {
	raw := makeGIF(64, 64, 3)
	if _, _, ok := Compress(raw, 1024); ok {
		t.Error("小 GIF 也不该被重编码")
	}
}
