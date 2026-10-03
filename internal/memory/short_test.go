package memory

// ⚠️ MaxFacts 现在是**包级可变状态**（2026-10-03 从 const 改成 var 以便配置）。
// 任何改它的测试都必须用 t.Cleanup 改回去，否则会污染同包的其他用例。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshNameOverwritesStaleCodeName(t *testing.T) {
	s := New(10)
	// 模拟历史固化：群名被写成了 openid 尾 8 位代号
	s.Group("G1", "00000000")
	if got := s.Group("G1", "").Name; got != "00000000" {
		t.Fatalf("前置条件不对: %q", got)
	}
	s.RefreshName("G1", "测试群三号")
	if got := s.Group("G1", "").Name; got != "测试群三号" {
		t.Errorf("RefreshName 必须覆盖固化的代号, got %q", got)
	}
	// 空 openid 不 panic、不建群
	s.RefreshName("", "x")
	if len(s.All()) != 1 {
		t.Errorf("空 openid 不应建群: %d", len(s.All()))
	}
}

func TestLeftRoundTrip(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.SetLeft(true)
	left, at := g.Left()
	if !left || at.IsZero() {
		t.Fatal("SetLeft(true) 未生效")
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	left2, _ := s2.Group("G1", "").Left()
	if !left2 {
		t.Error("left 标记未随持久化保留")
	}
	// 清除后为零值
	s2.Group("G1", "").SetLeft(false)
	left3, at3 := s2.Group("G1", "").Left()
	if left3 || !at3.IsZero() {
		t.Error("SetLeft(false) 未清除标记")
	}
}

func TestRemoveGroup(t *testing.T) {
	s := New(10)
	s.Group("G1", "群一").SetFact("k", "v")
	s.Group("G2", "群二")
	s.RemoveGroup("G1")
	if len(s.All()) != 1 {
		t.Fatalf("移除后群数不对: %d", len(s.All()))
	}
	// 移除后再 Group() 是全新上下文，不残留 facts
	if got := s.Group("G1", "").Facts(); got != "" {
		t.Errorf("移除后旧数据残留: %q", got)
	}
}

// TestFactLRUEvictsOldest 满容量后写新要点，应淘汰「最后写入时间最早」的那条
func TestFactLRUEvictsOldest(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	base := time.Now().Add(-time.Hour)
	for i := 0; i < MaxFacts; i++ {
		k := fmt.Sprintf("k%02d", i)
		if ev := g.SetFact(k, "v"); ev != "" {
			t.Fatalf("未满时不该淘汰, 却淘汰了 %q", ev)
		}
		g.factAt[k] = base.Add(time.Duration(i) * time.Minute) // 造出确定的先后顺序
	}

	// 更新一条已有 key：不该触发淘汰
	if ev := g.SetFact("k00", "v2"); ev != "" {
		t.Fatalf("更新已有 key 不该淘汰, got %q", ev)
	}

	// 再插新 key：此时的「最旧」是 k01（k00 刚被刷新过）
	ev := g.SetFact("kNEW", "v")
	if ev != "k01" {
		t.Fatalf("应淘汰 k01, got %q", ev)
	}
	list := g.FactsList()
	if len(list) != MaxFacts {
		t.Fatalf("总数应保持 %d, got %d", MaxFacts, len(list))
	}
	hasNew, hasOld := false, false
	for _, it := range list {
		switch it.Key {
		case "kNEW":
			hasNew = true
		case "k01":
			hasOld = true
		}
	}
	if !hasNew {
		t.Error("新要点没写进去")
	}
	if hasOld {
		t.Error("k01 应已被淘汰")
	}
	if !strings.Contains(g.Facts(), "kNEW：v") {
		t.Errorf("新要点内容没进 Facts() 文本: %q", g.Facts())
	}
}

// TestFactsZeroTimestampEvictedFirst 旧文件升级上来的要点（零值时间）应最先被淘汰
func TestFactsZeroTimestampEvictedFirst(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.SetFact("老要点", "老内容")
	// SetFact 一定会写时间戳，所以显式清成零值来模拟「从旧 memory.json 加载上来」的要点
	g.factAt["老要点"] = time.Time{}
	for i := 0; i < MaxFacts-1; i++ {
		g.SetFact(fmt.Sprintf("n%02d", i), "v")
	}
	if ev := g.SetFact("新要点", "新内容"); ev != "老要点" {
		t.Errorf("零值时间的老要点应最先被淘汰, got %q", ev)
	}
}

// TestDelFactAndListOrder 删除要点、以及列表按最后更新倒序
func TestDelFactAndListOrder(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.SetFact("a", "1")
	g.SetFact("b", "2")
	g.SetFact("c", "3")
	base := time.Now()
	g.factAt["a"] = base.Add(-3 * time.Minute)
	g.factAt["b"] = base.Add(-2 * time.Minute)
	g.factAt["c"] = base.Add(-1 * time.Minute)

	list := g.FactsList()
	if len(list) != 3 {
		t.Fatalf("want 3, got %d", len(list))
	}
	if list[0].Key != "c" || list[1].Key != "b" || list[2].Key != "a" {
		t.Errorf("应按最后更新倒序: %+v", list)
	}

	g.DelFact("b")
	if got := g.FactsList(); len(got) != 2 {
		t.Fatalf("删除后应剩 2, got %d", len(got))
	}
	if strings.Contains(g.Facts(), "b：2") {
		t.Error("删除后 Facts() 文本里仍含被删的 b")
	}
	g.DelFact("不存在的") // 不该 panic
}

// TestFactsPersistRoundTrip 要点与其时间戳都要跨保存/加载保留
func TestFactsPersistRoundTrip(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.SetFact("群主", "小明")
	at := time.Now().Truncate(time.Second)
	g.factAt["群主"] = at

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	list := s2.Group("G1", "").FactsList()
	if len(list) != 1 || list[0].Key != "群主" || list[0].Value != "小明" {
		t.Fatalf("要点未正确恢复: %+v", list)
	}
	if !list[0].UpdatedAt.Equal(at) {
		t.Errorf("时间戳未恢复: got %v want %v", list[0].UpdatedAt, at)
	}
}

// TestLoadLegacyFactsWithoutTimestamps 旧 memory.json 只有 facts、没有 facts_at，
// 必须能加载、时间戳为零值，且回写后 facts 仍是 {"k":"v"}（不破坏 export_chatlog.py）
func TestLoadLegacyFactsWithoutTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	legacy := `{"groups":[{"openid":"G1","name":"群一","facts":{"旧要点":"旧内容"}}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(10)
	if err := s.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	list := s.Group("G1", "").FactsList()
	if len(list) != 1 || list[0].Value != "旧内容" {
		t.Fatalf("旧格式要点未恢复: %+v", list)
	}
	if !list[0].UpdatedAt.IsZero() {
		t.Errorf("旧格式应得到零值时间, got %v", list[0].UpdatedAt)
	}

	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	facts, ok := raw.Groups[0]["facts"].(map[string]any)
	if !ok {
		t.Fatalf("facts 必须仍是对象 {\"k\":\"v\"}, got %T", raw.Groups[0]["facts"])
	}
	if facts["旧要点"] != "旧内容" {
		t.Errorf("facts 内容被破坏: %v", facts)
	}
}

// TestMaxFactsIsSettable 上限可配（2026-10-03 从 const 改成 var）。
func TestMaxFactsIsSettable(t *testing.T) {
	old := MaxFacts
	t.Cleanup(func() { MaxFacts = old })
	MaxFacts = 3
	g := NewGroup("g", "群")
	for i := 0; i < 3; i++ {
		if ev := g.SetFact(string(rune('a'+i)), "v"); ev != "" {
			t.Fatalf("第 %d 条不该淘汰（上限 3）: %s", i+1, ev)
		}
	}
	if ev := g.SetFact("d", "v"); ev == "" {
		t.Fatal("第 4 条应淘汰最旧的一条（上限已设为 3）")
	}
	if got := len(g.FactsList()); got != 3 {
		t.Fatalf("总数应保持 3，got %d", got)
	}
}

// TestClearRequiresBackupBeforeSaveTo 守住「清记忆」的落盘顺序。
//
// 这条不是洁癖：guardAgainstEmptyOverwrite 存在的目的正是拦住空覆盖，
// 于是 Clear() 之后直接 SaveTo 会被拒（磁盘 1 群 vs 快照 0 群落进它的判定区间）。
// 后果是按钮报成功、内存清了、文件没动，进程一重启记忆原样回来。
//
// admin.handleMemoryClear 因此必须「备份 → 移走原文件 → Clear → SaveTo」。
// 这个测试把那两步钉死：只 Clear 不挪文件会被拒，挪走之后就能写成空快照。
func TestClearRequiresBackupBeforeSaveTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	s := New(4)
	s.Group("g1", "群一").SetFact("k", "v")
	s.Group("g2", "群二").SetFact("k", "v")
	if err := s.SaveTo(path); err != nil {
		t.Fatalf("首次落盘失败：%v", err)
	}

	s.Clear()
	if n := len(s.All()); n != 0 {
		t.Fatalf("Clear 之后还剩 %d 个群", n)
	}

	// 不挪文件直接写：必须被守卫拒绝。
	if err := s.SaveTo(path); err == nil {
		t.Fatal("空快照直接覆盖原文件居然成功了——守卫失效，清记忆按钮会变成假成功")
	} else if !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("期望守卫的拒绝覆盖错误，实际：%v", err)
	}

	// 挪走原文件（备份）之后就能正常写空快照。
	backup := path + ".bak"
	if err := os.Rename(path, backup); err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	if err := s.SaveTo(path); err != nil {
		t.Fatalf("移走原文件后仍写不进空快照：%v", err)
	}

	// 备份里必须还留着原来的两个群，否则这个「备份」是假的。
	var back snapshot
	b, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("读备份失败：%v", err)
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("备份不是合法 JSON：%v", err)
	}
	if len(back.Groups) != 2 {
		t.Fatalf("备份里群数 = %d，期望 2", len(back.Groups))
	}

	// 新文件必须是能读回来的空快照（不是 0 字节，0 字节会在下次 LoadFrom 触发隔离）。
	var cur snapshot
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读新文件失败：%v", err)
	}
	if err := json.Unmarshal(b2, &cur); err != nil {
		t.Fatalf("空快照不是合法 JSON：%v", err)
	}
	if len(cur.Groups) != 0 {
		t.Fatalf("新快照里还有 %d 个群", len(cur.Groups))
	}
}
