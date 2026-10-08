package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

// 「看不见」这件事必须既不被提示词教出来、也不被模型说出来。
//
// 2026-10-02 生产实况（测试群二号群）：群友甲引用群友发的那张猫的表情包，
// 问「我引用的这条是谁发的聊天记录？」，机器人答
//
//	「你引用的那条我这儿看不见  截图发出来」
//
// 群里每个人都能看见那条引用。这两句一出口，它就不是群里的人了——
// 而且是在**被直接问到**的时候露馅，最糟的一种。
//
// 防线分三层，缺一层模型都会换个说法漏出去：
//  1. 固定段明令禁止，并给出「真人会怎么应对」的具体替代说法
//  2. 动态段（quoteNote / 图片说明）不许出现「平台没告诉我」这类措辞
//  3. 新符号要在符号表里解释，否则模型不知道那是什么、只能问「这是什么」

func promptFor(t *testing.T) string {
	t.Helper()
	cfg := *config.Default()
	g := memory.New(cfg.Brain.MaxHistory).Group("g", "测试群")
	return systemPrompt(cfg, g, MoodSignal{}, "", "", "群友甲")
}

// TestSystemPromptForbidsSayingCannotSee 固定段必须禁止自曝，且**不得**把被禁串本身写进去。
//
// 这条断言在 2026-10-02 之后被翻转过一次。原来它要求固定段必须逐字包含
// 「我看不见」「截图发出来」——理由是「点名它真说过的原话，换个说法照样漏」。
//
// 那个前提是错的。把这 9 个被禁串写进提示词，等于在上下文里以近乎相同的
// 字面反复抬高它们的注意力权重，而模型在长文本尾部最容易复现其中之一。
// 2026-10-02 那次露馅（「你引用的那条我这儿看不见 截图发出来」）就是这么来的——
// 提示词亲手把这两句原话摆在了它眼前。
//
// 现在钉住的是**反向不变量**：替代行为必须在，被禁串一个都不许出现。
func TestSystemPromptForbidsSayingCannotSee(t *testing.T) {
	s := promptFor(t)

	if !strings.Contains(s, "看不见的东西，绝对不许说出口") {
		t.Fatal("固定段缺了「不许说看不见」这一节——删掉它模型就会照实说「我看不见」")
	}

	// 只禁不给路会漏：必须给具体替代行为，这是替代清单存在的唯一理由
	// 只禁不给路会漏：必须给替代行为。
	//
	// 原来钉的是「不是你的吗」——那是给模型的现成的串，而它是老爹的口吻，
	// 写在两台机器人共用的固定段里，奶酱照抄就成了「我是老东西」。
	// 现成的串拆掉了，改钉「顺着上下文接话」这个行为本身还在。
	for _, alt := range []string{"顺着聊天记录里的上下文接话", "quiet"} {
		if !strings.Contains(s, alt) {
			t.Errorf("必须给出具体的替代行为 %q，只禁不给路模型会换个方式犯", alt)
		}
	}

	// 核心不变量：这些串一个字都不许出现在提示词里。
	// 「我看不见」和「截图发出来」是 2026-10-02 它真说过的原话，
	// 其余是同一类语义的变体——一并禁掉，防止换个说法漏出去。
	for _, banned := range []string{
		"我看不见", "我这边没有", "我看不到", "没显示出来",
		"加载不出来", "截图发出来", "你再发一遍", "发个文字描述",
		"我这边只收到一个占位",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("被禁串 %q 出现在固定段里了：把要避免的字符串写进提示词，"+
				"等于在上下文里反复抬高它的权重，模型反而更容易复现。"+
				"改成描述行为（做什么/不做什么），不要列出禁用词", banned)
		}
	}
}

// TestSystemPromptExplainsNewSymbols 新加的占位符必须在符号表里解释。
//
// 模型没见过「（表情包）」，不解释的话它会当成乱码，然后追问「这是啥」。
func TestSystemPromptExplainsNewSymbols(t *testing.T) {
	s := promptFor(t)

	for _, sym := range []string{"（表情包）", "（一张图）", "（内容无法解析）"} {
		if !strings.Contains(s, sym) {
			t.Errorf("符号表里缺 %s 的解释，模型没见过会当成乱码", sym)
		}
	}
	// 「内容无法解析」要给出应对方式，否则模型会去追问/要截图
	if !strings.Contains(s, "别追问、别要截图") {
		t.Error("「（内容无法解析）」必须写明别追问别要截图")
	}
}

// TestNoExposureSectionIsInFixedPart 禁止自曝那一节必须在**固定段**里。
//
// 放到动态段（尾部）的话每轮内容都不一样，前缀缓存按字节匹配，
// 命中不了等于每轮按全价计费——几千 token 的提示词，一天就是钱。
// 判据用「在环境行之前」：环境行是已知的固定段/动态段分界（见 TestSystemPromptHasEnv）。
func TestNoExposureSectionIsInFixedPart(t *testing.T) {
	cfg := *config.Default()
	cfg.Brain.WeatherPlace = "示例市"
	g := memory.New(cfg.Brain.MaxHistory).Group("g", "测试群")

	s := systemPrompt(cfg, g, MoodSignal{}, "", "现在时间：2026年10月02日 01:30:00（周四）", "")

	idx := strings.Index(s, "看不见的东西，绝对不许说出口")
	if idx < 0 {
		t.Fatal("找不到禁止自曝那一节")
	}
	if env := strings.Index(s, "现在时间："); env >= 0 && idx > env {
		t.Errorf("这一节落在动态段（位置 %d，环境行 %d），会打碎前缀缓存", idx, env)
	}
}

// TestMentionMarkerIsExplainedInPrompt 〔@某人〕 必须在提示词里有图例。
//
// 标记本身没有自解释性——模型看到 `〔@...〕` 如果不知道那是什么，
// 只会当成又一段莫名其妙的符号。2026-10-02 那次它把 `〔@...〕`
// 读成了「发个省略号」，回复是「大半夜发个省略号，你这是困傻了还是省电模式」。
// 图例和标记必须成对存在，缺一边这个功能就等于没做。
func TestMentionMarkerIsExplainedInPrompt(t *testing.T) {
	cfg := *config.Default()
	cfg.Brain.WeatherPlace = "示例市"
	g := memory.New(cfg.Brain.MaxHistory).Group("g", "测试群")

	s := systemPrompt(cfg, g, MoodSignal{}, "", "现在时间：2026年10月02日 01:30:00（周四）", "")
	if !strings.Contains(s, "〔@某人〕") {
		t.Error("固定段里没有 〔@某人〕 的图例：模型不知道这个标记是什么意思，" +
			"就会把那个群名片当成聊天正文读")
	}
	// 图例必须说清「括号里是名字，不是他打的字」——这正是老 bug 的误解点
	if !strings.Contains(s, "群名片") {
		t.Error("〔@某人〕 的图例必须说明括号里是对方的群名片，" +
			"否则昵称是「...」「（）」这类内容时模型仍会当成正文")
	}
	// 图例在固定段里（环境行之前），否则每轮位置变化会打碎前缀缓存
	idx := strings.Index(s, "〔@某人〕")
	if env := strings.Index(s, "现在时间："); env >= 0 && idx > env {
		t.Errorf("图例落在动态段（位置 %d，环境行 %d），会打碎前缀缓存", idx, env)
	}
}

// TestMentionMarkerSurvivesSanitize 〔@某人〕 到模型手上必须还是〔@某人〕。
//
// 〔〕 是「这是一次 @ 动作」的唯一结构标记（agent/tags.go 的 markMention
// 产出，平台那个昵称就叫「...」的群友是真实存在的）。
// sanitizeChatText 会把群友正文里的 【】<>{} 换成全角近形字来防伪造分段——
// 万一有人往那张表里加上 〔〕，标记就在进上下文前被抹平，
// 机器人又会退回「把 @... 读成三个点」的老毛病，而这里什么都看不出来。
func TestMentionMarkerSurvivesSanitize(t *testing.T) {
	for _, s := range []string{"〔@...〕", "〔@全体成员〕", "〔@Pytorch搬运工〕"} {
		if got := sanitizeChatText(s); got != s {
			t.Errorf("sanitizeChatText(%q) = %q：〔〕 是 @ 动作的结构标记，不能被改写", s, got)
		}
	}
}

// TestFixedPartIsByteStable 固定段必须逐字节稳定。
//
// 为什么这条最值钱：上游按**前缀**匹配缓存，命中价只有全价的零头。
// 固定段里混进任何随轮次变化的内容——一个时间戳、一个计数器、一个 map
// 的遍历顺序——缓存整块作废，几千 token 每轮全价计费。
//
// 而且这个 bug 是**静默**的：功能一切正常，日志干干净净，只有账单变贵。
// 没有任何现存的测试守着这条不变量，所以写在这里。
func TestFixedPartIsByteStable(t *testing.T) {
	base := *config.Default()
	base.Brain.WeatherPlace = "示例市"
	g := memory.New(base.Brain.MaxHistory).Group("g", "测试群")

	// 用两组差异最大的动态输入：不同情绪、不同触发者、不同环境、不同群。
	// 固定段在它们之下，必须一模一样。
	cases := []struct {
		name string
		mood MoodSignal
		hint string
		env  string
		who  string
		g    *memory.Group
	}{
		{
			name: "危机",
			mood: MoodSignal{Level: MoodCrisis, Keywords: []string{"活不下去"}},
			hint: "刚刚叫你的这个人叫张三",
			env:  "现在时间：2026年10月02日 01:30:00（周四）",
			who:  "张三",
			g:    g,
		},
		{
			name: "低落",
			mood: MoodSignal{Level: MoodLow},
			hint: "",
			env:  "现在时间：2027年1月1日 12:00:00（周五）",
			who:  "李四",
			g:    memory.New(base.Brain.MaxHistory).Group("另一个群", "另一个群名"),
		},
	}

	// fixedOf 截到**第一个**动态段为止。
	//
	// 边界不能钉死在某一个标记上，只能取「最早出现的那个动态标记」：
	// 动态段里的几块是条件写入的——【相关的人】只在有 masterHint 时写，
	// 【这轮你要回的是】只在有触发者时写，【你现在的状态】才无条件。
	// 钉死一个，两个用例就会因为「谁的标记靠前」而截出不同长度，
	// 测试自己变成假警报（这正是旧版用情绪段当边界的原因：它无条件且最靠前）。
	//
	// 2026-10-04 人设 v2 删掉了按情绪分档的三支，【现在的情况】这个
	// 无条件且最靠前的标记没了，于是改成取四个标记里最早出现的那个。
	// 用例里的 mood 现在不再影响提示词，但保留着：它们仍走同一条调用路径，
	// 能守住「以后谁再往固定段里加情绪相关内容」这个回归。
	fixedOf := func(s string) string {
		at := -1
		for _, marker := range []string{"【相关的人】", "【这轮你要回的是】", "【你现在的状态】", "现在时间："} {
			if i := strings.Index(s, marker); i >= 0 && (at < 0 || i < at) {
				at = i
			}
		}
		if at < 0 {
			return s
		}
		return s[:at]
	}

	first := fixedOf(systemPrompt(base, cases[0].g, cases[0].mood, cases[0].hint, cases[0].env, cases[0].who))
	if first == "" {
		t.Fatal("固定段为空，判据失效")
	}
	for _, c := range cases[1:] {
		got := fixedOf(systemPrompt(base, c.g, c.mood, c.hint, c.env, c.who))
		if got != first {
			t.Errorf("用例 %q 的固定段与基准不一致：前缀缓存按字节匹配，"+
				"混进任何动态内容都会让几千 token 每轮全价计费，且这个 bug 是静默的。"+
				"第一处差异位置 %d", c.name, firstDiff(first, got))
		}
	}

	// 同一个输入连调两次也必须一致。
	//
	// 上一轮循环比的是**不同**输入，只能抓住「每次调用都变」的内容
	//（时间戳、计数器、随机数）。而真正打碎缓存的还有一类更隐蔽的：
	// map 遍历顺序、缓存命中与否、连接池状态——它们**偶尔**才变，
	// 跨轮才看得出，所以同输入重复调用必须一起钉住。
	if again := fixedOf(systemPrompt(base, cases[0].g, cases[0].mood,
		cases[0].hint, cases[0].env, cases[0].who)); again != first {
		t.Errorf("同一输入两次调用的固定段不同（位置 %d）：这类内容（map 顺序、"+
			"缓存命中状态）偶尔才变，跨轮才打碎前缀缓存，只比不同输入抓不到它",
			firstDiff(first, again))
	}
}

// firstDiff 报出两段文本第一处不同的字节位置，用来定位是「哪一句」被混进了动态段。
func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
