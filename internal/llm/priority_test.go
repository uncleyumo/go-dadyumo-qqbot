package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// epJSONWithPriority 构造带优先级的接入点，模型写成 "main:1" 这样的形式
func epJSONWithPriority(id, baseURL string, models ...string) string {
	ms := make([]string, 0, len(models))
	for _, m := range models {
		name, prio, _ := strings.Cut(m, ":")
		ms = append(ms, `{"id":"`+name+`","label":"`+name+`","enabled":true,"priority":`+prio+
			`,"max_ctx":8000,"max_out":512,"stream":false}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","base_url":"` + baseURL +
		`","api_key":"k","enabled":true,"timeout_ms":5000,"models":[` + strings.Join(ms, ",") + `]}`
}

// TestPriorityBeatsFasterBackup 主力即使比兜底慢，也应该走主力。
//
// 这条守的是「指定主力就别被延迟排序推翻」：路由原本是纯健康度打分，
// 而延迟分用的是候选集内相对归一化——两个目标时哪怕只差几十毫秒，
// 慢的那个也会被归一成 0 分、快的是满分，差距被拉满。
// 用加权加成是救不回来的（加成必须小于故障扣分才不会卡死不切换），
// 所以 priority 必须是硬分层。
func TestPriorityBeatsFasterBackup(t *testing.T) {
	old := exploreEpsilon
	exploreEpsilon = 0 // 关掉探路，做确定性断言
	defer func() { exploreEpsilon = old }()

	m := newMock()
	m.on("main", func(w http.ResponseWriter, model string, stream bool) {
		time.Sleep(80 * time.Millisecond) // 主力明显更慢
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok:main"}}},
		})
	})
	m.on("backup", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok:backup"}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSONWithPriority("E1", srv.URL, "main:1", "backup:0")+`]`)
	r := NewRouter(st)

	for i := 0; i < 3; i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("预热失败: %v", err)
		}
	}
	before := m.count("main")
	for i := 0; i < 8; i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("调用失败: %v", err)
		}
	}
	if used := m.count("main") - before; used != 8 {
		t.Fatalf("主力更慢也应稳定走主力，8 次里只走了 %d 次（兜底被调用 %d 次）",
			used, m.count("backup"))
	}
}

// TestPriorityYieldsWhenMainFails 主力连续失败时，兜底必须能接上。
//
// 这条守的是反面：硬分层不能变成「主力挂了也死磕主力」。
// 靠的是 observeFailure 的冷却机制——失败会立刻把主力踢出候选集。
func TestPriorityYieldsWhenMainFails(t *testing.T) {
	old := exploreEpsilon
	exploreEpsilon = 0
	defer func() { exploreEpsilon = old }()

	m := newMock()
	m.on("main", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
	})
	m.on("backup", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok:backup"}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSONWithPriority("E1", srv.URL, "main:1", "backup:0")+`]`)
	r := NewRouter(st)

	rescued := 0
	for i := 0; i < 6; i++ {
		res, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
		if err == nil && res.Model == "backup" {
			rescued++
		}
	}
	if rescued == 0 {
		t.Fatalf("主力持续失败却从未由兜底接管：main=%d backup=%d", m.count("main"), m.count("backup"))
	}
	t.Logf("主力连续失败 6 次，其中 %d 次由兜底接管", rescued)
}

// TestPriorityDefaultsAndClamped 缺省为 0，负数被夹到 0
func TestPriorityDefaultsAndClamped(t *testing.T) {
	st := testStore(t, `"endpoints":[`+epJSON("A", "http://x", "m1")+`]`)
	if p := st.Get().LLM.Endpoints[0].Models[0].Priority; p != 0 {
		t.Fatalf("未配置 priority 时应为 0，实际 %d", p)
	}
	st2 := testStore(t, `"endpoints":[`+epJSONWithPriority("A", "http://x", "m1:-3")+`]`)
	if p := st2.Get().LLM.Endpoints[0].Models[0].Priority; p != 0 {
		t.Fatalf("负优先级应被夹到 0，实际 %d", p)
	}
}

// TestPriorityPropagatesToTarget 优先级要传到 Target 上，否则排序时读不到
func TestPriorityPropagatesToTarget(t *testing.T) {
	st := testStore(t, `"endpoints":[`+epJSONWithPriority("E1", "http://x", "main:2")+`]`)
	r := NewRouter(st)
	for _, tg := range r.targets {
		if tg.Model == "main" {
			if tg.Priority != 2 {
				t.Fatalf("优先级未传到 Target: %d", tg.Priority)
			}
			return
		}
	}
	t.Fatal("没找到目标 main")
}
