package qqapi

import (
	"sync"
	"time"
)

// 被动消息限制：群聊 5 分钟有效，同一条用户消息最多回复 5 次
const (
	passiveTTL    = 5 * time.Minute
	passiveMaxUse = 5
)

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
}

// NewAnchorPool 创建锚点池，keep 为每个群保留的锚点上限
func NewAnchorPool(keep int) *AnchorPool {
	if keep <= 0 {
		keep = 20
	}
	return &AnchorPool{m: map[string][]*anchor{}, keep: keep}
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
		refIdx: refIdx, ts: time.Now()})
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
func (p *AnchorPool) PickAndReserve(groupOpenID, preferSender string) (msgID, refIdx string, seq uint32, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	alive := make([]*anchor, 0, len(p.m[groupOpenID]))
	for _, a := range p.m[groupOpenID] {
		if now.Sub(a.ts) <= passiveTTL && a.used < passiveMaxUse {
			alive = append(alive, a)
		}
	}
	p.m[groupOpenID] = alive
	if len(alive) == 0 {
		return "", "", 0, false
	}

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

	target.used++
	target.seq++
	return target.msgID, target.refIdx, target.seq, true
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
	now := time.Now()
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
