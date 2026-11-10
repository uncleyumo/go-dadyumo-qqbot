package qqapi

import (
	"sort"
	"sync"
	"time"
)

// 被动消息限制：群聊 5 分钟有效，同一条用户消息最多回复 5 次
const (
	passiveTTL    = 5 * time.Minute
	passiveMaxUse = 5
)

// anchorWindowMax 是只读快照一次最多返回的条数。
// 与建池时的 keep 无关：keep 可能调大，但管理端只看最近 20 条就够。
const anchorWindowMax = 20

type anchor struct {
	msgID string
	// sender 是这条消息的发送者 openid。
	//
	// 为什么必须存在：被动回复靠挂 msg_id 实现，而挂错了人群里就看成
	// 「它在跟别人说话」——内容是对的，对话关系是错的。攒批窗口里不同人的
	// 消息会混在一起，只认「群里最新那条」必然有相当比例挂错人。
	sender string
	name   string
	ts     time.Time
	used   int
	// seq 是这条消息上已发出的第几条回复（msg_seq）。
	//
	// 关键不变式：**seq 只增不减，任何失败路径都不得回滚它**。
	// QQ 按 (msg_id, msg_seq) 判重，一旦某个 seq 被用过又回滚重发，
	// 平台会当成重复消息拒掉，表现为这个群突然彻底哑火。
	// 早期版本在发送失败时 seq-- 就是踩了这个坑。
	seq uint32
	// refIdx 是这条消息的引用 id（平台给的 REFIDX_xxx==），
	// 来自事件的 message_scene.ext。
	//
	// 为什么需要它：被动回复挂的 msg_id 让 QQ 客户端渲染成「@原发送者」的样式，
	// 而真正独立的引用气泡要靠 message_reference。两者可以同时出现在一次请求里
	// （官方示例就是 msg_id 与 message_reference 并存），所以接上引用不必牺牲被动回复。
	//
	// 为空是常态而不是错误：平台不一定每条消息都给 refIdx，
	// 拿不到时整个 message_reference 字段省略，退化成改动前的行为。
	refIdx string
}

// AnchorPool 维护每个群最近的用户消息，作为被动回复的挂载点。
// 群越活跃，可用锚点越多，机器人就越不需要消耗主动消息额度。
type AnchorPool struct {
	mu   sync.Mutex
	m    map[string][]*anchor
	keep int
	// now 是取当前时间的函数，默认 time.Now；测试可以换成冻结时钟，
	// 用来稳定地构造「已过期」而不用真的 sleep 5 分钟。
	now func() time.Time
}

// NewAnchorPool 创建锚点池，keep 为每个群保留的锚点上限
func NewAnchorPool(keep int) *AnchorPool {
	if keep <= 0 {
		keep = 20
	}
	return &AnchorPool{m: map[string][]*anchor{}, keep: keep, now: time.Now}
}

// nowTime 取当前时间。零值 AnchorPool 没走构造函数时退回 time.Now，避免空指针。
func (p *AnchorPool) nowTime() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// Add 记录一条可用于被动回复的用户消息。
// sender/openID 缺省为空（旧调用方），此时该锚点只能作为「回退到最新」的候选，
// 不会被优先选中——宁可挂最新的，也别挂到一个不知道是谁的。
//
// refIdx 为空（平台没给）不影响任何东西，只是这条锚点发出去时不带 message_reference。
func (p *AnchorPool) Add(groupOpenID, msgID, senderOpenID, senderName, refIdx string) {
	if groupOpenID == "" || msgID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.m[groupOpenID]
	for _, a := range list {
		if a.msgID == msgID {
			// 同一条消息补登时把发送人补上：平台可能先推附件、后推正文
			if a.sender == "" && senderOpenID != "" {
				a.sender = senderOpenID
				a.name = senderName
			}
			// refIdx 同理：平台可能第一条事件里还没有 message_scene
			if a.refIdx == "" && refIdx != "" {
				a.refIdx = refIdx
			}
			return
		}
	}
	list = append(list, &anchor{msgID: msgID, sender: senderOpenID, name: senderName,
		refIdx: refIdx, ts: p.nowTime()})
	if len(list) > p.keep {
		list = list[len(list)-p.keep:]
	}
	p.m[groupOpenID] = list
}

// PickAndReserve 选出一个仍有效的锚点，并在同一把锁内完成「占用额度 + 取出下一个 seq」。
//
// 为什么合并成一步：原先的 Pick 只读、MarkUsed 在发送成功后才递增，
// 于是两个并发发送（被动应答与攒批发言可以并发）会拿到同一个 msg_seq，
// 平台必然拒掉一条。现在占用与取出是原子的，并发下也不会重号。
//
// preferSender 非空时优先挂到这个人最近的一条消息下——这是「回对谁」的关键。
// 找不到（他太久没说话 / 锚点已满）就退回群里最新的一条。
//
// 多返回一个 refIdx（那条消息的引用 id，可能为空）：
// 调用方拿它填 message_reference，就能发出真正的引用气泡而不只是被动挂靠。
// 为空时整个字段省略——平台不给 refIdx 是常态，不该因此发不出去。
//
// **matched 说返回的锚点是不是恰好属于 preferSender 那个人。**
// 它唯一的作用是让调用方在「不是他」时把 refIdx 丢掉：
// 引用气泡里出现一个陌生人的消息，比没有气泡糟糕得多——
// 群里人会看到「他在引用别人」而内容明显是在回这个人。
// 2026-10-06 生产实况：模型填的 to 指向一个刚说过话的人，
// 而真正选中的是群里最新那条（另一个人），引用就这么挂错了。
// msg_id 仍然用退回的那条（被动回复授权需要它，丢了就发不出去）。
func (p *AnchorPool) PickAndReserve(groupOpenID, preferSender string) (msgID, refIdx string, seq uint32, ok, matched bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.nowTime()
	alive := make([]*anchor, 0, len(p.m[groupOpenID]))
	for _, a := range p.m[groupOpenID] {
		if now.Sub(a.ts) <= passiveTTL && a.used < passiveMaxUse {
			alive = append(alive, a)
		}
	}
	// 注意这里**不再把 p.m[groupOpenID] 覆盖成 alive**：
	// 只读快照（Window/List）要靠过期与用满的条目区分
	// no_anchor / expired / quota_exhausted，提前删掉就只剩 no_anchor。
	// 容量仍由 Add 里的 keep 上限约束，不会无限增长。
	if len(alive) == 0 {
		return "", "", 0, false, false
	}

	// 「挂错人」只发生在一种情况：**指定了某人，但池里没有他的锚点**，
	// 于是退回了群里最新那条——那是别人。
	//
	// 不指定（空串）时挂最新那条正是本意，不算错。
	// 所以判据是「选中这条是不是就是他要的那个人」，而不是「有没有指定人」。
	var target *anchor
	if preferSender != "" {
		// 倒序找：同一人的多条里取最新的一条，避免把额度浪费在快过期的旧消息上
		for i := len(alive) - 1; i >= 0; i-- {
			if alive[i].sender == preferSender {
				target = alive[i]
				break
			}
		}
	}
	if target == nil {
		target = alive[len(alive)-1]
	}
	matched = preferSender == "" || target.sender == preferSender

	target.used++
	target.seq++
	return target.msgID, target.refIdx, target.seq, true, matched
}

// Release 发送失败：把这次占用的回复额度还回去。
//
// 注意只回滚 used，**不回滚 seq**——seq 一旦发出去就必须单调递增，
// 回滚会让下一次重发撞上平台判重，把一次网络抖动放大成整个群 5 分钟失声。
func (p *AnchorPool) Release(groupOpenID, msgID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.m[groupOpenID] {
		if a.msgID == msgID && a.used > 0 {
			a.used--
			return
		}
	}
}

// Stats 各群当前可用锚点数
func (p *AnchorPool) Stats() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.m))
	now := p.nowTime()
	for g, list := range p.m {
		n := 0
		for _, a := range list {
			if now.Sub(a.ts) <= passiveTTL && a.used < passiveMaxUse {
				n++
			}
		}
		out[g] = n
	}
	return out
}

// AnchorInfo 是一条用户消息锚点的只读副本。
//
// 字段与设计文档 §4.6 的 takeover.AnchorSnapshot 一一对应。快照故意不带
// refIdx / seq：它只供管理端展示与发送前预检，真正发送仍由 PickAndReserve
// 在锁内重新选点并占用额度，额度以实际占用为准。
type AnchorInfo struct {
	MsgID        string `json:"msg_id"`
	SenderOpenID string `json:"sender_openid"`
	SenderName   string `json:"sender_name"`
	Used         int    `json:"used"`
	MaxUse       int    `json:"max_use"`
	Alive        bool   `json:"alive"`      // 仍在 5 分钟 TTL 内且 used < max_use
	ExpiresAt    int64  `json:"expires_at"` // unix 秒
}

// snapshotLocked 把内部锚点转成只读副本，调用方必须已持有 p.mu。
func (p *AnchorPool) snapshotLocked(a *anchor, now time.Time) AnchorInfo {
	return AnchorInfo{
		MsgID:        a.msgID,
		SenderOpenID: a.sender,
		SenderName:   a.name,
		Used:         a.used,
		MaxUse:       passiveMaxUse,
		Alive:        now.Sub(a.ts) <= passiveTTL && a.used < passiveMaxUse,
		ExpiresAt:    a.ts.Add(passiveTTL).Unix(),
	}
}

// Window 返回池里保留的全部锚点，最多 anchorWindowMax 条。
//
// 排序：preferSender 的存活锚点最前，其次是其余存活锚点，过期/用满的排最后；
// 同一档内按最新在前。过期与用满的条目**要保留**，否则管理端无法区分
// no_anchor / expired / quota_exhausted 三种拒绝原因。
func (p *AnchorPool) Window(groupOpenID, preferSender string) []AnchorInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	list := p.m[groupOpenID]
	now := p.nowTime()
	out := make([]AnchorInfo, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, p.snapshotLocked(list[i], now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return anchorRank(out[i], preferSender) < anchorRank(out[j], preferSender)
	})
	if len(out) > anchorWindowMax {
		out = out[:anchorWindowMax]
	}
	return out
}

// anchorRank 是 Window 的排序档位：0 指发送人的存活锚点，1 其他存活锚点，2 过期或用满。
func anchorRank(a AnchorInfo, preferSender string) int {
	switch {
	case a.Alive && preferSender != "" && a.SenderOpenID == preferSender:
		return 0
	case a.Alive:
		return 1
	default:
		return 2
	}
}

// List 返回池里保留的全部锚点，纯按最新在前，含已用满与已过期条目，
// 最多 anchorWindowMax 条。需要「存活优先、指定发送人优先」时用 Window。
func (p *AnchorPool) List(groupOpenID string) []AnchorInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	list := p.m[groupOpenID]
	now := p.nowTime()
	out := make([]AnchorInfo, 0, len(list))
	for i := len(list) - 1; i >= 0 && len(out) < anchorWindowMax; i-- {
		out = append(out, p.snapshotLocked(list[i], now))
	}
	return out
}

// KnownGroups 返回曾经登记过锚点的群，即使当前已经没有任何存活锚点。
// 排序固定，方便调用方（EndAll）稳定遍历，也让测试可以逐项断言。
func (p *AnchorPool) KnownGroups() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, 0, len(p.m))
	for g := range p.m {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// 以下是 Client 上的转发方法，供 main 里的 takeover.AnchorSource 适配器直接调用，
// 调用方不需要知道 AnchorPool 这个类型。

// Window 转发锚点池的只读快照，语义见 AnchorPool.Window。
func (c *Client) Window(groupOpenID, preferSender string) []AnchorInfo {
	return c.anchors.Window(groupOpenID, preferSender)
}

// ListAnchors 转发锚点池的全部锚点快照，语义见 AnchorPool.List。
func (c *Client) ListAnchors(groupOpenID string) []AnchorInfo {
	return c.anchors.List(groupOpenID)
}

// AnchorGroups 转发「登记过锚点的群」，语义见 AnchorPool.KnownGroups。
func (c *Client) AnchorGroups() []string {
	return c.anchors.KnownGroups()
}
