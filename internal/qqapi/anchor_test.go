package qqapi

import (
	"sync"
	"testing"
)

// TestPickPrefersTargetSender 是「回复挂错人」的核心回归测试。
//
// 攒批窗口里不同人的消息会混在一起。如果实现只是取「群里最新那条」，
// 那么模型回答 A 的问题、消息却挂到 B 名下——内容是对的，对话关系是错的。
func TestPickPrefersTargetSender(t *testing.T) {
	p := NewAnchorPool(20)
	// 张三先说话，李四后说话（李四的更「新」）
	p.Add("g1", "m-old", "openid-zhang", "张三", "")
	p.Add("g1", "m-new", "openid-li", "李四", "")

	msgID, _, seq, ok := p.PickAndReserve("g1", "openid-zhang")
	if !ok {
		t.Fatal("应能取到锚点")
	}
	if msgID != "m-old" {
		t.Errorf("指定回张三时应挂他的消息，却拿到 %q", msgID)
	}
	if seq != 1 {
		t.Errorf("首个 msg_seq 应为 1，got %d", seq)
	}

	// 不指定回谁时才退回「最新那条」
	msgID2, _, _, _ := p.PickAndReserve("g1", "")
	if msgID2 != "m-new" {
		t.Errorf("未指定时应挂最新一条，got %q", msgID2)
	}
}

// TestSeqNeverRegressAfterRelease 是「一次网络抖动让某个群彻底哑火」的回归测试。
//
// 早期实现在发送失败时把 seq 回滚，于是下一次重发又用回那个已被平台消费过的
// seq，被判重复拒掉；seq 被钉死之后这个群在锚点 TTL 内一个字都发不出去。
func TestSeqNeverRegressAfterRelease(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "openid-a", "张三", "")

	_, _, seq1, _ := p.PickAndReserve("g1", "openid-a")
	if seq1 != 1 {
		t.Fatalf("首个 seq 应为 1，got %d", seq1)
	}
	// 发送失败
	p.Release("g1", "m1")

	_, _, seq2, _ := p.PickAndReserve("g1", "openid-a")
	if seq2 <= seq1 {
		t.Fatalf("失败后重试的 msg_seq 必须大于已用过的 %d，got %d（回滚会让平台判重复）", seq1, seq2)
	}

	// 额度确实还回去了：还能再取 4 次（passiveMaxUse=5）
	for i := 0; i < 4; i++ {
		if _, _, _, ok := p.PickAndReserve("g1", "openid-a"); !ok {
			t.Fatalf("额度应已归还，第 %d 次取用不应失败", i+2)
		}
	}
	if _, _, _, ok := p.PickAndReserve("g1", "openid-a"); ok {
		t.Error("额度用满 5 次后不应再能取用")
	}
}

// TestConcurrentPickGetsDistinctSeq 覆盖被动应答与攒批发言并发时的重号问题。
//
// 旧实现里 Pick 只读、MarkUsed 在发送成功后才递增，两个并发发送会拿到同一个
// msg_seq，平台必然拒掉一条。现在占用与取 seq 在同一把锁内完成。
func TestConcurrentPickGetsDistinctSeq(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "openid-a", "张三", "")

	const n = 5
	var mu sync.Mutex
	seen := map[uint32]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, seq, ok := p.PickAndReserve("g1", "openid-a")
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[seq] {
				t.Errorf("并发取用拿到了重复的 msg_seq=%d", seq)
			}
			seen[seq] = true
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("并发取用应得到 %d 个不同的 seq，got %d", n, len(seen))
	}
}

// TestReleaseDoesNotAffectOthers 确认释放只影响对应那条消息。
func TestReleaseDoesNotAffectOthers(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "openid-a", "张三", "")
	p.Add("g1", "m2", "openid-b", "李四", "")

	p.PickAndReserve("g1", "openid-a")
	p.PickAndReserve("g1", "openid-b")
	p.Release("g1", "m1")

	// 李四那条的额度不该被动过：还能再取 4 次
	for i := 0; i < 4; i++ {
		if _, _, _, ok := p.PickAndReserve("g1", "openid-b"); !ok {
			t.Fatalf("释放 m1 不该影响 m2，第 %d 次取用失败", i+2)
		}
	}
}

// TestUnknownSenderFallsBackToNewest 未知发送人（openid 传空）时不得被优先选中。
func TestUnknownSenderFallsBackToNewest(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m-old", "openid-zhang", "张三", "")
	p.Add("g1", "m-new", "", "", "")

	msgID, _, _, ok := p.PickAndReserve("g1", "openid-zhang")
	if !ok {
		t.Fatal("应能取到锚点")
	}
	if msgID != "m-old" {
		t.Errorf("指定回张三时应挂他的消息，got %q", msgID)
	}
}
