package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSummarySurvivesRoundTrip 滚动摘要必须真的落盘。
// 事故复现：摘要只存在于内存时，journal 里「已更新前文提要」出现 33 次，
// 而生产 memory.json 里 6 个群的 summary 全是空串——每次重启都白算。
func TestSummarySurvivesRoundTrip(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	for i := 0; i < 12; i++ {
		g.Append(Line{Role: RoleUser, Content: "聊点啥"}, 10)
	}
	g.SetSummary("前文提要：大家在讨论 A 项目的上线时间。")

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	// 落盘文件里必须能直接看到摘要（外部脚本/人工排障都要看它）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "前文提要：大家在讨论 A 项目的上线时间。") {
		t.Fatalf("memory.json 里没有 summary，原文:\n%s", raw)
	}

	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	g2 := s2.Group("G1", "")
	if got := g2.Summary(); got != "前文提要：大家在讨论 A 项目的上线时间。" {
		t.Errorf("重开后摘要丢失，got %q", got)
	}
	// 游标必须被推到末尾：已压缩过的历史不能再摘要一遍
	if n := g2.PendingSummary(); n != 0 {
		t.Errorf("重开后 PendingSummary 应为 0（否则会重复摘要），got %d", n)
	}
}

// TestSummaryCursorAfterReload 恢复后新追加的条目应当正常计入待摘要
func TestSummaryCursorAfterReload(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	for i := 0; i < 5; i++ {
		g.Append(Line{Role: RoleUser, Content: "x"}, 10)
	}
	g.SetSummary("早期摘要")

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	g2 := s2.Group("G1", "")
	g2.Append(Line{Role: RoleUser, Content: "新话题"}, 10)
	g2.Append(Line{Role: RoleUser, Content: "新话题2"}, 10)
	if n := g2.PendingSummary(); n != 2 {
		t.Errorf("新增 2 条后 PendingSummary 应为 2，got %d", n)
	}
}

// TestSummaryClampedOnSave 超长摘要必须被裁剪，避免 memory.json 无界膨胀
func TestSummaryClampedOnSave(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.SetSummary(strings.Repeat("啊", 5000)) // 远超 1200 上限

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	got := s2.Group("G1", "").Summary()
	r := []rune(got)
	if len(r) > maxSummaryRunes+len([]rune(summaryTruncatedMark)) {
		t.Fatalf("摘要未按上限裁剪: %d rune", len(r))
	}
	if len(r) <= maxSummaryRunes {
		t.Errorf("应裁到上限附近并留标记, got %d rune", len(r))
	}
	if !strings.HasSuffix(got, summaryTruncatedMark) {
		t.Errorf("裁剪后应有截断标记, tail=%q", string(r[max(0, len(r)-20):]))
	}
}

// TestSaveToRefusesEmptyOverwrite 反向保护的核心场景：
// LoadFrom 失败 → 调用方 Warn 后继续 → 空 store 的 flush 覆写原文件。
// 这里直接构造那个空 store，SaveTo 必须拒绝，且原文件一个字节都不能动。
func TestSaveToRefusesEmptyOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	// 先造一个有 6 个群的正常文件
	s := New(10)
	for i := 0; i < 6; i++ {
		g := s.Group(string(rune('A'+i)), "群")
		g.SetSummary("摘要")
		g.SetFact("k", "v")
	}
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟「恢复失败后继续运行」：内存里是个空 store
	empty := New(10)
	err = empty.SaveTo(path)
	if err == nil {
		t.Fatal("空 store 覆盖已有 memory.json 竟然成功了 —— 反向保护没生效")
	}
	if !errors.Is(err, ErrSnapshotShrunk) {
		t.Errorf("应返回 ErrSnapshotShrunk, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("被拒绝的写入仍动了原文件")
	}
	// 且不能留下 .tmp 残骸
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("拒绝写入后不该留下 .tmp")
	}
}

// TestSaveToAllowsNormalShrink 逐个删群是正常管理操作，不能被误杀
func TestSaveToAllowsNormalShrink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	s := New(10)
	for i := 0; i < 6; i++ {
		s.Group(string(rune('A'+i)), "群")
	}
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	// 删掉 1 个群（6 -> 5）应当正常写入
	s.RemoveGroup("A")
	if err := s.SaveTo(path); err != nil {
		t.Fatalf("正常删除一个群不该被拦: %v", err)
	}
	// 同样地 6 -> 3（删一半）也不拦：只在「本次不足磁盘一半」时才拦
	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	s2.RemoveGroup("B")
	s2.RemoveGroup("C")
	if err := s2.SaveTo(path); err != nil {
		t.Fatalf("5->3 不该被拦: %v", err)
	}
}

// TestLoadFromCorruptFails 坏文件必须让 LoadFrom 明确报错（不静默降级）
func TestLoadFromCorruptFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(`{"groups":[{"openid":"G1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(10)
	if err := s.LoadFrom(path); err == nil {
		t.Fatal("半截 JSON 竟然解析成功了")
	}
	if len(s.All()) != 0 {
		t.Error("解析失败时不该写入任何群")
	}
}

// TestQuarantineIsolatesBadFile Quarantine 必须把坏文件挪走并返回新路径，
// 这样调用方才能在被空 store 覆盖之前把现场保住。
func TestQuarantineIsolatesBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	bad := `{"groups":[{"openid":"G1"`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	q, err := Quarantine(path)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if q == "" || q == path {
		t.Fatalf("隔离路径不对: %q", q)
	}
	if !strings.Contains(q, "corrupt-") {
		t.Errorf("隔离文件名应带 corrupt- 标记: %q", q)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("原路径应已空出")
	}
	// 坏文件内容必须完好保留在隔离路径
	got, err := os.ReadFile(q)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bad {
		t.Error("隔离文件内容与原文件不一致")
	}
	// 原路径空出来后，空 store 的 SaveTo 不再触发保护（没有既有数据可保护）
	if err := New(10).SaveTo(path); err != nil {
		t.Errorf("隔离后应能正常写新文件: %v", err)
	}
}

// TestQuarantineMissingFile 文件不存在时返回错误，调用方好走失败分支
func TestQuarantineMissingFile(t *testing.T) {
	if _, err := Quarantine(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("文件不存在时应返回错误")
	}
}

// TestSaveToCreatesDir 首次部署时目录可能还不存在
func TestSaveToCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "memory.json")
	s := New(10)
	s.Group("G1", "群")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestNoTmpLeftBehind 正常保存后不该留下 .tmp
func TestNoTmpLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	s := New(10)
	s.Group("G1", "群")
	for i := 0; i < 3; i++ {
		if err := s.SaveTo(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("多次保存后残留了 .tmp")
	}
}

// TestLoadFromMissingFileOK 首次启动没有文件不算错
func TestLoadFromMissingFileOK(t *testing.T) {
	s := New(10)
	if err := s.LoadFrom(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Errorf("文件不存在应返回 nil, got %v", err)
	}
}

// TestLoadFromLegacyFileWithoutSummary 老文件（无 summary 字段）仍要能恢复
func TestLoadFromLegacyFileWithoutSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	legacy := `{"groups":[{"openid":"G1","name":"群一","facts":{"k":"v"}}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(10)
	if err := s.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	g := s.Group("G1", "")
	if got := g.Summary(); got != "" {
		t.Errorf("老文件应恢复出空摘要, got %q", got)
	}
	// 空摘要 -> summaryUntil 保持 0，但 totalLines 也是 0，PendingSummary 不为负
	if n := g.PendingSummary(); n < 0 {
		t.Errorf("PendingSummary 不应为负: %d", n)
	}
	// 老文件再存回去，facts 不能丢
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// 防止测试里的时间戳逻辑被误改：隔离文件名必须可排序
func TestQuarantineNameIsSortable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := Quarantine(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(q), "memory.json.corrupt-"), "")
	if _, err := time.Parse("20060102-150405", ts); err != nil {
		t.Errorf("隔离名里的时间戳应可解析: %q", ts)
	}
}
