package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
)

// responsesMock 模拟 /responses 端点，同时支持流式与非流式
type responsesMock struct {
	onFull   func(w http.ResponseWriter)
	onStream func(w http.ResponseWriter)
}

func (m *responsesMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 关键断言：Responses API 必须打到 /responses，而不是 /chat/completions
	if r.URL.Path != "/responses" {
		http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if stream, _ := body["stream"].(bool); stream && m.onStream != nil {
		m.onStream(w)
		return
	}
	if m.onFull != nil {
		m.onFull(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output_text": "默认回复"})
}

func newTarget(url, model string, apiType string) *Target {
	return &Target{
		EndpointID: "t", EndpointName: "t", BaseURL: url, APIKey: "k",
		APIType: apiType, Model: model, MaxOut: 256, Timeout: 5 * time.Second, Enabled: true,
	}
}

func TestResponsesUsesCorrectPath(t *testing.T) {
	srv := httptest.NewServer(&responsesMock{})
	defer srv.Close()

	res, cerr := Call(context.Background(), newTarget(srv.URL, "m", config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if cerr != nil {
		t.Fatalf("调用应成功，实际: %v", cerr)
	}
	if res.Content != "默认回复" {
		t.Fatalf("内容不符: %q", res.Content)
	}
}

func TestResponsesSeparatesSystemIntoInstructions(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, map[string]any{"output_text": "ok"})
	}))
	defer srv.Close()

	_, cerr := Call(context.Background(), newTarget(srv.URL, "m", config.APIResponses), Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "你是羽沫老爹"},
			{Role: RoleUser, Content: "在吗"},
		},
	})
	if cerr != nil {
		t.Fatalf("调用应成功，实际: %v", cerr)
	}
	// system 不能混进 input，否则 Responses API 会报 role 非法
	if instr, _ := got["instructions"].(string); !strings.Contains(instr, "羽沫老爹") {
		t.Fatalf("system 应放进 instructions，实际: %v", got["instructions"])
	}
	input, _ := got["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input 里应只剩 user 一条，实际 %d 条: %v", len(input), got["input"])
	}
}

func TestResponsesReadsOutputArray(t *testing.T) {
	// 有些实现不给 output_text，只给 output 数组
	srv := httptest.NewServer(&responsesMock{onFull: func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{
			"output": []map[string]any{
				{"type": "reasoning", "content": []map[string]any{{"type": "summary_text", "text": "想了一下"}}},
				{"type": "message", "role": "assistant", "content": []map[string]any{
					{"type": "output_text", "text": "我\n真的\n不知道"},
				}},
			},
		})
	}})
	defer srv.Close()

	res, cerr := Call(context.Background(), newTarget(srv.URL, "m", config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "你知道不"}},
	})
	if cerr != nil {
		t.Fatalf("调用应成功，实际: %v", cerr)
	}
	if res.Content != "我\n真的\n不知道" {
		t.Fatalf("应从 output 数组里取出文本，实际: %q", res.Content)
	}
	// reasoning 的摘要绝不能被当成它要说的话
	if strings.Contains(res.Content, "想了一下") {
		t.Fatalf("推理摘要不应混入输出: %q", res.Content)
	}
}

func TestResponsesStreamConcatenatesDeltas(t *testing.T) {
	srv := httptest.NewServer(&responsesMock{onStream: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		// 首个 delta 之前先睡一小会儿，否则整个响应在时钟的tick 内就写完了，
		// TTFT 与总延迟都会量成 0，`TTFTMS > 0` 随机失败
		// （-count=200 实测偶发 26 次，与被测逻辑无关）。
		time.Sleep(2 * time.Millisecond)
		for _, d := range []string{"我", "真的", "不", "知道"} {
			_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
				"type":  "response.output_text.delta",
				"delta": d,
			}) + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	}})
	defer srv.Close()

	tgt := newTarget(srv.URL, "m", config.APIResponses)
	tgt.Stream = true
	res, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "你知道不"}},
	})
	if cerr != nil {
		t.Fatalf("流式调用应成功，实际: %v", cerr)
	}
	if res.Content != "我真的不知道" {
		t.Fatalf("流式拼接错误: %q", res.Content)
	}
	if res.TTFTMS <= 0 {
		t.Fatalf("流式应测到首字延迟，实际 %.2f", res.TTFTMS)
	}
}

func TestResponsesStreamFallsBackToCompletedPayload(t *testing.T) {
	// 不发 delta，只在 completed 事件里给完整内容
	srv := httptest.NewServer(&responsesMock{onStream: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"output_text": "牛逼"},
		}) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	}})
	defer srv.Close()

	tgt := newTarget(srv.URL, "m", config.APIResponses)
	tgt.Stream = true
	res, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if cerr != nil {
		t.Fatalf("应能从完成事件里兜底取到内容，实际: %v", cerr)
	}
	if res.Content != "牛逼" {
		t.Fatalf("兜底内容不符: %q", res.Content)
	}
}

func TestResponsesEmptyContentIsFailure(t *testing.T) {
	srv := httptest.NewServer(&responsesMock{onFull: func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{"output_text": "   "})
	}})
	defer srv.Close()

	if _, cerr := Call(context.Background(), newTarget(srv.URL, "m", config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); cerr == nil {
		t.Fatal("空内容必须视为失败，否则上层会误判为「它选择了闭嘴」")
	}
}

func TestResponsesUsageIsCaptured(t *testing.T) {
	srv := httptest.NewServer(&responsesMock{onFull: func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{
			"output_text": "6",
			"usage":       map[string]any{"input_tokens": 1234, "output_tokens": 56},
		})
	}})
	defer srv.Close()

	res, cerr := Call(context.Background(), newTarget(srv.URL, "m", config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if cerr != nil {
		t.Fatalf("调用应成功，实际: %v", cerr)
	}
	if res.PromptTokens != 1234 || res.OutputTokens != 56 {
		t.Fatalf("应捕获用量用于成本核算，实际 %+v", res)
	}
}

func TestChatCompletionsStillDefault(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "好"}}},
		})
	}))
	defer srv.Close()

	// 不填 api_type 时必须走经典路径，老配置不能被改坏
	if _, cerr := Call(context.Background(), newTarget(srv.URL, "m", ""), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); cerr != nil {
		t.Fatalf("默认应走 chat/completions，实际: %v", cerr)
	}
	if path != "/chat/completions" {
		t.Fatalf("默认路径应为 /chat/completions，实际 %s", path)
	}
}
