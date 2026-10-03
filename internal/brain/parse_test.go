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

// TestParseDecisionQuietFallbackKeepsOS 降级闭嘴时必须保住 os。
//
// 2026-10-03 生产实况：管理端决策日志里，模型选择闭嘴时 os 几乎总是空的，
// 「它为什么不说话」完全看不出来。根因在 ParseDecision 的第一条降级出口
// 漏了 OS: os——模型明明写了内心活动，只是 JSON 被截断，那段 os 被丢了。
// 而紧邻的两条出口都带着，看起来像「这条特意不要」，实际是漏的。
//
// 这组用例必须逐条钉住「走的是哪条出口」：只看 os 非空是不够的，
// 那两条出口本来就会带 os，测不出是不是被别的路径「碰巧」满足
//（atothers_test.go 批评过这种假绿）。
func TestParseDecisionQuietFallbackKeepsOS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, why string
	}{
		// A 类：os 写完就没下文了，os 之后连一个 { 都没有。
		// 这是最贴近生产的形态——免费小模型经常吐完 os 就撞上 max_tokens。
		{`<os>懒得理他</os>`, "os 后无任何 JSON"},
		// A 类边界：json 标签在，但内容是空的
		{`<os>算了</os><json>`, "json 标签内为空"},
		// B 类：引号没闭合，braceMatch 与补 } 都配不出来
		{`<os>怕说错</os><json>{"act":"quiet`, "引号未闭合，JSON 配不出来"},
	}
	for _, c := range cases {
		d, issue := ParseDecision(c.in)
		// 先确认真的走了降级这条出口，否则下面的断言可能只是在测另一条路
		if issue != ParseFallback {
			t.Errorf("ParseDecision(%q) 的 issue 应为 fallback，实际 %q（%s）——"+
				"这条用例本来是想钉住「剥完什么都不剩」那条出口的，"+
				"走错分支就等于没测到", c.in, issue, c.why)
			continue
		}
		if d.Act != "quiet" {
			t.Errorf("ParseDecision(%q) 的 act 应为 quiet，实际 %q", c.in, d.Act)
		}
		want := extractTag(c.in, "os")
		if d.OS != want {
			t.Errorf("降级闭嘴时 os 被丢了：ParseDecision(%q) 得 %q，期望 %q——"+
				"「它为什么不说话」在管理端就成了一片空白", c.in, d.OS, want)
		}
	}
}

// TestParseDecisionTruncatedJSONStillKeepsOS 对照组：能捞回 JSON 的截断必须保住 os。
//
// 这条和上面那组是同一次事故的两面，必须一起钉：
// 上面的用例守住「捞不回来时也别丢 os」，这条守住「捞得回来时别把路径改坏」。
//
// 尤其注意 {"act":"quiet" 这个带收尾引号的输入——它走的是 extractJSON 里
// `return body, ParseRecovered` 那条（parse.go:281），今天就能拿到 os。
// 如果有人「简化」那里（比如改成 body 非空就一律当 fallback），
// 一批本来正常的轮次会突然掉进降级路径，调用量与行为都会变。
func TestParseDecisionTruncatedJSONStillKeepsOS(t *testing.T) {
	t.Parallel()
	d, issue := ParseDecision(`<os>懒得理他</os><json>{"act":"quiet"`)
	if issue != ParseRecovered {
		t.Fatalf("引号闭合的截断 JSON 应被补全成 recovered，实际 %q——"+
			"这条路径被改坏的话，一批正常轮次会掉进降级出口", issue)
	}
	if d.OS != "懒得理他" {
		t.Errorf("走 recovered 路径时 os 也要保住，实际 %q", d.OS)
	}
	if d.Act != "quiet" {
		t.Errorf("act 应为 quiet，实际 %q", d.Act)
	}
}

// TestParseDecisionEmptyInputHasNoOS 整串为空时 os 本来就为空。
//
// 这条是**反向**钉住：第 26 行那条出口（text == ""）**不带** os 是对的，
// 因为 extractTag 在空串上必然返回空。加上 OS: os 只是死代码，
// 还会让人误以为那条也能捞到 os。此测试把这个判断写死，
// 免得后人「顺手补齐」时把上面的用例一起推翻。
func TestParseDecisionEmptyInputHasNoOS(t *testing.T) {
	t.Parallel()
	d, issue := ParseDecision("")
	if issue != ParseFallback {
		t.Errorf("空输入的 issue 应为 fallback，实际 %q", issue)
	}
	if d.OS != "" {
		t.Errorf("整串为空时 os 必为空，实际 %q", d.OS)
	}
	if d.Act != "quiet" {
		t.Errorf("空输入应闭嘴，实际 act=%q", d.Act)
	}
}
