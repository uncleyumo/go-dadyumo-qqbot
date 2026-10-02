package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"dadyumo/internal/statsdb"
)

// TestChatRecordsCalls 成功与失败的尝试都必须写进统计库
func TestChatRecordsCalls(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()
	m.on("bad", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": "boom"}})
	})

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "good", "bad")+`]`)
	r := NewRouter(st)

	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := statsdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetStats(db)

	// bad 同档随机排序下可能第二轮才被选中，多打几轮保证两个目标都被试过。
	// 每轮 Chat 最终都应成功（good 兜底），且每个被试过的目标都要记一条明细。
	for i := 0; i < 10 && (m.count("good") == 0 || m.count("bad") == 0); i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("调用失败: %v", err)
		}
	}
	if m.count("good") == 0 || m.count("bad") == 0 {
		t.Fatalf("10 轮后仍有目标未被试过: good=%d bad=%d", m.count("good"), m.count("bad"))
	}
	if err := db.Close(); err != nil { // Close 排空队列
		t.Fatal(err)
	}

	db2, err := statsdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	buckets, err := db2.HourlyToday()
	if err != nil {
		t.Fatal(err)
	}
	b := buckets[time.Now().Hour()]
	if b.Models["good"].Calls < 1 || b.Models["good"].Fails != 0 {
		t.Errorf("good 应只记成功: %+v", b.Models["good"])
	}
	if b.Models["bad"].Calls < 1 || b.Models["bad"].Fails != b.Models["bad"].Calls {
		t.Errorf("bad 应只记失败: %+v", b.Models["bad"])
	}
	// 明细总数 = mock 实际收到的请求数（每次尝试一条，不多不少）
	if b.Total != m.count("good")+m.count("bad") {
		t.Errorf("明细总数 %d 与实际尝试数 %d 不符", b.Total, m.count("good")+m.count("bad"))
	}
}

// TestRestoreTargets 恢复只回填存在的 key，且不恢复冷却/下线状态
func TestRestoreTargets(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "fast")+`]`)
	r := NewRouter(st)

	key := "A|fast"
	r.RestoreTargets(map[string]statsdb.TargetState{
		key: {
			Key: key, Total: 100, Fails: 10, ConsecFail: 2,
			SuccessEWA: 0.9, TTFTMS: 500, LatencyMS: 900,
			LastError: "曾经挂过",
		},
		"ghost|gone": {Key: "ghost|gone", Total: 5}, // 配置里不存在，必须忽略
	})

	var h Health
	r.mu.RLock()
	if tg, ok := r.targets[key]; ok {
		h = tg.Health()
	}
	r.mu.RUnlock()
	if h.Total != 100 || h.Fails != 10 || h.SuccessEWA != 0.9 || h.TTFTMS != 500 {
		t.Errorf("健康度未恢复: %+v", h)
	}
	if h.LastError != "曾经挂过" {
		t.Errorf("LastError 未恢复: %q", h.LastError)
	}
	if h.Dead || h.Cooling(time.Now()) {
		t.Error("冷却/下线状态不应从持久层恢复")
	}
}

// TestFlushTargetsPersist 快照写库后重开能读回
func TestFlushTargetsPersist(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "fast")+`]`)
	r := NewRouter(st)
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := statsdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetStats(db)

	if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	r.FlushTargets()
	if err := db.Close(); err != nil { // Close 会排空队列
		t.Fatal(err)
	}

	db2, err := statsdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	m2, err := db2.LoadTargets()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := m2["A|fast"]
	if !ok {
		t.Fatalf("快照未落库: %v", m2)
	}
	if s.Total != 1 || s.Fails != 0 {
		t.Errorf("快照数据不对: %+v", s)
	}
	// 明细也要在
	buckets, err := db2.HourlyToday()
	if err != nil {
		t.Fatal(err)
	}
	if buckets[time.Now().Hour()].Total != 1 {
		t.Errorf("调用明细未落库: %+v", buckets[time.Now().Hour()])
	}
}
