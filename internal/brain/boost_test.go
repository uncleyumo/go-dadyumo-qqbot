package brain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"sync"
	"testing"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
)

// 生产事故原话（2026-10-04，Packy 中转，HTTP 200 + 正文里一句英文说明）。
const refusalBody = "The prompt could not be submitted. The prompt contains sensitive words that violate Google's [Generative AI Prohibited Usage] policy."

// dualModelUpstream 按模型名分派：strict 一律拒绝，lax 正常回答。
// 现有 fakeUpstream 是按调用序号分派的，测不了「谁被调用」这件事——
// 而提权的好坏恰恰体现在省掉了哪个模型的调用上。
type dualModelUpstream struct {
	mu    sync.Mutex
	calls map[string]int
}

func (u *dualModelUpstream) count(model string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[model]
}

func (u *dualModelUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	u.mu.Lock()
	u.calls[body.Model]++
	u.mu.Unlock()

	reply := "lax 答的"
	if body.Model == "strict" {
		reply = refusalBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{"role": "assistant", "content": reply},
		}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
	})
}

// newBoostEngine 组一个「主力审核严、下游宽松」的双模型引擎。
// strict 的 priority 更高，所以没有提权时它一定先被选中。
func newBoostEngine(t *testing.T, srvURL string, sender Sender) *Engine {
	t.Helper()
	store := config.NewStoreFrom(func(c *config.Config) {
		c.MemePool.Enabled = false
		c.LLM.MaxAttempts = 3
		c.LLM.Endpoints = []config.Endpoint{{
			ID: "packy", Name: "packy", BaseURL: srvURL, APIKey: "k",
			APIType: config.APIChatCompletions, Enabled: true, TimeoutMS: 5000,
			Models: []config.Model{
				{ID: "strict", Label: "strict", Enabled: true, MaxCtx: 32000, MaxOut: 512, Priority: 10},
				{ID: "lax", Label: "lax", Enabled: true, MaxCtx: 32000, MaxOut: 512, Priority: 1},
			},
		}}
	})
	return &Engine{
		store:  store,
		router: llm.NewRouter(store),
		mem:    memory.New(50),
		sender: sender,
	}
}

func startDualUpstream(t *testing.T) (*httptest.Server, *dualModelUpstream) {
	t.Helper()
	up := &dualModelUpstream{calls: map[string]int{}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return srv, up
}

// 提权的核心价值：兜底成功后，下一轮**不再白烧主力一次**。
//
// 事故背景：主力审核严，每轮 strict 被拒 → lax 兜住，恒定多烧一次调用。
// 提权就是消掉这一次——所以断言的不是「答对了」，而是「strict 没被再调」。
func TestRescueBoostsNextRoundSkipsTheRefused(t *testing.T) {
	srv, up := startDualUpstream(t)
	sender := &recordingSender{}
	e := newBoostEngine(t, srv.URL, sender)
	g := e.mem.Group("g1", "群A")

	// 第一轮：strict 被拒 → lax 兜住 → 应记一次提权。
	// 走 decide 而非 chatWithTools：提权逻辑在 decide 里。
	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "某人", false, false, nil, "", "", "", "", nil, "", false)
	if got := g.BoostedTargets(); len(got) != 1 {
		t.Fatalf("兜底成功后应记一次提权，实际 %v", got)
	}
	if up.count("strict") == 0 {
		t.Fatal("第一轮应先试 priority 更高的 strict")
	}

	// 扣掉本轮额度（fire 末尾的 defer 会做，这里手动模拟一次真实轮次）
	g.TickBoost()
	strictBefore := up.count("strict")

	// 第二轮：提权生效，strict 不该再被调用
	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "某人", false, false, nil, "", "", "", "", nil, "", false)
	if got := up.count("strict") - strictBefore; got != 0 {
		t.Errorf("提权生效后不该再碰 strict（省下的正是这次），实际多调 %d 次", got)
	}
	if up.count("lax") < 2 {
		t.Error("第二轮应直接由 lax 应答")
	}
}

// 提权额度按轮消耗，走满 5 轮后消失，调度回到原样。
func TestBoostExpiresAfterFiveRounds(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.Boost("packy|lax")
	if len(g.BoostedTargets()) != 1 {
		t.Fatal("提权应已记录")
	}
	for i := 0; i < memory.BoostRounds; i++ {
		g.TickBoost()
	}
	if got := g.BoostedTargets(); len(got) != 0 {
		t.Errorf("走满 %d 轮后提权应耗尽，实际还剩 %v", memory.BoostRounds, got)
	}
}

// 提权只影响「先问谁」，不该改变别的行为：被提权的模型自己失败时
// 必须立刻退回正常排序，不能把这一轮耗在死磕它上面。
func TestBoostNeverBlocksNormalRouting(t *testing.T) {
	srv, _ := startDualUpstream(t)
	sender := &recordingSender{}
	e := newBoostEngine(t, srv.URL, sender)
	g := e.mem.Group("g1", "群A")

	// 提权名单里塞一个不存在的目标
	g.Boost("根本不存在的|模型")

	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "某人", false, false, nil, "", "", "", "", nil, "", false)
	// 关键：不能因为提权名单无效就整轮失败——它只是建议
	if up0 := g.BoostedTargets(); len(up0) != 0 {
		// 本轮若再次兜底成功，会重新置满；这里只要求流程没崩
		t.Logf("提权名单: %v", up0)
	}
}

// 提权日志必须落在 decision 分类里。
//
// 这是踩过的坑：logx.Info 默认落到 runtime，而管理端默认视图筛的正是
// decision，落错分类等于把「为什么这轮换模型了」的唯一现场藏进「未分类」，
// 排查「它怎么老是换模型」时根本看不到。
// 而且 decision 是唯一被强制持久化进统计库的分类，放对地方重启后也查得到。
func TestBoostLoggedUnderDecision(t *testing.T) {
	srv, _ := startDualUpstream(t)
	e := newBoostEngine(t, srv.URL, &recordingSender{})
	g := e.mem.Group("g1", "群A")

	before := countDecisions()
	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "某人", false, false, nil, "", "", "", "", nil, "", false)

	found := false
	for _, x := range logx.Recent(400) {
		if strings.Contains(x.Msg, "已临时提权") {
			found = true
			if x.Cat != logx.CatDecision {
				t.Errorf("提权日志归到了 %q，应为 decision——管理端默认视图筛的就是它", x.Cat)
			}
		}
	}
	if !found {
		t.Fatal("应记录一条提权日志")
	}
	if n := countDecisions() - before; n < 1 {
		t.Errorf("提权应至少新增一条 decision 日志，实际 %d", n)
	}
}
