package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// 成功一次置满 5 轮，**不累加**。
//
// 累加看着更慷慨，实际有害：同一个模型连续救场时额度会一路涨到十几轮，
// 而那段时间上下文早滚走了好几轮，它早已不是最贴合的选择。
// 置满让「最后兜住的那个模型」稳定占优。
func TestBoostSetsNotAccumulates(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.Boost("a|m1")
	g.Boost("a|m1")
	g.Boost("a|m1")
	g.mu.Lock()
	n := g.boosted["a|m1"]
	g.mu.Unlock()
	if n != BoostRounds {
		t.Fatalf("重复提权应置满 %d，实际 %d", BoostRounds, n)
	}
}

// 所有目标同步递减，**不是谁被选中谁才减**。
//
// 这是 2026-10-05 讨论定下的规则：轮次是这个群共同的时间轴。
// 若只在被选中时递减，没被选中的那个会把额度一直攥着，
// 哪怕它已经十几轮没上场——它的上下文早就走远了，
// 继续占着优先位只是让真正合适的模型排不到前面。
func TestTickBoostDecrementsAllTargets(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.Boost("a|m1") // 假设 B 先前救过场
	g.Boost("b|m2") // 随后 C 又救了一次

	// 走一轮，两个都该减，且谁都没被"选中"
	g.TickBoost()
	g.mu.Lock()
	n1, n2 := g.boosted["a|m1"], g.boosted["b|m2"]
	g.mu.Unlock()
	if n1 != BoostRounds-1 || n2 != BoostRounds-1 {
		t.Fatalf("所有提权应同步递减，实际 m1=%d m2=%d", n1, n2)
	}

	// 减到 0 的移出，不能留在表里白占位置
	for i := 0; i < BoostRounds; i++ {
		g.TickBoost()
	}
	if got := g.BoostedTargets(); len(got) != 0 {
		t.Fatalf("额度耗尽后应移出，实际还剩 %v", got)
	}
}

// 多个模型可以同时提权，按剩余轮数从多到少返回。
// 「最近救回来的那个」靠置满自然占优，老的自己淡出。
func TestBoostedTargetsSortedByRemaining(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.Boost("old|m1")
	g.Boost("new|m2")

	// 走一轮，让两者剩余不同：谁被提权得晚，谁就该排前面。
	g.TickBoost()
	g.Boost("new|m2") // C 兜底成功，重新置满 → 5；old 只剩 4

	got := g.BoostedTargets()
	if len(got) != 2 {
		t.Fatalf("应有两个提权目标，实际 %v", got)
	}
	if got[0] != "new|m2" {
		t.Errorf("剩余轮数多的（刚兜底成功的）应排前面，实际 %v", got)
	}
}

// 提权是纯运行时状态，**绝不落盘**。
// 重启后不该记得半小时前谁救过场——那只会造成莫名其妙的偏袒。
func TestBoostNotPersisted(t *testing.T) {
	// 落盘是白名单制：groupSnapshot 显式列字段，没列的天然不落盘。
	// 这里直接把该结构序列化一遍，确认 JSON 里没有提权痕迹。
	g := NewGroup("g1", "群A")
	g.Boost("a|m1")

	b, err := json.Marshal(groupSnapshot{OpenID: "g1", Name: "群A"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "boost") {
		t.Errorf("提权状态不应出现在落盘 JSON 里: %s", string(b))
	}
}

func TestBoostEmptyKeyIgnored(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.Boost("")
	if got := g.BoostedTargets(); len(got) != 0 {
		t.Errorf("空 key 不应产生提权，实际 %v", got)
	}
}