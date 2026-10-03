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

// TestLiveEmojiReadable 用真实模型验证：QQ 表情还原成 [中文名] 之后，
// 模型能读懂，而不是像修复前那样回「搁这刷表情包呢」。
//
// 事故原文（2026-10-01 13:18，测试群一号）：群友乙和群友甲
// 连发一串表情，模型回「搁这刷表情包呢」——它只看出「有东西」，
// 认不出那是流泪还是呲牙，因为入站是 <faceType=1,faceId="5",ext="base64"/>。
//
// 需要环境变量 QQBOT_LIVE_CONFIG 指向一份真实 config.json。
func TestLiveEmojiReadable(t *testing.T) {
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

	g := memory.New(cfg.Brain.MaxHistory).Group("live-emoji", "测试群一号")
	g.TouchMember("openid-self", "羽沫老爹")
	g.TouchMember("openid-qingniao", "群友乙")
	g.TouchMember("openid-dashu", "群友甲")

	// 修复后模型看到的内容：圆括号包着的中文表情名
	lines := []memory.Line{
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（你懂的）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（出去玩）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（自信学霸）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（流泪）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（秋秋赏月）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（惊吓）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（微笑）"},
		{Role: memory.RoleUser, Name: "群友甲", OpenID: "openid-dashu", Content: "（闭嘴）"},
	}
	hist := TrimHistory(lines, 2000)

	system := systemPrompt(cfg, g, MoodSignal{}, "", "", "群友甲")
	user := userPrompt(cfg, g, hist,
		buildTrigger(false, false, false, false, false, false, false, 6, "群友甲", false), "")

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
	t.Logf("内心: %s", dec.OS)
	t.Logf("正文: %s", dec.Text)

	// 关键验收：模型必须把表情读成具体的情绪，而不是当成「刷表情包」。
	// 第一版用方括号 [流泪] 时它的原话是「表情包批发呢你」「有事说事，别光发图」——
	// 说明它把方括号和 [图片] 归成了一类「媒体附件」，压根没读出情绪。
	if dec.Act == "say" {
		for _, bad := range []string{"表情包批发", "别光发图", "有事说事", "发这么多"} {
			if strings.Contains(dec.Text, bad) {
				t.Errorf("模型仍把表情当「刷图/刷包」处理（命中 %q）: %q", bad, dec.Text)
			}
		}
		t.Logf("已检查：回复未落入「刷图/刷包」话术")
	}

	// 无论说什么，都不能出现平台标记残留
	for _, s := range dropEmpty(DedupeMentions(SplitSegments(dec.Text, cfg.Speak.MaxSegChars, cfg.Speak.MaxSegments))) {
		if strings.ContainsAny(s, "<>") || strings.Contains(s, "faceType") || strings.Contains(s, "eyJ0") {
			t.Errorf("输出里有平台标记残留: %q", s)
		}
	}
}
