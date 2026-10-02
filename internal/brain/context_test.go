package brain

import (
	"strings"
	"testing"
	"time"

	"dadyumo/internal/memory"
)

func TestEstimateTokensChinese(t *testing.T) {
	// 中文按约 1 token/字 估，再加 10% 边际
	n := EstimateTokens("你好啊")
	if n < 3 || n > 5 {
		t.Fatalf("3 个汉字应估在 3~5 token，实际 %d", n)
	}
}

func TestEstimateTokensEnglishCheaperThanChinese(t *testing.T) {
	cn := EstimateTokens(strings.Repeat("字", 30))
	en := EstimateTokens(strings.Repeat("a", 30))
	if en >= cn {
		t.Fatalf("同样长度的英文应比中文便宜：中文=%d 英文=%d", cn, en)
	}
}

func TestTrimHistoryKeepsNewest(t *testing.T) {
	lines := []memory.Line{
		{Content: "第一条", Name: "A"},
		{Content: "第二条", Name: "B"},
		{Content: "第三条", Name: "C"},
	}
	// 预算只够放下最后一条
	got := TrimHistory(lines, EstimateTokens("第三条")+12)
	if len(got) != 1 {
		t.Fatalf("预算不足时应只保留最新一条，实际 %d 条: %#v", len(got), got)
	}
	if got[0].Content != "第三条" {
		t.Fatalf("应保留最新的那条，实际 %q", got[0].Content)
	}
}

func TestTrimHistoryKeepsOrder(t *testing.T) {
	lines := []memory.Line{
		{Content: "一", Name: "A"},
		{Content: "二", Name: "B"},
		{Content: "三", Name: "C"},
	}
	got := TrimHistory(lines, 10000)
	if len(got) != 3 {
		t.Fatalf("预算充足时应全部保留，实际 %d 条", len(got))
	}
	// 裁剪是倒序收集的，必须翻回正序，否则模型会看到倒着的对话
	if got[0].Content != "一" || got[1].Content != "二" || got[2].Content != "三" {
		t.Fatalf("顺序被弄反了: %#v", got)
	}
}

func TestTrimHistoryZeroBudget(t *testing.T) {
	if got := TrimHistory([]memory.Line{{Content: "x"}}, 0); len(got) != 0 {
		t.Fatalf("零预算应返回空，实际 %#v", got)
	}
}

func TestContextBudgetRespectsSmallModel(t *testing.T) {
	// 32K 的小模型，配置预算却写了 60000，应以模型为准
	got := ContextBudget(60000, 32768, 512)
	if got >= 60000 {
		t.Fatalf("应受模型上下文约束，实际 %d", got)
	}
	if got <= 0 {
		t.Fatalf("预算不应为负: %d", got)
	}
}

func TestContextBudgetFloor(t *testing.T) {
	// 极端小的模型上下文也不能算出个没法用的预算
	if got := ContextBudget(100, 2048, 512); got < 800 {
		t.Fatalf("应有最小兜底，实际 %d", got)
	}
}

func TestTrimHistoryBudgetRealistic(t *testing.T) {
	// 模拟一个活跃群：30 条消息，每条约 32 字
	lines := make([]memory.Line, 0, 30)
	for i := 0; i < 30; i++ {
		lines = append(lines, memory.Line{
			TS:      time.Now(),
			Role:    memory.RoleUser,
			Name:    "群友",
			Content: strings.Repeat("这事我觉得挺有意思的，你们怎么看", 2),
		})
	}

	// 6000 token 是默认预算，装得下这 30 条——预算够时就不该乱剪
	if got := TrimHistory(lines, 6000); len(got) != 30 {
		t.Fatalf("预算充足时不应裁剪，实际保留 %d 条", len(got))
	}

	// 换到真正紧张的小上下文（比如只剩 800 token 可用）时必须剪
	got := TrimHistory(lines, 800)
	if len(got) == 0 || len(got) == 30 {
		t.Fatalf("预算紧张时应裁掉一部分，实际保留 %d 条", len(got))
	}
	total := 0
	for _, l := range got {
		total += EstimateTokens(l.Content) + EstimateTokens(l.Name) + 8
	}
	if total > 800 {
		t.Fatalf("裁剪后仍超预算：%d > 800", total)
	}
	if got[len(got)-1].Content != lines[len(lines)-1].Content {
		t.Fatal("裁剪后最后一条必须是最新的那条")
	}
	t.Logf("30 条 × 32 字，800 token 预算下保留 %d 条", len(got))
}
