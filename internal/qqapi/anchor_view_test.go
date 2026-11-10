package qqapi

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// testClock 是可手动推进的时钟，避免测试真的 sleep 5 分钟去等锚点过期。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newClockedPool 建一个用冻结时钟的锚点池。
func newClockedPool(keep int) (*AnchorPool, *testClock) {
	clk := newTestClock()
	p := NewAnchorPool(keep)
	p.now = clk.Now
	return p, clk
}

// TestListNewestFirstIncludesDead 钉死 List 的三件事：
// 最新在前、含已用满条目、含已过期条目。
//
// 管理端要靠这些条目区分 no_anchor / expired / quota_exhausted；
// 如果快照把不存活的条目滤掉，三种原因就只剩 no_anchor。
func TestListNewestFirstIncludesDead(t *testing.T) {
	p, clk := newClockedPool(20)
	p.Add("g1", "m1", "s1", "甲", "")
	p.Add("g1", "m2", "s2", "乙", "")
	p.Add("g1", "m3", "s3", "丙", "")

	got := p.List("g1")
	if len(got) != 3 {
		t.Fatalf("应返回 3 条，got %d", len(got))
	}
	for i, want := range []string{"m3", "m2", "m1"} {
		if got[i].MsgID != want {
			t.Errorf("第 %d 条应为 %s（最新在前），got %s", i, want, got[i].MsgID)
		}
	}

	// 把 m2 用满 5 次。仍然要出现在 List 里，且 Alive=false。
	for i := 0; i < passiveMaxUse; i++ {
		if _, _, _, ok, _ := p.PickAndReserve("g1", "s2"); !ok {
			t.Fatalf("第 %d 次占用 m2 不应失败", i+1)
		}
	}
	got = p.List("g1")
	if len(got) != 3 {
		t.Fatalf("用满后仍应返回 3 条，got %d", len(got))
	}
	if got[1].MsgID != "m2" || got[1].Used != passiveMaxUse || got[1].Alive {
		t.Errorf("m2 应为 used=%d、Alive=false 且保留在列表里，got %+v", passiveMaxUse, got[1])
	}

	// 时钟推进 6 分钟：全部过期，但仍要保留。
	clk.Advance(passiveTTL + time.Minute)
	got = p.List("g1")
	if len(got) != 3 {
		t.Fatalf("过期后仍应返回 3 条，got %d", len(got))
	}
	for _, it := range got {
		if it.Alive {
			t.Errorf("%s 已过期，Alive 应为 false", it.MsgID)
		}
		if it.ExpiresAt != clk.Now().Add(-time.Minute).Unix() {
			t.Errorf("%s 的 expires_at 应为登记时间 + 5 分钟，got %d", it.MsgID, it.ExpiresAt)
		}
	}
}

// TestListCappedAtTwenty 快照最多 20 条，与建池时的 keep 无关。
func TestListCappedAtTwenty(t *testing.T) {
	p, _ := newClockedPool(40)
	for i := 0; i < 30; i++ {
		p.Add("g1", fmt.Sprintf("m%02d", i), "s1", "甲", "")
	}
	got := p.List("g1")
	if len(got) != anchorWindowMax {
		t.Fatalf("应截断到 %d 条，got %d", anchorWindowMax, len(got))
	}
	if got[0].MsgID != "m29" || got[len(got)-1].MsgID != "m10" {
		t.Errorf("截断后应保留最新 20 条（m29…m10），got %s…%s", got[0].MsgID, got[len(got)-1].MsgID)
	}
}

// TestWindowPrefersSenderThenNewest 覆盖 Window 的排序：
// 指定发送人的存活锚点最前，其次按最新在前。
func TestWindowPrefersSenderThenNewest(t *testing.T) {
	p, _ := newClockedPool(20)
	p.Add("g1", "m-zhang", "openid-zhang", "张三", "")
	p.Add("g1", "m-li", "openid-li", "李四", "")
	p.Add("g1", "m-wang", "openid-wang", "王五", "")

	got := p.Window("g1", "openid-zhang")
	if len(got) != 3 {
		t.Fatalf("应返回 3 条，got %d", len(got))
	}
	want := []string{"m-zhang", "m-wang", "m-li"}
	for i := range want {
		if got[i].MsgID != want[i] {
			t.Errorf("第 %d 条应为 %s，got %s", i, want[i], got[i].MsgID)
		}
	}

	// 不指定发送人时纯按最新在前。
	got = p.Window("g1", "")
	want = []string{"m-wang", "m-li", "m-zhang"}
	for i := range want {
		if got[i].MsgID != want[i] {
			t.Errorf("未指定时第 %d 条应为 %s，got %s", i, want[i], got[i].MsgID)
		}
	}
}

// TestWindowPutsDeadLast 已用满、已过期的条目排在存活条目之后，
// 即使它们更新——否则管理端会把「最新但已用完」误当成还能发。
func TestWindowPutsDeadLast(t *testing.T) {
	p, clk := newClockedPool(20)
	p.Add("g1", "m-old", "s1", "甲", "")
	p.Add("g1", "m-new", "s2", "乙", "")

	for i := 0; i < passiveMaxUse; i++ {
		p.PickAndReserve("g1", "s2")
	}
	got := p.Window("g1", "")
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，got %d", len(got))
	}
	if got[0].MsgID != "m-old" || !got[0].Alive {
		t.Errorf("存活的 m-old 应排最前，got %+v", got[0])
	}
	if got[1].MsgID != "m-new" || got[1].Alive {
		t.Errorf("用满的 m-new 应排最后，got %+v", got[1])
	}

	// 再把 m-old 也放过期，两条都排进最后一档，按最新在前。
	clk.Advance(passiveTTL + time.Minute)
	got = p.Window("g1", "")
	if got[0].MsgID != "m-new" || got[1].MsgID != "m-old" {
		t.Errorf("全部失效时应按最新在前，got %s, %s", got[0].MsgID, got[1].MsgID)
	}
	for _, it := range got {
		if it.Alive {
			t.Errorf("%s 已失效，Alive 应为 false", it.MsgID)
		}
	}
}

// TestKnownGroupsKeepsStaleGroups 「登记过锚点的群」不能因为锚点全过期就消失。
// EndAll 要遍历这些群恢复模式，漏一个就会留下一个仍在暂停的群。
func TestKnownGroupsKeepsStaleGroups(t *testing.T) {
	p, clk := newClockedPool(20)
	if got := p.KnownGroups(); len(got) != 0 {
		t.Fatalf("空池不应有群，got %v", got)
	}
	p.Add("g-b", "m1", "s1", "甲", "")
	p.Add("g-a", "m2", "s2", "乙", "")

	if got := p.KnownGroups(); len(got) != 2 || got[0] != "g-a" || got[1] != "g-b" {
		t.Fatalf("应返回排序后的 [g-a g-b]，got %v", got)
	}

	// 全部过期，并且真的走一次选点——PickAndReserve 不得顺手删掉这些群。
	clk.Advance(passiveTTL + time.Minute)
	if _, _, _, ok, _ := p.PickAndReserve("g-a", ""); ok {
		t.Fatal("锚点全部过期时不应选出锚点")
	}
	if got := p.KnownGroups(); len(got) != 2 {
		t.Errorf("锚点全部过期后群仍应保留，got %v", got)
	}
	if got := p.List("g-a"); len(got) != 1 || got[0].Alive {
		t.Errorf("过期条目仍应可在快照里看到，got %+v", got)
	}
}

// TestAnchorViewConcurrent 在并发增删读下跑 -race：
// Window/List/KnownGroups 必须与 Add/PickAndReserve/Release 共用同一把锁。
func TestAnchorViewConcurrent(t *testing.T) {
	p := NewAnchorPool(20)
	const workers = 8
	const rounds = 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			g := fmt.Sprintf("g%d", w%2)
			for i := 0; i < rounds; i++ {
				p.Add(g, fmt.Sprintf("m-%d-%d", w, i), fmt.Sprintf("s%d", w), "甲", "")
				p.PickAndReserve(g, fmt.Sprintf("s%d", w))
				p.Release(g, fmt.Sprintf("m-%d-%d", w, i))
				_ = p.Window(g, fmt.Sprintf("s%d", w))
				_ = p.List(g)
				_ = p.KnownGroups()
				_ = p.Stats()
			}
		}(w)
	}
	wg.Wait()
	if got := p.KnownGroups(); len(got) != 2 {
		t.Errorf("应记录 2 个群，got %v", got)
	}
}

// TestClientAnchorForwarders Client 上的转发方法要原样落到锚点池。
func TestClientAnchorForwarders(t *testing.T) {
	p, _ := newClockedPool(20)
	c := &Client{anchors: p}
	p.Add("g1", "m1", "s1", "甲", "")

	if got := c.ListAnchors("g1"); len(got) != 1 || got[0].MsgID != "m1" {
		t.Errorf("ListAnchors 应转发锚点池，got %+v", got)
	}
	if got := c.Window("g1", "s1"); len(got) != 1 || !got[0].Alive {
		t.Errorf("Window 应转发锚点池，got %+v", got)
	}
	if got := c.AnchorGroups(); len(got) != 1 || got[0] != "g1" {
		t.Errorf("AnchorGroups 应转发 KnownGroups，got %v", got)
	}
}
