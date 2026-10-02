package brain

import (
	"strings"
	"testing"
)

// TestStripTagsCloseBeforeOpenTerminates 回归测试：
// 闭标签出现在开标签之前时，老实现每轮都把中间段复制一遍，字符串净增长，
// 38 字节的输入能一路涨到 800MB 再也退不出来。修好后必须一次收敛。
func TestStripTagsCloseBeforeOpenTerminates(t *testing.T) {
	t.Parallel()
	cases := []string{
		`他说了</json>哦<json>{"text":"hi"}`,
		`</os><os>在想要不要说话`,
		`<json></json><json>`,
		`<think></think>想了三秒<think>还是算了`,
		strings.Repeat("<json></os>", 50),
	}
	for _, in := range cases {
		got := stripTags(in)
		// 收敛判据：结果不可能比输入长，长一点就说明还在复制自己
		if len(got) > len(in) {
			t.Errorf("stripTags(%q) 结果 %d 字节，比输入 %d 还长：没收敛", in, len(got), len(in))
		}
	}
}

// TestStripTagsUnclosed 开了标签没闭合：开标签往后全是残渣，直接截断
func TestStripTagsUnclosed(t *testing.T) {
	t.Parallel()
	got := stripTags(`前面还算正常<json>{"text":"hi"`)
	if want := "前面还算正常"; got != want {
		t.Errorf("未闭合 <json 应截断到开标签之前，got %q want %q", got, want)
	}
	// 全是残渣时应当剥成空串，而不是把 <json> 当内容发出去
	if got := stripTags(`<json>{"text":"hi"`); got != "" {
		t.Errorf("整串都是残渣时应剥空，got %q", got)
	}
}

// TestStripTagsKeepsChinese 结构行只砍碎片，砍完剩下的人话要留住
func TestStripTagsKeepsChinese(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"act":"say"} 我今天真不想说话`: "我今天真不想说话",
		"{":                      "",
		`{"a":`:                  "",
		"{\n}\n":                 "",
		"他就是这点出息":                "他就是这点出息",
	}
	for in, want := range cases {
		if got := stripTags(in); got != want {
			t.Errorf("stripTags(%q) = %q，want %q", in, got, want)
		}
	}
}

// TestParseDecisionQuotesInsideValue 值里的全角引号是内容，不能当成结构符号换掉。
// 老实现在 cleanFence 里无条件替换，这句直接被改成非法 JSON、整条决策静默丢弃。
func TestParseDecisionQuotesInsideValue(t *testing.T) {
	raw := `{"act":"say","text":"他说“没事”","tone":"roast"}`
	d, issue := ParseDecision(raw)
	if d.Act != "say" {
		t.Fatalf("合法的全角引号不该让决策丢掉，act=%q issue=%q", d.Act, issue)
	}
	if d.Text != "他说“没事”" {
		t.Errorf("text 被引号规范化改坏了，got %q", d.Text)
	}
	// 结构位置上的全角引号仍然要能救回来
	d2, issue2 := ParseDecision(`{“act”:“say”,“text”:“在”}`)
	if d2.Act != "say" || d2.Text != "在" {
		t.Errorf("整体失守的全角引号应被修复，act=%q text=%q issue=%q", d2.Act, d2.Text, issue2)
	}
	if issue2 != ParseRecovered {
		t.Errorf("修好的决策应报 recovered，got %q", issue2)
	}
}

// TestParseDecisionBrokenQuotesNotSpoken 引号被换坏之后的那串东西不能当发言发出去
func TestParseDecisionBrokenQuotesNotSpoken(t *testing.T) {
	d, _ := ParseDecision(`{"text":"他说"没事""}`)
	if d.Act != "quiet" {
		t.Errorf("半截 JSON 残渣不该变成发言，act=%q text=%q", d.Act, d.Text)
	}
	if d.Text != "" {
		t.Errorf("quiet 时 text 必须为空，got %q", d.Text)
	}
}

// TestParseDecisionFallbackGate 降级路径的「像不像人话」闸：
// 剥完标签剩下的还是协议碎片就闭嘴，别把 </json>、标点堆当发言发出去
func TestParseDecisionFallbackGate(t *testing.T) {
	noise := []string{
		`他说了</json>这么个事`,
		`。。。`,
		`。`,
		`我觉得<b>他</b>很烦`,
	}
	for _, in := range noise {
		d, issue := ParseDecision(in)
		if d.Act != "quiet" {
			t.Errorf("ParseDecision(%q) 应闭嘴，act=%q text=%q", in, d.Act, d.Text)
		}
		if issue != ParseNoise {
			t.Errorf("ParseDecision(%q) 应报 noise 以便排查，got %q", in, issue)
		}
	}
	// 正常人话照发不误
	d, issue := ParseDecision(`我今天真的一句话都不想说`)
	if d.Act != "say" || d.Text != "我今天真的一句话都不想说" {
		t.Errorf("正常中文应原样发出，act=%q text=%q issue=%q", d.Act, d.Text, issue)
	}
	if issue != ParseFallback {
		t.Errorf("无 JSON 的正常发言应报 fallback，got %q", issue)
	}
}

// TestParseDecisionSanitizeHooksMaxChars Sanitize 挂在出口上：残留标签、控制字符
// 都要被剥掉，max_chars 也要真的截断
func TestParseDecisionSanitizeHooksMaxChars(t *testing.T) {
	SetMaxChars(6)
	defer SetMaxChars(0)

	// 控制字符写成 JSON 的  转义：裸控制字符本身就是非法 JSON，
	// 得先解出来，Sanitize 才有东西可剥
	raw := "{\"act\":\"say\",\"text\":\"他说说了\\u0007一长串根本发不完的话\",\"tone\":\"roast\"}"
	d, issue := ParseDecision(raw)
	if d.Act != "say" {
		t.Fatalf("act=%q issue=%q", d.Act, issue)
	}
	if strings.ContainsRune(d.Text, 7) {
		t.Errorf("控制字符没被剥掉：%q", d.Text)
	}
	if n := len([]rune(d.Text)); n > 7 { // 6 字上限 + 省略号
		t.Errorf("max_chars 没生效，text %d 字：%q", n, d.Text)
	}
}

// TestSanitizeTagsAndLen Sanitize 自己也要能剥标签、限长
func TestSanitizeTags(t *testing.T) {
	if got := Sanitize(`<json>{"a":1}</json>说话`, 0); got != "说话" {
		t.Errorf("Sanitize 应剥掉标签块，got %q", got)
	}
	if got := Sanitize("一二三四五六", 3); got != "一二三…" {
		t.Errorf("Sanitize 应按 3 字截断，got %q", got)
	}
}

// TestLooksLikeSpeech 人话闸的边界
func TestLooksLikeSpeech(t *testing.T) {
	yes := []string{"你好", "行吧", "123", "他没事", "他说“没事”"}
	no := []string{"", "。", "。。", `{"a":1}`, "a<b", "</json>"}
	for _, s := range yes {
		if !looksLikeSpeech(s) {
			t.Errorf("looksLikeSpeech(%q) 应为真", s)
		}
	}
	for _, s := range no {
		if looksLikeSpeech(s) {
			t.Errorf("looksLikeSpeech(%q) 应为假", s)
		}
	}
}
