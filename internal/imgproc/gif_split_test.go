package imgproc

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeDisposalGIF 造一张**带 disposal 语义**的动图。
//
// 为什么不能用现成的 makeGIF（gif_test.go:13）：它每帧都是全画布、
// Delay 恒为 10，于是合成逻辑一行都测不到（子矩形帧从没出现），
// 「按总时长均分」和「按帧数均分」结果也完全一样。两者都是本文件
// 要钉住的东西，而它恰好把这两条都遮住了。
//
// 这里造的是真实 GIF 最常见的那种形态：首帧铺满画布，后续帧只更新一小块
// （局部更新），Disposal 混用 None/Background/Previous，延时不等。
//
// transparentIdx 允许把某个调色板下标设成全透明，模拟 GIF 的透明索引——
// 标准库解码时会把对应调色板项的 alpha 置 0，draw.Over 会自动跳过。
func makeDisposalGIF(w, h int, delays []int, disposal []byte, transparentIdx byte) []byte {
	g := &gif.GIF{LoopCount: 0}
	// 索引 1 = 纯黑（首帧底色），索引 2 = 纯白（局部帧画上去的颜色）。
	// 用黑白两个极端值：合成写错时亮度差是 0 vs 255，断言不必依赖容差，
	// JPEG 的轻微色彩偏移也干扰不了判断。
	// 调色板必须**每一项都非 nil**——gif.EncodeAll 见到 nil 就 panic，
	// 所以其余 253 项也要填上（测试里没人用到的颜色）。
	pal := make(color.Palette, 256)
	for c := range pal {
		pal[c] = color.RGBA{uint8(c), uint8(c), uint8(c), 255}
	}
	pal[1] = color.RGBA{0, 0, 0, 255}
	pal[2] = color.RGBA{255, 255, 255, 255}
	if transparentIdx < 255 {
		pal[transparentIdx] = color.RGBA{0, 0, 0, 0}
	}
	full := image.Rect(0, 0, w, h)
	// 第 2 帧起只更新左上角一小块：局部更新帧的标准形态
	part := image.Rect(0, 0, w/2, h/2)

	for i := 0; i < len(delays); i++ {
		var p *image.Paletted
		if i == 0 {
			p = image.NewPaletted(full, pal)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					p.SetColorIndex(x, y, 1) // 纯黑
				}
			}
		} else {
			p = image.NewPaletted(part, pal)
			for y := 0; y < part.Dy(); y++ {
				for x := 0; x < part.Dx(); x++ {
					p.SetColorIndex(x, y, 2) // 纯白
				}
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, delays[i])
		if disposal != nil {
			g.Disposal = append(g.Disposal, disposal[i])
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// 按总时长均分，不是按帧数均分。
//
// delays 故意让前三帧各 100、后三帧各 10（总时长 330，尾段只占 9%）。
// 5 个时间等分点是 0/66/132/198/264，按累积时长反查落在帧 0,0,1,1,2——
// 这正是「按时间均匀」与「按帧号均分」的区别：后者给 0,1,2,3,4，
// 第 4 个点就跳进了只占 9% 时长的尾段。
//
// 反查结果里多点落进同一帧是**正确**的（尾段在时间轴上本来就挤），
// 但模型要的是 5 个不同瞬间，所以不足的部分由 nearestFreeFrame 就近补位。
func TestSplitGIFFramesSamplesByTotalDuration(t *testing.T) {
	delays := []int{100, 100, 100, 10, 10, 10}
	raw := makeDisposalGIF(64, 64, delays, nil, 255)
	g := mustDecodeAll(t, raw)

	got := pickGIFFrameIndexes(g)
	if len(got) != 5 {
		t.Fatalf("应选出 5 个不同的瞬间，实际 %d 个: %v", len(got), got)
	}
	// 5 个时间等分点反查落在帧 0,0,1,1,2——三个点撞在前 3 帧里。
	// nearestFreeFrame 就近补位，于是补出来的两个必然落在 3 和 4。
	// 换句话说：即使时间轴 91% 的密度压在前 3 帧，给模型的 5 个瞬间
	// 仍会摊到 0..4。这与「按帧号均分」结果相同，但成因完全不同——
	// 真正区分两者的是下面这条：延时一改，选帧就跟着变。
	want := []int{0, 1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个选中的帧应是 %d，实际 %d（got=%v）", i, want[i], got[i], got)
		}
	}
	if got[0] != 0 {
		t.Errorf("必须含首帧（GIF 的信息量通常在开场），实际 %v", got)
	}
	seen := map[int]bool{}
	for _, i := range got {
		if seen[i] {
			t.Fatalf("选出的帧下标有重复: %v", got)
		}
		seen[i] = true
	}
}

// 同一个「形状」的动图，延时一变，选出的帧必须跟着变——
// 否则说明实现其实是按帧号均分的，延时只是个摆设。
func TestSplitGIFFramesSelectionTracksDelays(t *testing.T) {
	// 前 6 帧慢（各 100）、后 6 帧快（各 5）：时间轴 99% 在前 6 帧
	slow := mustDecodeAll(t, makeDisposalGIF(64, 64,
		[]int{100, 100, 100, 100, 100, 100, 5, 5, 5, 5, 5, 5}, nil, 255))
	// 延时反过来：后 6 帧慢
	fast := mustDecodeAll(t, makeDisposalGIF(64, 64,
		[]int{5, 5, 5, 5, 5, 5, 100, 100, 100, 100, 100, 100}, nil, 255))

	a, b := pickGIFFrameIndexes(slow), pickGIFFrameIndexes(fast)
	if len(a) != 5 || len(b) != 5 {
		t.Fatalf("两组都应给出 5 帧，实际 %d / %d", len(a), len(b))
	}
	// 慢段在前的那个，最后一个采样点应明显更靠前
	if a[len(a)-1] >= b[len(b)-1] {
		t.Errorf("慢帧集中在前半段时采样点应更靠前，实际 slow=%v fast=%v", a, b)
	}
}

// decodeView 把 JPEG data URI 解回图片，用于断言合成结果。
func decodeView(t *testing.T, uri string) image.Image {
	t.Helper()
	const pfx = "data:image/jpeg;base64,"
	if !strings.HasPrefix(uri, pfx) {
		t.Fatalf("view 应是 JPEG data URI，实际前缀 %.40s", uri)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, pfx))
	if err != nil {
		t.Fatalf("view 的 base64 解不开: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("view 解不开: %v", err)
	}
	return img
}

// lumAt 取某点的平均亮度（0~255），JPEG 有轻微色彩偏移所以只比量级。
func lumAt(t *testing.T, img image.Image, x, y int) int {
	t.Helper()
	b := img.Bounds()
	if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
		t.Fatalf("取样点 (%d,%d) 在画布 %v 之外", x, y, b)
	}
	r, g, bb, _ := img.At(x, y).RGBA()
	return int(r+g+bb) / 3 >> 8
}

func mustDecodeAll(t *testing.T, raw []byte) *gif.GIF {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("GIF 解不开: %v", err)
	}
	return g
}

// 抽出来的帧必须是 JPEG。
//
// 这条本身是 2026-10-01 事故的镜像：那次的症状就是「文件还是一张正常的 jpg，
// 只是不动了」。这里要保证动图进上下文后**每一帧都是静图但内容正确**，
// 而不是反过来变成一张不会动的动图。
func TestSplitGIFFramesYieldsJPEG(t *testing.T) {
	raw := makeDisposalGIF(64, 64, []int{10, 10, 10, 10, 10}, nil, 255)
	views, ok := splitGIFFrames(raw, 1024, 5)
	if !ok {
		t.Fatal("多帧 GIF 应拆出若干帧")
	}
	if len(views) != 5 {
		t.Fatalf("5 帧的 GIF 应给出 5 帧，实际 %d", len(views))
	}
	for i, v := range views {
		if !strings.HasPrefix(v, "data:image/jpeg;base64,") {
			t.Errorf("第 %d 帧应是 JPEG data URI，实际 %.40s", i, v)
		}
	}
}

// 延时均匀时，时间均分与帧数均分一致，但**不该**因为「一样」就少给帧。
//
// delays 必须非零：全 0 会走 spreadIndexes 退路，测的就不是这条路径了
// （很多 GIF 的编码器确实不写延时，所以那条退路本身也有价值，见下一条）。
func TestSplitGIFFramesUniformDelaysGiveAllDistinct(t *testing.T) {
	raw := makeDisposalGIF(64, 64, []int{10, 10, 10, 10, 10, 10, 10, 10, 10, 10}, nil, 255)
	g := mustDecodeAll(t, raw)
	got := pickGIFFrameIndexes(g)
	// 总时长 100，等分点 0/20/40/60/80。frameAtTime 用「累计 > target」，
	// 于是 20 落在累计 30 的帧（帧 2）、40 落在帧 4，依此类推。
	want := []int{0, 2, 4, 6, 8}
	if len(got) != len(want) {
		t.Fatalf("10 帧应给出 5 个等分点，实际 %d: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个等分点应是帧 %d，实际 %d（got=%v）", i, want[i], got[i], got)
		}
	}
}

// 延时全 0（编码器没写延时）时按帧数均分，仍然给满 5 帧。
func TestSplitGIFFramesZeroDelaysFallBackToEvenSpread(t *testing.T) {
	raw := makeDisposalGIF(64, 64, make([]int, 12), nil, 255)
	g := mustDecodeAll(t, raw)
	got := pickGIFFrameIndexes(g)
	if len(got) != 5 {
		t.Fatalf("全 0 延时也应给出 5 帧，实际 %d: %v", len(got), got)
	}
	if got[0] != 0 || got[len(got)-1] != 11 {
		t.Errorf("首尾必须取到第 0 帧与最后一帧，实际 %v", got)
	}
}

// 局部帧的子矩形之外，必须保留前一帧的内容。
//
// 这是最容易写错的一处：GIF 后续帧常常只更新一小块，标准库给的
// g.Image[i].Bounds() 是那个小矩形。直接把它编码出去，得到的是一张
// 四分之一大小的残图；不合成则更糟——整帧只有那一块有颜色，其余全透明。
func TestSplitGIFFramesCompositesPartialFrames(t *testing.T) {
	// 首帧铺满索引 7（偏暗），第 2 帧只在左上四分之一铺索引 200（偏亮）
	raw := makeDisposalGIF(64, 64, []int{50, 50}, nil, 255)
	views, ok := splitGIFFrames(raw, 1024, 2)
	if !ok || len(views) != 2 {
		t.Fatalf("应拆出 2 帧，实际 ok=%v n=%d", ok, len(views))
	}

	second := decodeView(t, views[1])
	b := second.Bounds()
	inside := lumAt(t, second, b.Min.X+8, b.Min.Y+8)  // 左上四分之一：第 2 帧画过
	outside := lumAt(t, second, b.Max.X-8, b.Max.Y-8) // 右下四分之三：应保留首帧

	if inside <= outside {
		t.Errorf("第 2 帧画过的区域(亮度%d)应比未画区域(亮度%d)亮——局部帧没合成上", inside, outside)
	}
	// 未画区域要保留首帧的黑色（索引 1），不是被清成全透明。
	// 不合成的话那块是透明，编成 JPEG 就是纯黑——而首帧底色也是黑，
	// 所以这条断言要配合上面那条一起看：合成对了 inside=255/outside=0，
	// 没合成则 inside 也是 0（局部帧的四分之一大小，采样点可能落在画布外）。
	if outside != 0 {
		t.Errorf("未画区域应保留首帧的黑(亮度0)，实际 %d——像是被别的颜色覆盖了", outside)
	}
	// 且整帧尺寸必须等于逻辑画布，不是子矩形——直接编码 g.Image[i] 会得到 32×32
	if b.Dx() != 64 || b.Dy() != 64 {
		t.Errorf("合成后的帧应是完整画布 64×64，实际 %dx%d", b.Dx(), b.Dy())
	}
}

// makeDisposalProbeGIF 造一张专门用来分辨三种 Disposal 的动图。
//
// 前两版素材都被突变验证整条放跑了，原因值得记下来：
//  1. **首帧必须铺满画布**。否则编码器把逻辑画布收紧到所有帧的并集
//     （实测传 64×64 被编成 32×32），Disposal 的作用范围就无从谈起。
//  2. **相邻帧必须画互不重叠的区域**，且首帧底色要与后续帧的灰差得开。
//     之前让每帧都画同一块不透明色：draw.Over 会把下层完全遮住，
//     于是「恢复了再叠」与「没恢复直接叠」读数**完全一样**，错误观察不到。
//     试过用半透明帧也不行——GIF 编码器把 alpha 量化掉了，仍是不透明。
//
// 现在：首帧铺满中灰 100；奇数帧只画 part 左半、偶数帧只画 part 右半。
// 于是左半那块在下一帧到来时的底色，完全取决于上一帧的 Disposal：
//
//	None       → 留着上一帧画的灰 20
//	Background → 被清成透明（读数 0）
//	Previous   → 恢复成首帧的中灰 100
//
// 三者读数清晰可分，Disposal 写没写对一眼就看得出。
func makeDisposalProbeGIF(w, h int, delays []int, disposal []byte) []byte {
	g := &gif.GIF{LoopCount: 0}
	pal := make(color.Palette, 256)
	for c := range pal {
		pal[c] = color.RGBA{uint8(c), uint8(c), uint8(c), 255}
	}
	full := image.Rect(0, 0, w, h)
	halfW := w / 4
	left := image.Rect(0, 0, halfW, h/2)
	right := image.Rect(halfW, 0, 2*halfW, h/2)
	for i := 0; i < len(delays); i++ {
		var p *image.Paletted
		switch {
		case i == 0:
			p = image.NewPaletted(full, pal)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					p.SetColorIndex(x, y, 100) // 中灰作底
				}
			}
		case i%2 == 1:
			p = image.NewPaletted(left, pal)
			fillRect(p, left, uint8(20*i))
		default:
			p = image.NewPaletted(right, pal)
			fillRect(p, right, uint8(20*i))
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, delays[i])
		if disposal != nil {
			g.Disposal = append(g.Disposal, disposal[i])
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func fillRect(p *image.Paletted, r image.Rectangle, idx uint8) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			p.SetColorIndex(x, y, idx)
		}
	}
}

// disposalProbeReads 把探针 GIF 拆开，返回每一帧**左半区**的亮度。
func disposalProbeReads(t *testing.T, disposal []byte) []int {
	t.Helper()
	raw := makeDisposalProbeGIF(64, 64, []int{50, 50, 50, 50}, disposal)
	views, ok := splitGIFFrames(raw, 1024, 4)
	if !ok || len(views) != 4 {
		t.Fatalf("应拆出 4 帧，实际 ok=%v n=%d", ok, len(views))
	}
	out := make([]int, len(views))
	for i, v := range views {
		out[i] = lumAt(t, decodeView(t, v), 4, 4)
	}
	return out
}

// DisposalBackground：本帧矩形在**显示之后**被清成透明。
//
// 帧 2 标了 Background，它画过的左半区必须被清掉；帧 3 画右半时，
// 左半读到的是透明（编成 JPEG 接近 0）。不处理则残留着帧 2 的灰 20。
//
// 阈值卡在 10 而不是「<=40」：三种 disposal 的正确读数是 0/20/100，
// 阈值必须落在 0 与 20 之间才区分得开。写成 <=40 时残留的 20 也能通过
// ——变异验证时正是这样把这条放跑的。
func TestSplitGIFFramesHonorsDisposalBackground(t *testing.T) {
	got := disposalProbeReads(t,
		[]byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalNone, gif.DisposalNone})
	if got[1] > 60 {
		t.Fatalf("帧 2 应画出灰20，实际 %d", got[1])
	}
	if got[2] > 10 {
		t.Errorf("帧 2 标了 DisposalBackground，其左半区应被清成透明（读数<=10），"+
			"实际 %d——残留没清掉（那是帧 2 画的灰20）", got[2])
	}
}

// DisposalPrevious：本帧矩形在**显示之后**恢复成绘制前的样子。
//
// 帧 2 标了 Previous，它画过的左半区应恢复到首帧的中灰 100。
// 不恢复则残留着帧 2 的灰 20，读数明显偏低。
func TestSplitGIFFramesHonorsDisposalPrevious(t *testing.T) {
	got := disposalProbeReads(t,
		[]byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone, gif.DisposalNone})
	if got[1] > 60 {
		t.Fatalf("帧 2 应画出灰20，实际 %d", got[1])
	}
	if got[2] < 70 {
		t.Errorf("帧 2 标了 DisposalPrevious，其左半区应恢复到首帧的中灰100（读数>=70），"+
			"实际 %d——恢复没生效", got[2])
	}
}

// 对照组：DisposalNone 时左半区就该保留上一帧画的灰，既不该被清也不该被恢复。
//
// 没有这条，上面两条就没有参照——「读数是 20」既能解释成 None 正常，
// 也能解释成 Previous 生效了（错误地被当成正确）。
func TestSplitGIFFramesDisposalNoneKeepsCanvas(t *testing.T) {
	got := disposalProbeReads(t,
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone, gif.DisposalNone})
	if got[2] > 60 {
		t.Errorf("DisposalNone 时帧 3 的左半区应保留帧 2 画的灰20（读数<=60），实际 %d", got[2])
	}
	if got[2] < 10 {
		t.Errorf("左半区读数 %d 过低，像是被清成了透明——None 不该清画布", got[2])
	}
}

// 理由见 maxGIFFramesToComposite 的注释：Disposal 是串行链，
// 400 帧的图会白白合成 400 次只为了取 5 张。
func TestSplitGIFFramesGivesUpOverFrameCap(t *testing.T) {
	delays := make([]int, maxGIFFramesToComposite+1)
	for i := range delays {
		delays[i] = 10
	}
	raw := makeDisposalGIF(32, 32, delays, nil, 255)
	if n := countGIFFrames(raw); n != maxGIFFramesToComposite+1 {
		t.Fatalf("素材应含 %d 帧，实际 %d", maxGIFFramesToComposite+1, n)
	}
	if _, ok := splitGIFFrames(raw, 1024, 5); ok {
		t.Error("超过帧数上限应放弃拆帧（而不是硬合成几百次）")
	}
}

// Compress 对动图仍然必须一律不碰。
//
// 2026-10-01 生产事故的回归钉子：GIF 被 JPEG 重编码成静图，文件还是
// 「一张正常的 jpg」只是不动了。新增的 gif.go 就在同包，隔壁代码顺手
// 改掉这条豁免的概率不低——所以在这里再钉一遍。
func TestCompressStillNeverTouchesGIF(t *testing.T) {
	raw := makeDisposalGIF(600, 600, make([]int, 12), nil, 255)
	if _, _, ok := Compress(raw, 1024); ok {
		t.Error("Compress 绝不能重编码 GIF——那会把动图变成静图")
	}
}

// 不是 GIF 的字节不该被当 GIF 解。
func TestFetchViewsPassesThroughNonGIF(t *testing.T) {
	png := makePNG(40, 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	views, src, err := FetchViews(srv.URL, 1024, 5)
	if err != nil {
		t.Fatalf("静图应正常返回: %v", err)
	}
	if len(views) != 1 || views[0] != src {
		t.Errorf("静图应只给 1 个 view 且等于 src，实际 views=%v", len(views))
	}
	if !strings.HasPrefix(views[0], "data:image/png;base64,") {
		t.Errorf("静图应保持 png 格式，实际 %.40s", views[0])
	}
}

// GIF 走 FetchViews：多个 view，但 src 必须仍是**原始动图**。
//
// 这是本文件最重要的一条。src 是给表情包池用的：如果它跟着拆帧一起变成
// JPEG 静帧，池子里就会多一张不会动的表情包——2026-10-01 那次事故换个
// 入口重演，而且 memepool 现有的测试抓不到（它直接调 Add，不经过这里）。
func TestFetchViewsKeepsOriginalGIFAsSource(t *testing.T) {
	raw := makeDisposalGIF(64, 64, make([]int, 6), nil, 255)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	views, src, err := FetchViews(srv.URL, 1024, 5)
	if err != nil {
		t.Fatalf("抓 GIF 失败: %v", err)
	}
	if len(views) != 5 {
		t.Fatalf("应拆出 5 帧给模型看，实际 %d", len(views))
	}
	if !strings.HasPrefix(src, "data:image/gif;base64,") {
		t.Fatalf("给池子的 src 必须是原始动图，实际 %.40s", src)
	}
	// 逐字节相等：不能有任何重编码
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(src, "data:image/gif;base64,"))
	if err != nil {
		t.Fatalf("src 的 base64 解不开: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Error("src 的字节与原始 GIF 不一致——动图被改写了")
	}
	// view 则是 JPEG 静帧
	if !strings.HasPrefix(views[0], "data:image/jpeg;base64,") {
		t.Errorf("view 应是 JPEG 静帧，实际 %.40s", views[0])
	}
}

// maxFrames<=0 是「不要拆帧」的明确退路。
func TestFetchViewsRespectsZeroMaxFrames(t *testing.T) {
	raw := makeDisposalGIF(64, 64, make([]int, 6), nil, 255)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	views, src, err := FetchViews(srv.URL, 1024, 0)
	if err != nil {
		t.Fatalf("抓 GIF 失败: %v", err)
	}
	if len(views) != 1 || views[0] != src {
		t.Errorf("maxFrames=0 应原样透传 1 帧，实际 %d 帧", len(views))
	}
}
