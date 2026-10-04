package brain

import (
	"fmt"
	"strings"
	"testing"

	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
)

// memUpstream 回一句带 mem 的决策。
//
// 为什么单独造一个：现有 fakeUpstream 的 fixture 全是 "mem":[]
// （decisionlog_test.go:242、toolloop_e2e_test.go:146 等），
// 于是「模型主动写入长期记忆」这条路径在整个测试套件里**从未被走过**——
// 没有测试覆盖它，就没法确认它现在还能不能跑。
func memUpstream(t *testing.T, mem string) string {
	t.Helper()
	srv, _ := startFakeUpstream(t, []fakeStep{{
		content: `<os>记一下</os><json>{"act":"quiet","mem":` + mem + `}</json>`,
	}})
	return srv.URL
}

// 模型主动写入长期记忆时必须留痕，且落在 decision 分类里。
//
// 这条日志是 2026-10-05 才补上的：此前写入路径（engine.go 的那个循环）
// 一行日志都没有，于是「它到底多久主动记一次东西」在生产上完全不可观测——
// 控制台里那几条长期要点，看不出是模型写的还是管理端手工加的，
// 当时只能靠 fact 落盘时间戳与 caddy 的 fact/set 请求逐秒比对才分辨出来。
//
// 用 act=quiet 是故意的：写入点在「要不要发言」那道闸**之前**，
// 闭嘴那轮照样该写。这条断言守住那个顺序不被后人挪动。
func TestFactWriteLoggedUnderDecision(t *testing.T) {
	srvURL := memUpstream(t, `[{"k":"老张在苏州做监理","v":"他自己在群里说的"}]`)
	e := newEngineWithUpstream(t, srvURL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	before := countDecisions()
	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "老张",
		false, false, nil, "", "", "", "", nil, "", false)

	if got := g.Facts(); !strings.Contains(got, "老张在苏州做监理") {
		t.Fatalf("模型给的 mem 应写进长期记忆，实际 %q", got)
	}

	entry := findDecision(t, "写入长期记忆")
	if entry.Cat != logx.CatDecision {
		t.Errorf("写入日志归到了 %q，应为 decision——管理端默认视图筛的就是它", entry.Cat)
	}
	if n := countDecisions() - before; n < 2 {
		t.Errorf("除决策行外还应有写入行，实际新增 decision 共 %d 条", n)
	}
}

// 池子满了挤掉一条时，日志必须说清挤掉的是谁。
//
// SetFact 返回被淘汰的 key，原来这个返回值被直接丢弃——
// 于是「它怎么把之前记的事忘了」在生产上根本查不出来，
// 而长期记忆无声替换恰恰是最难察觉的一类故障。
//
// 不断言「淘汰的一定是时间最早那条」：紧凑循环里写入时间戳可能并列，
// 此时 evictOldestFactLocked 按 key 字典序取最小（short.go:622），
// 那是它保证结果确定的 tie-break，不是缺陷。真正要钉住的是
// **日志说的那条确实就是真正消失的那条**——否则日志与实际不符，排查时照样被骗。
func TestFactEvictionIsLogged(t *testing.T) {
	srvURL := memUpstream(t, `[{"k":"新记的","v":"值"}]`)
	e := newEngineWithUpstream(t, srvURL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	// 填满池子，最后一条写入必然触发淘汰。
	// key 用下标拼：字母表在 MaxFacts 调大后会撞号，撞号会让「哪条被淘汰」
	// 变得不可预期，而这条测试的全部意义就是它可预期。
	for i := 0; i < memory.MaxFacts; i++ {
		g.SetFact(fmt.Sprintf("旧%03d", i), "值")
	}
	before := map[string]bool{}
	for _, it := range g.FactsList() {
		before[it.Key] = true
	}

	e.decide(e.store.Get(), g, "有人在群里说了句话", "", "某人",
		false, false, nil, "", "", "", "", nil, "", false)

	entry := findDecision(t, "挤掉了最旧一条")
	evicted, _ := entry.KV["淘汰"].(string)
	if evicted == "" {
		t.Fatalf("淘汰日志必须带上被挤掉的 key，实际 KV: %v", entry.KV)
	}
	if !before[evicted] {
		t.Errorf("日志说的 %q 在写入前并不存在，淘汰记录与实际不符", evicted)
	}
	if strings.Contains(g.Facts(), evicted) {
		t.Errorf("被记为淘汰的 %q 却还在长期记忆里，日志与实际不符", evicted)
	}
	if entry.KV["key"] != "新记的" {
		t.Errorf("日志应记下这轮写入的 key，实际 %v", entry.KV["key"])
	}
}
