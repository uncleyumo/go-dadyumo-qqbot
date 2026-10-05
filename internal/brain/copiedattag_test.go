package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/memory"
)

// 这组用例对应 2026-10-05 的生产事故：
// 模型不写 {"t":"at","name":...}，而是照抄出口渲染出来的标签
// <qqbot-at-user id="…" />，而那串 openid 是它自己编的。
// 生产实况里它蒙对了一个真实成员，于是没人发现这条路径根本没被管过。

// 照抄的标签，openid 在成员表里 → 收编成正规的 at 块，
// 走和协议 at 块完全一样的渲染与校验。
func TestCopiedAtTagBecomesRealAtBlock(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.TouchMember("OID-老王", "老王")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: `<qqbot-at-user id="OID-老王" /> 出来看戏`},
	}, "", 5, nil, func(openID string) bool {
		_, known := g.MemberOf(openID)
		return known
	})
	if !ok {
		t.Fatal("应保留内容")
	}
	if len(blocks) != 2 || blocks[0].T != BlockTypeAt || blocks[0].C != "OID-老王" {
		t.Fatalf("标签应收编成 at 块，实际 %+v", blocks)
	}
	plan := planDelivery(blocks, 40, 5)
	if len(plan) != 1 {
		t.Fatalf("at 不该单独占一条，实际 %+v", plan)
	}
	if want := `<qqbot-at-user id="OID-老王" /> 出来看戏`; plan[0].C != want {
		t.Errorf("渲染结果不对，实际 %q", plan[0].C)
	}
}

// 幻觉 openid：标签删掉，句子留下。
//
// 这是这次改动最要紧的一条——原样发出去等于当着全群艾特一个
// 不存在的人，或者把裸 openid 泄进群里。
func TestCopiedAtTagWithUnknownOpenIDIsStripped(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.TouchMember("OID-老王", "老王")

	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: `<qqbot-at-user id="模型编的" /> 出来看戏`},
	}, "", 5, nil, func(openID string) bool {
		_, known := g.MemberOf(openID)
		return known
	})
	if !ok {
		t.Fatal("句子还在，仍算有内容可发")
	}
	for _, b := range blocks {
		if strings.Contains(b.C, "qqbot-at-user") {
			t.Fatalf("对不上人的标签必须删掉，实际 %+v", b)
		}
		if b.T == BlockTypeAt {
			t.Fatalf("不许凭空造出一个 at 块，实际 %+v", b)
		}
	}
	if len(blocks) != 1 || blocks[0].C != "出来看戏" {
		t.Fatalf("句子应原样留下，实际 %+v", blocks)
	}
}

// 整条只有标签、没有正文时，删完就是空气，不能发出去。
func TestCopiedAtTagAloneIsDropped(t *testing.T) {
	blocks, ok := allowBlocks([]Block{
		{T: BlockTypeText, C: `<qqbot-at-user id="编的" />`},
	}, "", 5, nil, func(string) bool { return false })
	if ok || len(blocks) != 0 {
		t.Fatalf("只剩一个假标签时不该发任何东西，实际 ok=%v %+v", ok, blocks)
	}
}

// 标签夹在句子中间：不提成 at 块（前面半句话就没地方放了），
// 但也不能让它原样发出去。
func TestCopiedAtTagInsideSentenceIsStrippedNotPromoted(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.TouchMember("OID-老王", "老王")

	blocks, _ := allowBlocks([]Block{
		{T: BlockTypeText, C: `我喊一下 <qqbot-at-user id="OID-老王" /> 出来看戏`},
	}, "", 5, nil, func(openID string) bool {
		_, known := g.MemberOf(openID)
		return known
	})
	for _, b := range blocks {
		if b.T == BlockTypeAt {
			t.Fatalf("句中的标签不该提成 at 块，实际 %+v", blocks)
		}
	}
	if strings.Contains(blocks[0].C, "qqbot-at-user") {
		t.Fatalf("标签必须从正文里删掉，实际 %q", blocks[0].C)
	}
	if !strings.Contains(blocks[0].C, "出来看戏") {
		t.Fatalf("句子不该被吞掉，实际 %q", blocks[0].C)
	}
}

// 写进记忆的必须是还原后的形态。
//
// 这是事故的**传播路径**：裸标签进了记忆 → 模型把它当自己说过的话学 →
// 下一轮继续编 openid。换回 〔@名字〕 就把这条路径断了。
func TestSpokenForMemoryReplacesTagWithName(t *testing.T) {
	g := memory.NewGroup("g1", "群A")
	g.TouchMember("OID-老王", "老王")

	got := spokenForMemory([]string{`<qqbot-at-user id="OID-老王" /> 出来看戏`}, g)
	if got != "〔@老王〕 出来看戏" {
		t.Fatalf("记忆里应写成〔@名字〕，实际 %q", got)
	}

	// 认不出来的人（退群了/没登记）整个标签删掉，别留半截。
	if got := spokenForMemory([]string{`<qqbot-at-user id="查无此人" /> 就这吧`}, g); got != "就这吧" {
		t.Fatalf("认不出的标签应整个删掉，实际 %q", got)
	}
}
