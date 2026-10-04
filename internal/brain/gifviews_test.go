package brain

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/memepool"
)

// makeTestGIF 造一张能被 imgproc 认出来的多帧动图。
//
// 帧内容必须**真的不一样**（逐帧不同灰阶），否则即使把整张动图换成一张
// 静帧，很多断言照样会通过——那正是 2026-10-01 那次事故难被发现的原因：
// 出来的文件「是一张正常的 jpg」，只是不动了，肉眼扫代码看不出来。
func makeTestGIF(w, h, frames int) []byte {
	g := &gif.GIF{LoopCount: 0}
	pal := make(color.Palette, 256)
	for c := range pal {
		pal[c] = color.RGBA{uint8(c), uint8(c), uint8(c), 255}
	}
	for i := 0; i < frames; i++ {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		lvl := uint8(20 * (i + 1))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				p.SetColorIndex(x, y, lvl)
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 50)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// 动图给模型看的是若干张静态帧，但**给池子的 src 必须逐字节等于原动图**。
//
// 这条是本轮改动里最要紧的防线。dataURLs 与 poolSrcs 一一对应但内容不同：
// 前者是拆出来的 JPEG 静帧，后者是原始 GIF。
// 若 collectMemes 拿 dataURLs 取图，模型报「第 3 张有梗」时
// 池子里会存进一张不会动的图——2026-10-01 那次事故换个入口重演，
// 而 memepool 自己的测试抓不到（它直接调 Add，不经过 dataURLs 这条路）。
func TestGIFFrameViewsKeepOriginalForPool(t *testing.T) {
	raw := makeTestGIF(48, 48, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	e := newTestEngine(t, &recordingSender{}, nil, false)
	views, srcs := e.fetchImages([]string{srv.URL}, 3, 1024, 5)

	if len(views) != 5 {
		t.Fatalf("动图应拆出 5 帧给模型看，实际 %d 帧", len(views))
	}
	if len(srcs) != len(views) {
		t.Fatalf("srcs 必须与 views 逐下标对应，实际 views=%d srcs=%d", len(views), len(srcs))
	}
	for i, v := range views {
		if !strings.HasPrefix(v, "data:image/jpeg;base64,") {
			t.Errorf("第 %d 个 view 应是 JPEG 静帧，实际 %.40s", i, v)
		}
		if !strings.HasPrefix(srcs[i], "data:image/gif;base64,") {
			t.Fatalf("第 %d 个 src 必须是原始动图，实际 %.40s", i, srcs[i])
		}
		if srcs[i] != srcs[0] {
			t.Errorf("同一张图拆出的各帧应共享同一个 src，第 %d 个不同", i)
		}
	}
	// 帧内容确实逐帧不同——静态化会立刻被这条抓到
	uniq := map[string]bool{}
	for _, v := range views {
		if uniq[v] {
			t.Error("拆出的帧内容完全相同，说明动图被压成了同一张图")
		}
		uniq[v] = true
	}
}

// 端到端：模型报「第 2 张有梗」，池子里必须是**会动的**那张 GIF。
func TestCollectMemeFromGIFFrameStoresAnimatedGIF(t *testing.T) {
	stub := &memStorageStub{}
	pool := memepool.New(memepool.DefaultConfig(), stub)
	e := newTestEngine(t, &recordingSender{}, pool, true)

	raw := makeTestGIF(48, 48, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	cfg := e.store.Get()
	views, srcs := e.fetchImages([]string{srv.URL}, 3, 1024, 5)
	if len(views) < 2 {
		t.Fatalf("素材不足：只拆出 %d 帧", len(views))
	}
	// 模型报的是「第 2 张」——在 views 的序号空间里
	e.collectMemes(cfg, []Collect{{I: 2, D: "会动的猫"}}, views, srcs)

	if pool.Len() != 1 {
		t.Fatalf("应收进 1 张，got %d", pool.Len())
	}
	ms := pool.Browse(time.Now())
	if ms[0].Descr != "会动的猫" {
		t.Errorf("描述应原样入库，got %q", ms[0].Descr)
	}
	// 关键断言：池子里存进去的必须是会动的 GIF 字节，不是 JPEG 静帧。
	// 逐字节比对原图——静帧化会立刻被抓到。
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.data) != 1 {
		t.Fatalf("存储里应有 1 个对象，实际 %d", len(stub.data))
	}
	for _, got := range stub.data {
		if !bytes.Equal(got, raw) {
			t.Errorf("入池的字节应与原始 GIF 逐字节一致，实际存了 %d 字节、前缀 %.10q",
				len(got), string(got[:min(10, len(got))]))
		}
	}
}

// GIF 帧走独立额度，不占 max_images_per_call。
//
// limit=1 但 gifFrames=5：一张动图就该给满 5 帧，不被 limit 砍成 1 帧。
// 这是用户 2026-10-05 明确选的口径——动图不该把同批次的静图全挤掉。
func TestGIFFramesDoNotConsumeImageBudget(t *testing.T) {
	raw := makeTestGIF(48, 48, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	e := newTestEngine(t, &recordingSender{}, nil, false)
	views, srcs := e.fetchImages([]string{srv.URL}, 1, 1024, 5)
	if len(views) != 5 {
		t.Errorf("gifFrames=5 时不该被 limit=1 砍成 1 帧，实际 %d 帧", len(views))
	}
	if len(srcs) != 5 {
		t.Errorf("srcs 也应同步有 5 项，实际 %d", len(srcs))
	}
}

// 静图不受影响：仍占 limit，且 src 与 view 是同一张。
func TestStillImagesUnaffectedByGIFFrames(t *testing.T) {
	png := makeTestPNG(40, 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	e := newTestEngine(t, &recordingSender{}, nil, false)
	views, srcs := e.fetchImages([]string{srv.URL}, 3, 1024, 5)
	if len(views) != 1 || len(srcs) != 1 {
		t.Fatalf("静图应只产出 1 项，实际 views=%d srcs=%d", len(views), len(srcs))
	}
	if views[0] != srcs[0] {
		t.Error("静图没有「拆帧版 vs 原图」之分，两者应完全相同")
	}
}

// gif_frames=0 是明确的「不拆」退路：动图原样透传，只给 1 帧。
func TestZeroGIFFramesKeepsOriginal(t *testing.T) {
	raw := makeTestGIF(48, 48, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	e := newTestEngine(t, &recordingSender{}, nil, false)
	views, srcs := e.fetchImages([]string{srv.URL}, 3, 1024, 0)
	if len(views) != 1 {
		t.Fatalf("gifFrames=0 时应原样透传 1 帧，实际 %d", len(views))
	}
	if views[0] != srcs[0] {
		t.Error("不拆帧时 view 与 src 应是同一串")
	}
	if !strings.HasPrefix(views[0], "data:image/gif;base64,") {
		t.Errorf("不拆帧时应保持 GIF 原样，实际 %.40s", views[0])
	}
}

// 多张图时，两边的下标必须始终对齐——错位一次就是「收错了图」。
func TestViewAndSrcStayAlignedAcrossMixedImages(t *testing.T) {
	gif1 := makeTestGIF(48, 48, 6)
	png := makeTestPNG(40, 40)
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		if hit == 1 {
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write(gif1)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	e := newTestEngine(t, &recordingSender{}, nil, false)
	views, srcs := e.fetchImages([]string{srv.URL, srv.URL}, 10, 1024, 5)
	if len(views) != len(srcs) {
		t.Fatalf("两边长度必须一致，views=%d srcs=%d", len(views), len(srcs))
	}
	// 前 5 项属于动图（src 是 GIF），后 1 项是静图
	for i := 0; i < 5; i++ {
		if !strings.HasPrefix(srcs[i], "data:image/gif;base64,") {
			t.Errorf("第 %d 项应仍属动图，实际 %.40s", i, srcs[i])
		}
	}
	if !strings.HasPrefix(srcs[5], "data:image/png;base64,") {
		t.Errorf("第 5 项应是静图，实际 %.40s", srcs[5])
	}
}

func makeTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 3), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
