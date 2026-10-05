package brain

import (
	"strings"

	"dadyumo/internal/config"
	"testing"

	"dadyumo/internal/memory"
)

// atNameOK 是一个「什么名字都认」的解析器，用于只测管道形状的用例。
func atNameOK(name string) string { return "OID-" + name }

// atOpenIDOK 只认 atNameOK 造出来的那些 openid，用于照抄标签的用例。
func atOpenIDOK(openID string) bool { return strings.HasPrefix(openID, "OID-") }

// at 块必须渲染成平台的 @ 内嵌标签。
//
// QQ 的 @ 不是 msg_type=5（那是 botgo 频道时代的遗留常量，群/单聊接口里
// 枚举的 msg_type 只有 0/2/7），而是文本内嵌标签。旧格式 <@userid> 官方标注
// 「即将弃用」且客户端已不再解析，会原样显示成字面量。
func TestAtBlockRendersPlatformTag(t *testing.T) {
	got, ok := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "老张"},
		{T: BlockTypeText, C: "这图是你吧"},
	}, "", 5, atNameOK, atOpenIDOK)
	if !ok || len(got) != 2 {
		t.Fatalf("应保留 at 与 text 两个块，实际 ok=%v n=%d", ok, len(got))
	}
	if got[0].T != BlockTypeAt || got[0].C != "OID-老张" {
		t.Fatalf("at 块应把解析出的 openid 存在 C 里待渲染，实际 %+v", got[0])
	}

	plan := planDelivery(got, 40, 5)
	if len(plan) != 1 {
		t.Fatalf("at 块不该单独占一条消息，实际展开成 %d 条: %+v", len(plan), plan)
	}
	want := `<qqbot-at-user id="OID-老张" /> 这图是你吧`
	if plan[0].C != want {
		t.Errorf("at 应渲染成平台标签并作为前缀，实际 %q", plan[0].C)
	}
}

// at 块挂在**紧随**的那句上，不许飘到下一轮去。
//
// 「@张三 我觉得不是这样」和「我 @ 张三 说不是这样」在群里是两件事。
func TestAtAttachesToTheFollowingText(t *testing.T) {
	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "老张"},
		{T: BlockTypeText, C: "第一条"},
		{T: BlockTypeText, C: "第二条"},
	}, "", 5, atNameOK, atOpenIDOK)

	plan := planDelivery(blocks, 40, 5)
	if len(plan) != 2 {
		t.Fatalf("两条文字应展开成两条消息，实际 %d: %+v", len(plan), plan)
	}
	if !strings.Contains(plan[0].C, "老张") {
		t.Errorf("@ 应挂在第一条上，实际 %q", plan[0].C)
	}
	if strings.Contains(plan[1].C, "qqbot-at-user") {
		t.Errorf("第二条不该再带 @（一轮艾特一个人就够），实际 %q", plan[1].C)
	}
}

// 名字对不上群成员表时，这个 at 块必须被丢掉。
//
// 猜错 @ 人比不 @ 糟得多：那等于当着全群艾特了一个不相干的人。
// 而模型照抄的名字对不上是常事（群名片带后缀、同名、它自己写错）。
func TestAtDroppedWhenNameUnresolvable(t *testing.T) {
	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "查无此人"},
		{T: BlockTypeText, C: "说句话"},
	}, "", 5, func(string) string { return "" }, nil)

	if !ok {
		t.Fatal("文字还在，仍算有内容可发")
	}
	for _, b := range blocks {
		if b.T == BlockTypeAt {
			t.Fatal("对不上人的 @ 块必须丢掉——猜错 @ 人比不 @ 糟得多")
		}
	}
	plan := planDelivery(blocks, 40, 5)
	if len(plan) != 1 || strings.Contains(plan[0].C, "qqbot-at-user") {
		t.Errorf("丢掉 at 块后应只发一句普通文字，实际 %+v", plan)
	}
}

// @全体成员 仍然必须删掉。
//
// 2026-10-01 真实事故的回归钉子：有人说「把该打游戏的人艾特出来」，
// 模型学了聊天记录里 @全体成员 的字样，回了「你自己@all不就完了」——
// 演一个它压根做不到的动作。
//
// 平台侧也确认过这动作做不到：<qqbot-at-everyone /> 官方标注
// 「仅在文字子频道可用」，群聊等于不支持。所以删掉是对的，不是保守。
func TestAtAllStillRemovedEvenThoughAtIsAllowed(t *testing.T) {
	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "@全体成员"},
		{T: BlockTypeText, C: "起床了"},
	}, "", 5, atNameOK, atOpenIDOK)
	if !ok {
		t.Fatal("文字还在，仍算有内容可发")
	}
	for _, b := range blocks {
		if b.T == BlockTypeAt {
			t.Error("@全体成员 必须删掉——平台不支持这个动作，演出来只会露馅")
		}
	}
	// 出口那条老路也要继续有效
	if s := stripAtAll("你自己@all不就完了"); strings.Contains(s, "@all") {
		t.Errorf("stripAtAll 必须继续删掉 @all，实际 %q", s)
	}
}

// 一个 text 块里只留第一个 @。
//
// 「@甲 @乙 说句话」在群里像挨个点名刷存在感，人不会那么发。
func TestOnlyOneAtPerDelivery(t *testing.T) {
	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "甲"},
		{T: BlockTypeAt, Name: "乙"},
		{T: BlockTypeText, C: "说句话"},
	}, "", 5, atNameOK, atOpenIDOK)

	plan := planDelivery(blocks, 40, 5)
	if len(plan) != 1 {
		t.Fatalf("应只展开成一条，实际 %d: %+v", len(plan), plan)
	}
	if n := strings.Count(plan[0].C, "qqbot-at-user"); n != 1 {
		t.Errorf("一句话里只该有一个 @，实际 %d 个: %q", n, plan[0].C)
	}
}

// at 后面紧跟图片时，这个 @ 没有可修饰的对象，丢掉。
//
// 「@张三」+ 一张图，在 QQ 上没有「把 @ 加在图片前面」这种表达。
func TestAtBeforeImageIsDropped(t *testing.T) {
	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "老张"},
		{T: BlockTypeImg, ID: 42},
	}, "", 5, atNameOK, atOpenIDOK)

	plan := planDelivery(blocks, 40, 5)
	for _, b := range plan {
		if strings.Contains(b.C, "qqbot-at-user") {
			t.Errorf("@ 后面是图时不该保留该 @，实际 %+v", plan)
		}
	}
}

// at 块不占条数预算。
//
// 它是前缀不是消息——真人不会单发一条只有 @ 的消息。
// 这条保证引入 at 之后「一轮最多 N 条」的口径不变。
func TestAtDoesNotConsumeSegmentBudget(t *testing.T) {
	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "老张"},
		{T: BlockTypeText, C: "一句话"},
	}, "", 5, atNameOK, atOpenIDOK)

	plan := planDelivery(blocks, 40, 1)
	if len(plan) != 1 {
		t.Fatalf("maxSent=1 时应只有一条，实际 %d: %+v", len(plan), plan)
	}
	if !strings.Contains(plan[0].C, "qqbot-at-user") {
		t.Errorf("预算只够一条时 @ 仍应挂在那条上，实际 %q", plan[0].C)
	}
}

// 提示词不许再说「你没有 @ 的能力」。
//
// 那句话是 2026-10-05 之前写的，那时候确实接了真 @。留着它会让模型
// 被明确告知自己做不到，于是它只会用正文里的字面 @昵称 演一个假的动作。
func TestPromptNoLongerDeniesAtCapability(t *testing.T) {
	var cfg config.Config
	cfg.Persona.Style = "你说话很随便"
	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg, e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")

	if strings.Contains(sys, "你压根没有 @") || strings.Contains(sys, "不用写") && strings.Contains(sys, "平台上根本不会真的艾特") {
		t.Error("提示词仍在否认 @ 能力——接了真 @ 之后这会让模型去演假的动作")
	}
	// 反面对照：@全体成员 做不得到这件事必须还留着（2026-10-01 事故的教训）
	if !strings.Contains(sys, "@全体成员 你做不到") {
		t.Error("「@全体成员 做不到」这句必须保留——平台确实不支持，删了模型会去演它")
	}
}

// 提示词必须教 at 块怎么用，否则模型不会写它。
func TestPromptDocumentsAtBlock(t *testing.T) {
	var cfg config.Config
	cfg.Persona.Style = "你说话很随便"
	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg, e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")

	// 提示词里写的是 JSON 字面量 {"t":"at","name":"群名片"}，
	// 到运行时就是普通引号——断言要用**未转义**的写法。
	if !strings.Contains(sys, `{"t":"at","name"`) {
		t.Error("输出格式说明里必须给出 at 块的写法——否则模型根本不会写它")
	}
	if !strings.Contains(sys, "绝大多数时候不用 at") {
		t.Error("必须说清「少用」——能艾特不等于该艾特，不写这条会变成刷存在感")
	}
}

// at 的名字要能用真实的成员表解析出来（端到端的映射）。
func TestAtNameResolvesThroughMemberTable(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.TouchMember("OID-1", "老张")
	g.TouchMemberCard("OID-1", "张工")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeAt, Name: "张工"},
		{T: BlockTypeText, C: "在吗"},
	}, "", 5, func(n string) string {
		oid, _ := lookupMemberOpenID(g, n)
		return oid
	}, func(openID string) bool {
		_, known := g.MemberOf(openID)
		return known
	})
	if !ok {
		t.Fatal("应保留内容")
	}
	found := false
	for _, b := range blocks {
		if b.T == BlockTypeAt {
			found = true
			if b.C != "OID-1" {
				t.Errorf("群名片「张工」应解析到 OID-1，实际 %q", b.C)
			}
		}
	}
	if !found {
		t.Fatal("群名片能对上时 at 块不该被丢")
	}
}
