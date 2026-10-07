package brain

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memory"
)

// liveSender 把「真发出去」换成「记下来」，这样可以在不打扰任何真实群的前提下
// 把决策链路整条跑通：提示词 → 路由 → 上游 → 解析 → 决定发给谁。
type liveSender struct {
	mu   sync.Mutex
	sent []struct {
		text   string
		toOpen string
	}
}

func (s *liveSender) SendGroup(ctx context.Context, groupID, content string) error {
	return s.SendGroupTo(ctx, groupID, content, "")
}

func (s *liveSender) SendGroupTo(ctx context.Context, groupID, content, replyToOpenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, struct {
		text   string
		toOpen string
	}{content, replyToOpenID})
	return nil
}

func (s *liveSender) SendGroupQuote(ctx context.Context, groupID, content, replyToOpenID string) error {
	return s.SendGroupTo(ctx, groupID, content, replyToOpenID)
}

// TestLiveDecisionAgainstRealModel 拿真实配置打真实模型，验证整条决策链路。
//
// 需要环境变量 QQBOT_LIVE_CONFIG 指向一份真实 config.json；没设置就跳过。
// 它会真的花钱调 LLM，所以默认不进常规测试。
func TestLiveDecisionAgainstRealModel(t *testing.T) {
	cfgPath := os.Getenv("QQBOT_LIVE_CONFIG")
	if cfgPath == "" {
		t.Skip("未设置 QQBOT_LIVE_CONFIG，跳过真实模型链路测试")
	}
	if testing.Short() {
		t.Skip("short 模式跳过真实调用")
	}

	store, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg := store.Get()

	router := llm.NewRouter(store)
	sender := &liveSender{}
	memStore := memory.New(cfg.Brain.MaxHistory)
	engine := NewEngine(store, router, memStore, sender)

	// 构造一个三人小群：李四 @ 机器人问「在吗」，王五同时说了句别的。
	// 这正是历史上「回复挂错人」最容易发生的场面。
	g := memStore.Group("live-test-group", "测试群")
	g.TouchMember("openid-botmaster", "羽沫老爹")
	g.TouchMember("openid-li", "李四")
	g.TouchMember("openid-wang", "王五")

	for _, ev := range []*Event{
		{GroupID: "live-test-group", OpenID: "openid-wang", Name: "王五", Content: "今天群里怎么没人说话"},
		{GroupID: "live-test-group", OpenID: "openid-li", Name: "李四", Content: "在吗"},
	} {
		engine.OnMessage(ev)
	}

	// 攒批是 debounce 的，这里直接触发一次决策
	engine.stateOf("live-test-group")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	g2 := memStore.Group("live-test-group", "")
	lines := TrimHistory(g2.Recent(cfg.Brain.MaxHistory), 3000)
	system := systemPrompt(cfg, g2, MoodSignal{}, "", "", "李四")
	user := userPrompt(cfg, g2, lines, buildTrigger(true, false, false, false, false, false, false, 2, "李四", false), "", time.Now())

	if strings.Contains(user, "【") && strings.Contains(user, "（主人）") {
		t.Log("提示词已包含主人标记")
	}

	res, cerr := router.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		},
		Temperature: cfg.Brain.Temperature,
		MaxTokens:   cfg.Brain.MaxOutTokens,
	})
	if cerr != nil {
		t.Fatalf("真实调用失败: %v", cerr)
	}
	t.Logf("模型=%s 用时=%.0fms prompt=%d out=%d", res.Model, res.LatencyMS, res.PromptTokens, res.OutputTokens)

	dec, issue := ParseDecision(res.Content)
	t.Logf("act=%q to=%q tone=%q 解析=%v", dec.Act, dec.To, dec.Tone, issue)
	t.Logf("决策原文: %s", truncate(res.Content, 300))

	if issue == ParseNoise {
		t.Fatal("输出被判为协议残渣，说明输出被截断或模型格式失控")
	}

	if dec.Act == "say" {
		if strings.TrimSpace(dec.Text) == "" {
			t.Fatal("act=say 但没有内容")
		}
		replyTo := engine.resolveReplyTarget(g2, cfg, dec.To, "openid-li")
		t.Logf("决定回给: to=%q -> openid=%q", dec.To, replyTo)
		if replyTo != "" && replyTo != "openid-li" && replyTo != "openid-wang" {
			t.Errorf("回复对象不在群成员里: %q", replyTo)
		}
		// 确认渲染出来的名字能反查回本人——这是整个改动的前提
		if _, ok := lookupMemberOpenID(g2, dec.To); dec.To != "" && !ok {
			t.Logf("注意: 模型填的 to=%q 在成员表里反查不到，会退回触发者", dec.To)
		}
		engine.speak(cfg, g2, dec.Text, replyTo)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	t.Logf("共发出 %d 条", len(sender.sent))
	for _, s := range sender.sent {
		t.Logf("  -> %q (挂给 %s)", s.text, s.toOpen)
	}
}
