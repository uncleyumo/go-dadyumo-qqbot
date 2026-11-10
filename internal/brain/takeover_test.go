package brain

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/takeover"
)

// fakeClock 让租约 TTL 测试不 sleep。所有方法并发安全，
// 因为并发用例会在 goroutine 里读它。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTakeoverEngine 组一个带真实 takeover.Store 的引擎。
// 模式文件写到临时目录，避免污染仓库。
func newTakeoverEngine(t *testing.T) (*Engine, *takeover.Store, *fakeClock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "takeover.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"default_mode":"window","epoch":0,"groups":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := takeover.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, &recordingSender{}, nil, false)
	// newTestEngine 用字面量构造，没有初始化 states；OnMessage 用例需要它。
	e.states = map[string]*groupState{}
	e.SetTakeoverStore(st)
	fc := &fakeClock{t: time.Now()}
	e.tkNow = fc.now
	return e, st, fc
}

func TestTakeoverDefaultsNoPause(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	if e.AIPaused("G1") {
		t.Fatal("未注入模式时默认 window 且无租约，不该暂停")
	}
	if paused, reason, _, n := e.AIPauseInfo("G1"); paused || reason != "" || n != 0 {
		t.Fatalf("默认状态应为未暂停，got paused=%v reason=%q leases=%d", paused, reason, n)
	}
	if e.TakeoverMode("G1") != takeover.ModeWindow {
		t.Fatalf("未知群应默认 window，got %q", e.TakeoverMode("G1"))
	}
}

func TestTakeoverAlwaysPausesWithoutLease(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	if !e.AIPaused("G1") {
		t.Fatal("always 档应一直暂停")
	}
	paused, reason, since, n := e.AIPauseInfo("G1")
	if !paused || reason != "mode_always" || n != 0 || since == 0 {
		t.Fatalf("always 派生状态不对：paused=%v reason=%q since=%d n=%d", paused, reason, since, n)
	}
	// always 不建租约：acquire 是无操作成功。
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatalf("always 下 acquire 应无操作成功，got %v", err)
	}
	if info := e.LeaseInfo("G1", "L1"); info.Valid {
		t.Fatal("always 下不该真的建租约")
	}
}

func TestTakeoverOffRejectsLease(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	if err := st.SetMode("G1", takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G1", "L1", false, 0); !errors.Is(err, ErrTakeoverModeOff) {
		t.Fatalf("off 下 acquire 应返回 mode_off，got %v", err)
	}
	if err := e.BeatLease("G1", "L1", false, 0); !errors.Is(err, ErrTakeoverModeOff) {
		t.Fatalf("off 下 beat 应返回 mode_off，got %v", err)
	}
	// release 永远允许，off 下也要能把残留租约清掉。
	if _, err := e.ReleaseLease("G1", "L1", false, false, 0); err != nil {
		t.Fatalf("off 下 release 不该报错，got %v", err)
	}
}

func TestLeaseVisibleTTL(t *testing.T) {
	e, _, fc := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	fc.advance(9 * time.Second)
	if !e.AIPaused("G1") {
		t.Fatal("9 秒内仍应暂停")
	}
	fc.advance(2 * time.Second)
	if e.AIPaused("G1") {
		t.Fatal("11 秒无心跳应恢复")
	}
}

func TestLeaseHiddenTTL(t *testing.T) {
	e, _, fc := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "L1", true, 0); err != nil {
		t.Fatal(err)
	}
	fc.advance(29 * time.Minute)
	if !e.AIPaused("G1") {
		t.Fatal("隐藏租约 29 分钟内仍应暂停")
	}
	fc.advance(2 * time.Minute)
	if e.AIPaused("G1") {
		t.Fatal("隐藏租约 31 分钟无心跳应恢复")
	}
}

func TestClosingGraceKeepsPausedAndRevives(t *testing.T) {
	e, _, fc := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	start := fc.now()
	if released, err := e.ReleaseLease("G1", "L1", true, false, 0); err != nil || !released {
		t.Fatalf("closing release 应成功，released=%v err=%v", released, err)
	}
	fc.advance(4 * time.Second)
	if !e.AIPaused("G1") {
		t.Fatal("5 秒宽限内仍应暂停")
	}
	if paused, _, _, n := e.AIPauseInfo("G1"); !paused || n != 1 {
		t.Fatalf("宽限内应计 1 条租约，got paused=%v n=%d", paused, n)
	}

	// 宽限内同 id 复活：closing 被清掉，Start 保留。
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatalf("宽限内 acquire 应复活，got %v", err)
	}
	info := e.LeaseInfo("G1", "L1")
	if !info.Valid || info.Closing {
		t.Fatalf("复活后应为活跃租约，got %+v", info)
	}
	if _, _, since, _ := e.AIPauseInfo("G1"); since != start.Unix() {
		t.Fatalf("复活后 ai_paused_since 应沿用原 Start，got %d want %d", since, start.Unix())
	}

	// 再次卸载且不复活：宽限到期后恢复。
	if _, err := e.ReleaseLease("G1", "L1", true, false, 0); err != nil {
		t.Fatal(err)
	}
	fc.advance(6 * time.Second)
	if e.AIPaused("G1") {
		t.Fatal("宽限到期后应恢复")
	}
}

func TestFinalTombBlocksReacquire(t *testing.T) {
	e, _, fc := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReleaseLease("G1", "L1", false, true, 0); err != nil {
		t.Fatal(err)
	}
	fc.advance(30 * time.Second)
	if err := e.AcquireLease("G1", "L1", false, 0); !errors.Is(err, ErrTakeoverLeaseReleased) {
		t.Fatalf("墓碑内 acquire 应返回 lease_released，got %v", err)
	}
	if info := e.LeaseInfo("G1", "L1"); !info.Released {
		t.Fatalf("墓碑内 LeaseInfo.Released 应为 true，got %+v", info)
	}
	fc.advance(31 * time.Second)
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatalf("61 秒后应可 acquire，got %v", err)
	}
}

func TestReleaseMissingLeaseDoesNotCreateGrace(t *testing.T) {
	e, _, fc := newTakeoverEngine(t)
	released, err := e.ReleaseLease("G1", "L1", true, false, 0)
	if err != nil || released {
		t.Fatalf("租约不存在时 release 应返回 released=false，got %v %v", released, err)
	}
	fc.advance(time.Second)
	if e.AIPaused("G1") {
		t.Fatal("closing:true 不能给不存在的租约新建宽限")
	}
	// 显式 final 例外：即使租约不存在也写墓碑。
	if _, err := e.ReleaseLease("G1", "L1", false, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G1", "L1", false, 0); !errors.Is(err, ErrTakeoverLeaseReleased) {
		t.Fatalf("final 应写墓碑，got %v", err)
	}
}

func TestLeaseEpochCheck(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	if _, err := st.BumpEpoch(); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G1", "L1", false, 0); !errors.Is(err, ErrTakeoverStaleEpoch) {
		t.Fatalf("旧 epoch 应返回 stale_epoch，got %v", err)
	}
	if err := e.AcquireLease("G1", "L1", false, st.Epoch()); err != nil {
		t.Fatalf("当前 epoch 应可 acquire，got %v", err)
	}
}

func TestMultiWindowLeaseCount(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "A", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G1", "B", false, 0); err != nil {
		t.Fatal(err)
	}
	if paused, _, _, n := e.AIPauseInfo("G1"); !paused || n != 2 {
		t.Fatalf("两个窗口应计 2 条租约，got paused=%v n=%d", paused, n)
	}
	if _, err := e.ReleaseLease("G1", "A", false, false, 0); err != nil {
		t.Fatal(err)
	}
	if paused, _, _, n := e.AIPauseInfo("G1"); !paused || n != 1 {
		t.Fatalf("释放一个后仍应暂停且计 1，got paused=%v n=%d", paused, n)
	}
	if _, err := e.ReleaseLease("G1", "B", false, false, 0); err != nil {
		t.Fatal(err)
	}
	if e.AIPaused("G1") {
		t.Fatal("最后一个租约释放后应立即恢复")
	}
}

func TestEndAllResetsKnownGroups(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	// 三个来源各放一个群：memory、takeover.json 显式条目。
	e.mem.Group("G-MEM", "")
	// 先切 off 再切回 window，制造一条显式条目——Store 对「等于默认值」
	// 的设置不写条目，直接 SetMode(window) 不会出现在 Groups() 里。
	if err := st.SetMode("G-STORE", takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMode("G-STORE", takeover.ModeWindow); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G-MEM", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G-STORE", "L2", false, 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.EndAll()
	if err != nil {
		t.Fatal(err)
	}
	if res.ModesReset != 2 {
		t.Fatalf("应把 2 个非 off 群改成 off，got %d", res.ModesReset)
	}
	if res.LeasesReleased != 2 {
		t.Fatalf("应清掉 2 条租约，got %d", res.LeasesReleased)
	}
	if res.Epoch != 1 {
		t.Fatalf("epoch 应递增到 1，got %d", res.Epoch)
	}
	for _, g := range []string{"G-MEM", "G-STORE"} {
		if e.AIPaused(g) {
			t.Fatalf("%s 在 end_all 后不该暂停", g)
		}
		if e.TakeoverMode(g) != takeover.ModeOff {
			t.Fatalf("%s 应被设为 off，got %q", g, e.TakeoverMode(g))
		}
	}
	// end_all 不写墓碑：切回 window 后同 id 可以正常 acquire。
	if err := st.SetMode("G-MEM", takeover.ModeWindow); err != nil {
		t.Fatal(err)
	}
	if err := e.AcquireLease("G-MEM", "L1", false, st.Epoch()); err != nil {
		t.Fatalf("end_all 后同 id 应可重新 acquire，got %v", err)
	}
}

func TestSetTakeoverModeOffClearsLeases(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.SetTakeoverMode("G1", takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	if info := e.LeaseInfo("G1", "L1"); info.Valid || info.Closing {
		t.Fatalf("切 off 应清空租约，got %+v", info)
	}
	if e.AIPaused("G1") {
		t.Fatal("off 下不该暂停")
	}
}

// 并发用例：-race 下不能有数据竞争，也不能死锁。
func TestTakeoverConcurrentAccess(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('A' + i))
			_ = e.AcquireLease("G1", id, false, 0)
			_ = e.BeatLease("G1", id, false, 0)
			_ = e.AIPaused("G1")
			_, _ = e.ReleaseLease("G1", id, true, false, 0)
			_, _, _, _ = e.AIPauseInfo("G1")
		}(i)
	}
	wg.Wait()
}

// OnMessage 的主拦截：消息仍要登记（成员表、最近消息），但不能排程。
func TestOnMessageRegistersButDoesNotScheduleDuringTakeover(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	e.OnMessage(&Event{GroupID: "G1", OpenID: "U1", Name: "张三", Content: "在吗"})

	g := e.mem.Group("G1", "")
	if len(g.Recent(10)) != 1 {
		t.Fatalf("接管期间消息仍应进上下文，got %d 条", len(g.Recent(10)))
	}
	if _, ok := g.MemberOf("U1"); !ok {
		t.Fatal("接管期间成员表仍应登记")
	}
	stt := e.stateOf("G1")
	stt.mu.Lock()
	defer stt.mu.Unlock()
	if stt.newCount != 0 || stt.timer != nil {
		t.Fatalf("接管期间不该排程，newCount=%d timer=%v", stt.newCount, stt.timer != nil)
	}
}

// fire 的兜底闸：接管开始时已经排好的定时器到点也不能进决策。
// router 是 nil，闸门失效会以 nil 解引用 panic 收场。
func TestFireGateStopsAlreadyScheduledBatch(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	e.fire("G1")
}
