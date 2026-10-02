package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 近期会话窗口落盘。
//
// 这次改动推翻了 short.go 和 persist.go 里原有的判断：「短期窗口故意不落盘，
// 重启后它本来就该忘记刚才聊到哪」。理由是真人记得昨天聊过什么，
// 而滚动摘要要攒够 24 条才跑一次，救不了发版就失忆的空档。
//
// 但「不该接没人记得的茬」那个顾虑本身是对的，所以有三重上限。
// 这组测试把它们逐条钉死——它们是「记得昨天」与「别翻旧账」的边界。

func newTestStore() *Store { return New(30) }

// TestRecentSurvivesRoundTrip 近期窗口必须真的落盘并恢复。
func TestRecentSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	now := time.Now()
	g1.Append(Line{TS: now.Add(-2 * time.Minute), Role: RoleUser, Name: "张三", OpenID: "O1", Content: "在吗"}, 30)
	g1.Append(Line{TS: now.Add(-time.Minute), Role: RoleUser, Name: "李四", OpenID: "O2", Content: "刚聊到哪了"}, 30)
	if err := s1.SaveTo(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	got := s2.Group("g1", "").Recent(30)
	if len(got) != 2 {
		t.Fatalf("恢复后有 %d 条，期望 2 条：这次改动的全部意义就是别再失忆", len(got))
	}
	if got[0].Content != "在吗" || got[1].Content != "刚聊到哪了" {
		t.Errorf("内容不对: %q / %q", got[0].Content, got[1].Content)
	}
	// 身份必须一起落盘：只有名字的话，改过名的群友会被认成另一个人，
	// 而认错人比失忆更糟（见 short.go 里 OpenID 字段的注释）
	if got[0].OpenID != "O1" || got[1].Name != "李四" {
		t.Errorf("身份没落盘: openid=%q name=%q", got[0].OpenID, got[1].Name)
	}
}

// TestRecentDropsExpiredOnLoad 超过 6 小时的记录恢复时必须丢掉。
//
// 这是「真人记得昨天」和「不该接没人记得的茬」之间的分界线。
func TestRecentDropsExpiredOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	old := time.Now().Add(-8 * time.Hour)
	g1.Append(Line{TS: old, Role: RoleUser, Name: "张三", Content: "八小时前说的话"}, 30)
	g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "刚说的话"}, 30)
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	got := s2.Group("g1", "").Recent(30)
	if len(got) != 1 {
		t.Fatalf("恢复后 %d 条，期望 1 条（8 小时前那条应被丢掉）", len(got))
	}
	if got[0].Content != "刚说的话" {
		t.Errorf("留错了那条: %q", got[0].Content)
	}
}

// TestRecentAgeUsesSnapshotTimeNotNow 年龄基准必须是 recent_at，不能是 now。
//
// 场景很具体：部署 → 机器人跑了一天 → 机器重启 → LoadFrom。
//
// **构造必须落在两种基准给出不同结论的区间里**，否则测试是废的。
// 第一版把记录设成「1 小时前」，那种情况下 now-6h 和 recent_at-6h 都会保留它，
// 用哪个基准结果一样——变异后测试照样全绿（这个假绿真的踩了一次）。
//
// 正确的构造：记录写在 9 小时前，recent_at 也写在 9 小时前，
// 但「现在」是 3 小时前。
//   - 以 recent_at 为准：age = 0 小时 → 保留
//   - 以 now 为基准：     age = 3 小时 → 也保留，还是区分不开
//
// 再往前推：记录写于 T-9h，recent_at 写于 T-9h，而现在假设是 T-0h，
// maxRecentAge = 6h → 两者都过期，仍然区分不开。
//
// 唯一能分开的是 **recent_at 与记录时间分处 6 小时的两侧**：
//   - recent_at = T-1h（文件一小时前落盘）
//   - 记录时间  = T-7h（这条消息在落盘前 6 小时就已写出）
//   以 recent_at 为准：age = 6h ≤ 6h → 保留
//   以 now 为基准：    age = 7h > 6h → 丢弃
func TestRecentAgeUsesSnapshotTimeNotNow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "七小时前说的话"}, 30)
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	// 把 recent_at 改到 1 小时前：文件一小时前落盘，
	// 而那条消息在落盘前 6 小时就写出来了，age 正好卡在阈值上。
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	g0 := snap["groups"].([]any)[0].(map[string]any)
	recents := g0["recent"].([]any)
	// 记录时间：7 小时前（明确早于阈值）
	recents[0].(map[string]any)["ts"] = time.Now().Add(-7 * time.Hour).Format(time.RFC3339Nano)
	// 落盘时刻：1 小时前（age = 6h，正好在阈值内）
	g0["recent_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	patched, _ := json.Marshal(snap)
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if got := s2.Group("g1", "").Recent(30); len(got) != 1 {
		t.Errorf("恢复后 %d 条，期望 1 条。年龄基准用了 now 而不是 recent_at："+
			"以落盘时刻算 age=6h 刚好保留，以当前时刻算 age=7h 就被丢掉。"+
			"现实场景就是「消息是早上写的、下午部署、晚上重启」", len(got))
	}
}

// TestMsgIDNotPersisted msg_id 既不能落盘，也不能在恢复后回到内存。
//
// 它是只写不读的死字段，而且被动回复的锚点池在内存里、TTL 5 分钟，
// 重启后必然过期。写进去只会误导下一个读文件的人以为还能挂回复。
//
// **两处都要查**：只查落盘 JSON 会漏掉「恢复时补上 MsgID」那种改法——
// 那个字段写进内存后再落一次就出现了。第一版就只查了文件，
// 于是「在 restoreRecent 里补 MsgID」的变异完全逃逸（假绿）。
func TestMsgIDNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "带 msgid 的消息", MsgID: "MID-123"}, 30)
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	// 1) 落盘文件里不能有
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "MID-123") || strings.Contains(string(b), "msg_id") ||
		strings.Contains(string(b), "MsgID") {
		t.Errorf("msg_id 落盘了：它是只写不读的死字段，锚点池重启后必然过期，"+
			"写进去只会误导人。got: %s", string(b))
	}

	// 2) 恢复后的内存对象里也不能有
	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	for i, l := range s2.Group("g1", "").Recent(30) {
		if l.MsgID != "" {
			t.Errorf("第 %d 条恢复后 MsgID=%q：锚点池在内存里、TTL 5 分钟，"+
				"重启后必然过期，恢复它只会让人误以为还能挂被动回复", i, l.MsgID)
		}
	}
}

// TestPendingSummaryNeverNegative 恢复后 PendingSummary 必须 >= 0。
//
// total_lines 小于 recent 条数时，PendingSummary 会变负，摘要游标往回跑，
// 把**已经摘要过的历史再摘要一遍**——每次都白烧一次模型调用，还可能把摘要写歪。
//
// **构造必须真的造出负数场景。** 前两版都栽在这儿：
//  1. 把 summary_until 一起改小 → 两者抵消，pending 恒为 0；
//  2. 只改 total_lines → summary_until 被 omitempty 省略，
//     LoadFrom 按「有 summary 就把游标推到 total_lines」重算成 1，仍是 0。
//
// 现在这样造：文件里 summary 为空（LoadFrom 不会推游标，summaryUntil 留 0），
// 而 total_lines=1、recent 有 5 条 → 不对齐时 pending = 1-0 = ... 仍不为负。
// 所以真正的负数条件是 summaryUntil > totalLines，那是另一条钳位在防。
//
// 换个角度测**不变量本身**：不管文件被改成什么样，恢复后
// totalLines >= summaryUntil 且 totalLines >= len(recent)。
// 这两条正是 restoreRecent 里两次钳位要维持的东西，
// 且它们在坏数据下**确实会违反**——这才是能抓住变异的判据。
func TestPendingSummaryNeverNegative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	for i := 0; i < 5; i++ {
		g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "消息"}, 30)
	}
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	// 打坏：total_lines 改成 1，而 recent 有 5 条（文件被手工改坏的情形）
	b, _ := os.ReadFile(path)
	patched := strings.Replace(string(b), `"total_lines": 5`, `"total_lines": 1`, 1)
	if patched == string(b) {
		t.Fatalf("没能打坏 total_lines，文件内容：%s", string(b))
	}
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	g := s2.Group("g1", "")
	if p := g.PendingSummary(); p < 0 {
		t.Errorf("PendingSummary = %d，摘要游标会往回跑，"+
			"把已摘要过的历史重新摘要一遍（白烧调用，还可能写歪摘要）", p)
	}
	if g.totalLines < len(g.recent) {
		t.Errorf("totalLines=%d 小于恢复出的 %d 条：全局计数器被窗口长度带偏，"+
			"摘要游标会算错", g.totalLines, len(g.recent))
	}
}

// TestRecentCapDoesNotShrinkTotalLines recent 被截到 12 条时 total_lines 必须不变。
//
// total_lines 是全局单调计数器，recent 只是它尾部的窗口，两者是独立的。
// 混在一起改就会把摘要游标算错——2026 年那次「摘要算了 33 次却全是空的」
// 就是这块坏了的征兆。
func TestRecentCapDoesNotShrinkTotalLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	const n = 40
	for i := 0; i < n; i++ {
		g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "消息"}, 30)
	}
	wantTotal := g1.totalLines
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	g2 := s2.Group("g1", "")
	if got := len(g2.Recent(30)); got != maxPersistRecent {
		t.Errorf("恢复 %d 条，期望 %d（maxPersistRecent）", got, maxPersistRecent)
	}
	if g2.totalLines < wantTotal {
		t.Errorf("totalLines 被 recent 的长度带偏了：%d < %d。"+
			"它是全局计数器，跟窗口长度无关", g2.totalLines, wantTotal)
	}
}

// TestLongLineTruncated 单条超长必须截断，防止撑爆 memory.json。
func TestLongLineTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s1 := newTestStore()
	g1 := s1.Group("g1", "测试群")
	huge := strings.Repeat("很长的群聊内容", 500) // 3500 字
	g1.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: huge}, 30)
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	s2 := newTestStore()
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	got := s2.Group("g1", "").Recent(30)
	if len(got) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(got))
	}
	if n := len([]rune(got[0].Content)); n > maxLineRunes+1 {
		t.Errorf("单条 %d 字没截断（上限 %d）：群友贴一整段网页原文就能把文件撑大", n, maxLineRunes)
	}
}

// TestGuardAgainstEmptyOverwriteStillWorks 加了 recent 之后空覆盖保护必须照旧生效。
//
// 回归防护：新字段不该削弱任何既有防线。
func TestGuardAgainstEmptyOverwriteStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	// 先造一个多群的内存文件
	s1 := newTestStore()
	for i := 0; i < 4; i++ {
		g := s1.Group(string(rune('a'+i)), "群")
		g.Append(Line{TS: time.Now(), Role: RoleUser, Name: "张三", Content: "消息"}, 30)
	}
	if err := s1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	// 用只含 1 群的新 Store 覆盖：应该被拒
	s2 := New(30)
	s2.Group("a", "群")
	if err := s2.SaveTo(path); err == nil {
		t.Error("4 群的内存快照覆盖 4 群的磁盘文件被放行了，" +
			"这是空覆盖保护的老路，必须继续拦")
	}
}