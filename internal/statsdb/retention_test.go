package statsdb

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenCreatesParentDir 首次部署时数据目录还不存在。
// 事故场景：SQLite 不会自建父目录，sql.Open 不报错但 Exec(schema) 直接失败，
// 而仓里唯一的 MkdirAll 在 memory.SaveTo 里，要等 5 分钟后的首次 flush——
// 远晚于 Open。统计功能就此永久降级。
func TestOpenCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "nested", "stats.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open 未创建父目录: %v", err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.HourlyToday(); err != nil {
		t.Fatalf("建库后查询失败: %v", err)
	}
}

// TestCallsTSIndexExists calls.ts 必须有索引：MinutelyN 与 deleteExpired
// 都按 ts 范围过滤，而现有两个索引以 day 打头，帮不上忙。
// 没有它时这两条查询只能全表扫描，而 SetMaxOpenConns(1) 下
// 管理端每刷一次图表就独占唯一连接扫一遍，写协程全程排队。
func TestCallsTSIndexExists(t *testing.T) {
	d := openTemp(t)
	var name string
	err := d.sql.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='calls' AND name='idx_calls_ts'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("idx_calls_ts 缺失: %v", err)
	}
	if name != "idx_calls_ts" {
		t.Errorf("索引名不对: %q", name)
	}
}

// TestDeleteExpiredUsesTsIndex deleteExpired 的执行计划必须走索引而不是全表扫描
func TestDeleteExpiredUsesTsIndex(t *testing.T) {
	d := openTemp(t)
	now := time.Now()
	for i := 0; i < 50; i++ {
		d.RecordCall(Call{TS: now, EndpointID: "ep", Model: "m", OK: true})
	}
	// 等落库
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := d.HourlyToday()
		if b[now.Hour()].Total >= 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待落库超时")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var plan string
	err := d.sql.QueryRow(
		`EXPLAIN QUERY PLAN DELETE FROM calls WHERE ts < ?`, now.Unix(),
	).Scan(&plan)
	if err == nil && !strings.Contains(plan, "idx_calls_ts") {
		t.Errorf("DELETE 未走 ts 索引，执行计划: %s", plan)
	}

	var qplan string
	if err := d.sql.QueryRow(
		`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM calls WHERE ts >= ?`, now.Unix(),
	).Scan(&qplan); err == nil && !strings.Contains(qplan, "idx_calls_ts") {
		t.Errorf("按 ts 过滤的查询未走索引，执行计划: %s", qplan)
	}
}

// TestRetentionAdvancesOnFailure DELETE 失败时也必须推进 lastRetention，
// 否则 time.Since(零值) 恒 > 24h，写协程每 200ms 就重跑一次全表 DELETE，
// 顺带每秒 5 条 Warn 把真正的故障信息淹掉。
func TestRetentionAdvancesOnFailure(t *testing.T) {
	d := openTemp(t)

	// 关掉底层连接制造持续失败
	if err := d.sql.Close(); err != nil {
		t.Fatal(err)
	}
	// 把 lastRetention 拨回过去，复现「清理长期没成功过」的现场
	d.forceRetention(time.Now().Add(-48 * time.Hour))
	first := d.lastRetentionSnap()
	d.deleteExpired() // 失败路径
	second := d.lastRetentionSnap()

	if second.IsZero() {
		t.Fatal("deleteExpired 失败后 lastRetention 仍为零值 —— 会每 200ms 重跑一次")
	}
	if !second.After(first) {
		t.Errorf("失败后 lastRetention 未推进到未来: %v -> %v", first, second)
	}
	// 立刻再判定：不该再到期，否则退避等于没做
	if d.retentionDue(time.Now()) {
		t.Error("失败后仍判定为到期，退避未生效")
	}
	// 连续失败计数递增（指数退避的输入）
	if got := d.retentionFailsSnap(); got != 1 {
		t.Errorf("retentionFails = %d, want 1", got)
	}

	// 再次到期后再失败一次：退避间隔应变长
	d.forceRetention(time.Now().Add(-time.Hour))
	d.deleteExpired()
	if got := d.retentionFailsSnap(); got != 2 {
		t.Errorf("再次失败后 retentionFails = %d, want 2", got)
	}
	if gap := d.lastRetentionSnap().Sub(time.Now()); gap <= retentionBackoffBase {
		t.Errorf("第二次失败的退避应长于基准 %v, got %v", retentionBackoffBase, gap)
	}
}

// TestRetentionBackoffBounded 退避必须单调增长且封顶，不能无限指数膨胀
func TestRetentionBackoffBounded(t *testing.T) {
	prev := time.Duration(0)
	for fails := 1; fails <= 20; fails++ {
		got := retentionBackoff(fails)
		if got < retentionBackoffBase {
			t.Errorf("fails=%d 退避过短: %v", fails, got)
		}
		if got > retentionBackoffMax {
			t.Errorf("fails=%d 退避未被封顶: %v", fails, got)
		}
		if got < prev {
			t.Errorf("fails=%d 退避反而变短: %v < %v", fails, got, prev)
		}
		prev = got
	}
	// 极端输入（人为把失败次数拉满）也不能溢出成负数
	if got := retentionBackoff(1 << 20); got < 0 {
		t.Errorf("极端失败次数导致溢出: %v", got)
	}
}

// TestRetentionResetsAfterSuccess 成功一次后应回到 24h 周期、失败计数清零
func TestRetentionResetsAfterSuccess(t *testing.T) {
	d := openTemp(t)
	d.setRetentionFails(5)
	d.deleteExpired()
	if got := d.retentionFailsSnap(); got != 0 {
		t.Errorf("成功后 retentionFails 应清零, got %d", got)
	}
	if d.retentionDue(time.Now()) {
		t.Error("成功后不应立即再到期")
	}
	// 把 lastRetention 拨到 25 小时前，判定应到期
	d.forceRetention(time.Now().Add(-25 * time.Hour))
	if !d.retentionDue(time.Now()) {
		t.Error("超过 24h 后应判定为到期")
	}
}
