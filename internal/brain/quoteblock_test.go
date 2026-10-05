package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

// 这组用例对应 2026-10-05 的第二个事故：修好 refIdx 解析后，
// 引用气泡从「从来不出」变成「每条都出」——用户在群里实测受不了这个观感。
//
// 引用什么时候该出现是个语气判断，只能交给模型，所以它是一个块级标记（Block.Q），
// 而不是一个全局开关。

// 模型没开口要引用，就不许有引用气泡。
//
// 平台每条消息都给 msg_idx 里的 REFIDX，所以「程序自己决定要不要引用」
// 等价于「永远引用」。这条用例守住那个错误的反面。
func TestNoQuoteMarkerMeansNoQuote(t *testing.T) {
	r := &recordingSender{}
	e := newTestEngine(t, r, nil, false)
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	g := memory.NewGroup("g1", "群A")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: "随口接一句"},
	}, "", 5, nil, nil)
	if !ok {
		t.Fatal("应保留内容")
	}
	e.deliver(cfg.Get(), g, blocks, "openid-a", false)

	if len(r.quoted) != 0 {
		t.Fatalf("模型没要引用就不该发引用气泡，实际 %+v", r.quoted)
	}
	if len(r.texts) != 1 {
		t.Fatalf("内容该照常发出去，实际 %d 条", len(r.texts))
	}
}

// 模型开口了就真的有引用气泡。
func TestQuoteMarkerProducesQuote(t *testing.T) {
	r := &recordingSender{}
	e := newTestEngine(t, r, nil, false)
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	g := memory.NewGroup("g1", "群A")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: "这句我不同意", Q: true},
	}, "", 5, nil, nil)
	if !ok {
		t.Fatal("应保留内容")
	}
	e.deliver(cfg.Get(), g, blocks, "openid-a", false)

	if len(r.quoted) != 1 || r.quoted[0] != "这句我不同意" {
		t.Fatalf("该有一条走引用通道，实际 %+v", r.quoted)
	}
}

// 一轮最多一条带引用。
//
// 连着两条都套引用卡片看着像在强调自己，而且被动回复额度本来就该省着用。
func TestQuoteAtMostOncePerRound(t *testing.T) {
	r := &recordingSender{}
	e := newTestEngine(t, r, nil, false)
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	g := memory.NewGroup("g1", "群A")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: "第一句要引用", Q: true},
		{T: BlockTypeText, C: "第二句别引了", Q: true},
		{T: BlockTypeText, C: "第三句也别引"},
	}, "", 5, nil, nil)
	if !ok {
		t.Fatal("应保留内容")
	}
	e.deliver(cfg.Get(), g, blocks, "openid-a", false)

	if len(r.quoted) != 1 {
		t.Fatalf("一轮最多一条引用，实际 %d 条：%+v", len(r.quoted), r.quoted)
	}
	if len(r.texts) != 3 {
		t.Fatalf("三条内容都该发出去，实际 %d 条", len(r.texts))
	}
}

// Q 必须一路活到 planDelivery 之后。
//
// 引用标记只挂在 Block 上，中间隔着 allowBlocks 和 planDelivery 两层，
// 任何一层漏抄字段，结果就是「模型要了引用但发出去没有」——
// 而且不会有任何报错，只会安静地少一个气泡，很难发现。
func TestQuoteMarkerSurvivesPlanDelivery(t *testing.T) {
	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: "甲乙丙丁戊己庚辛", Q: true},
		{T: BlockTypeText, C: "下一句", Q: false},
	}, "", 5, nil, nil)
	if !ok {
		t.Fatal("应保留内容")
	}
	plan := planDelivery(blocks, 40, 5)
	if len(plan) == 0 {
		t.Fatal("不该被清空")
	}
	if !plan[0].Q {
		t.Errorf("第一段该带着引用标记，实际 %+v", plan[0])
	}
	for _, b := range plan[1:] {
		if b.Q {
			t.Errorf("后面的段不该跟着带引用标记，实际 %+v", b)
		}
	}
}

// 引用标记不该影响文字本身，也不该混进记忆里的内容。
func TestQuoteMarkerDoesNotAlterText(t *testing.T) {
	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeText, C: "就这一句", Q: true},
	}, "", 5, nil, nil)
	plan := planDelivery(blocks, 40, 5)
	if len(plan) != 1 {
		t.Fatalf("应展开成一条，实际 %d", len(plan))
	}
	if strings.Contains(plan[0].C, "q") && strings.Contains(plan[0].C, `"`) {
		t.Errorf("内容里不该冒出标记本身，实际 %q", plan[0].C)
	}
	if plan[0].C != "就这一句" {
		t.Errorf("内容应原样，实际 %q", plan[0].C)
	}
}

// 提示词必须教模型这件事，否则它不会凭空写 q:true。
func TestPromptTeachesQuote(t *testing.T) {
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg.Get(), e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")
	for _, want := range []string{"【艾特和引用】", `"q":true`, "一轮里最多第一条"} {
		if !strings.Contains(sys, want) {
			t.Errorf("提示词里应有 %q", want)
		}
	}
}
