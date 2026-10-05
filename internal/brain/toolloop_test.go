package brain

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
)

// 工具循环的验收标准。
//
// 最要紧的一条：**工具轮绝不发送任何消息**。
// 工具轮发出去的东西在最终轮失败时无法回滚，重试就会重复发言——
// 「内容对但发了两次」在群里比「这次没说话」糟得多。

type recordingSender struct {
	mu     sync.Mutex
	texts  []string
	quoted []string
	imgs   []int64
}

// SendGroup 满足 brain.Sender（3 参数，无挂载人）
func (r *recordingSender) SendGroup(ctx context.Context, groupID, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts = append(r.texts, content)
	return nil
}

// SendGroupTo 记录带挂载人的那条（brain 实际走这条）
func (r *recordingSender) SendGroupTo(ctx context.Context, groupID, content, replyTo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts = append(r.texts, content)
	return nil
}

// SendGroupQuote 记进 quoted：引用与普通发送在测试里必须分得开，
// 否则「模型开口要了引用」和「程序擅自加的引用」测不出区别。
func (r *recordingSender) SendGroupQuote(ctx context.Context, groupID, content, replyTo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quoted = append(r.quoted, content)
	r.texts = append(r.texts, content)
	return nil
}

func (r *recordingSender) SendImage(ctx context.Context, groupID string, data []byte, mime, reply string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.imgs = append(r.imgs, int64(len(data)))
	return nil
}

func (r *recordingSender) count() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.texts), len(r.imgs)
}

// newTestEngine 组一个不依赖真实 LLM 的引擎。
// memeOn 控制表情包功能开关——默认配置里它是关的（依赖 MinIO 与
// QQ 富媒体两条外部链路，没通就不该带着上线），所以要测池子行为得显式打开。
func newTestEngine(t *testing.T, sender Sender, pool *memepool.Pool, memeOn bool) *Engine {
	t.Helper()
	store := config.NewStoreFrom(func(c *config.Config) { c.MemePool.Enabled = memeOn })
	return &Engine{
		store:  store,
		mem:    memory.New(50),
		sender: sender,
		memes:  pool,
	}
}

// 池子空时不给出工具——模型没必要去调一个必然返回空列表的东西
func TestToolDefsHiddenWhenPoolEmpty(t *testing.T) {
	e := newTestEngine(t, &recordingSender{}, nil, false)
	if got := e.toolDefs(nil); got != nil {
		t.Errorf("池子未配时不该给工具，got %d 个", len(got))
	}

	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e2 := newTestEngine(t, &recordingSender{}, pool, true)
	if got := e2.toolDefs(nil); got != nil {
		t.Errorf("池子空时不该给工具，got %d 个", len(got))
	}
}

// 池子里有图才给工具
func TestToolDefsShownWhenPoolHasMemes(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "笑死我了", time.Now())
	e := newTestEngine(t, &recordingSender{}, pool, true)
	tools := e.toolDefs(nil)
	if len(tools) != 1 || tools[0].Name != "browse_meme_pool" {
		t.Errorf("池子有图时应给出工具，got %+v", tools)
	}
}

// 工具轮返回的列表里只有「ID + 描述」，不含任何权重/评分——
// 心情完全靠排序表达，塞进上下文只会增加模型的认知负担
func TestBrowseMemePoolNoWeightsInContext(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	id, _ := pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "笑死我了", time.Now())
	pool.MarkUsed(id, time.Now())
	pool.ApplyQuality(map[int64]float64{id: 0.95})

	e := newTestEngine(t, &recordingSender{}, pool, true)
	out := e.browseMemePool(e.store.Get(), `{"mood":"无语"}`)

	if !strings.Contains(out, "笑死我了") {
		t.Errorf("应含描述，got %q", out)
	}
	if !strings.Contains(out, "排越前的你此刻越愿意用") {
		t.Errorf("应说明排序含义，got %q", out)
	}
	for _, leak := range []string{"0.95", "quality", "affection", "uses"} {
		if strings.Contains(out, leak) {
			t.Errorf("不该把 %q 写进上下文: %q", leak, out)
		}
	}
}

// 池子是空的：明确告诉模型用文字回答，别让它瞎试
func TestBrowseMemePoolEmpty(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e := newTestEngine(t, &recordingSender{}, pool, true)
	out := e.browseMemePool(e.store.Get(), "")
	if !strings.Contains(out, "空") || !strings.Contains(out, "文字") {
		t.Errorf("应明说池子是空的并建议用文字，got %q", out)
	}
}

// 未知工具不该让整轮崩，返回一句说明让模型改用文字
func TestUnknownToolDegradesGracefully(t *testing.T) {
	e := newTestEngine(t, &recordingSender{}, nil, false)
	out := e.runTool(context.Background(), e.store.Get(), nil, llm.ToolCall{Name: "delete_everything"})
	if out == "" {
		t.Error("未知工具应返回一句说明，不是空串")
	}
}

// 情绪参数解析不了也不能出事
func TestParseMoodArgTolerant(t *testing.T) {
	cases := map[string]string{
		`{"mood":"无语"}`:         "无语",
		`{"mood": "开心", "x":1}`: "开心",
		`{}`:                    "",
		``:                      "",
		`{坏掉的json`:              "",
		`{"mood":"非常非常非常非常非常长的情绪"}`: "非常非常非常非常非常长的情绪",
	}
	for in, want := range cases {
		got := parseMoodArg(in)
		if want == "" {
			if got != "" {
				t.Errorf("%q 应返回空，got %q", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%q 应得 %q，got %q", in, want, got)
		}
	}
	// 超长要截断，防着占上下文
	long := parseMoodArg(`{"mood":"` + strings.Repeat("长", 40) + `"}`)
	if len([]rune(long)) > 16 {
		t.Errorf("超长情绪应截断到 16 字，实际 %d", len([]rune(long)))
	}
}

// deliver：文字与表情包按 blocks 顺序发，且共用 5 次额度
func TestDeliverMixedOrder(t *testing.T) {
	sender := &recordingSender{}
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	id, _ := pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "笑死", time.Now())
	e := newTestEngine(t, sender, pool, true)
	e.SetMemePool(pool, sender)

	cfg := e.store.Get()
	g := e.mem.Group("g1", "测试群")

	blocks := []Block{
		{T: BlockTypeText, C: "看这个"},
		{T: BlockTypeImg, ID: id},
		{T: BlockTypeText, C: "补一句"},
	}
	sent := e.deliver(cfg, g, blocks, "", false)

	if sent != 3 {
		t.Errorf("应发出 3 条，got %d", sent)
	}
	if len(sender.texts) != 2 || len(sender.imgs) != 1 {
		t.Errorf("文字图片各应按序发出，texts=%v imgs=%v", sender.texts, sender.imgs)
	}
	if sender.texts[0] != "看这个" || sender.texts[1] != "补一句" {
		t.Errorf("文字顺序不对: %v", sender.texts)
	}
}

// 图片发送失败要退化成纯文本，不能让整轮泡汤
func TestDeliverImageFailureDegrades(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	id, _ := pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "笑死", time.Now())
	sender := &recordingSender{}
	e := newTestEngine(t, sender, pool, true)
	// 故意不 SetMemePool 的 imgSender——图片通道缺失
	e.SetMemePool(pool, nil)

	cfg := e.store.Get()
	g := e.mem.Group("g1", "测试群")
	blocks := []Block{
		{T: BlockTypeText, C: "看这个"},
		{T: BlockTypeImg, ID: id},
	}
	sent := e.deliver(cfg, g, blocks, "", false)
	// 图发不出去，但文字必须发出去
	if len(sender.texts) != 1 || sender.texts[0] != "看这个" {
		t.Errorf("文字应照常发出，texts=%v", sender.texts)
	}
	if sent < 1 {
		t.Errorf("至少应送出文字，got %d", sent)
	}
}

// MaxImagesPerReply 限制：模型填 5 张图也只发配置的数量
func TestDeliverRespectsImageLimit(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	var ids []int64
	for i := 0; i < 5; i++ {
		id, _ := pool.Add([]byte("fake-jpeg-"+string(rune('a'+i))), "image/jpeg", "图", time.Now())
		ids = append(ids, id)
	}
	sender := &recordingSender{}
	e := newTestEngine(t, sender, pool, true)
	e.SetMemePool(pool, sender)

	cfg := e.store.Get()
	cfg.MemePool.Enabled = true
	cfg.MemePool.MaxImagesPerReply = 1

	var blocks []Block
	for _, id := range ids {
		blocks = append(blocks, Block{T: BlockTypeImg, ID: id})
	}
	g := e.mem.Group("g1", "测试群")
	e.deliver(cfg, g, blocks, "", false)

	if len(sender.imgs) != 1 {
		t.Errorf("配置说 1 张就只该发 1 张，got %d", len(sender.imgs))
	}
}

// 发过图要在记忆里留痕，否则模型下一轮不知道自己发过，会反复甩同一张
func TestDeliverRecordsImageInMemory(t *testing.T) {
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	id, _ := pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "笑死", time.Now())
	sender := &recordingSender{}
	e := newTestEngine(t, sender, pool, true)
	e.SetMemePool(pool, sender)

	cfg := e.store.Get()
	g := e.mem.Group("g1", "测试群")
	e.deliver(cfg, g, []Block{{T: BlockTypeImg, ID: id}}, "", false)

	lines := g.Recent(20)
	found := false
	for _, l := range lines {
		if strings.Contains(l.Content, "表情包") {
			found = true
		}
	}
	if !found {
		t.Error("发过图应在记忆里留痕")
	}
}

// memStorageStub 测试用存储
type memStorageStub struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memStorageStub) Put(k string, d []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[k] = d
	return nil
}

func (m *memStorageStub) Get(k string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[k]
	if !ok {
		return nil, "", memepool.ErrNotFound
	}
	return d, "image/jpeg", nil
}

func (m *memStorageStub) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}
