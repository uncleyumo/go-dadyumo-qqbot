package brain

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memepool"
)

func dataURL(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// 收图：模型报序号 + 描述，图要真的进池
func TestCollectMemesAddsToPool(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e := newTestEngine(t, &recordingSender{}, pool, true)

	cfg := e.store.Get()
	urls := []string{
		dataURL("image/jpeg", []byte("jpeg-frame-1")),
		dataURL("image/jpeg", []byte("jpeg-frame-2")),
	}
	e.collectMemes(cfg, []Collect{{I: 2, D: "无语到翻白眼"}}, urls)

	if pool.Len() != 1 {
		t.Fatalf("应收进 1 张，got %d", pool.Len())
	}
	ms := pool.Browse(time.Now())
	if ms[0].Descr != "无语到翻白眼" {
		t.Errorf("描述应原样入库，got %q", ms[0].Descr)
	}
}

// 序号越界必须忽略，不能拿一个不存在的下标去取图
func TestCollectRejectsOutOfRangeIndex(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e := newTestEngine(t, &recordingSender{}, pool, true)
	cfg := e.store.Get()
	urls := []string{dataURL("image/jpeg", []byte("a"))}

	e.collectMemes(cfg, []Collect{{I: 0, D: "x"}, {I: 5, D: "y"}, {I: -1, D: "z"}}, urls)
	if pool.Len() != 0 {
		t.Errorf("越界序号不该收进任何东西，got %d", pool.Len())
	}
}

// 单轮收编上限：模型一头热报一堆也不能把池子塞爆
func TestCollectRespectsPerRoundCap(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e := newTestEngine(t, &recordingSender{}, pool, true)
	cfg := e.store.Get()

	var urls []string
	var items []Collect
	for i := 0; i < 8; i++ {
		urls = append(urls, dataURL("image/jpeg", []byte{byte(i), 0xFF, 0xD8}))
		items = append(items, Collect{I: int64(i + 1), D: "图"})
	}
	e.collectMemes(cfg, items, urls)
	if pool.Len() != maxCollectPerRound {
		t.Errorf("单轮最多收 %d 张，got %d", maxCollectPerRound, pool.Len())
	}
}

// 池子没开时收编要静默跳过，不能因为「没有池子」把整轮带崩
func TestCollectNoopWhenPoolDisabled(t *testing.T) {
	e := newTestEngine(t, &recordingSender{}, nil, false)
	cfg := e.store.Get()
	e.collectMemes(cfg, []Collect{{I: 1, D: "x"}}, []string{dataURL("image/jpeg", []byte("a"))})
	// 能跑完不 panic 就够了
}

// 坏 data URI 只跳过这一张，后面的照收
func TestCollectSkipsBadDataURL(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e := newTestEngine(t, &recordingSender{}, pool, true)
	cfg := e.store.Get()

	urls := []string{
		"data:image/jpeg;base64,!!!不是base64!!!",
		dataURL("image/jpeg", []byte("good")),
	}
	e.collectMemes(cfg, []Collect{{I: 1, D: "坏的"}, {I: 2, D: "好的"}}, urls)

	if pool.Len() != 1 {
		t.Fatalf("只该收进好的那张，got %d", pool.Len())
	}
	if got := pool.Browse(time.Now())[0].Descr; got != "好的" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeDataURL(t *testing.T) {
	raw := []byte("hello")
	d, mime, err := decodeDataURL(dataURL("image/png", raw))
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || string(d) != "hello" {
		t.Errorf("got %q %q", mime, d)
	}
	for _, bad := range []string{"", "http://x/y.png", "data:image/png,abc", "data:image/png;base64,"} {
		if _, _, err := decodeDataURL(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// normalize 要把没用的 collect 条目剔掉：序号非正或描述为空都收不了
func TestNormalizeFiltersCollect(t *testing.T) {
	d := &Decision{
		Act: "quiet",
		Collect: []Collect{
			{I: 0, D: "没序号"},
			{I: 2, D: "  "},
			{I: 3, D: "无语的猫"},
		},
	}
	normalize(d)
	if len(d.Collect) != 1 || d.Collect[0].I != 3 || d.Collect[0].D != "无语的猫" {
		t.Errorf("got %+v", d.Collect)
	}
	// 闭嘴那轮照样要留下 collect：收图与发不发言无关
	if d.Collect == nil {
		t.Error("quiet 不该清掉 collect")
	}
}

// 描述会被塞进上下文里给模型挑图，必须限长，否则一句长文能把池子描述撑爆
func TestCollectDescrIsLengthCapped(t *testing.T) {
	d := &Decision{Act: "quiet", Collect: []Collect{{I: 1, D: strings.Repeat("长", 200)}}}
	normalize(d)
	// Sanitize 截断时会补一个省略号，所以是 40 字 + 1
	if n := len([]rune(d.Collect[0].D)); n > 41 {
		t.Errorf("描述应截断到 40 字（+省略号），实际 %d", n)
	}
}

// 没给池子挂图时不该出现 collect 相关提示（避免给模型加无用的心智负担）
func TestNoCollectHintWhenPoolDisabled(t *testing.T) {
	if got := (&Engine{}).poolOf(config.NewDefaultStore().Get()); got != nil {
		t.Error("默认配置下池子应为 nil")
	}
}
