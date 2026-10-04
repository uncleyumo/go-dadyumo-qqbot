package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/config"
)

// 全部目标拒绝时发的那句话，**绝不能**是上游的拒绝说明。
//
// 2026-10-04 生产事故的核心不是「发错一句」，而是那句话被 recordSent
// 写回了记忆——于是含「sensitive words」的英文进了每一轮上下文，
// 下一轮继续触发拒绝、继续写回，自我复制（memory.json 里 12 行有 8 行是它）。
// 所以这条测试盯的是「发出去的每个字都必须来自配置好的中文兜底池」。
func TestDeflectOnlySendsConfiguredFallback(t *testing.T) {
	sender := &recordingSender{}
	e := newTestEngine(t, sender, nil, false)

	pool := []string{"少发这种，容易把我号封了", "不是哥们这种都能发吗？", "牛逼"}
	cfg := config.Config{}
	cfg.Persona.FallbackLines = pool

	// 多跑几轮覆盖随机抽取（deliver 每条有一次拟真的发送延迟，
	// 所以这里只跑到「三条都出现过」为止，不铺满 30 次）
	for i := 0; i < 12; i++ {
		e.deflect(cfg, e.mem.Group("g1", "群A"))
	}

	sender.mu.Lock()
	got := append([]string(nil), sender.texts...)
	sender.mu.Unlock()

	if len(got) == 0 {
		t.Fatal("配了兜底话术就该发一句")
	}
	allowed := map[string]bool{}
	for _, p := range pool {
		allowed[p] = true
	}
	for _, s := range got {
		if !allowed[s] {
			t.Errorf("发出了不在兜底池里的话: %q", s)
		}
		// 这是事故里真正致命的形态：任何带 sensitive/prompt 的痕迹都不能出去
		lower := strings.ToLower(s)
		for _, bad := range []string{"sensitive", "prompt", "policy", "submitted", "usage"} {
			if strings.Contains(lower, bad) {
				t.Errorf("兜底话术里混进了上游原文痕迹 %q: %q", bad, s)
			}
		}
	}
}

// 没配兜底话术就闭嘴——总比发一句空气强。
// 注意这里**不能**发空消息：发出去在 QQ 里是一条空白。
func TestDeflectSilentWhenNoFallbackConfigured(t *testing.T) {
	sender := &recordingSender{}
	e := newTestEngine(t, sender, nil, false)

	e.deflect(config.Config{}, e.mem.Group("g1", "群A"))

	if n, _ := sender.count(); n != 0 {
		t.Errorf("没配兜底话术时不应发任何消息，实际发了 %d 条", n)
	}
}

// 池子里混进空串时不能把它发出去。
func TestDeflectSkipsBlankLines(t *testing.T) {
	sender := &recordingSender{}
	e := newTestEngine(t, sender, nil, false)

	cfg := config.Config{}
	cfg.Persona.FallbackLines = []string{"   ", "", "\n"}
	for i := 0; i < 6; i++ {
		e.deflect(cfg, e.mem.Group("g1", "群A"))
	}
	if n, _ := sender.count(); n != 0 {
		t.Errorf("全是空白的兜底池不应发出消息，实际 %d 条", n)
	}
}