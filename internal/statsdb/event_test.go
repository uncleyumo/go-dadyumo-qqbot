package statsdb

import (
	"path/filepath"
	"testing"
	"time"

	"dadyumo/internal/logx"
)

func openEventDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// flush 等队列里的写真正落到库里。
//
// writerLoop 是异步的（200ms 或攒满 64 条刷一次），不等它的话测试会
// 偶发查不到刚写的东西——那种 flaky 最难查。
// 判据用「查得到期望条数」而不是「队列为空」：DB 上没有 Pending()，
// 而队列空只说明已交给写协程，不一定已经提交。
func flush(t *testing.T, d *DB, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := d.CountEvents(EventFilter{}); n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := d.CountEvents(EventFilter{})
	t.Fatalf("3 秒内只落库 %d/%d 条", got, want)
}

// TestEventRoundTrip 事件日志必须能写进去、查出来，字段完整。
func TestEventRoundTrip(t *testing.T) {
	d := openEventDB(t)
	d.RecordEvent(Event{
		Cat: "decision", Level: "INFO",
		Msg: "跳过：本次摇骰子没上线", Group: "测试群",
		KV: `{"摇到":"0.73","在线率":"0.20","档位":"平时"}`,
	})
	flush(t, d, 1)

	rows, err := d.QueryEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("查到 %d 条，期望 1", len(rows))
	}
	r := rows[0]
	if r.Cat != "decision" || r.Level != "INFO" || r.Msg != "跳过：本次摇骰子没上线" {
		t.Errorf("基本字段不对: %+v", r)
	}
	if r.Group != "测试群" {
		t.Errorf("群名没落: %q", r.Group)
	}
	if r.KV["摇到"] != "0.73" || r.KV["在线率"] != "0.20" {
		t.Errorf("kv 没解析回来: %v", r.KV)
	}
	if r.TS.IsZero() {
		t.Error("时间戳为零值")
	}
}

// TestQueryEventsFilters 各类筛选必须生效。
func TestQueryEventsFilters(t *testing.T) {
	d := openEventDB(t)
	base := time.Now().Add(-2 * time.Hour)
	for i, e := range []Event{
		{Cat: "decision", Level: "INFO", Msg: "本次摇骰子没上线", Group: "群A"},
		{Cat: "decision", Level: "WARN", Msg: "预算用尽", Group: "群A"},
		{Cat: "speak", Level: "INFO", Msg: "已发言 你看看你", Group: "群B"},
		{Cat: "chat", Level: "INFO", Msg: "群消息 内容", Group: "群B"},
		{Cat: "", Level: "INFO", Msg: "某条未分类的系统日志", Group: "群C"},
	} {
		e.TS = base.Add(time.Duration(i) * time.Minute)
		e.KV = `{"k":"v"}`
		d.RecordEvent(e)
	}
	flush(t, d, 5)

	cases := []struct {
		name string
		f    EventFilter
		want int
	}{
		{"不限", EventFilter{}, 5},
		{"按分类", EventFilter{Cats: []string{"decision"}}, 2},
		{"多分类", EventFilter{Cats: []string{"decision", "speak"}}, 3},
		{"按级别", EventFilter{Levels: []string{"warn"}}, 1},
		// runtime 是「未分类」的显示名，落库时是空串 —— 这个翻译必须生效，
		// 否则前端选「未分类」会一条都查不到
		{"未分类", EventFilter{Cats: []string{"runtime"}}, 1},
		{"关键字", EventFilter{Q: "摇骰子"}, 1},
		{"关键字查 kv", EventFilter{Q: "预算"}, 1},
		{"分类+级别", EventFilter{Cats: []string{"decision"}, Levels: []string{"INFO"}}, 1},
		// 5 条都写在 2 小时前（含 0~4 分钟的错开），最近 1 小时内一条都没有。
		// 这条断言的作用是「时间过滤确实会筛掉东西」——期望 0 而不是随便写个数。
		{"时间范围（全都超期）", EventFilter{Since: time.Now().Add(-time.Hour)}, 0},
		// 放宽到 3 小时：5 条全部落回区间内
		{"时间范围（全部落回）", EventFilter{Since: time.Now().Add(-3 * time.Hour)}, 5},
		{"组合", EventFilter{Cats: []string{"decision", "speak"}, Q: "用"}, 1}, // 「预算用尽」
		{"无匹配", EventFilter{Q: "不存在的关键词"}, 0},
	}
	for _, c := range cases {
		rows, err := d.QueryEvents(c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(rows) != c.want {
			t.Errorf("%s: 查到 %d 条，期望 %d", c.name, len(rows), c.want)
		}
	}
}

// TestQueryEventsLimitClamped limit 必须被夹住。
func TestQueryEventsLimitClamped(t *testing.T) {
	d := openEventDB(t)
	for i := 0; i < 5; i++ {
		d.RecordEvent(Event{Level: "INFO", Msg: "行", KV: "{}"})
	}
	flush(t, d, 5)

	rows, _ := d.QueryEvents(EventFilter{Limit: 999999})
	if len(rows) != 5 {
		t.Errorf("超大 limit 应该夹到实际条数，实际 %d", len(rows))
	}
	rows, _ = d.QueryEvents(EventFilter{Limit: 2})
	if len(rows) != 2 {
		t.Errorf("limit=2 应返回 2 条，实际 %d", len(rows))
	}
}

// TestLikeWildcardsEscaped 关键字里的 % 和 _ 必须被转义。
//
// 不转义的话，用户搜一个「%」就会匹配全部行——那不是搜索，是全表扫描；
// 而 events 按 ts 有索引却仍会扫出几十万行，SetMaxOpenConns(1) 下
// 会把唯一连接占死，整个管理端卡住。
func TestLikeWildcardsEscaped(t *testing.T) {
	d := openEventDB(t)
	for i, m := range []string{"普通消息", "含%百分号", "含_下划线"} {
		d.RecordEvent(Event{Level: "INFO", Msg: m, KV: "{}", TS: time.Now().Add(time.Duration(i) * time.Second)})
	}
	flush(t, d, 1)

	if rows, _ := d.QueryEvents(EventFilter{Q: "%"}); len(rows) != 1 {
		t.Errorf("搜 %% 应只匹配那一条，实际匹配 %d 条 —— 通配符没转义", len(rows))
	}
	if rows, _ := d.QueryEvents(EventFilter{Q: "_"}); len(rows) != 1 {
		t.Errorf("搜 _ 应只匹配那一条，实际匹配 %d 条 —— 通配符没转义", len(rows))
	}
	// 字面量含通配符的也要能搜到
	if rows, _ := d.QueryEvents(EventFilter{Q: "含%"}); len(rows) != 1 {
		t.Errorf("搜「含%%」应匹配 1 条，实际 %d", len(rows))
	}
}

// TestCountEvents 计数必须忽略 limit，否则前端显示的「共 N 条」是错的。
//
// **数据量必须超过 limit**，否则测不出来。
// 第一版只写了 10 条、limit=3：CountEvents 内部把 Limit 归零后再查，
// 所以返回 10 是对的；而「错误实现复用 QueryEvents 的长度」那条变异
// 同样返回 10（10 < 默认 200），测试照样绿——假绿。
// 现在写 300 条，让任何「偷偷用 limit」的实现在数字上露出来。
func TestCountEvents(t *testing.T) {
	d := openEventDB(t)
	const n = 300
	for i := 0; i < n; i++ {
		d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "行", KV: "{}"})
	}
	flush(t, d, n)

	total, err := d.CountEvents(EventFilter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Errorf("CountEvents 忽略了 limit：期望 %d，实际 %d", n, total)
	}
	total, _ = d.CountEvents(EventFilter{Cats: []string{"decision"}})
	if total != n {
		t.Errorf("按分类计数不对：%d", total)
	}
	total, _ = d.CountEvents(EventFilter{Cats: []string{"speak"}})
	if total != 0 {
		t.Errorf("不存在的分类应返回 0，实际 %d", total)
	}
}

// TestEventCats 分类统计要把空 cat 归到 runtime。
func TestEventCats(t *testing.T) {
	d := openEventDB(t)
	d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "a", KV: "{}"})
	d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "b", KV: "{}"})
	d.RecordEvent(Event{Cat: "", Level: "INFO", Msg: "c", KV: "{}"})
	flush(t, d, 3)

	cats, err := d.EventCats()
	if err != nil {
		t.Fatal(err)
	}
	if cats["decision"] != 2 {
		t.Errorf("decision 计数错: %d", cats["decision"])
	}
	if cats["runtime"] != 1 {
		t.Errorf("未分类应归到 runtime，实际 %v", cats)
	}
}

// TestLogSinkConvertsEntry LogSink 必须把 Entry 映射完整，且 group 从 kv 里提出来。
func TestLogSinkConvertsEntry(t *testing.T) {
	d := openEventDB(t)
	d.LogSink()(logx.Entry{
		TS:    time.Now().Format("2006-01-02 15:04:05.000"),
		Cat:   logx.CatDecision,
		Level: "INFO",
		Msg:   "跳过：本次摇骰子没上线",
		KV:    map[string]any{"group": "测试群二号", "摇到": "0.73"},
	})
	flush(t, d, 1)

	rows, _ := d.QueryEvents(EventFilter{})
	if len(rows) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(rows))
	}
	r := rows[0]
	if r.Cat != "decision" {
		t.Errorf("分类没带过来: %q", r.Cat)
	}
	// group 必须从 kv 提出来单独存：SQL 侧没法在 JSON 里 WHERE，
	// 而「只看某个群」是最常见的筛选
	if r.Group != "测试群二号" {
		t.Errorf("group 没从 kv 提取出来: %q", r.Group)
	}
	if r.KV["摇到"] != "0.73" {
		t.Errorf("kv 丢了: %v", r.KV)
	}
	if r.TS.IsZero() {
		t.Error("时间戳解析失败（LogSink 的时间格式与 logx 不一致？）")
	}
}

// TestEventRetentionSeparate 日志保留期必须比调用明细短。
func TestEventRetentionSeparate(t *testing.T) {
	if eventRetention >= retention {
		t.Errorf("日志保留期 %v 不该 ≥ 调用明细的 %v：日志量大得多",
			eventRetention, retention)
	}
	if eventRetention < 7*24*time.Hour {
		t.Errorf("日志保留期 %v 太短了——「它上周为什么不理我」这种问题得能查到", eventRetention)
	}
}

// TestEventTruncated 超长 msg/kv 必须截断，否则一条长群消息能撑爆库。
func TestEventTruncated(t *testing.T) {
	d := openEventDB(t)
	long := make([]rune, 1000)
	for i := range long {
		long[i] = '字'
	}
	d.RecordEvent(Event{Level: "INFO", Msg: string(long), KV: `{"x":"` + string(long) + `"}`})
	flush(t, d, 1)

	rows, _ := d.QueryEvents(EventFilter{})
	if len(rows) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(rows))
	}
	if n := len([]rune(rows[0].Msg)); n > 401 {
		t.Errorf("msg 截到 %d 字符，仍太长", n)
	}
}