package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"dadyumo/internal/logx"
)

// 持久化存两类东西：
//
//  1. 长期记忆——长期要点、成员画像、滚动摘要。它们本来就是「模型要读的」。
//  2. 近期会话窗口——最近十几条聊天记录。
//
// 第 2 类曾经**故意不落盘**，理由写在旧版这里：「重启后它本来就该忘记
// 刚才聊到哪，死抱着旧上下文反而会让它一上线就接一堆没人记得的茬。」
//
// 那个判断被推翻了。真人是记得昨天聊过什么的——重启之后接不上话，
// 表现得像个刚上线的机器人，这比「接了旧茬」露馅更快。
// 而且滚动摘要救不了这个空档：它要等攒够 summary_every（生产 24）条才跑一次，
// 部署一次就抹掉十几条，等于每次发版都要失忆一次。
//
// 但「不该接没人记得的茬」这个顾虑本身是对的，所以落盘有三重上限：
// 只留尾部若干条（maxPersistRecent）、只认一定时长内的（maxRecentAge）、
// 单条截断（maxLineRunes）。这三条共同定义了「记得昨天」与「别翻旧账」的边界。

// maxSummaryRunes 单群摘要的落盘上限（rune 数）。
// 实测摘要约 300 字且会随着压缩持续变长；不设上限的话，
// 一个天天聊的群就能让 memory.json 无界膨胀。
const maxSummaryRunes = 1200

// maxPersistRecent 每群最多落盘多少条近期记录。
//
// 为什么是 12 而不是 Append 的 keep（生产 30）：prompt 侧的 TrimHistory 按
// max_ctx_tokens（生产 3200）算下来实际也只留得下十几条，落 30 条大半永远
// 到不了模型，纯撑大文件。12 条足够「接上话」，又不至于把整个窗口搬上盘。
const maxPersistRecent = 12

// maxRecentAge 恢复时只认这么多次时间内的记录。
//
// 为什么是 6 小时：覆盖「下午崩了重启」「晚上发版重启」这两个真实场景。
// 再长它就开始复述昨天的梗——群里没人记得的话它突然提一句，比失忆更怪。
const maxRecentAge = 6 * time.Hour

// maxLineRunes 单条落盘上限（rune）。群友偶尔贴一整段网页原文进来，
// 200 字足够认出「当时在聊什么」，再多只是撑大文件。
const maxLineRunes = 200

// summaryTruncatedMark 摘要被裁剪后留下的标记，避免「读起来像完整摘要」的自欺。
const summaryTruncatedMark = "…（摘要超长，已截断）"

type groupSnapshot struct {
	OpenID  string             `json:"openid"`
	Name    string             `json:"name"`
	Facts   map[string]string  `json:"facts,omitempty"`
	Members map[string]*Member `json:"members,omitempty"`
	Left    bool               `json:"left,omitempty"` // 机器人已被移出该群
	LeftAt  time.Time          `json:"left_at,omitempty"`

	// FactsAt 是 facts 每个 key 的最后写入时间，用于满容量时的 LRU 淘汰。
	// 故意做成旁路字段、而不把 facts 的值改成对象：这样 facts 的落盘格式
	// 保持 {"k":"v"} 不变，scripts/export_chatlog.py 等外部消费端零改动。
	// 旧 memory.json 没有这个字段 → 反序列化为 nil → 那些 key 时间戳为零值，
	// 天然最早被淘汰（语义正合适：老知识先让位）。
	FactsAt map[string]time.Time `json:"facts_at,omitempty"`

	// Summary 是「前文提要」：模型压缩出来的、已被 recent 窗口裁掉的那部分历史。
	// 生产事故：这三字段曾长期只存在于内存，重启即被抹成空串
	// （journal 里「已更新前文提要」出现 33 次，落盘的 summary 全是 ""）。
	// 摘要花钱花时间算出来，却每次重启都从零重来，等于白算。
	Summary string `json:"summary,omitempty"`
	// TotalLines 是历史累计条数（recent 会被裁掉，不能用 len 当游标）。
	// 与 Summary 一起落盘，才能在重启后接着往摘要后面追加，
	// 而不是把同一段历史再摘要一遍。
	TotalLines int `json:"total_lines,omitempty"`
	// SummaryUntil 是冗余持久化字段：LoadFrom 一律按 total_lines 重算，
	// 落盘它只是为了不改动结构体形状（外部脚本会遍历这个 JSON）。
	SummaryUntil int `json:"summary_until,omitempty"`

	// Recent 是短期会话窗口的落盘子集，供重启后接上话。
	// 上限见 maxPersistRecent / maxRecentAge / maxLineRunes 三条。
	Recent []lineSnapshot `json:"recent,omitempty"`

	// RecentAt 是这份 recent 的落盘时刻。
	//
	// 恢复时算「每条有多旧」必须以它为基准，**不能用 now**：
	// 文件可能放在那儿好几天没人动过，用 now 算会把所有记录全判成过期，
	// 那这个字段就白存了（落地即废弃）。
	RecentAt time.Time `json:"recent_at,omitempty"`
}

// lineSnapshot 是 recent 落盘的专用形态，**不复用 memory.Line**。
//
// 两个原因：
//  1. Line 没有任何 json tag，直接序列化会写出 "TS"/"OpenID" 这种大写驼峰，
//     跟本文件其它段（facts_at / total_lines / left_at）的风格不一致。
//  2. Line.MsgID 是只写不读的死字段：被动回复的锚点池在 agent.go 里由**活事件**
//     灌进内存、TTL 5 分钟，重启后必然过期。复用它等于把死数据写进长期文件，
//     还会误导下一个读文件的人以为它有用。
type lineSnapshot struct {
	TS      time.Time `json:"ts"`
	Role    string    `json:"role"`
	Name    string    `json:"name,omitempty"`
	OpenID  string    `json:"openid,omitempty"`
	Content string    `json:"c"`
}

type snapshot struct {
	Groups []groupSnapshot `json:"groups"`
}

var fileMu sync.Mutex

// ErrSnapshotShrunk 表示本次快照的群数远少于磁盘上的现有文件，已拒绝覆盖。
// 这是「恢复失败后用空 Store 盖掉原文件」的最后一道防线：
// 那条路径一次触发就会把全部长期要点永久删掉，且不留 .bak。
var ErrSnapshotShrunk = errors.New("拒绝覆盖：本次快照群数远少于现有 memory.json")

// SaveTo 把长期记忆原子写入指定文件。
//
// 写入分三步，每一步都在为「断电 / 崩溃 / 半截文件」兜底：
// 反向保护（宁可漏判不可误杀地识别空覆盖）→ 写临时文件并 fsync → 原子 rename → fsync 父目录。
func (s *Store) SaveTo(path string) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	s.mu.RLock()
	groups := make([]groupSnapshot, 0, len(s.m))
	for _, g := range s.m {
		g.mu.Lock()
		gs := groupSnapshot{
			OpenID:  g.OpenID,
			Name:    g.Name,
			Facts:   cloneMap(g.facts),
			FactsAt: cloneTimeMap(g.factAt),
			Members: cloneMembers(g.members),
			Left:    g.left,
			LeftAt:  g.leftAt,
			Summary: clampSummary(g.summary),
			// totalLines/summaryUntil 在 LoadFrom 里由 summary 反推，
			// 这里原样带上，保证「存进去再读出来」是等值的。
			TotalLines:   g.totalLines,
			SummaryUntil: g.summaryUntil,
			Recent:       snapshotRecent(g.recent),
			RecentAt:     time.Now(),
		}
		g.mu.Unlock()
		groups = append(groups, gs)
	}
	s.mu.RUnlock()

	// 反向保护：磁盘上现有文件比本次快照「显著更多群」时直接拒绝覆盖。
	// 这正是 LoadFrom 解析失败 → 调用方只 Warn 就继续 → 5 分钟后的
	// flushLoop 用空 store 覆写原文件的形状。
	if err := guardAgainstEmptyOverwrite(path, len(groups)); err != nil {
		return err
	}

	b, err := json.MarshalIndent(snapshot{Groups: groups}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 父目录也要 fsync：rename 只是改了目录项，目录项本身落盘之前断电，
	// 依旧可能退回旧版本甚至空文件。Windows 上对目录 fsync 会直接失败，
	// 用 GOOS 判断跳过——不能因为它让 Windows 编译不过。
	if runtime.GOOS != "windows" {
		syncDir(filepath.Dir(path))
	}
	return nil
}

// guardAgainstEmptyOverwrite 读现有文件，若它能解析且群数显著大于本次快照就拒绝覆盖。
//
// 判定刻意保守：只有「多 1 个以上」且「本次不足它的一半」才拦。
// 少删一个群不该被拦（管理端是逐个删群的），而真正的空覆盖
// （本次 0 群或 1 群，磁盘上 6 群）必然落进这个区间。
// 代价是漏判：磁盘 2 群、本次 1 群的覆盖会照常发生。
func guardAgainstEmptyOverwrite(path string, n int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在或读不出来：没有可保护的既有数据，正常写。
		return nil
	}
	var cur snapshot
	if err := json.Unmarshal(b, &cur); err != nil {
		// 现有文件本身已经坏了：不认识它就无法比较，放行
		// （调用方应先用 Quarantine 把它挪开）。
		return nil
	}
	curN := 0
	for _, g := range cur.Groups {
		if g.OpenID != "" {
			curN++
		}
	}
	if curN >= n+1 && n*2 <= curN {
		logx.Error("长期记忆疑似空覆盖，已拒绝写入",
			"path", path, "磁盘群数", curN, "本次快照群数", n)
		return fmt.Errorf("%w：磁盘 %d 群 vs 快照 %d 群", ErrSnapshotShrunk, curN, n)
	}
	return nil
}

// writeFileSync 写文件并 fsync，保证内容真正落到介质上再返回。
// 断电后 memory.json 长度 0 或半截 JSON 会直接引爆「恢复失败 → 空覆盖」链路。
func writeFileSync(path string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// syncDir fsync 父目录；调用方需自行判断 GOOS（Windows 上目录不能 fsync）。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return // 目录打不开时无法 fsync，记日志但不阻断主流程
	}
	if err := f.Sync(); err != nil {
		logx.Warn("长期记忆目录 fsync 失败", "dir", dir, "err", err.Error())
	}
	_ = f.Close()
}

// Quarantine 把无法解析的记忆文件改名隔离，返回隔离后的新路径（未改动则返回空串）。
//
// 存在的意义：LoadFrom 解析失败时调用方只 Warn 就继续运行，
// 内存里是个空 Store，而空 Store 会被下一次 flush 写回原路径——
// 长期要点就此永久消失。改名隔离后原路径空出来，新数据写新文件，
// 坏文件留在旁边等人来捞。
// 注意：调用方需要在 LoadFrom 失败时显式调用它，本包不擅自搬走用户的文件。
func Quarantine(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	q := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
	if err := os.Rename(path, q); err != nil {
		return "", err
	}
	logx.Error("长期记忆文件无法解析，已隔离保存", "原路径", path, "隔离路径", q)
	return q, nil
}

// LoadFrom 恢复长期记忆；文件不存在不算错误（首次启动本就没有）
func (s *Store) LoadFrom(path string) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}
	restored := 0
	withSummary := 0
	restoredRecent := 0
	s.mu.Lock()
	for _, gs := range snap.Groups {
		if gs.OpenID == "" {
			continue
		}
		g := NewGroup(gs.OpenID, gs.Name)
		if gs.Facts != nil {
			g.facts = gs.Facts
		}
		// 旧文件没有 facts_at：补零值即可（零值天然最先被淘汰），
		// 也让「有 facts 却没有时间戳」的 key 不会在保存时丢掉时间字段。
		g.factAt = map[string]time.Time{}
		for k := range g.facts {
			if at, ok := gs.FactsAt[k]; ok {
				g.factAt[k] = at
			}
		}
		if gs.Members != nil {
			g.members = gs.Members
		}
		g.left = gs.Left
		g.leftAt = gs.LeftAt
		g.summary = gs.Summary
		g.totalLines = gs.TotalLines
		if gs.Summary != "" {
			// 摘要已经覆盖了「前 totalLines 条」，把游标一并推到末尾。
			// 不这么做的话，重启后 PendingSummary() 会把同一段已压缩过的历史
			// 重新算作待摘要，白白再调一次模型、还可能把摘要写歪。
			g.summaryUntil = gs.TotalLines
			withSummary++
		}
		restoredRecent += restoreRecent(g, gs)
		s.m[gs.OpenID] = g
		restored++
	}
	s.mu.Unlock()
	if restored > 0 {
		logx.Info("长期记忆已恢复", "path", path, "群数", restored,
			"含摘要群数", withSummary, "恢复近期消息条数", restoredRecent)
	}
	return nil
}

// restoreRecent 恢复近期会话窗口，顺手把 totalLines 与窗口长度对齐。
//
// 返回实际恢复的条数。
//
// **年龄基准是 RecentAt 而不是 now**：文件可能放着不动好几天，
// 用 now 当基准会把所有记录判成过期，那 recent_at 字段就白存了。
// 缺失 recent_at 的旧文件（第一次上线前的）退化到 now，
// 即「全丢」——那等价于本次改动之前的行为，安全。
//
// **totalLines 的对齐是必须的**：它是全局单调计数器，而 recent 只是它尾部的
// 窗口。正常情况下 total_lines 必然 >= len(recent)，但文件被手工编辑过、
// 或从更早格式迁上来时可能不是。此时 PendingSummary() 会返回负数，
// 摘要游标往回跑，把**已经摘要过的历史再摘要一遍**——每次都白烧一次模型调用，
// 还可能把摘要写歪。宁可让摘要晚触发，也不能让游标倒着走。
func restoreRecent(g *Group, gs groupSnapshot) int {
	if len(gs.Recent) == 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxRecentAge)
	if !gs.RecentAt.IsZero() {
		cutoff = gs.RecentAt.Add(-maxRecentAge)
	}
	kept := 0
	for _, s := range gs.Recent {
		if s.TS.Before(cutoff) {
			continue
		}
		// 注意不恢复 MsgID：它是只写不读的死字段，锚点池在内存里且 5 分钟 TTL，
		// 重启后必然过期，落回来只会误导人以为还能挂被动回复。
		g.recent = append(g.recent, Line{
			TS:      s.TS,
			Role:    s.Role,
			Name:    s.Name,
			OpenID:  s.OpenID,
			Content: s.Content,
		})
		kept++
	}
	if n := len(g.recent); g.totalLines < n {
		g.totalLines = n
	}
	if g.summaryUntil > g.totalLines {
		g.summaryUntil = g.totalLines
	}
	return kept
}

// snapshotRecent 把内存窗口压成落盘形态：只留尾部若干条，每条截断到上限。
//
// 复制而不是取引用：recent 会被 Append 改，而序列化发生在解锁之后，
// 直接把切片塞进快照会跟 Append 抢同一块内存。
func snapshotRecent(lines []Line) []lineSnapshot {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > maxPersistRecent {
		lines = lines[len(lines)-maxPersistRecent:]
	}
	out := make([]lineSnapshot, 0, len(lines))
	for _, l := range lines {
		out = append(out, lineSnapshot{
			TS:      l.TS,
			Role:    l.Role,
			Name:    l.Name,
			OpenID:  l.OpenID,
			Content: truncateRunes(l.Content, maxLineRunes),
		})
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// clampSummary 裁剪超长摘要：按 rune 截断并留标记。
func clampSummary(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxSummaryRunes {
		return s
	}
	return string(r[:maxSummaryRunes]) + summaryTruncatedMark
}

func cloneMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneTimeMap(m map[string]time.Time) map[string]time.Time {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneMembers(m map[string]*Member) map[string]*Member {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*Member, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		cp := *v
		out[k] = &cp
	}
	return out
}
