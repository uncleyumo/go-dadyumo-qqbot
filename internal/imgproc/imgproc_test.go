package imgproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// 造一张指定尺寸、照片/截图质感的 PNG（值噪声：低频随机网格 + 双线性插值）。
// 不能用随机噪点（JPEG 压不动）也不能用纯渐变（PNG 本身就很小）——
// 这两种都会触发「压完更大就不压」的保护逻辑，测不出真实效果。
func makePNG(w, h int) []byte {
	rnd := rand.New(rand.NewSource(7))
	const grid = 120
	g := make([][][3]float64, grid+1)
	for i := range g {
		g[i] = make([][3]float64, grid+1)
		for j := range g[i] {
			g[i][j] = [3]float64{rnd.Float64(), rnd.Float64(), rnd.Float64()}
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		fy := float64(y) * grid / float64(h)
		y0 := int(fy)
		ty := fy - float64(y0)
		for x := 0; x < w; x++ {
			fx := float64(x) * grid / float64(w)
			x0 := int(fx)
			tx := fx - float64(x0)
			var c [3]float64
			for k := 0; k < 3; k++ {
				a := g[y0][x0][k]*(1-tx) + g[y0][x0+1][k]*tx
				b := g[y0+1][x0][k]*(1-tx) + g[y0+1][x0+1][k]*tx
				c[k] = a*(1-ty) + b*ty
			}
			img.Set(x, y, color.RGBA{uint8(c[0] * 255), uint8(c[1] * 255), uint8(c[2] * 255), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestCompressImageShrinksBigScreenshot(t *testing.T) {
	raw := makePNG(1080, 2400)
	out, ct, ok := Compress(raw, 1024)
	if !ok {
		t.Fatal("大图应该被压缩")
	}
	if ct != "image/jpeg" {
		t.Fatalf("压缩输出应为 jpeg, got %s", ct)
	}
	if len(out) >= len(raw) {
		t.Fatalf("压完应更小: 原 %d, 压后 %d", len(raw), len(out))
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("压缩结果无法解码: %v", err)
	}
	b := img.Bounds()
	longest := b.Dx()
	if b.Dy() > longest {
		longest = b.Dy()
	}
	if longest != 1024 {
		t.Fatalf("长边应为 1024, got %d", longest)
	}
	t.Logf("1080x2400 PNG %d 字节 -> %d 字节 (省 %.0f%%)",
		len(raw), len(out), 100*(1-float64(len(out))/float64(len(raw))))
}

func TestCompressImageLeavesSmallAlone(t *testing.T) {
	// 小图且尺寸达标：不重编码，省 CPU
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 120, 80)), &jpeg.Options{Quality: 80})
	if _, _, ok := Compress(buf.Bytes(), 1024); ok {
		t.Fatal("小图不应被压缩")
	}
}

func TestCompressImageKeepsAspect(t *testing.T) {
	raw := makePNG(2000, 1000)
	out, _, ok := Compress(raw, 1024)
	if !ok {
		t.Fatal("应压缩")
	}
	img, _, _ := image.Decode(bytes.NewReader(out))
	b := img.Bounds()
	if b.Dx() != 1024 || b.Dy() != 512 {
		t.Fatalf("比例应保持 2:1, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestCompressImageUnknownFormatPassesThrough(t *testing.T) {
	// 假图（webp 之类标准库解不了）：解不出来就原样放行，别把图丢了
	fake := []byte("RIFF....WEBPVP8 fake data that is not decodable")
	if _, _, ok := Compress(fake, 1024); ok {
		t.Fatal("解不了的格式不应声称压缩成功")
	}
}

