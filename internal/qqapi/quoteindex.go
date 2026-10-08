package qqapi

import "sync"

// QuoteSrc 是本机见过的一条消息，供别人引用它时反查。
type QuoteSrc struct {
	OpenID string
	Name   string
	Text   string
	Images []string
}

// QuoteIndex 按群的「引用 id → 那条消息」有界索引。
//
// # 为什么非要有它
//
// QQ 给「引用消息」的入站回调里，被引用的那条**只有一个不透明的 id**：
//
//   - 2026-10-08 实测，老爹一天 81 条含引用的回调，带 attachments 的 **0 条**，
//     带 msg_elements 的 20 条里带 author 的 **0 条**。
//   - 也就是说：被引用的是谁发的、里面有没有图，平台一个字都不给。
//   - 机器人发的引用更绝（message_type=0），连 msg_elements 都没有。
//
// 于是 `internal/agent` 里读 `el.Author` 取原作者那条路在真实数据上永远落空。
//
// 但那个 id 是本机见过的东西：同一条消息在**它自己**的回调里以
// `message_scene.ext` 的 `msg_idx=` 出现，别人引用它时以 `ref_msg_idx=` 出现，
// 两边是同一个字符串。把这两头对起来，被引用的是谁、说了什么、发了哪张图，
// 就全都回来了——图还是本机收到时那份带 CDN 地址的原件，能直接喂给视觉模型。
//
// # 两个来源
//
//   - 收到的每条群消息（agent 侧登记）。
//   - 本机自己发出去的消息（发送响应 ext_info.ref_idx，sendGroup 侧登记）——
//     平台不会把自己的消息推回给自己，不收这一路的话「有人引用了老爹说过的话」
//     就永远查不到。2026-10-08 实测这两路合起来覆盖当天引用的 74/81。
type QuoteIndex struct {
	mu   sync.Mutex
	m    map[string][]quoteEntry
	keep int
}

type quoteEntry struct {
	refIdx string
	src    QuoteSrc
}

// defaultQuoteKeep 是每个群保留的条数上限。
//
// 不设 TTL 只设条数：引用可以指向很久以前的消息，按时间淘汰会莫名其妙丢东西；
// 而按条数封顶，内存是死的（每群约 200 条），而且超出后淘汰的必然是更早的，
// 与「越久越查不到」的自然预期一致。
const defaultQuoteKeep = 200

// NewQuoteIndex 建索引，keep 为每群保留条数上限（<=0 取默认值）。
func NewQuoteIndex(keep int) *QuoteIndex {
	if keep <= 0 {
		keep = defaultQuoteKeep
	}
	return &QuoteIndex{m: map[string][]quoteEntry{}, keep: keep}
}

// Add 登记一条本机见过的消息。
//
// refIdx 为空直接忽略：平台不一定每条消息都给 msg_idx，那不是错误。
// 同一条消息重复登记时以最后一次为准（平台可能分多次推同一条的正文与附件）。
func (q *QuoteIndex) Add(groupOpenID, refIdx string, src QuoteSrc) {
	if q == nil || groupOpenID == "" || refIdx == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.m[groupOpenID]
	for i := range list {
		if list[i].refIdx == refIdx {
			list[i].src = src
			q.m[groupOpenID] = list
			return
		}
	}
	list = append(list, quoteEntry{refIdx: refIdx, src: src})
	if len(list) > q.keep {
		list = list[len(list)-q.keep:]
	}
	q.m[groupOpenID] = list
}

// Lookup 反查被引用的那条消息。查不到是常态（那条本机没见过），不是错误。
func (q *QuoteIndex) Lookup(groupOpenID, refIdx string) (QuoteSrc, bool) {
	if q == nil || groupOpenID == "" || refIdx == "" {
		return QuoteSrc{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// 线性扫。keep 是 200 这个量级，而且只在有引用时才走一次，
	// 换成 map 索引要多维护一份淘汰逻辑，不值。
	list := q.m[groupOpenID]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].refIdx == refIdx {
			return list[i].src, true
		}
	}
	return QuoteSrc{}, false
}
