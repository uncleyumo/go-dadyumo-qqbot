package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dadyumo/internal/config"
)

// openAIMock 模拟 OpenAI 兼容接口。handler 按模型名决定怎么响应。
// mu 保护 calls/handler 两张表：并发测试（TestConcurrentReloadAndRoute）会让多个
// 请求同时进来，ServeHTTP 里对 calls 的懒初始化本身就是 data race。
type openAIMock struct {
	mu      sync.Mutex
	calls   map[string]*int64 // model -> 调用次数
	handler map[string]func(w http.ResponseWriter, model string, stream bool)
}

func newMock() *openAIMock {
	return &openAIMock{calls: map[string]*int64{}, handler: map[string]func(http.ResponseWriter, string, bool){}}
}

func (m *openAIMock) count(model string) int64 {
	m.mu.Lock()
	p, ok := m.calls[model]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	return atomic.LoadInt64(p)
}

func (m *openAIMock) on(model string, fn func(w http.ResponseWriter, model string, stream bool)) {
	m.mu.Lock()
	m.calls[model] = new(int64)
	m.handler[model] = fn
	m.mu.Unlock()
}

func (m *openAIMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// body 只能读一次，解析后把关键字段传给 handler
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	if _, ok := m.calls[body.Model]; !ok {
		m.calls[body.Model] = new(int64)
	}
	counter, fn := m.calls[body.Model], m.handler[body.Model]
	m.mu.Unlock()
	atomic.AddInt64(counter, 1)
	if fn != nil {
		fn(w, body.Model, body.Stream)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok:" + body.Model}}},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"choices": []map[string]any{{"delta": map[string]string{"content": c}}},
		}) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func testStore(t *testing.T, endpoints string) *config.Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"qq":{"app_id":"1","app_secret":"s"},"llm":{"max_attempts":4,` + endpoints + `}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(p)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return st
}

func epJSON(id, baseURL string, models ...string) string {
	ms := make([]string, 0, len(models))
	for _, m := range models {
		ms = append(ms, `{"id":"`+m+`","label":"`+m+`","enabled":true,"max_ctx":8000,"max_out":512,"stream":false}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","base_url":"` + baseURL + `","api_key":"k","enabled":true,"timeout_ms":5000,"models":[` + strings.Join(ms, ",") + `]}`
}

func TestRouterSucceeds(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "fast")+`]`)
	r := NewRouter(st)
	res, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("应调用成功，实际: %v", err)
	}
	if res.Content != "ok:fast" {
		t.Fatalf("返回内容不符: %q", res.Content)
	}
	if res.Endpoint != "A" || res.Model != "fast" {
		t.Fatalf("归属信息不符: %+v", res)
	}
	if m.count("fast") != 1 {
		t.Fatalf("应只调用 1 次，实际 %d", m.count("fast"))
	}
}

func TestRouterFailsOverToNextTarget(t *testing.T) {
	m := newMock()
	m.on("bad", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "boom"}})
	})
	m.on("good", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "救场成功"}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "good", "bad")+`]`)
	r := NewRouter(st)

	// 先让坏目标失败一次，确认失败会被记录并进入冷却
	bad := r.targets["A|bad"]
	_, cerr := Call(context.Background(), bad, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if cerr == nil {
		t.Fatal("坏目标本应调用失败")
	}
	bad.recordFailure(cerr)

	found := false
	for _, s := range r.Stats() {
		if s.Model == "bad" {
			found = true
			if s.Total != 1 || s.ConsecFail != 1 {
				t.Fatalf("失败目标应记录失败: %+v", s)
			}
			if !s.Cooling {
				t.Fatalf("失败目标应处于冷却状态: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("统计中应包含 bad")
	}

	// 此时整体调用仍应被消费：自动落到可用的 good 上
	res, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("存在可用目标时不应失败: %v", err)
	}
	if res.Content != "救场成功" {
		t.Fatalf("应切换到可用模型，实际: %q", res.Content)
	}
}

func TestRouterExhaustsAllTargets(t *testing.T) {
	m := newMock()
	for _, name := range []string{"m1", "m2", "m3"} {
		m.on(name, func(w http.ResponseWriter, _ string, _ bool) {
			writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "全挂了"}})
		})
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "m1", "m2", "m3")+`]`)
	r := NewRouter(st)
	_, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("所有目标失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "均不可用") {
		t.Fatalf("错误信息应说明全部不可用，实际: %v", err)
	}
	total := m.count("m1") + m.count("m2") + m.count("m3")
	if total != 3 {
		t.Fatalf("应尝试全部 3 个目标，实际 %d", total)
	}
}

func TestRouterPrefersFasterTarget(t *testing.T) {
	m := newMock()
	m.on("slow", func(w http.ResponseWriter, _ string, _ bool) {
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "慢的"}}},
		})
	})
	m.on("fast", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "快的"}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "fast", "slow")+`]`)
	r := NewRouter(st)

	// 关掉 ε-贪心：这里要验证的是排序偏好本身，
	// 掺入随机探路会让断言变成掷骰子，掩盖真正的回归。
	old := exploreEpsilon
	exploreEpsilon = 0
	defer func() { exploreEpsilon = old }()

	// 预热：冷启动阶段「未试过优先」的规则会保证两个目标各被试到一次
	for i := 0; i < 3; i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("预热失败: %v", err)
		}
	}
	if m.count("fast") == 0 || m.count("slow") == 0 {
		t.Fatalf("预热阶段两个目标都应被试到，实际 fast=%d slow=%d", m.count("fast"), m.count("slow"))
	}

	before := m.count("fast")
	for i := 0; i < 5; i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("调用失败: %v", err)
		}
	}
	fastUsed := m.count("fast") - before
	if fastUsed != 5 {
		t.Fatalf("稳定阶段应每次都选快的模型，实际 5 次里只用了 %d 次", fastUsed)
	}
}

func TestNotFoundMarksDead(t *testing.T) {
	m := newMock()
	m.on("gone", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "model not found"}})
	})
	m.on("alive", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "我在"}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "alive", "gone")+`]`)
	r := NewRouter(st)

	// 直接对 gone 发起一次调用，触发 404
	tgt := r.targets["A|gone"]
	_, cerr := Call(context.Background(), tgt, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if cerr == nil || cerr.Kind != ErrKindNotFound {
		t.Fatalf("应识别为模型不存在，实际: %v", cerr)
	}
	tgt.markDead(cerr.Message)

	var dead bool
	for _, s := range r.Stats() {
		if s.Model == "gone" {
			dead = s.Dead
		}
	}
	if !dead {
		t.Fatal("404 的模型应被标记为不可用")
	}
	// 后续调用不应再选中它
	for i := 0; i < 3; i++ {
		if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("调用失败: %v", err)
		}
	}
	if m.count("gone") != 1 {
		t.Fatalf("dead 模型不应被再次调用，实际调用 %d 次", m.count("gone"))
	}
}

func TestAuthFailureCoolsWholeEndpoint(t *testing.T) {
	m := newMock()
	m.on("x", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid key"}})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "x", "y")+`]`)
	r := NewRouter(st)
	tgt := r.targets["A|x"]
	_, cerr := Call(context.Background(), tgt, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if cerr == nil || cerr.Kind != ErrKindAuth {
		t.Fatalf("应识别为鉴权失败，实际: %v", cerr)
	}
	r.cooldownEndpoint("A", 2*time.Minute, cerr.Message)
	for _, s := range r.Stats() {
		if !s.Cooling {
			t.Fatalf("同接入点的模型都应进入冷却: %+v", s)
		}
	}
}

func TestStreamMeasuresTTFT(t *testing.T) {
	m := newMock()
	m.on("stream", func(w http.ResponseWriter, _ string, stream bool) {
		if !stream {
			writeJSON(w, 200, map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": "非流式"}}},
			})
			return
		}
		time.Sleep(120 * time.Millisecond)
		writeSSE(w, []string{"你", "好", "呀"})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "stream")+`]`)
	r := NewRouter(st)
	r.targets["A|stream"].Stream = true

	res, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if res.Content != "你好呀" {
		t.Fatalf("流式内容拼接错误: %q", res.Content)
	}
	if res.TTFTMS < 100 {
		t.Fatalf("TTFT 应测到约 120ms，实际 %.0f", res.TTFTMS)
	}
	if res.LatencyMS < res.TTFTMS {
		t.Fatalf("端到端延迟应不小于首字延迟: %v", res)
	}
}

func TestEmptyContentTreatedAsFailure(t *testing.T) {
	m := newMock()
	m.on("blank", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "   "}}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "blank")+`]`)
	r := NewRouter(st)
	if _, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Fatal("空内容应视为失败")
	}
}
