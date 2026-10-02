package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"dadyumo/internal/statsdb"
)

func openTestStats(t *testing.T) *statsdb.DB {
	t.Helper()
	d, err := statsdb.Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatalf("打开统计库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type logsResp struct {
	OK    bool               `json:"ok"`
	Total int                `json:"total"`
	Cats  map[string]int     `json:"cats"`
	Rows  []statsdb.EventRow `json:"rows"`
}

func callLogs(t *testing.T, s *Server, qs string) logsResp {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs"+qs, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，body=%s", rec.Code, rec.Body.String())
	}
	var r logsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	return r
}

// TestHandleLogsFilters 参数必须真的传达到查询层。
func TestHandleLogsFilters(t *testing.T) {
	d := openTestStats(t)
	base := time.Now().Add(-2 * time.Hour)
	for i, e := range []statsdb.Event{
		{Cat: "decision", Level: "INFO", Msg: "跳过：冲动值不足", Group: "群A"},
		{Cat: "decision", Level: "WARN", Msg: "跳过：预算用尽", Group: "群A"},
		{Cat: "speak", Level: "INFO", Msg: "已发言 你看看你", Group: "群B"},
	} {
		e.TS = base.Add(time.Duration(i) * time.Minute)
		e.KV = `{"k":"v"}`
		d.RecordEvent(e)
	}
	waitEvents(t, d, 3)
	s := &Server{stats: d}

	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"不限", "", 3},
		{"按分类", "?cat=decision", 2},
		{"多分类", "?cat=decision,speak", 3},
		{"按级别", "?level=WARN", 1},
		{"关键字", "?q=冲动", 1},
		// range 必须真的翻译成时间窗而不是被忽略：
		// 数据全在 2 小时前，最近 1 小时应该一条都没有
		{"时间窗内", "?range=60m", 0},
		{"时间窗外", "?range=7d", 3},
		{"组合", "?cat=decision&q=预算", 1},
	}
	for _, c := range cases {
		r := callLogs(t, s, c.qs)
		if len(r.Rows) != c.want {
			t.Errorf("%s: 期望 %d 条，实际 %d 条（total=%d）", c.name, c.want, len(r.Rows), r.Total)
		}
	}
}

// TestHandleLogsTotalIsTotal total 必须是匹配总数而不是本页条数。
//
// 前端显示「共 N 条」，用本页条数冒充的话用户会以为那就是全部，
// 而日志恰恰是用来翻历史的——「只有 200 条」这个错觉会直接误导判断。
func TestHandleLogsTotalIsTotal(t *testing.T) {
	d := openTestStats(t)
	const n = 60
	for i := 0; i < n; i++ {
		d.RecordEvent(statsdb.Event{Cat: "decision", Level: "INFO", Msg: "行", KV: "{}"})
	}
	waitEvents(t, d, n)
	s := &Server{stats: d}

	r := callLogs(t, s, "?cat=decision&limit=5")
	if len(r.Rows) != 5 {
		t.Fatalf("limit=5 应返回 5 条，实际 %d", len(r.Rows))
	}
	if r.Total != n {
		t.Errorf("total 应是 %d（匹配总数），实际 %d —— 用本页条数冒充会让用户以为只有这些", n, r.Total)
	}
}

// TestHandleLogsReturnsCats 分类清单必须来自实际数据而不是前端硬编码。
func TestHandleLogsReturnsCats(t *testing.T) {
	d := openTestStats(t)
	d.RecordEvent(statsdb.Event{Cat: "decision", Level: "INFO", Msg: "a", KV: "{}"})
	d.RecordEvent(statsdb.Event{Cat: "speak", Level: "INFO", Msg: "b", KV: "{}"})
	waitEvents(t, d, 2)
	s := &Server{stats: d}

	r := callLogs(t, s, "")
	if r.Cats["decision"] != 1 || r.Cats["speak"] != 1 {
		t.Errorf("分类统计不对: %v", r.Cats)
	}
}

// TestHandleLogsLimitClamped limit 必须被夹住。
//
// 这是个 GET 接口，任何人都能构造 ?limit=999999。
// 而 DB 是 SetMaxOpenConns(1)——一条失控的查询会把唯一连接占死，
// 整个管理端全部接口一起卡住（db.go 里 idx_calls_ts 的注释讲的是同一个坑）。
func TestHandleLogsLimitClamped(t *testing.T) {
	d := openTestStats(t)
	for i := 0; i < 40; i++ {
		d.RecordEvent(statsdb.Event{Level: "INFO", Msg: "行", KV: "{}"})
	}
	waitEvents(t, d, 40)
	s := &Server{stats: d}

	r := callLogs(t, s, "?limit=999999")
	if len(r.Rows) > statsdb.MaxEventLimit {
		t.Errorf("limit 没被夹住：返回 %d 条，上限应是 %d", len(r.Rows), statsdb.MaxEventLimit)
	}
	if r.Total != 40 {
		t.Errorf("total 应是 40，实际 %d", r.Total)
	}
}

// TestHandleLogsDegraded 统计库没开时不能 500，要降级成 enabled:false。
func TestHandleLogsDegraded(t *testing.T) {
	s := &Server{stats: nil}
	r := callLogs(t, s, "")
	if !r.OK {
		t.Error("降级时也应返回 ok=true，只是 enabled=false")
	}
	if len(r.Rows) != 0 {
		t.Errorf("降级时不该返回数据，实际 %d 条", len(r.Rows))
	}
}

// TestLogRangeSince range 参数翻译要正确。
func TestLogRangeSince(t *testing.T) {
	if !logRangeSince("60m").After(time.Now().Add(-61 * time.Minute)) {
		t.Error("60m 起点不对")
	}
	today := logRangeSince("today")
	y, m, d := time.Now().Date()
	if today.Year() != y || today.Month() != m || today.Day() != d {
		t.Errorf("today 应是今天零点，得到 %v", today)
	}
	if !logRangeSince("7d").After(time.Now().Add(-8 * 24 * time.Hour)) {
		t.Error("7d 起点不对")
	}
	if !logRangeSince("30d").After(time.Now().Add(-31 * 24 * time.Hour)) {
		t.Error("30d 起点不对")
	}
	// 认不出来的值 = 不限（零值），不能当成「0 秒前」把历史全滤掉
	if !logRangeSince("乱填").IsZero() {
		t.Error("未知的 range 应返回零值表示不限")
	}
	if !logRangeSince("").IsZero() {
		t.Error("空 range 应返回零值表示不限")
	}
}

// TestSplitCSV 逗号参数拆分。
func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0}, {"  ", 0}, {",", 0},
		{"a", 1}, {"a,b", 2}, {" a , b ", 2}, {"a,,b", 2}, {"a, b, ", 2},
	}
	for _, c := range cases {
		if got := len(splitCSV(c.in)); got != c.want {
			t.Errorf("splitCSV(%q) 得到 %d 项，期望 %d", c.in, got, c.want)
		}
	}
}

// waitEvents 等异步写队列落库。
func waitEvents(t *testing.T, d *statsdb.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := d.CountEvents(statsdb.EventFilter{}); n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := d.CountEvents(statsdb.EventFilter{})
	t.Fatalf("3 秒内只落库 %d/%d 条", n, want)
}