package brain

import (
	"errors"
	"time"

	"dadyumo/internal/logx"
	"dadyumo/internal/takeover"
)

// 人工接管（takeover）在决策引擎里的状态与闸门。
//
// 这里只放**派生状态**：模式在 takeover.Store，租约在内存。两者都不进
// config.Config——配置保存是整包覆盖，旧页面回写会把新增字段抹成零值。
//
// 锁序见 engine.go 的 Engine 字段注释与设计文档 §2.4。要点：
//   - tkMu 是叶子锁，持有它时绝不取 Store.mu / st.mu / mem 锁；
//   - 需要 Store 时先短读拿到值、释放，再取 tkMu；
//   - 需要清攒批状态时先释放 tkMu，再取 st.mu。
const (
	// leaseTTLVisible 可见标签页的租约 TTL。前端每 2 秒 beat 一次，
	// 10 秒没心跳就当页面已经崩了。10 秒是设计文档的定稿值。
	leaseTTLVisible = 10 * time.Second
	// leaseTTLHidden 隐藏标签页的租约 TTL。后台标签页心跳降到 20 秒一次，
	// 30 分钟是单条租约的硬上限，不是「本次接管总时长」的上限。
	leaseTTLHidden = 30 * time.Minute
	// closingGrace 页面卸载后的复活宽限。宽限内仍计入暂停，
	// 刷新页面用同一 lease_id 在 5 秒内 acquire 可以无缝续上。
	closingGrace = 5 * time.Second
	// tombTTL 显式结束（final）后的墓碑时长。墓碑内同 id acquire 返回
	// lease_released，防止旧页面在 60 秒内自动复活。
	tombTTL = 60 * time.Second
)

// 租约接口的错误。Error() 的文本就是管理端的 reason 字符串，
// admin 用 errors.Is 映射到 409 的具体错误码。
var (
	// ErrTakeoverNotReady 表示 Engine 没有注入 Store，属于装配错误。
	ErrTakeoverNotReady = errors.New("not_initialized")
	// ErrTakeoverModeOff off 档下 acquire/beat 被拒绝。
	ErrTakeoverModeOff = errors.New("mode_off")
	// ErrTakeoverStaleEpoch end_all 之后旧页面拿过期 epoch 来续约。
	ErrTakeoverStaleEpoch = errors.New("stale_epoch")
	// ErrTakeoverLeaseExpired 租约因 TTL 被惰性删除，或还在 closing 宽限里。
	ErrTakeoverLeaseExpired = errors.New("lease_expired")
	// ErrTakeoverLeaseReleased 显式结束后墓碑期内又用同一个 id 来 acquire。
	ErrTakeoverLeaseReleased = errors.New("lease_released")
	// ErrBadLease 租约参数缺失。
	ErrBadLease = errors.New("bad_lease")
)

// lease 是一条存活租约。所有时间都用服务端时钟。
type lease struct {
	ID       string    // 窗口生成的 lease_id
	Start    time.Time // 首次 acquire 时间；刷新复活时保留
	LastBeat time.Time // 最后一次 acquire/beat，TTL 从这里算
	Hidden   bool      // 最近一次心跳上报的 visibility
	Epoch    int64     // acquire 时的 Store.Epoch()
}

// closingRec 是页面卸载后留下的 5 秒复活宽限。
type closingRec struct {
	Start    time.Time // 原租约 Start，复活时沿用
	LastBeat time.Time // 原租约最后一次心跳，复活后 TTL 从这里续
	Expires  time.Time // 宽限截止；now >= Expires 后不再计入暂停
	Hidden   bool      // 原租约的可见性
	Epoch    int64     // 原租约 Epoch
}

// active 判断宽限是否还在生效。
func (c closingRec) active(now time.Time) bool { return now.Before(c.Expires) }

// pauseState 是某一刻「AI 是否暂停」的现算结果，不落盘。
type pauseState struct {
	paused bool
	reason string // "" | "mode_always" | "window_open"
	since  time.Time
	leases int
}

// LeaseInfo 是单条租约的只读快照，供管理端做 lease_valid / can_send 判定。
type LeaseInfo struct {
	Valid     bool  // 活跃租约（不含 closing 宽限）
	Closing   bool  // 处于 5 秒复活宽限
	Released  bool  // 处于 60 秒墓碑内
	ExpiresAt int64 // unix 秒；0 表示没有
}

// EndAllResult 是「全部恢复正常」的计数结果。
type EndAllResult struct {
	ModesReset     int   // 实际从非 off 改成 off 的群数
	LeasesReleased int   // 清掉的活跃租约与 closing 宽限总数
	Epoch          int64 // 递增后的持久化 epoch
	Rev            int64 // 递增后的 takeover_rev
}

// SetTakeoverStore 注入模式存储。main 在创建 Engine 之后调用。
func (e *Engine) SetTakeoverStore(st *takeover.Store) {
	if e == nil {
		return
	}
	e.tkStore = st
}

// SetTakeoverAnchors 注入锚点快照来源。nil 表示管理端按「无锚点」处理。
func (e *Engine) SetTakeoverAnchors(src takeover.AnchorSource) {
	if e == nil {
		return
	}
	e.anchors = src
}

// HasAnchors 判断锚点池里是否有该群的记录。供管理端判 unknown_group，
// 不创建幽灵群（memory.Store.Group 会创建）。
func (e *Engine) HasAnchors(groupOpenID string) bool {
	if e == nil || e.anchors == nil || groupOpenID == "" {
		return false
	}
	for _, g := range e.anchors.AnchorGroups() {
		if g == groupOpenID {
			return true
		}
	}
	return false
}

// now 返回接管状态使用的服务端时钟。测试注入 tkNow，线上是 time.Now。
func (e *Engine) now() time.Time {
	if e == nil || e.tkNow == nil {
		return time.Now()
	}
	return e.tkNow()
}

// takeoverRevShift 是 takeover_rev 的启动时间基数位移：rev = 启动秒<<20 | 计数。
// 进程重启后 tkRev 归零，但启动秒不同，新快照的 rev 不会小于页面记下的旧值。
const takeoverRevShift = 20

// takeoverRevLocked 拼出跨进程单调的 takeover_rev。调用方必须持有 tkMu。
// tkBoot 为 0 的 Engine 字面量（测试里常见）退化成纯计数，行为与旧版一致。
func (e *Engine) takeoverRevLocked() int64 {
	if e == nil {
		return 0
	}
	return (e.tkBoot << takeoverRevShift) + e.tkRev
}

// TakeoverRev 返回接管状态版本号。管理端每次模式/租约/end_all 变更后递增，
// 前端只应用不小于本地 appliedRev 的快照，防旧响应覆盖新状态。
//
// 高位带启动时间：进程重启后计数归零，老页面也不会因为「新 rev 更小」
// 而永久丢掉后续快照（设计 §5.3 的群卡片徽标是 always 档唯一安全网）。
func (e *Engine) TakeoverRev() int64 {
	if e == nil {
		return 0
	}
	e.tkMu.Lock()
	defer e.tkMu.Unlock()
	return e.takeoverRevLocked()
}

// TakeoverMode 返回该群当前生效的模式；未注入 Store 时按 window 处理。
func (e *Engine) TakeoverMode(groupID string) takeover.Mode {
	if e == nil || e.tkStore == nil {
		return takeover.ModeWindow
	}
	return e.tkStore.Mode(groupID)
}

// TakeoverModeSince 返回该群当前模式的生效时刻；没有条目时返回零值。
func (e *Engine) TakeoverModeSince(groupID string) time.Time {
	if e == nil || e.tkStore == nil {
		return time.Time{}
	}
	return e.tkStore.Since(groupID)
}

// TakeoverEpoch 返回持久化 epoch。end_all 会递增它，旧页面拿旧值续约会吃 stale_epoch。
func (e *Engine) TakeoverEpoch() int64 {
	if e == nil || e.tkStore == nil {
		return 0
	}
	return e.tkStore.Epoch()
}

// AIPaused 判断该群此刻是否因人工接管而暂停。nil-safe：未注入 Store 时返回 false。
func (e *Engine) AIPaused(groupID string) bool {
	return e.aiPauseState(groupID, e.now()).paused
}

// AIPauseInfo 返回派生状态，供管理端群循环直接调用，不需要判 engine 是否为 nil。
func (e *Engine) AIPauseInfo(groupID string) (paused bool, reason string, since int64, leaseCount int) {
	st := e.aiPauseState(groupID, e.now())
	if st.paused && !st.since.IsZero() {
		since = st.since.Unix()
	}
	return st.paused, st.reason, since, st.leases
}

// aiPausedByTakeover 是闸门用的快捷判断。
func (e *Engine) aiPausedByTakeover(groupID string) bool {
	return e.aiPauseState(groupID, e.now()).paused
}

// aiPauseState 按「模式 + 租约 + 当前时钟」现算暂停状态。
//
// 绝不缓存：配置热重载、租约过期、页面复活都发生在两次调用之间，
// 存第二份状态只会制造「管理端说暂停、AI 却在回话」这种对不上的现象。
func (e *Engine) aiPauseState(groupID string, now time.Time) pauseState {
	if e == nil || e.tkStore == nil {
		return pauseState{}
	}
	// 短读锁拿模式，释放后再进 tkMu——两把叶子锁不能同时持有。
	mode := e.tkStore.Mode(groupID)
	switch mode {
	case takeover.ModeOff:
		return pauseState{}
	case takeover.ModeAlways:
		return pauseState{paused: true, reason: "mode_always", since: e.tkStore.Since(groupID)}
	default: // window
		e.tkMu.Lock()
		defer e.tkMu.Unlock()
		e.pruneLeasesLocked(groupID, now)
		ls := e.leases[groupID]
		// 活跃租约 + 未过期的 5 秒宽限一起算：宽限内 AI 仍然暂停。
		cs := countActiveClosing(e.closing[groupID], now)
		n := len(ls) + cs
		if n == 0 {
			return pauseState{}
		}
		since := earliestStart(ls)
		if cs > 0 {
			if s := earliestClosingStart(e.closing[groupID], now); since.IsZero() || s.Before(since) {
				since = s
			}
		}
		return pauseState{paused: true, reason: "window_open", since: since, leases: n}
	}
}

// ensureLeasesLocked 兜住 Engine 字面量构造（测试里很常见）没有初始化 map 的情况。
// 调用方必须持有 tkMu。
func (e *Engine) ensureLeasesLocked() {
	if e.leases == nil {
		e.leases = map[string]map[string]*lease{}
	}
	if e.closing == nil {
		e.closing = map[string]map[string]closingRec{}
	}
	if e.tomb == nil {
		e.tomb = map[string]time.Time{}
	}
}

// leaseTTL 按租约最近一次上报的可见性取 TTL。
func leaseTTL(hidden bool) time.Duration {
	if hidden {
		return leaseTTLHidden
	}
	return leaseTTLVisible
}

// pruneLeasesLocked 惰性删除过期租约、过期宽限与过期墓碑。
// 调用方必须持有 tkMu。
func (e *Engine) pruneLeasesLocked(groupID string, now time.Time) {
	e.ensureLeasesLocked()
	if ls := e.leases[groupID]; len(ls) > 0 {
		for id, l := range ls {
			if now.Sub(l.LastBeat) > leaseTTL(l.Hidden) {
				delete(ls, id)
			}
		}
		if len(ls) == 0 {
			delete(e.leases, groupID)
		}
	}
	if cs := e.closing[groupID]; len(cs) > 0 {
		for id, c := range cs {
			if !c.active(now) {
				delete(cs, id)
			}
		}
		if len(cs) == 0 {
			delete(e.closing, groupID)
		}
	}
	prefix := groupID + "|"
	for key, until := range e.tomb {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix && !now.Before(until) {
			delete(e.tomb, key)
		}
	}
}

// countActiveClosing 只数仍在宽限期内的 closing 记录。
func countActiveClosing(m map[string]closingRec, now time.Time) int {
	n := 0
	for _, c := range m {
		if c.active(now) {
			n++
		}
	}
	return n
}

// earliestStart 取一组租约里最早的 Start；空集合返回零值。
func earliestStart(m map[string]*lease) time.Time {
	var out time.Time
	for _, l := range m {
		if out.IsZero() || l.Start.Before(out) {
			out = l.Start
		}
	}
	return out
}

// earliestClosingStart 取仍在宽限期内的 closing 记录里最早的 Start。
func earliestClosingStart(m map[string]closingRec, now time.Time) time.Time {
	var out time.Time
	for _, c := range m {
		if !c.active(now) {
			continue
		}
		if out.IsZero() || c.Start.Before(out) {
			out = c.Start
		}
	}
	return out
}

// tombKey 墓碑按「群 + 租约」定位，不同窗口的 lease_id 互不干扰。
func tombKey(groupID, leaseID string) string { return groupID + "|" + leaseID }

// AcquireLease 新建或续期一条租约。
//
// epoch 校验在 window 档进行：end_all 之后旧页面拿旧 epoch 续约必须失败，
// 否则「全部恢复正常」会被页面自动 acquire 撤销。always 档不建租约、无操作成功；
// off 档拒绝。
func (e *Engine) AcquireLease(groupID, leaseID string, hidden bool, epoch int64) error {
	if e == nil || e.tkStore == nil {
		return ErrTakeoverNotReady
	}
	if groupID == "" || leaseID == "" {
		return ErrBadLease
	}

	// 模式/epoch 必须在 tkMu 外短读（锁序：持有 tkMu 时不得取 Store.mu）。
	// 读完再进锁时用 tkGen 复核：SetTakeoverMode(off/always) / EndAll 清空租约
	// 若发生在「读模式」与「建租约」之间，这里要重新判断，不能插一条
	// 没人会心跳的幽灵租约（它会让切回 window 后的 AI 无故暂停到 TTL 到期）。
	for attempt := 0; attempt < 4; attempt++ {
		e.tkMu.Lock()
		gen := e.tkGen
		e.tkMu.Unlock()

		mode := e.tkStore.Mode(groupID)
		switch mode {
		case takeover.ModeOff:
			return ErrTakeoverModeOff
		case takeover.ModeAlways:
			return nil
		}
		if epoch < e.tkStore.Epoch() {
			return ErrTakeoverStaleEpoch
		}

		now := e.now()
		e.tkMu.Lock()
		if e.tkGen != gen {
			e.tkMu.Unlock()
			continue
		}
		err := e.acquireLeaseLocked(groupID, leaseID, hidden, epoch, now)
		e.tkMu.Unlock()
		return err
	}
	// 连续被清理打断：此刻状态在反复变化，让页面按只读处理，
	// 下一次 2 秒轮询会重新判断。返回 mode_off 与 end_all 的语义一致。
	return ErrTakeoverModeOff
}

// acquireLeaseLocked 在 tkMu 内建租约或续期。调用方已完成模式、epoch 与
// tkGen 复核，这里不再读 Store。
func (e *Engine) acquireLeaseLocked(groupID, leaseID string, hidden bool, epoch int64, now time.Time) error {
	e.ensureLeasesLocked()
	e.pruneLeasesLocked(groupID, now)

	if until, ok := e.tomb[tombKey(groupID, leaseID)]; ok && now.Before(until) {
		return ErrTakeoverLeaseReleased
	}
	if l, ok := e.leases[groupID][leaseID]; ok {
		// 同 id 重复 acquire 只续期，Start 保留——刷新前后 ai_paused_since 才连续。
		l.LastBeat = now
		l.Hidden = hidden
		l.Epoch = epoch
		e.tkRev++
		return nil
	}
	start := now
	if cs := e.closing[groupID]; cs != nil {
		if c, ok := cs[leaseID]; ok {
			// 5 秒宽限内用同一 id 复活：清 closing，沿用原 Start。
			start = c.Start
			delete(cs, leaseID)
			if len(cs) == 0 {
				delete(e.closing, groupID)
			}
		}
	}
	if e.leases[groupID] == nil {
		e.leases[groupID] = map[string]*lease{}
	}
	e.leases[groupID][leaseID] = &lease{
		ID:       leaseID,
		Start:    start,
		LastBeat: now,
		Hidden:   hidden,
		Epoch:    epoch,
	}
	e.tkRev++
	return nil
}

// BeatLease 刷新一条存活租约的心跳。
//
// closing 宽限里的租约不能 beat：它已经进入「等复活」状态，只接受同 id 的 acquire。
func (e *Engine) BeatLease(groupID, leaseID string, hidden bool, epoch int64) error {
	if e == nil || e.tkStore == nil {
		return ErrTakeoverNotReady
	}
	if groupID == "" || leaseID == "" {
		return ErrBadLease
	}
	mode := e.tkStore.Mode(groupID)
	switch mode {
	case takeover.ModeOff:
		return ErrTakeoverModeOff
	case takeover.ModeAlways:
		return nil
	}
	if epoch < e.tkStore.Epoch() {
		return ErrTakeoverStaleEpoch
	}

	now := e.now()
	e.tkMu.Lock()
	defer e.tkMu.Unlock()
	e.ensureLeasesLocked()
	e.pruneLeasesLocked(groupID, now)

	l, ok := e.leases[groupID][leaseID]
	if !ok {
		return ErrTakeoverLeaseExpired
	}
	l.LastBeat = now
	l.Hidden = hidden
	// beat 不递增 tkRev：徽标与暂停状态没变，递增只会让前端每 2 秒丢一次缓存。
	return nil
}

// ReleaseLease 释放租约，按参数分叉：
//   - final=true：删租约并写 60 秒墓碑，优先于 closing；
//   - closing=true：写 5 秒复活宽限，宽限内仍计入暂停；
//   - 都不传：立即删除，最后一条释放后 AI 立即恢复。
//
// 租约已经因 TTL 被删时返回 released=false；closing=true 不新建宽限，
// 只有显式 final=true 才额外写墓碑（防止旧页面在 60 秒内复活）。
func (e *Engine) ReleaseLease(groupID, leaseID string, closing, final bool, epoch int64) (bool, error) {
	if e == nil || e.tkStore == nil {
		return false, ErrTakeoverNotReady
	}
	if groupID == "" || leaseID == "" {
		return false, ErrBadLease
	}
	// release 永远允许：off 下也要能把残留租约清掉，always 下是无操作成功。
	if e.tkStore.Mode(groupID) == takeover.ModeAlways {
		return false, nil
	}

	now := e.now()
	e.tkMu.Lock()
	defer e.tkMu.Unlock()
	e.ensureLeasesLocked()
	e.pruneLeasesLocked(groupID, now)

	released := false
	if ls := e.leases[groupID]; ls != nil {
		if l, ok := ls[leaseID]; ok {
			delete(ls, leaseID)
			if len(ls) == 0 {
				delete(e.leases, groupID)
			}
			released = true
			if !final && closing {
				if e.closing[groupID] == nil {
					e.closing[groupID] = map[string]closingRec{}
				}
				e.closing[groupID][leaseID] = closingRec{
					Start:    l.Start,
					LastBeat: l.LastBeat,
					Expires:  now.Add(closingGrace),
					Hidden:   l.Hidden,
					Epoch:    l.Epoch,
				}
			}
		}
	}
	if final {
		e.tomb[tombKey(groupID, leaseID)] = now.Add(tombTTL)
	}
	if released || final {
		e.tkRev++
	}
	return released, nil
}

// LeaseInfo 返回单条租约的只读快照。nil-safe。
func (e *Engine) LeaseInfo(groupID, leaseID string) LeaseInfo {
	if e == nil || e.tkStore == nil || groupID == "" || leaseID == "" {
		return LeaseInfo{}
	}
	now := e.now()
	e.tkMu.Lock()
	defer e.tkMu.Unlock()
	e.ensureLeasesLocked()
	e.pruneLeasesLocked(groupID, now)

	if l, ok := e.leases[groupID][leaseID]; ok {
		return LeaseInfo{Valid: true, ExpiresAt: l.LastBeat.Add(leaseTTL(l.Hidden)).Unix()}
	}
	if c, ok := e.closing[groupID][leaseID]; ok && c.active(now) {
		return LeaseInfo{Closing: true, ExpiresAt: c.Expires.Unix()}
	}
	if until, ok := e.tomb[tombKey(groupID, leaseID)]; ok && now.Before(until) {
		return LeaseInfo{Released: true, ExpiresAt: until.Unix()}
	}
	return LeaseInfo{}
}

// ClearTakeoverState 清空单个群的租约、宽限与墓碑，不动模式文件。
//
// 切到 off/always 时由 SetTakeoverMode 调用；墓碑一并清掉，
// 因为 off 之后租约接口先判模式就返回 mode_off，墓碑没有消费场景。
func (e *Engine) ClearTakeoverState(groupID string) {
	if e == nil || groupID == "" {
		return
	}
	e.tkMu.Lock()
	defer e.tkMu.Unlock()
	e.ensureLeasesLocked()
	// 无条件推进清理代号：即使没有租约可清，也要让正在 acquire 的调用
	// 重新读一次模式，避免把租约建在「刚被切走」的群上。
	e.tkGen++
	changed := len(e.leases[groupID]) > 0 || len(e.closing[groupID]) > 0
	delete(e.leases, groupID)
	delete(e.closing, groupID)
	prefix := groupID + "|"
	for key := range e.tomb {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(e.tomb, key)
			changed = true
		}
	}
	if changed {
		e.tkRev++
	}
}

// DeleteTakeoverState 清空接管状态并删除模式条目。群被移除时由管理端调用。
func (e *Engine) DeleteTakeoverState(groupID string) {
	if e == nil || groupID == "" {
		return
	}
	e.ClearTakeoverState(groupID)
	if e.tkStore != nil {
		if err := e.tkStore.Delete(groupID); err != nil {
			logx.Warn("删除接管模式条目失败", "group", groupID, "err", err.Error())
			return
		}
	}
	// 模式条目没了也是一次状态变更：即使没有租约可清，前端也要刷新。
	e.tkMu.Lock()
	e.tkGen++
	e.tkRev++
	e.tkMu.Unlock()
}

// SetTakeoverMode 设置单个群的接管档位。
//
// 先写盘（失败则内存与租约都不动），再清租约：off 下租约必须清空；
// always 也清空——租约只服务 window，留着会在切回 window 时让 AI 无故暂停。
func (e *Engine) SetTakeoverMode(groupID string, m takeover.Mode) error {
	if e == nil || e.tkStore == nil {
		return ErrTakeoverNotReady
	}
	prev := e.tkStore.Mode(groupID)
	if err := e.tkStore.SetMode(groupID, m); err != nil {
		return err
	}
	if prev == m {
		return nil
	}
	if m == takeover.ModeOff || m == takeover.ModeAlways {
		e.ClearTakeoverState(groupID)
	}
	e.tkMu.Lock()
	e.tkRev++
	e.tkMu.Unlock()
	return nil
}

// EndAll 把所有已知群设为 off，清空全部租约/宽限/墓碑并递增 epoch。
//
// 已知群取三方并集：memory 里聊过的群、锚点池里有记录的群、takeover.json
// 里有模式的群。只用最后一方会漏掉「从没改过模式但一直在聊」的群。
func (e *Engine) EndAll() (EndAllResult, error) {
	var out EndAllResult
	if e == nil || e.tkStore == nil {
		return out, ErrTakeoverNotReady
	}

	seen := map[string]struct{}{}
	add := func(g string) {
		if g != "" {
			seen[g] = struct{}{}
		}
	}
	if e.mem != nil {
		for _, g := range e.mem.All() {
			if g != nil {
				add(g.OpenID)
			}
		}
	}
	if e.anchors != nil {
		for _, g := range e.anchors.AnchorGroups() {
			add(g)
		}
	}
	for _, g := range e.tkStore.Groups() {
		add(g)
	}

	// 逐个写盘，不持 tkMu。SetMode 失败就停：已经改掉的群保持 off，
	// 返回错误让管理端知道这次没有全部完成。
	for g := range seen {
		if e.tkStore.Mode(g) == takeover.ModeOff {
			continue
		}
		if err := e.tkStore.SetMode(g, takeover.ModeOff); err != nil {
			return out, err
		}
		out.ModesReset++
	}

	e.tkMu.Lock()
	e.ensureLeasesLocked()
	for _, ls := range e.leases {
		out.LeasesReleased += len(ls)
	}
	for _, cs := range e.closing {
		out.LeasesReleased += len(cs)
	}
	// 墓碑是清掉而不是写入：end_all 之后都是 off，租约接口先判模式，
	// 墓碑没有消费场景；写入反而会挡住 60 秒内切回 window 后的正常 acquire。
	e.leases = map[string]map[string]*lease{}
	e.closing = map[string]map[string]closingRec{}
	e.tomb = map[string]time.Time{}
	e.tkGen++
	e.tkRev++
	out.Rev = e.takeoverRevLocked()
	e.tkMu.Unlock()

	epoch, err := e.tkStore.BumpEpoch()
	out.Epoch = epoch
	return out, err
}
