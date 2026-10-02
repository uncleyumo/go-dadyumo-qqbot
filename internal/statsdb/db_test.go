package statsdb

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestRecordCallRoundTrip(t *testing.T) {
	d := openTemp(t)
	now := time.Now()
	d.RecordCall(Call{TS: now, EndpointID: "ep1", Model: "m-a", OK: true, LatencyMS: 800, TTFTMS: 300, PromptTokens: 100, OutputTokens: 50})
	d.RecordCall(Call{TS: now, EndpointID: "ep1", Model: "m-a", OK: false, Err: "boom"})
	d.RecordCall(Call{TS: now, EndpointID: "ep2", Model: "m-b", OK: true, LatencyMS: 400})

	// 队列是异步的：Close 会排空，这里直接关库再查就太迟，用另一个 DB 读同一文件也不行（单连接）。
	// 等写协程落库：轮询查询直到看到数据或超时。
	deadline := time.Now().Add(3 * time.Second)
	var buckets []Bucket
	for {
		var err error
		buckets, err = d.HourlyToday()
		if err != nil {
			t.Fatalf("HourlyToday: %v", err)
		}
		if buckets[now.Hour()].Total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待落库超时，当前桶数据: %+v", buckets[now.Hour()])
		}
		time.Sleep(50 * time.Millisecond)
	}
	b := buckets[now.Hour()]
	if got := b.Models["m-a"].Calls; got != 2 {
		t.Errorf("m-a calls = %d, want 2", got)
	}
	if got := b.Models["m-a"].Fails; got != 1 {
		t.Errorf("m-a fails = %d, want 1", got)
	}
	if got := b.Models["m-a"].PromptTokens; got != 100 {
		t.Errorf("m-a prompt_tokens = %d, want 100", got)
	}
	if got := b.Models["m-b"].Calls; got != 1 {
		t.Errorf("m-b calls = %d, want 1", got)
	}
	// 24 个桶必须全量返回
	if len(buckets) != 24 {
		t.Errorf("buckets len = %d, want 24", len(buckets))
	}
}

func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.RecordCall(Call{TS: time.Now(), EndpointID: "ep1", Model: "m-a", OK: true})
	d.RecordTarget(TargetState{Key: "ep1|m-a", Total: 7, Fails: 2, SuccessEWA: 0.8, TTFTMS: 500, LatencyMS: 900, LastError: "x"})
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = d2.Close() }()

	m, err := d2.LoadTargets()
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	s, ok := m["ep1|m-a"]
	if !ok {
		t.Fatalf("重开后目标状态丢失: %v", m)
	}
	if s.Total != 7 || s.Fails != 2 || s.SuccessEWA != 0.8 || s.LastError != "x" {
		t.Errorf("目标状态不一致: %+v", s)
	}
	buckets, err := d2.HourlyToday()
	if err != nil {
		t.Fatalf("HourlyToday: %v", err)
	}
	if buckets[time.Now().Hour()].Total != 1 {
		t.Errorf("重开后明细丢失: total=%d", buckets[time.Now().Hour()].Total)
	}
}

func TestQueueFullNeverBlocks(t *testing.T) {
	d := openTemp(t)
	// 写协程批量消费很快，直接灌远超队列容量的量验证不阻塞、不计死
	start := time.Now()
	for i := 0; i < 20000; i++ {
		d.RecordCall(Call{EndpointID: "ep", Model: "m", OK: true})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RecordCall 阻塞了主流程: %v", elapsed)
	}
}

func TestRetention(t *testing.T) {
	d := openTemp(t)
	old := time.Now().Add(-retention - time.Hour)
	d.RecordCall(Call{TS: old, EndpointID: "ep", Model: "old", OK: true})
	d.RecordCall(Call{TS: time.Now(), EndpointID: "ep", Model: "new", OK: true})
	// 等落库后手动触发清理
	deadline := time.Now().Add(3 * time.Second)
	for {
		buckets, _ := d.HourlyToday()
		if buckets[time.Now().Hour()].Total >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待落库超时")
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.deleteExpired()
	d7, err := d.DailyN(7)
	if err != nil {
		t.Fatalf("DailyN: %v", err)
	}
	var total int64
	for _, b := range d7 {
		total += b.Total
	}
	if total != 1 {
		t.Errorf("过期明细未被清理: 近7天 total=%d, want 1", total)
	}
}

func TestDeleteTargetsNotIn(t *testing.T) {
	d := openTemp(t)
	d.RecordTarget(TargetState{Key: "ep1|keep", Total: 1})
	d.RecordTarget(TargetState{Key: "ep1|stale", Total: 1})
	time.Sleep(400 * time.Millisecond) // 等写协程落库
	d.DeleteTargetsNotIn([]string{"ep1|keep"})
	m, err := d.LoadTargets()
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if _, ok := m["ep1|keep"]; !ok {
		t.Error("保留的目标被误删")
	}
	if _, ok := m["ep1|stale"]; ok {
		t.Error("孤立目标未被清理")
	}
}

func TestDailyNFillsEmptyDays(t *testing.T) {
	d := openTemp(t)
	d7, err := d.DailyN(7)
	if err != nil {
		t.Fatalf("DailyN: %v", err)
	}
	if len(d7) != 7 {
		t.Fatalf("DailyN len = %d, want 7", len(d7))
	}
	// 最后一个桶必须是今天
	if d7[6].Day != time.Now().Format("2006-01-02") {
		t.Errorf("最后一个桶不是今天: %s", d7[6].Day)
	}
}

// TestMinutelyNBucketsAndAggregation 分钟级聚合：窗口长度、按分钟分桶、空分钟补零
func TestMinutelyNBucketsAndAggregation(t *testing.T) {
	d := openTemp(t)
	now := time.Now()
	// 落在「当前这一分钟」和「3 分钟前」
	d.RecordCall(Call{TS: now, EndpointID: "ep1", Model: "m-a", OK: true, PromptTokens: 10, OutputTokens: 5})
	d.RecordCall(Call{TS: now, EndpointID: "ep1", Model: "m-a", OK: false, Err: "boom"})
	d.RecordCall(Call{TS: now.Add(-3 * time.Minute), EndpointID: "ep1", Model: "m-b", OK: true})

	deadline := time.Now().Add(3 * time.Second)
	var buckets []Bucket
	for {
		var err error
		buckets, err = d.MinutelyN(60)
		if err != nil {
			t.Fatalf("MinutelyN: %v", err)
		}
		cur := buckets[len(buckets)-1]
		if cur.Total == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待落库超时，当前分钟桶: %+v", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(buckets) != 60 {
		t.Fatalf("应返回 60 个桶, got %d", len(buckets))
	}
	// 最后一个桶 = 当前分钟
	cur := buckets[len(buckets)-1]
	if cur.Total != 2 {
		t.Errorf("当前分钟应有 2 次调用: %+v", cur)
	}
	if st := cur.Models["m-a"]; st.Calls != 2 || st.Fails != 1 || st.PromptTokens != 10 || st.OutputTokens != 5 {
		t.Errorf("当前分钟 m-a 聚合不对: %+v", st)
	}
	// 3 分钟前的那个桶
	older := buckets[len(buckets)-4]
	if older.Total != 1 || older.Models["m-b"].Calls != 1 {
		t.Errorf("3 分钟前的桶不对: %+v", older)
	}
	// 空分钟必须存在且为 0（前端不用自己补缺口）
	if buckets[0].Total != 0 || buckets[0].Models == nil {
		t.Errorf("空分钟桶应存在且 Models 已初始化: %+v", buckets[0])
	}
	// 时间标签升序且形如 HH:MM
	for i := 1; i < len(buckets); i++ {
		if buckets[i].Label <= buckets[i-1].Label && !(buckets[i-1].Label == "23:59" && buckets[i].Label == "00:00") {
			t.Errorf("标签应升序: %q -> %q", buckets[i-1].Label, buckets[i].Label)
		}
	}
}

// TestMinutelyNExcludesOutsideWindow 窗口外的记录不能算进来
func TestMinutelyNExcludesOutsideWindow(t *testing.T) {
	d := openTemp(t)
	now := time.Now()
	d.RecordCall(Call{TS: now.Add(-2 * time.Hour), EndpointID: "ep1", Model: "old", OK: true})

	buckets, err := d.MinutelyN(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range buckets {
		if b.Total != 0 {
			t.Fatalf("2 小时前的调用不该落在 10 分钟窗口里: %+v", b)
		}
	}
}

// TestRecentCallsNewestFirst 最近调用明细：最新在前，字段完整
func TestRecentCallsNewestFirst(t *testing.T) {
	d := openTemp(t)
	base := time.Now().Add(-10 * time.Minute)
	for i := 0; i < 5; i++ {
		d.RecordCall(Call{
			TS: base.Add(time.Duration(i) * time.Minute), EndpointID: "ep1",
			Model: "m" + string(rune('0'+i)), OK: i != 2, LatencyMS: int64(100 * (i + 1)), Err: "e",
		})
	}
	deadline := time.Now().Add(3 * time.Second)
	var rows []CallRow
	for {
		var err error
		rows, err = d.RecentCalls(10)
		if err != nil {
			t.Fatalf("RecentCalls: %v", err)
		}
		if len(rows) == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待落库超时, got %d 条", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows[0].Model != "m4" {
		t.Errorf("最新的一条应排最前, got %q", rows[0].Model)
	}
	if rows[4].Model != "m0" {
		t.Errorf("最旧的一条应排最后, got %q", rows[4].Model)
	}
	// 失败那条（m2）的 ok 要正确读回 false
	for _, r := range rows {
		if r.Model == "m2" && r.OK {
			t.Error("m2 是失败的，OK 应为 false")
		}
		if r.Model == "m2" && r.Err != "e" {
			t.Errorf("错误信息应读回, got %q", r.Err)
		}
	}
	// limit 生效
	if got, _ := d.RecentCalls(2); len(got) != 2 {
		t.Errorf("limit=2 应只返回 2 条, got %d", len(got))
	}
}
