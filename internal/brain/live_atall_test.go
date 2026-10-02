package brain

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memory"
)

// TestLiveAtAllNotLeaked 用真实模型验证：@全体成员 不会再被模型当成话术说出来。
//
// 事故原文（2026-10-01 13:14/13:16/13:18，测试群一号）：
// 用户说「把今天该打游戏的人都艾特出来」，模型回
// 「你自己@all不就完了」「剩下那帮人你@all自己喊」。
//
// 修复是双保险：提示词明确说「你没有 @ 任何人的能力」，
// 出口 stripAtAll 硬删。这里验的是提示词那一半——
// 如果模型听话，stripAtAll 就永远用不上（那是给不听话时准备的）。
//
// 需要环境变量 QQBOT_LIVE_CONFIG 指向一份真实 config.json。
func TestLiveAtAllNotLeaked(t *testing.T) {
	cfgPath := os.Getenv("QQBOT_LIVE_CONFIG")
	if cfgPath == "" {
		t.Skip("未设置 QQBOT_LIVE_CONFIG，跳过真实调用")
	}
	store, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg := store.Get()
	router := llm.NewRouter(store)

	g := memory.New(cfg.Brain.MaxHistory).Group("live-atall", "测试群一号")
	g.TouchMember("openid-self", "羽沫老爹")
	g.TouchMember("openid-laoshan", "LV100群主")

	// 完整重放事故现场：用户 @ 了全体成员要机器人去艾特人
	lines := []memory.Line{
		{Role: memory.RoleUser, Name: "LV100群主", OpenID: "openid-laoshan", Content: "@全体成员"},
		{Role: memory.RoleUser, Name: "LV100群主", OpenID: "openid-laoshan",
			Content: "@羽沫老爹 你把今天该打游戏的人都艾特出来"},
		{Role: memory.RoleUser, Name: "LV100群主", OpenID: "openid-laoshan", Content: "求求你帮忙艾特出来"},
		{Role: memory.RoleUser, Name: "群友乙", OpenID: "openid-qingniao", Content: "[流泪][呲牙]"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu",
			Content: "[汪汪]"},
	}
	hist := TrimHistory(lines, 2000)

	system := systemPrompt(cfg, g, MoodSignal{}, "", "", "LV100群主")
	user := userPrompt(cfg, g, hist,
		buildTrigger(true, false, false, true, false, 5, "LV100群主", true), "")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

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
	t.Logf("act=%q to=%q 解析=%v", dec.Act, dec.To, issue)
	t.Logf("决策原文: %s", truncate(res.Content, 300))

	if dec.Act != "say" {
		t.Logf("模型选择闭嘴（act=%q），@all 泄漏无从发生，通过", dec.Act)
		return
	}

	// 走一遍真实出口清理
	segs := dropEmpty(DedupeMentions(SplitSegments(dec.Text, cfg.Speak.MaxSegChars, cfg.Speak.MaxSegments)))
	for _, s := range segs {
		t.Logf("最终发出: %q", s)
		if atAllInOutput.MatchString(s) {
			t.Errorf("出口兜底没拦住 @全体成员: %q", s)
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("清出了空段: %#v", segs)
		}
	}
	t.Logf("共 %d 条", len(segs))
}
