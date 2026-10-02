package agent

import (
	"strings"
	"testing"
)

// 下面这些样本全部来自生产日志（2026-10-01 13:1x，测试群一号），
// base64 是从 journald 原文逐字节抠出来的，不是手抄。
// 平台把表情和 @ 塞在 content 正文里的真实形态就长这样。
const (
	faceFlow   = `<faceType=1,faceId="5",ext="eyJ0ZXh0Ijoi5rWB5rOqIn0="/>`   // 流泪
	faceGrin   = `<faceType=1,faceId="13",ext="eyJ0ZXh0Ijoi5ZGy54mZIn0="/>`  // 呲牙
	faceScare  = `<faceType=1,faceId="325",ext="eyJ0ZXh0Ijoi5oOK5ZCTIn0="/>` // 惊吓
	faceStrong = `<faceType=1,faceId="349",ext="eyJ0ZXh0Ijoi5Z2a5by6In0="/>` // 坚强
	// faceId=277 后面平台多带了一个 👍，名字本身是「汪汪」
	faceWoof = `<faceType=1,faceId="277",ext="eyJ0ZXh0Ijoi5rGq5rGqIn0=">👍` // 汪汪
	// faceType=6 是「魔法表情」，名字自带 [/壳]
	faceMagic = `<faceType=6,faceId="0",ext="eyJ0ZXh0IjoiWy/lub3ngbVdIn0="/>` // [/幽灵]
	// 空 ext：平台自己也给不出名字
	faceEmpty = `<faceType=6,faceId="0",ext="eyJ0ZXh0IjoiIn0="/>`
	// 群友乙 @ 了某人，日志原文
	mentionOther = `<@AAAABBBBCCCCDDDDEEEEFFFF00001111>`
	// 群友甲 @ 了全体成员，日志原文
	mentionAll = `<@all>`
)

func TestCleanTagsFaces(t *testing.T) {
	cases := []struct{ in, want string }{
		{faceFlow, "（流泪）"},
		{faceGrin, "（呲牙）"},
		{faceScare, "（惊吓）"},
		{faceStrong, "（坚强）"},
		// 结尾的 👍 是平台冗余附带的，名字已经解出来了，不该重复留一个
		{faceWoof, "（汪汪）"},
		// 魔法表情的名字带壳，去掉更好读
		{faceMagic, "（幽灵）"},
		// 空 ext 不是内置表情，是群友发的表情包（见 TestNamelessExtIsMeme）
		{faceEmpty, "（表情包）"},
	}
	for _, c := range cases {
		got, _ := cleanTags(c.in, nil)
		if got != c.want {
			t.Errorf("cleanTags(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 生产现场：连发 5 个表情，每条都只由一个标签构成。
// 修复前模型看到的是 base64 残渣，只能回「搁这刷表情包呢」。
func TestCleanTagsRealEmojiBurst(t *testing.T) {
	all := faceFlow + faceGrin + faceWoof + faceScare + faceStrong
	got, atAll := cleanTags(all, nil)
	if atAll {
		t.Error("纯表情不该报 atAll")
	}
	for _, want := range []string{"（流泪）", "（呲牙）", "（汪汪）", "（惊吓）", "（坚强）"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %s，实际 %q", want, got)
		}
	}
	if strings.Contains(got, "faceType") || strings.Contains(got, "eyJ0ZXh0") {
		t.Errorf("平台标记没清干净: %q", got)
	}
}

// 表情 + 文字混排：文字必须原样保留，只替换标签
func TestCleanTagsKeepsSurroundingText(t *testing.T) {
	got, _ := cleanTags("今天打不打"+faceFlow+"上号", nil)
	if got != "今天打不打（流泪）上号" {
		t.Errorf("got %q", got)
	}
}

// 机器人自己的 @ 要换成人设名
func TestCleanTagsSelfMentionBecomesPersona(t *testing.T) {
	self := "55556666777788889999000011112222"
	names := map[string]string{self: "羽沫老爹"}
	got, _ := cleanTags("<@"+self+"> 你咋不上号", func(oid string) string { return names[oid] })
	if got != "〔@羽沫老爹〕 你咋不上号" {
		t.Errorf("got %q", got)
	}
}

// 旧格式 <@!openid> 也要认
func TestCleanTagsLegacyMentionBang(t *testing.T) {
	names := map[string]string{"ABC123": "群友乙"}
	got, _ := cleanTags("<@!ABC123> 来一下", func(oid string) string { return names[oid] })
	if got != "〔@群友乙〕 来一下" {
		t.Errorf("got %q", got)
	}
}

// 认不出来的 openid：标签整个删掉，绝不能把 32 位十六进制留给模型
func TestCleanTagsUnknownOpenIDDropped(t *testing.T) {
	got, _ := cleanTags(mentionOther+" 在吗", func(string) string { return "" })
	if got != "在吗" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(got, "AAAA5555") {
		t.Errorf("裸 openid 泄漏: %q", got)
	}
}

// @全体成员：翻成人话，并如实上报这个事实。
// 平台不给它 mentions 条目、也不置 at=true，只在正文留一个 <@all>。
func TestCleanTagsAtAll(t *testing.T) {
	got, atAll := cleanTags(mentionAll, nil)
	if !atAll {
		t.Error("应识别出 @全体成员")
	}
	if got != "〔@全体成员〕" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(got, "<@") {
		t.Errorf("标签没清干净: %q", got)
	}
}

// TestMentionWrappedForMisleadingNickname @ 必须包进 〔〕，不能只剩「@昵称」。
//
// 2026-10-02 生产实况（测试群一号）：有个群友的群名片就叫「...」，
// 别人 @ 他时上下文里只有「@...」三个点，模型把它读成一句莫名其妙的
// 省略号，回的是「大半夜发个省略号，你这是困傻了还是省电模式」——
// 平台明明给的是一个结构化的 @ 动作，模型却当成了聊天正文。
//
// 昵称是不可控输入，所以「@昵称」这种裸拼接在任何情况下都不够：
// 昵称可以是「...」「（）」「——」甚至一整句话。
func TestMentionWrappedForMisleadingNickname(t *testing.T) {
	for _, nick := range []string{"...", "（）", "——", "。", "？", "、"} {
		oid := "88F43AFA91444338743C0B7E2E9E30A11"
		got, _ := cleanTags("<@"+oid+">", func(string) string { return nick })
		if got != "〔@"+nick+"〕" {
			t.Errorf("昵称 %q 应包成 〔@%s〕，实际 %q", nick, nick, got)
		}
		// 反过来验：包过之后，裸的「@昵称」不该再出现在结果里，
		// 否则模型还是会先读到那串误导性的字符
		if strings.HasPrefix(got, "@"+nick) {
			t.Errorf("昵称 %q 没有被包起来，模型仍会读成正文: %q", nick, got)
		}
	}
}

// 生产现场：`<@F636...>` 后面又跟了一个 `<@all>`
func TestCleanTagsMixedRealLine(t *testing.T) {
	names := map[string]string{"AAAABBBBCCCCDDDDEEEEFFFF00001111": "群友乙"}
	got, atAll := cleanTags(mentionOther+" "+mentionAll, func(oid string) string { return names[oid] })
	if !atAll {
		t.Error("应识别出 @全体成员")
	}
	if got != "〔@群友乙〕 〔@全体成员〕" {
		t.Errorf("got %q", got)
	}
}

// 空 ext / 坏 base64 / 非 JSON 都不能 panic，也不能把原文吐回去。
//
// 解不出来时返回**空串**而不是「表情」：回退成什么由 faceName 决定，
// 它要按 faceType 分辨内置表情和表情包——一律叫「表情」会把表情包说成小符号。
func TestFaceTextDegrades(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"!!!notb64!!": "",
		"aGVsbG8=":    "", // 合法 base64 但不是 JSON
		// {"text":""} —— faceType=6 的表情包就是这样，ext 解得开但里面没名字
		"eyJ0ZXh0IjoiIn0=": "",
	}
	for in, want := range cases {
		if got := faceText(in); got != want {
			t.Errorf("faceText(%q) = %q, want %q", in, got, want)
		}
	}
}

// 内置小表情照常解出名字。
func TestFaceNameBuiltinFace(t *testing.T) {
	if got := faceName(faceFlow); got != "（流泪）" {
		t.Errorf("内置表情应解出名字，got %q", got)
	}
	if got := faceName(faceGrin); got != "（呲牙）" {
		t.Errorf("内置表情应解出名字，got %q", got)
	}
}

// TestNamelessExtIsMeme ext 解得开但里面没名字时，那是**表情包**，不是内置表情。
//
// 2026-10-02 生产实况（测试群二号群）：有人引用群友发的那张猫的表情包，
// 问「我引用的这条是谁发的聊天记录？」。被引用的元素里装的是
//
//	<faceType=6,faceId="0",ext="eyJ0ZXh0IjoiIn0="/>   → {"text":""}
//
// 平台把它当图片处理（所以不给 attachments），引用时却只塞一个空标签，
// 既没有名字也没有地址。说成「表情」会让模型以为对方甩了个小符号，
// 而它面对的是一张**它看不到、平台也没告诉它发送者**的图片。
//
// 判据是 **ext 为空**而不是 faceType=6：6 还包括有名字的魔法表情
// （见 faceMagic → 幽灵），那种必须照常显示名字。
func TestNamelessExtIsMeme(t *testing.T) {
	if got := faceName(faceEmpty); got != "（表情包）" {
		t.Errorf("空 ext 应叫表情包，got %q", got)
	}
	// faceType=6 但有名字 → 照常显示名字，绝不能叫表情包
	if got := faceName(faceMagic); got != "（幽灵）" {
		t.Errorf("有名字的魔法表情应按名字走，got %q", got)
	}
	// ext 压根不是合法 base64 / 不是 JSON：多半平台改了格式，不该猜成表情包
	for _, tag := range []string{
		`<faceType=6,faceId="0",ext="!!!notb64!!!"/>`,
		`<faceType=6,faceId="0",ext="aGVsbG8="/>`, // 合法 base64 但不是 JSON
		`<faceType=6,faceId="0",ext=""/>`,
	} {
		if got := faceName(tag); got != "（表情）" {
			t.Errorf("无法判定时该保守叫表情，got %q（%s）", got, tag)
		}
	}
}

// 自定义表情可能带超长名字，别烧 token
func TestFaceTextTruncatesLongName(t *testing.T) {
	// {"text":"一二三四五六七八九十十一十二"}
	got := faceText("eyJ0ZXh0Ijoi5LiO5LiA5Li95LiB6L2v5Y2V5LiA5Y2VLZTVLiA55YWL6ZSL5Li65bCP6K6w5p2D5Liq6ZmiIn0=")
	if n := len([]rune(got)); n > 12 {
		t.Errorf("名字过长未截断: %q (%d 字)", got, n)
	}
}

func TestCleanTagsEmptyInput(t *testing.T) {
	got, atAll := cleanTags("", nil)
	if got != "" || atAll {
		t.Errorf("got %q atAll=%v", got, atAll)
	}
}
