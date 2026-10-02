package brain

import (
	"testing"
)

// 「有人在跟别人说话」时不该抢话。
//
// 2026-10-01 生产实况（测试群一号群）：
//
//	23:26:38  群友甲：<@WorkBuddy> 说话      ← at=false，@给的是别人
//	23:26:38  群友甲：（对方回了一条）
//	23:26:53  群友甲：你在干嘛
//	23:26:55  羽沫老爹：煮面呢 咋了              ← 抢话了
//
// 冲动值 0.25(闲聊) + 0.15(攒批) = 0.40，恰好 ≥ 阈值 0.40。
//
// 这些测试直接调真实方法（recordAtOthers / applyBatchPenalties），
// 不复刻逻辑——之前那版复刻版在把修复改成 `if false` 时依然全绿，
// 属于假绿，钉不住任何东西。
const prodThreshold = 0.4

// 攒批：记一条消息，返回本批累计的冲动值。
// atMe / atOthers 直接对应平台给的两个字段。
func batch(st *groupState, atMe, atOthers bool, newCount int) float64 {
	st.newCount = newCount
	st.recordAtOthers(atMe, atOthers)
	imp := ComputeImpulse(ImpulseInput{
		AtMe: atMe, Chatter: true, NewCount: newCount,
	})
	if imp.Score > st.impulse {
		st.impulse = imp.Score
		st.reasons = imp.Reasons
	}
	st.applyBatchPenalties()
	return st.impulse
}

func TestAtOthersThenChatterStaysUnderThreshold(t *testing.T) {
	st := &groupState{}
	// 第一条 @ 了别人
	batch(st, false, true, 1)
	// 后面跟一堆闲聊。必须攒到 5 条才有 wManyNew，
	// 0.25+0.15=0.40 才是生产实况里那个「恰好压线过闸」的值；
	// 只攒 3 条的话 0.25 本来就够不着门限，测了也测不出什么。
	got := batch(st, false, false, 5)

	if got >= prodThreshold {
		t.Errorf("有人在跟别人说话时不该过门限：impulse=%.2f >= %.2f（生产实况就是这里抢的话）",
			got, prodThreshold)
	}
	if len(st.reasons) == 0 || st.reasons[len(st.reasons)-1] != "有人在跟别人说话" {
		t.Errorf("原因里应记上「有人在跟别人说话」，实际 %v", st.reasons)
	}
}

// 前置条件：不扣分时那次实况确实会过线。
// 没有这条，上面那个测试可能是因为「本来也进不去」而绿，那就没意义了。
func TestWithoutPenaltyProdIncidentWouldPass(t *testing.T) {
	st := &groupState{}
	// 攒到 5 条才有 wManyNew，0.25+0.15=0.40 才是实况里那个压线值
	for n := 1; n <= 5; n++ {
		imp := ComputeImpulse(ImpulseInput{Chatter: true, NewCount: n})
		if imp.Score > st.impulse {
			st.impulse = imp.Score
		}
	}
	if st.impulse < prodThreshold {
		t.Fatalf("前置条件不成立：不扣分时应为 0.40 恰好过线，实际 %.2f", st.impulse)
	}
	t.Logf("不扣分时 impulse=%.2f ≥ 阈值 %.2f → 抢话（2026-10-01 实况）",
		st.impulse, prodThreshold)
}

// 后来有人 @ 了机器人 → 撤销 atOthers，且不再压分
func TestAtMeClearsAtOthers(t *testing.T) {
	st := &groupState{}
	batch(st, false, true, 1)
	if !st.atOthers {
		t.Fatal("前置条件：@ 他人后 atOthers 应为 true")
	}
	got := batch(st, true, false, 2)
	if st.atOthers {
		t.Error("被 @ 后应撤销 atOthers")
	}
	if got < 0.8 {
		t.Errorf("被 @ 时冲动值应保持高位（wAtMe=0.90），实际 %.2f", got)
	}
}

// 「先 @ 别人、后 @ 我」很常见，该回的是我
func TestAtOthersThenAtMeStillReplies(t *testing.T) {
	st := &groupState{}
	batch(st, false, true, 1)
	got := batch(st, true, false, 2)
	if got < prodThreshold {
		t.Errorf("被 @ 时应过门限，实际 %.2f", got)
	}
	if len(st.reasons) == 0 || st.reasons[0] != "被@" {
		t.Errorf("触发原因应记为「被@」，实际 %v", st.reasons)
	}
}

// 只被叫名字（没 @）也该撤销——那也是明确在跟它说话
func TestNameCalledClearsAtOthers(t *testing.T) {
	st := &groupState{}
	batch(st, false, true, 1)
	if !st.atOthers {
		t.Fatal("前置条件：@ 他人后 atOthers 应为 true")
	}
	// 后一条叫了它的名字（没 @）。recordAtOthers 只在 atOthers 时置位、
	// atMe 时撤销，nameCalled 是在别处置的——所以这里模拟真实顺序：
	// 先记 @ 他人，再由叫名字那条把标记压下去。
	st.atOthers = false
	st.nameCalled = true
	st.impulse = wNameCalled // 被叫名字的分数
	st.applyBatchPenalties()
	if st.impulse < prodThreshold {
		t.Errorf("被叫名字时不该被压分，实际 %.2f", st.impulse)
	}
}

// 纯闲聊（没人 @ 任何人）时不该被压——这条规则不能误伤正常插话。
// 攒到 5 条才有 wManyNew，3 条只是 0.25，本来就够不着门限。
func TestPlainChatterNotPenalized(t *testing.T) {
	st := &groupState{}
	got := batch(st, false, false, 5) // 0.25 + 0.15 = 0.40，恰好过线
	if got < prodThreshold {
		t.Errorf("普通闲聊攒够一批时不该被压分，实际 %.2f", got)
	}
}
