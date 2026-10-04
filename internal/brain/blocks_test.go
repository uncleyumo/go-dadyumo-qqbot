package brain

import (
	"strings"
	"testing"
)

// blocks 是「模型说什么」到「实际发什么」之间唯一的闸门。
// 这些测试锁的是：模型填得很随意时，群里不能出现空白消息、超发、或顺序错乱。

func tb(c string) Block  { return Block{T: BlockTypeText, C: c} }
func ib(id int64) Block  { return Block{T: BlockTypeImg, ID: id} }

// 只有文字时，blocks 为空要回落到 text 字段（兼容旧格式）
func TestAllowBlocksFallsBackToText(t *testing.T) {
	got, ok := allowBlocks(nil, "第一句\n第二句", 5, nil)
	if !ok {
		t.Fatal("应有内容可发")
	}
	if len(got) != 2 || got[0].C != "第一句" || got[1].C != "第二句" {
		t.Fatalf("应按 \\n 拆成两个块，got %#v", got)
	}
}

// 全空 → 不发言。发空消息在 QQ 里显示成一条空气，比不说话糟得多
func TestAllowBlocksAllEmptyNoSpeak(t *testing.T) {
	cases := []struct {
		name   string
		blocks []Block
		text   string
	}{
		{"什么都没有", nil, ""},
		{"只有空白", nil, "   \n  "},
		{"空文字块", []Block{{T: BlockTypeText, C: "  "}}, ""},
		{"图片没有 ID", []Block{{T: BlockTypeImg, ID: 0}}, ""},
		{"未知类型", []Block{{T: "video", C: "x"}}, ""},
	}
	for _, c := range cases {
		if _, ok := allowBlocks(c.blocks, c.text, 5, nil); ok {
			t.Errorf("%s: 不该认为有内容可发", c.name)
		}
	}
}

// 「只甩一张图什么都不说」是群里最常见的表情包用法，必须能表达
func TestAllowBlocksImageOnly(t *testing.T) {
	got, ok := allowBlocks([]Block{ib(42)}, "", 5, nil)
	if !ok {
		t.Fatal("只有图片也算有内容")
	}
	if len(got) != 1 || got[0].T != BlockTypeImg || got[0].ID != 42 {
		t.Fatalf("got %#v", got)
	}
}

// 顺序必须原样保留——「先说后甩」和「先甩后说」在群里是两回事
func TestAllowBlocksPreservesOrder(t *testing.T) {
	in := []Block{tb("看这个"), ib(7), tb("补一句")}
	got, ok := allowBlocks(in, "", 5, nil)
	if !ok || len(got) != 3 {
		t.Fatalf("应原样保留 3 块，got %#v", got)
	}
	if got[0].T != BlockTypeText || got[1].T != BlockTypeImg || got[2].T != BlockTypeText {
		t.Errorf("顺序变了: %#v", got)
	}
}

// 超预算要截断，且优先保前面的（后面的内容在群里已经过期了）
func TestAllowBlocksTruncates(t *testing.T) {
	in := []Block{tb("1"), tb("2"), tb("3"), tb("4"), tb("5"), ib(9)}
	got, ok := allowBlocks(in, "", 3, nil)
	if !ok || len(got) != 3 {
		t.Fatalf("应截断到 3 块，got %d", len(got))
	}
	if got[0].C != "1" || got[2].C != "3" {
		t.Errorf("应保留最前面的，got %#v", got)
	}
}

// planDelivery：超长文字要拆条，图片算一条
func TestPlanDeliverySplitsText(t *testing.T) {
	long := strings.Repeat("啊", 100)
	got := planDelivery([]Block{tb(long)}, 40, 5)
	if len(got) < 2 {
		t.Fatalf("长文字应拆成多条，got %d", len(got))
	}
	for _, b := range got {
		if b.T != BlockTypeText {
			t.Fatalf("纯文字不该变出别的类型: %#v", b)
		}
	}
}

// 硬上限：同一条消息最多回 5 次，文字图片共用。
// 超了就截断——不能指望平台替我们拦，那样会表现为「话说一半」。
func TestPlanDeliveryRespectsPassiveLimit(t *testing.T) {
	blocks := []Block{
		tb("第一句很长很长很长很长很长很长很长"), tb("第二句很长很长很长很长很长很长很长"),
		ib(1), tb("第三句很长很长很长很长很长很长很长"), tb("第四句很长很长很长很长很长很长很长"),
		ib(2), tb("第五句很长很长很长很长很长很长很长"),
	}
	got := planDelivery(blocks, 20, 5)
	if len(got) > 5 {
		t.Fatalf("不得超过 5 条（QQ 的被动回复上限），got %d: %#v", len(got), got)
	}
}

// planDelivery 也不能产出空块
func TestPlanDeliveryNoEmptyBlocks(t *testing.T) {
	got := planDelivery([]Block{{T: BlockTypeText, C: "   "}, tb("真话")}, 40, 5)
	for _, b := range got {
		if b.T == BlockTypeText && strings.TrimSpace(b.C) == "" {
			t.Errorf("不该有空的文字块: %#v", b)
		}
	}
	if len(got) != 1 {
		t.Errorf("只该留下真话那一条，got %#v", got)
	}
}

// textOf：只有文字进记忆，图片不进（图片另走留痕）
func TestTextOf(t *testing.T) {
	got := textOf([]Block{tb("看这个"), ib(7), tb("补一句")})
	if got != "看这个 补一句" {
		t.Errorf("got %q", got)
	}
	if textOf([]Block{ib(1)}) != "" {
		t.Error("纯图片不该产生文本")
	}
}

func TestHasImage(t *testing.T) {
	if !hasImage([]Block{tb("a"), ib(1)}) {
		t.Error("应识别出含图片")
	}
	if hasImage([]Block{tb("a")}) {
		t.Error("纯文字不该判定含图片")
	}
}

// planDelivery 要过一遍出口清理：@全体成员 那类假动作不能发出去
func TestPlanDeliveryCleansFakeMentions(t *testing.T) {
	got := planDelivery([]Block{tb("自己@all不就完了"), tb("我又不是喊人的")}, 40, 5)
	for _, b := range got {
		if b.C != "" && strings.Contains(b.C, "@all") {
			t.Errorf("出口清理没拦住 @all: %q", b.C)
		}
	}
}