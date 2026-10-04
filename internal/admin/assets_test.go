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