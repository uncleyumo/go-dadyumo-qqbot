package admin

import (
	"strings"
	"testing"
)

// 管理端页面里，输入框的 id、renderPersona 填值的引用、collectPersona 收值的引用
// 是三处**互相独立**的字符串。任何一处对不上都不会报错，只是静默失效：
// 症状是「保存了没生效」或「打开页面框里是空的」，很难查。
//
// 这类 wiring 不适合逐个手点验证——加一项就是加三处字符串，
// 迟早会漏。所以在这里钉住。
func TestFallbackLinesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-fallback"`) {
		t.Error("页面缺少兜底话术输入框 pf-fallback")
	}
	// 渲染：把配置里的兜底话术填进框
	if !strings.Contains(html, `getElementById('pf-fallback').value = (p.fallback_lines || []).join('\n')`) {
		t.Error("renderPersona 没有把 fallback_lines 填进 pf-fallback（打开页面会是空框）")
	}
	// 收集：把框里的内容写回配置
	if !strings.Contains(html, `CFG.persona.fallback_lines = lines(document.getElementById('pf-fallback').value)`) {
		t.Error("collectPersona 没有把 pf-fallback 写回 CFG（保存后不生效）")
	}
}
// busy_lines 与 fallback_lines 一样是「填框 → 读回 → 存回」三处字符串，
// 少一处就静默失效。跟着上一个测试一起钉住。
func TestBusyLinesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-busy"`) {
		t.Error("页面缺少艾特兜底话术输入框 pf-busy")
	}
	if !strings.Contains(html, `getElementById('pf-busy').value = (p.busy_lines || []).join('\n')`) {
		t.Error("renderPersona 没有把 busy_lines 填进 pf-busy")
	}
	if !strings.Contains(html, `CFG.persona.busy_lines = lines(document.getElementById('pf-busy').value)`) {
		t.Error("collectPersona 没有把 pf-busy 写回 CFG（保存后不生效）")
	}
}

// reply_rules 是 2026-10-05 新增的字段，为了给「回复的指导」一个正向的落脚点
// （原先只能塞进「行为边界」那堆禁令里）。三处 wiring 与前两项同构。
func TestReplyRulesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-reply"`) {
		t.Error("页面缺少回话分寸输入框 pf-reply")
	}
	if !strings.Contains(html, `getElementById('pf-reply').value = (p.reply_rules || []).join('\n')`) {
		t.Error("renderPersona 没有把 reply_rules 填进 pf-reply（打开页面会是空框）")
	}
	if !strings.Contains(html, `CFG.persona.reply_rules = lines(document.getElementById('pf-reply').value)`) {
		t.Error("collectPersona 没有把 pf-reply 写回 CFG（保存后不生效）")
	}
}

// 分区重组（2026-10-05）：人设页由两张卡拆成四张。
// 卡片的 h2 标题是这次重组的全部可见产物，所以在这里钉住——
// 「怎么回话」独立成卡正是用户提的核心诉求，塌回「行为边界」就等于没改。
func TestPersonaPanelsSplitIntoFour(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	for _, want := range []string{"它是谁", "怎么回话", "不做什么", "应急话术"} {
		if !strings.Contains(html, ">"+want+" <span class=\"hint\"") {
			t.Errorf("人设页缺少卡片「%s」", want)
		}
	}
	// 旧的两个标题必须消失，否则重组等于没发生
	for _, gone := range []string{"人设本体", "行为边界"} {
		if strings.Contains(html, ">"+gone+" <span class=\"hint\"") {
			t.Errorf("旧卡片「%s」还在，四卡拆分没生效", gone)
		}
	}
	// pf-reply 必须落在「怎么回话」那张卡里，不能漂到别处。
	// 用位置比对：它出现在 pf-catch 之后、pf-roast 之前——
	// 顺序由上面那张表的排布决定，漂了这条就红。
	reply := strings.Index(html, `id="pf-reply"`)
	catch := strings.Index(html, `id="pf-catch"`)
	roast := strings.Index(html, `id="pf-roast"`)
	if reply < 0 || catch < 0 || roast < 0 {
		t.Fatalf("缺少输入框: pf-reply=%d pf-catch=%d pf-roast=%d", reply, catch, roast)
	}
	if !(catch < reply && reply < roast) {
		t.Errorf("pf-reply 应排在「怎么回话」卡里（pf-catch 之后、pf-roast 之前），实际位置 %d/%d/%d",
			catch, reply, roast)
	}
}
