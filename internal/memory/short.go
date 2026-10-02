// Package memory 维护群聊会话上下文。当前实现短期滑动窗口（内存），
// 后续阶段在此基础上叠加滚动摘要与长期事实库。
package memory

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxFacts 每个群最多记住多少条长期要点。
// 要点会进提示词，记太多既烧 token 又容易把模型带跑偏。
// 导出给管理端，用于展示「已用 n/MaxFacts」。
const MaxFacts = 24

// Role 说话人角色
const (
	RoleUser = "user"
	RoleBot  = "bot"
	RoleNote = "note" // 系统注入的提示，不直接对模型可见为「消息」
)

// Line 一条聊天记录
type Line struct {
	TS   time.Time
	Role string
	Name string
	// OpenID 是说话人的稳定身份。
	//
	// 为什么不能只有 Name：群成员随时可以改昵称，还可能两个人撞同一个昵称。
	// 记录里存「当时看到的名字」，那么这个人改名之后历史里就同时躺着
	// 「张三」和「李四」两条来自同一个人的记录，模型会当成两个人。
	// 有了 OpenID，渲染时按身份取当前称呼，改名能一次性回溯生效。
	OpenID  string
	Content string
	MsgID   string // 用户消息的 msg_id，用于被动回复挂载
}

// Member 群里一个人的粗画像。
// 不用做多精细，够支撑「谁是谁、谁活跃、谁是主人」就行。
type Member struct {
	OpenID   string    `json:"openid"`
	Name     string    `json:"name"`
	LastSeen time.Time `json:"last_seen"`
	MsgCount int       `json:"msg_count"`
	IsMaster bool      `json:"is_master"`
	Note     string    `json:"note,omitempty"` // 模型自己记下的关于这个人的要点

	// Card 是他在群里的名片（群名片）。
	//
	// 平台对同一个人在不同上下文给的名字不同，而且这个差异是系统性的：
	//   - author（他自己发言时）→ **账号昵称**
	//   - mentions（别人 @ 他时）→ **群名片**
	// 生产数据四个样本零例外：11112222 账号「群友甲」/名片「群名片甲」，
	// AAAA5555 账号「群友乙」/名片「群名片丁」，
	// 55558888 账号「群友丙」/名片「群名片乙」，
	// 66667777 账号「群友丁」/名片「群名片丙」。
	// （昵称与 openid 均为脱敏后的样本值，非真实群成员）
	//
	// 为什么主名用账号昵称：那是这个人**自称**的名字，是他在群里希望
	// 被怎么称呼。群名片是他自己改的展示名，两者不一致时以自称优先。
	// 名片仍然要记下来——群友 @ 他时屏幕上显示的是那个，模型得能认出来。
	Card string `json:"card,omitempty"`

	// Aliases 是这个人先后用过、已经改掉的称呼，从旧到新，最多留几个。
	// 用途有二：改名之后仍能从旧称呼认出是他；模型偶尔会照着更早的上下文里
	// 那个名字填 to，靠它能兜住。
	//
	// 关键：必须去重。账号昵称与群名片会随群里的互动**交替出现**
	// （他自己说话→账号名、别人 @ 他→名片），不去重的话 4 个名额
	// 会被这两个名字刷满，真正的改名记录反而被挤掉。
	Aliases []string `json:"aliases,omitempty"`
}

// addAlias 追加一条旧称呼，跳过已存在的。
//
// 调用点在 TouchMember 里，且是「先记旧名、再改新名」——调用时 m.Name 还是
// 那个要记录的旧名本身，所以这里**不能**拿 name 跟 m.Name 比。
// addAlias 追加一条旧称呼，跳过已存在的。
//
// 调用点在 touchMember 里，且是「先记旧名、再改新名」——调用时 m.Name 还是
// 那个要记录的旧名本身，所以这里**绝不能**拿 name 跟 m.Name 比，
// 否则每一次改名都会被自己挡掉。别名永远记不进去。
//
// 唯一要跳过的重复是：已经记过的不再记（账号名与群名片会交替出现，
// 不去重的话 4 个名额会被这两个名字刷满）。
func (m *Member) addAlias(name string) {
	name = strings.TrimSpace(name)
	if name == "" || name == m.Card {
		return
	}
	for _, a := range m.Aliases {
		if a == name {
			return
		}
	}
	m.Aliases = append(m.Aliases, name)
	if len(m.Aliases) > maxAliases {
		m.Aliases = m.Aliases[len(m.Aliases)-maxAliases:]
	}
}

// KnownNames 返回这个人所有可能被叫到的称呼：现名 + 群名片 + 历史别名。
//
// 用途是**让「群名片丁」和「群友乙」被认成同一个人**。
// 模型在 to 里填哪个、群友在聊天里提到哪个，都应该能对上同一个人。
func (m *Member) KnownNames() []string {
	out := make([]string, 0, len(m.Aliases)+2)
	if m.Name != "" {
		out = append(out, m.Name)
	}
	if m.Card != "" && m.Card != m.Name {
		out = append(out, m.Card)
	}
	for _, a := range m.Aliases {
		out = append(out, a)
	}
	return out
}

// maxAliases 每人最多记几个旧称呼。多了只是噪音，而且每条都要占提示词。
const maxAliases = 4

// maxAliasRune 称呼的长度上限，防止有人用超长昵称把内存和提示词撑爆
const maxAliasRune = 24

// Group 一个群的会话上下文
type Group struct {
	OpenID string
	Name   string

	mu          sync.Mutex
	recent      []Line
	lastSpeak   time.Time
	lastText    string
	consecutive int // 连续主动发言次数
	lastUserAt  time.Time
	lastAtHit   time.Time // 上次被 @ / 被点名的时间，用于在线宽限
	mutedUntil  time.Time
	notes       []string
	facts       map[string]string    // 长期要点：模型自己觉得该记住的东西
	factAt      map[string]time.Time // key -> 最后写入时间，用于满容量时按 LRU 淘汰
	members     map[string]*Member   // openid -> 画像

	// 滚动摘要相关。
	// totalLines 是单调递增的累计条数（recent 会被裁剪，不能用 len(recent) 当游标）；
	// summaryUntil 表示「前多少条已经被压进 summary」，两者之差就是待摘要的新增条数。
	summary      string
	totalLines   int
	summaryUntil int

	// 机器人是否已被移出该群（GROUP_DEL_ROBOT 事件标记）。
	// 平台不提供群列表查询，这是唯一能拿到的「已退群」信号。
	left   bool
	leftAt time.Time
}

// NewGroup 创建群上下文
func NewGroup(openID, name string) *Group {
	return &Group{
		OpenID:  openID,
		Name:    name,
		facts:   map[string]string{},
		factAt:  map[string]time.Time{},
		members: map[string]*Member{},
	}
}

// Store 全部群的会话上下文
type Store struct {
	mu   sync.RWMutex
	m    map[string]*Group
	keep int
}

// New 创建记忆存储，keep 为每群保留的条数
func New(keep int) *Store {
	if keep <= 0 {
		keep = 40
	}
	return &Store{m: map[string]*Group{}, keep: keep}
}

// Group 获取（不存在则创建）
func (s *Store) Group(openID, name string) *Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.m[openID]
	if !ok {
		g = NewGroup(openID, name)
		s.m[openID] = g
	}
	if name != "" && g.Name == "" {
		g.Name = name
	}
	return g
}

// All 返回全部群
func (s *Store) All() []*Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Group, 0, len(s.m))
	for _, g := range s.m {
		out = append(out, g)
	}
	return out
}

// RefreshName 用管理端设置的别名无条件覆盖群名。
// 区别于 Group() 的「只在没名字时回填」：历史 memory.json 里固化的是 openid 尾 8 位代号，
// 别名就是用来纠正这些固化错误名的，必须能覆盖。
func (s *Store) RefreshName(openID, name string) {
	name = strings.TrimSpace(name)
	if openID == "" {
		return
	}
	g := s.Group(openID, name)
	g.mu.Lock()
	g.Name = name
	g.mu.Unlock()
}

// RemoveGroup 彻底移除一个群的全部记忆（管理端手动清理已退群的死数据）
func (s *Store) RemoveGroup(openID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, openID)
}

// SetLeft 标记/清除「机器人已被移出该群」
func (g *Group) SetLeft(left bool) {
	g.mu.Lock()
	g.left = left
	if left {
		g.leftAt = time.Now()
	} else {
		g.leftAt = time.Time{}
	}
	g.mu.Unlock()
}

// Left 返回机器人是否已被移出该群，以及事件发生时间
func (g *Group) Left() (bool, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.left, g.leftAt
}

// Append 追加一条记录
func (g *Group) Append(line Line, keep int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if line.TS.IsZero() {
		line.TS = time.Now()
	}
	g.recent = append(g.recent, line)
	g.totalLines++
	g.summaryUntil = mini(g.summaryUntil, g.totalLines)
	if len(g.recent) > keep {
		g.recent = g.recent[len(g.recent)-keep:]
	}
}

// Summary 返回已压缩的「前文提要」，没有则空串
func (g *Group) Summary() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.summary
}

// SetSummary 写入摘要，并把游标推到当前总条数（表示这些条已经消化过了）
func (g *Group) SetSummary(s string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.summary = s
	g.summaryUntil = g.totalLines
}

// PendingSummary 返回自上次摘要以来新增了多少条
func (g *Group) PendingSummary() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.totalLines - g.summaryUntil
}

func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Recent 返回最近 n 条（副本）
func (g *Group) Recent(n int) []Line {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n <= 0 || n > len(g.recent) {
		n = len(g.recent)
	}
	out := make([]Line, n)
	copy(out, g.recent[len(g.recent)-n:])
	return out
}

// LastUserLine 最后一条非机器人消息
func (g *Group) LastUserLine() (Line, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.recent) - 1; i >= 0; i-- {
		if g.recent[i].Role == RoleUser {
			return g.recent[i], true
		}
	}
	return Line{}, false
}

// IdleFor 距离最后一条用户消息过去了多久
func (g *Group) IdleFor() time.Duration {
	line, ok := g.LastUserLine()
	if !ok {
		return 0
	}
	return time.Since(line.TS)
}

// MarkBotSpoke 记录机器人刚说过话
func (g *Group) MarkBotSpoke(text string) {
	g.mu.Lock()
	g.lastSpeak = time.Now()
	g.lastText = text
	g.consecutive++
	g.mu.Unlock()
}

// MarkAttempted 记录「尝试过发言，但一条都没送出去」。
//
// 为什么需要它：QQ 侧限流、权限变更这类失败是持续性的，不推进状态的话
// 下一条消息进来会立刻再打一次，历史上有过 19 分钟内 20 次决策、
// 76 次注定失败的发送、群里一个字都没出现的记录。
//
// 只推进 lastSpeak，不动 lastText 也不加 consecutive：它确实没说出话，
// 不该让「你刚刚说过」变成一句从没说过的话，也不该算进「连着说了几轮」。
func (g *Group) MarkAttempted() {
	g.mu.Lock()
	g.lastSpeak = time.Now()
	g.mu.Unlock()
}

// MarkUserTurn 用户发言后打断机器人的连续发言
func (g *Group) MarkUserTurn() {
	g.mu.Lock()
	g.consecutive = 0
	g.lastUserAt = time.Now()
	g.mu.Unlock()
}

// MarkAtHit 记录「刚被人点名了」。点名后的宽限期里机器人保持实时回应，
// 不再按在线时段的概率过滤——被人 @ 了还装死是最伤体验的。
func (g *Group) MarkAtHit() {
	g.mu.Lock()
	g.lastAtHit = time.Now()
	g.mu.Unlock()
}

// SinceAtHit 距离上次被点名过去了多久。从未被点过返回一个很大的值。
func (g *Group) SinceAtHit() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastAtHit.IsZero() {
		return time.Hour * 24
	}
	return time.Since(g.lastAtHit)
}

// SinceLastSpeak 距离上次发言的时间
func (g *Group) SinceLastSpeak() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastSpeak.IsZero() {
		return time.Hour * 24
	}
	return time.Since(g.lastSpeak)
}

// Consecutive 连续发言次数
func (g *Group) Consecutive() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.consecutive
}

// LastText 上次发言内容（用于避免复读）
func (g *Group) LastText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastText
}

// MutedUntil 被管理员要求闭嘴的截止时间
func (g *Group) MutedUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mutedUntil
}

// Mute 静默一段时间
func (g *Group) Mute(d time.Duration) {
	g.mu.Lock()
	g.mutedUntil = time.Now().Add(d)
	g.mu.Unlock()
}

// Notes 返回注入的备注
func (g *Group) Notes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.notes))
	copy(out, g.notes)
	return out
}

// AddNote 注入一条备注（例如「刚刚冷场，你可以主动起个话题」）
func (g *Group) AddNote(n string) {
	g.mu.Lock()
	g.notes = append(g.notes, n)
	if len(g.notes) > 20 {
		g.notes = g.notes[len(g.notes)-20:]
	}
	g.mu.Unlock()
}

// SetFact 记下一条长期要点。
//
// 同名 key 直接覆盖并刷新写入时间；新 key 在容量已满时，淘汰「最久没被写过」
// 的那条（LRU），再写入新条目。返回被淘汰的 key（没淘汰则为空串）。
//
// 早期实现在满容量时直接拒收新条目，结果是把群记忆冻在 24 条、永远学不进新东西。
// 改为 LRU 后，老知识自然让位给新内容，不需要人工清理。
func (g *Group) SetFact(k, v string) string {
	k = strings.TrimSpace(k)
	v = strings.TrimSpace(v)
	if k == "" || v == "" {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.facts == nil {
		g.facts = map[string]string{}
	}
	if g.factAt == nil {
		g.factAt = map[string]time.Time{}
	}
	evicted := ""
	if _, exists := g.facts[k]; !exists && len(g.facts) >= MaxFacts {
		evicted = g.evictOldestFactLocked()
	}
	g.facts[k] = v
	g.factAt[k] = time.Now()
	return evicted
}

// evictOldestFactLocked 淘汰「最后写入时间最早」的一条要点并返回其 key。
// 时间并列时按 key 字典序取最小，保证结果确定（map 遍历本身无序）。
// 调用方必须已持有 g.mu。
func (g *Group) evictOldestFactLocked() string {
	oldestKey := ""
	var oldestAt time.Time
	for k := range g.facts {
		at := g.factAt[k]
		if oldestKey == "" || at.Before(oldestAt) || (at.Equal(oldestAt) && k < oldestKey) {
			oldestKey, oldestAt = k, at
		}
	}
	if oldestKey == "" {
		return ""
	}
	delete(g.facts, oldestKey)
	delete(g.factAt, oldestKey)
	return oldestKey
}

// DelFact 删除一条长期要点（管理端手动清理，或模型改主意时）。
func (g *Group) DelFact(k string) {
	k = strings.TrimSpace(k)
	if k == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.facts, k)
	delete(g.factAt, k)
}

// FactItem 一条长期要点的结构化形态（管理端展示/编辑用）
type FactItem struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FactsList 返回全部长期要点的结构化快照，按最后写入时间倒序（最新在前）。
// 仅供管理端；喂给模型的仍然是有序文本 Facts()。
func (g *Group) FactsList() []FactItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]FactItem, 0, len(g.facts))
	for k, v := range g.facts {
		out = append(out, FactItem{Key: k, Value: v, UpdatedAt: g.factAt[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// Facts 返回全部长期要点，渲染成给模型看的文本
func (g *Group) Facts() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.facts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(g.facts))
	for k := range g.facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString("- " + k + "：" + g.facts[k] + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TouchMember 记录某人说过话，并更新画像。name 是账号昵称。
func (g *Group) TouchMember(openID, name string) {
	g.touchMember(openID, name, "")
}

// TouchMemberCard 记录某人被别人 @ 过，name 是群名片。
//
// 与 TouchMember 分开是因为平台给这两个名字的**字段不同**，而且会交替出现：
// 自己发言时 author 给账号昵称，别人 @ 他时 mentions 给群名片。
// 混在一个函数里就会互相覆盖——真群里出现过成员表被群名片占成主名、
// 账号昵称被塞进 aliases 的情况。
func (g *Group) TouchMemberCard(openID, card string) {
	g.touchMember(openID, "", card)
}

func (g *Group) touchMember(openID, name, card string) {
	if openID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.members == nil {
		g.members = map[string]*Member{}
	}
	m, ok := g.members[openID]
	if !ok {
		m = &Member{OpenID: openID}
		g.members[openID] = m
	}
	// 账号昵称：主名。首次拿到名片时把它记进别名，反之不记——
	// 先只知道他叫「群友乙」，后来发现群里 @ 他显示的是
	// 「群名片丁」，这时该补一条别名。
	if name != "" {
		name = clipName(name)
		if name != "" && name != m.Name {
			// 先记下旧称呼再更新。撞昵称不是改名（两个人可以同名），
			// 这里记的是「同一个人先后用过哪些名字」。
			if m.Name != "" {
				m.addAlias(m.Name)
			}
			m.Name = name
		}
	}
	// 群名片：存进独立的 Card 字段，不参与主名竞争。
	if card != "" {
		card = clipName(card)
		if card != "" && card != m.Card && card != m.Name {
			m.addAlias(card)
			m.Card = card
		}
	}
	m.LastSeen = time.Now()
	m.MsgCount++
}

// clipName 统一昵称长度上限
func clipName(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxAliasRune {
		return string([]rune(s)[:maxAliasRune])
	}
	return s
}

// SetMemberNote 给某人写一句备注（例如「这人喜欢玩梗」「他最近在找工作」）
func (g *Group) SetMemberNote(openID, note string) {
	if openID == "" || strings.TrimSpace(note) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.members == nil {
		g.members = map[string]*Member{}
	}
	m, ok := g.members[openID]
	if !ok {
		m = &Member{OpenID: openID}
		g.members[openID] = m
	}
	m.Note = strings.TrimSpace(note)
}

// Members 返回成员画像快照，按最近发言时间倒序
func (g *Group) Members() []Member {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Member, 0, len(g.members))
	for _, m := range g.members {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// NameOfByOpenID 取这个人当前的称呼。未知身份返回空串。
// 渲染历史时用它替代 Line 里记的旧名，这样改名能一次性回溯生效。
func (g *Group) NameOfByOpenID(openID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m, ok := g.members[openID]; ok {
		return m.Name
	}
	return ""
}

// DupNames 返回在当前成员里出现一次以上的称呼。
// 撞昵称时必须让模型能把两个人分开：否则它在 A 面前说的话，
// 群友看到的是挂在「同名」的另一个人名下。
// DupNames 统计重名的称呼。
//
// 群名片也算进去：群名片是展示名，撞的概率不比账号昵称低（很多人会取一样的
// 昵称），而认错人是最严重的一类 bug。宁可多打一个「·尾4位」后缀，
// 也不能让模型对着同名的人说话却挂到另一个名下。
func (g *Group) DupNames() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	// 谁用过哪些称呼：一个 openid 的现名、群名片、历史别名都算，
	// 同一个人自己名下重复出现不算撞名。
	owners := map[string]int{} // 称呼 -> 用过它的不同 openid 数
	seen := map[string]map[string]bool{}
	add := func(name, openID string) {
		if name == "" {
			return
		}
		if seen[name] == nil {
			seen[name] = map[string]bool{}
		}
		if seen[name][openID] {
			return
		}
		seen[name][openID] = true
		owners[name]++
	}
	for _, m := range g.members {
		add(m.Name, m.OpenID)
		add(m.Card, m.OpenID)
		for _, a := range m.Aliases {
			add(a, m.OpenID)
		}
	}
	for k, v := range owners {
		if v < 2 {
			delete(owners, k)
		}
	}
	return owners
}

// MemberOf 取某个人的画像
func (g *Group) MemberOf(openID string) (Member, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.members[openID]
	if !ok {
		return Member{}, false
	}
	return *m, true
}

// RenderRecent 把最近消息渲染成给模型看的文本
func (g *Group) RenderRecent(n int) string {
	lines := g.Recent(n)
	var sb strings.Builder
	for _, l := range lines {
		switch l.Role {
		case RoleBot:
			sb.WriteString("【你】" + l.Content + "\n")
		case RoleNote:
			sb.WriteString("（" + l.Content + "）\n")
		default:
			name := l.Name
			if name == "" {
				name = "某人"
			}
			sb.WriteString("【" + name + "】" + l.Content + "\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
