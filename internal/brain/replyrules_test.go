package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/config"
)

// 回话分寸必须进系统提示词，且是**独立一节**。
//
// 这是 2026-10-05 新增的字段，为了解决一个具体的抱怨：
// 「很多对于回复的指导找不到地方，放哪里都不合适，只得拆进行业边界中」。
// 它必须独立于【嘴臭的边界】——那一节连同闭嘴/不伺候/红线全是**禁令**，
// 模型读到一整节消极清单时会把它们当成消极人设，而不是当成「怎么回话」。
func TestReplyRulesEnterPromptAsOwnSection(t *testing.T) {
	cfg := config.Config{}
	cfg.Persona.Style = "你说话很随便"
	cfg.Persona.RoastRules = []string{"可以损菜，不损人"}
	cfg.Persona.ReplyRules = []string{
		"他认真问技术问题就多回两句",
		"纯吹牛的话题一笔带过就行",
	}

	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg, e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")

	for _, rule := range cfg.Persona.ReplyRules {
		if !strings.Contains(sys, rule) {
			t.Errorf("回话分寸 %q 没进系统提示词", rule)
		}
	}
	// 必须是**自己的一节**，标题独立
	if !strings.Contains(sys, "【怎么回话】") {
		t.Fatal("回话分寸应有独立小节标题「【怎么回话】」，而不是并进别节")
	}
	// 并且排在【你怎么说话】之后——那一节讲怎么开口，这一节讲接话时往哪儿使力
	styleAt := strings.Index(sys, "【你怎么说话】")
	replyAt := strings.Index(sys, "【怎么回话】")
	if styleAt < 0 || replyAt < 0 || replyAt < styleAt {
		t.Errorf("【怎么回话】应排在【你怎么说话】之后，实际 style=%d reply=%d", styleAt, replyAt)
	}
	// 关键：与「嘴臭的边界」必须是两个小节，不能被并进去
	roastAt := strings.Index(sys, "【嘴臭的边界】")
	if roastAt < 0 {
		t.Fatal("对照组缺失：【嘴臭的边界】本应存在")
	}
	if replyAt == roastAt {
		t.Error("【怎么回话】被并进了【嘴臭的边界】——这正是要解决的问题本身")
	}
	// 反面对照：嘴臭边界该在，确认上面不是因「什么都没进」而空过
	if !strings.Contains(sys, "可以损菜，不损人") {
		t.Error("嘴臭边界本应进系统提示词；它没进，说明上面几条断言是假通过")
	}
}

// 空数组时**不生成**这一节。
//
// 空节留在提示词里有两个坏处：一是白占前缀字节（固定段每轮都带），
// 二是给模型一个「这里该有内容但是空的」信号——
// 人设 v2 清空 catchphrases 就是因为清单本身会影响它的行为。
func TestReplyRulesOmittedWhenEmpty(t *testing.T) {
	cfg := config.Config{}
	cfg.Persona.Style = "你说话很随便"
	cfg.Persona.Catchphrases = []string{"图哪偷的"} // 对照组：本该进

	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg, e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")

	if strings.Contains(sys, "【怎么回话】") {
		t.Error("未填回话分寸时不该生成空的小节")
	}
	if !strings.Contains(sys, "图哪偷的") {
		t.Error("对照组：口头禅本应进系统提示词；它没进，说明上面那条是假通过")
	}
}

// 提示词里的这些小节属于**固定段**，位置不能乱。
//
// 具体挡的是：新增的【怎么回话】若被排到动态段（例如【你现在的状态】）之后，
// 就会击穿 prompt 缓存——上游按前缀匹配，前面几百字节变了就得全量重算，
// 而这些内容逐轮不变，几千 token 本该几乎免费。
func TestReplyRulesStayInFixedSegment(t *testing.T) {
	cfg := config.Config{}
	cfg.Persona.Style = "你说话很随便"
	cfg.Persona.ReplyRules = []string{"他认真问就多回两句"}

	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg, e.mem.Group("g1", "群A"), MoodSignal{}, "", "2026-10-05 03:00", "老张")

	replyAt := strings.Index(sys, "【怎么回话】")
	dynAt := strings.Index(sys, "【你现在的状态】")
	if replyAt < 0 || dynAt < 0 {
		t.Fatalf("小节缺失：reply=%d dynamic=%d", replyAt, dynAt)
	}
	if replyAt > dynAt {
		t.Errorf("【怎么回话】属于固定段，必须排在动态段【你现在的状态】之前，否则击穿 prompt 缓存")
	}
	// 环境行带具体时刻，是典型的动态段内容，用它确认动态段确实在动
	if !strings.Contains(sys, "2026-10-05") {
		t.Error("对照组：环境时间应出现在动态段里")
	}
}

// voice 决定固定段里那几句口吻，而且一个位置上只能有一句。
//
// 空串必须逐字等于改动前的老爹版：两台机器人共用这一段代码，
// 默认值变了等于老爹的人设被悄悄换掉。
// 「软」必须把老爹那几句换掉而不是叠加上去——两句并存时模型每轮
// 随机挑一句执行，这是本文件顶部注释里记过的老毛病。
func TestVoiceSelectsTone(t *testing.T) {
	e := newTestEngine(t, &recordingSender{}, nil, false)
	g := e.mem.Group("g1", "群A")

	def := systemPrompt(config.Config{}, g, MoodSignal{}, "", "", "")
	for _, keep := range []string{
		"别人服你，是因为你话少、说得准、被惹了不急",
		"大部分时候你还是不说话",
		"回「咋」「在」「嗯？」这种一两个字就够",
		"<os>懒得理他</os>",
	} {
		if !strings.Contains(def, keep) {
			t.Errorf("voice 为空时缺了老爹版的 %q：默认口吻被改了", keep)
		}
	}
	for _, gone := range []string{"别人乐意理你", "想逗他一下", "先哼一声"} {
		if strings.Contains(def, gone) {
			t.Errorf("voice 为空时不该出现奶酱版的 %q", gone)
		}
	}

	soft := config.Config{}
	soft.Persona.Voice = "软"
	got := systemPrompt(soft, g, MoodSignal{}, "", "", "")
	for _, want := range []string{
		"别人乐意理你，是因为你在的时候这群更热闹",
		"有反应就开口，但别把每条缝都填上",
		"一个语气词、先哼一声、或者一句反问就够",
		"<os>想逗他一下</os>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("voice=软 时缺了 %q", want)
		}
	}
	for _, gone := range []string{"别人服你", "懒得理他", "大部分时候你还是不说话"} {
		if strings.Contains(got, gone) {
			t.Errorf("voice=软 时老爹版的 %q 还在：两句并存，模型会随机挑一句", gone)
		}
	}
}
