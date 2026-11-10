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

// TestUnhealthyTargetDemotedBelowHealthy 连续失败达到阈值的目标不再享受
// priority 硬分层：它排到低优先级但健康的目标之后。成功一次后 ConsecFail
// 归零，自动回到原档位。
//
// 光靠冷却不够：冷却最长 5 分钟，而群聊消息间隔常常比它长，
// 一个连续失败的主力每次都会重新排第一，把整条兜底链堵在它身上。
func TestUnhealthyTargetDemotedBelowHealthy(t *testing.T) {
	st := testStore(t, `"endpoints":[`+epJSONWithPriority("E1", "http://x", "main:9", "backup:1")+`]`)
	r := NewRouter(st)

	r.mu.RLock()
	main := r.targets["E1|main"]
	r.mu.RUnlock()
	if main == nil {
		t.Fatal("找不到目标 main")
	}

	// 未到阈值时仍然是主力优先
	main.mu.Lock()
	main.h.Total = demoteAfterConsecFail - 1
	main.h.Fails = demoteAfterConsecFail - 1
	main.h.ConsecFail = demoteAfterConsecFail - 1
	main.h.LastUsed = time.Now()
	main.mu.Unlock()
	if c := r.PreviewChain(); len(c.Chain) < 2 || c.Chain[0].Model != "main" {
		t.Fatalf("未到降级阈值时应仍走主力: %+v", c.Chain)
	}

	// 达到阈值后沉底
	main.mu.Lock()
	main.h.Total = demoteAfterConsecFail
	main.h.Fails = demoteAfterConsecFail
	main.h.ConsecFail = demoteAfterConsecFail
	main.h.LastUsed = time.Now()
	main.mu.Unlock()
	c := r.PreviewChain()
	if len(c.Chain) < 2 {
		t.Fatalf("链路应有两个目标: %+v", c.Chain)
	}
	if c.Chain[0].Model != "backup" {
		t.Errorf("连续失败 %d 次后应排到健康目标之后，实际第一名是 %q",
			demoteAfterConsecFail, c.Chain[0].Model)
	}

	// 过了观察窗口，重新给它一次机会，避免降级变成永久下线
	main.mu.Lock()
	main.h.LastUsed = time.Now().Add(-2 * demoteProbation)
	main.mu.Unlock()
	if c := r.PreviewChain(); len(c.Chain) < 2 || c.Chain[0].Model != "main" {
		t.Errorf("过了观察窗口应重新参与正常排序: %+v", c.Chain)
	}
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
