package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	"testing"
	"time"

	"dadyumo/internal/config"
)

// 生产事故原样（2026-10-04，Packy 中转，HTTP 200 + 正文里一句英文说明）。
// 这句话曾经被当成模型的发言发进群，又被写回记忆自我复制。
const prodRefusal = "The prompt could not be submitted. The prompt contains sensitive words that violate Google's [Generative AI Prohibited Usage] policy."

func TestLooksLikeRefusal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"生产原话", prodRefusal, true},
		{"只有半句", "The prompt could not be submitted.", true},
		{"OpenAI 口径", "Your prompt was blocked due to our usage policies.", true},
		{"OpenRouter 枚举", "blocked by moderation: prohibited_use", true},
		{"Azure finish_reason", `{"content_filter": true}`, true},
		{"中文口径", "输入内容不符合使用规范", true},

		// 正常人话绝不能被误杀——这是这个判据最大的风险。
		{"普通中文", "你又发这个，烦不烦", false},
		{"正常提到敏感二字", "这词敏感得很，别乱说", false},
		{"短回复", "6", false},
		{"纯符号", "...", false},
		{"单个问号", "？", false},
		{"空串", "", false},
		{"中英混排的正常发言", "这个 bug 挺离谱的", false},
		{"短英文（低于字母阈值）", "ok", false},
	}
	for _, c := range cases {
		if got := looksLikeRefusal(c.in); got != c.want {
			t.Errorf("%s: looksLikeRefusal(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// 长英文没有汉字 → 必须判为拒绝。
// 这条兜底是为了漏掉没收录过的措辞：宁可误杀一条长英文，
// 也不能让含「sensitive words」的话进记忆（那个代价是自我复制的循环）。
func TestLongEnglishWithoutCJKIsRefusal(t *testing.T) {
	if !looksLikeRefusal("This is an entirely unrecognised moderation notice from upstream") {
		t.Error("未收录措辞的长英文应判为拒绝")
	}
	if looksLikeRefusal("嗯这个我看看") {
		t.Error("含汉字的正常发言不应被判为拒绝")
	}
}

// 决策协议本身就是纯 ASCII 的。这条是被 toolloop_e2e 的真实失败逼出来的：
// `<json>{"act":"quiet","mem":[]}</json>` 三十多个字母、零个汉字，
// 长得极像英文系统提示。判成拒绝会让每次「模型选择闭嘴」都变成调用失败。
func TestProtocolOutputIsNotRefusal(t *testing.T) {
	for _, s := range []string{
		`<json>{"act":"quiet","mem":[]}` + "\n" + `</json>`,
		`<json>{"act":"say","text":"ok"}</json>`,
	} {
		if looksLikeRefusal(s) {
			t.Errorf("协议输出不应被判为拒绝: %q", s)
		}
	}
}

// chat 协议：HTTP 200 + 正文是那句英文 → 必须判失败。
//
// 这条正是事故现场：状态码 200 让 parsed.Error 分支看不到东西，
// content 非空让 emptyContentErr 也判不出来，于是它作为「成功的回答」
// 一路送到 parse.go 的降级路径，被当成人话发进了群。
func TestChatRefusalInBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": prodRefusal},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 30},
		})
	}))
	defer srv.Close()

	tgt := &Target{
		EndpointID: "packy", EndpointName: "packy", BaseURL: srv.URL, APIKey: "k",
		APIType: config.APIChatCompletions, Model: "ds", MaxOut: 512,
	}
	res, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	if cerr == nil {
		t.Fatalf("被拒绝的正文绝不能作为成功结果返回，res=%+v", res)
	}
	if cerr.Kind != ErrKindRefused {
		t.Errorf("错误分类应为 ErrKindRefused，实际 %v", cerr.Kind)
	}
	// HTTP 200 之后才判定的失败上游同样计费，用量必须带出去
	if cerr.PromptTokens != 120 || cerr.OutputTokens != 30 {
		t.Errorf("应带回真实用量，实际 prompt=%d output=%d", cerr.PromptTokens, cerr.OutputTokens)
	}
}

// responses 协议走的是另一个读取分支，得单独覆盖。
func TestResponsesRefusalInBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "completed",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": prodRefusal}}}},
			"usage": map[string]any{"input_tokens": 90, "output_tokens": 20},
		})
	}))
	defer srv.Close()

	tgt := newResponsesTarget(srv.URL)
	if _, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	}); cerr == nil || cerr.Kind != ErrKindRefused {
		t.Fatalf("responses 分支也应判为 ErrKindRefused，实际 %v", cerr)
	}
}

// 工具轮优先于拒绝判定：模型决定调工具时正文为空是正常的，
// 绝不能被新加的这条误伤。
func TestToolCallNotTreatedAsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "",
					"tool_calls": []map[string]any{{
						"id": "c1", "type": "function",
						"function": map[string]any{"name": "search", "arguments": `{"q":"x"}`},
					}}},
			}},
		})
	}))
	defer srv.Close()

	tgt := &Target{
		EndpointID: "packy", EndpointName: "packy", BaseURL: srv.URL, APIKey: "k",
		APIType: config.APIChatCompletions, Model: "ds", MaxOut: 512,
	}
	res, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	if cerr != nil {
		t.Fatalf("工具轮不应判为失败: %v", cerr)
	}
	if len(res.ToolCalls) != 1 {
		t.Errorf("应拿到 1 个工具调用，实际 %d", len(res.ToolCalls))
	}
}

// 上层真正依赖的那句话：整条链都拒绝时，调用方能一眼分辨
// 「这是内容的问题」而不是「服务挂了」。
func TestRouterAllRefusedIsDistinguishable(t *testing.T) {
	m := newMock()
	m.on("ds", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": prodRefusal},
			}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("packy", srv.URL, "ds")+`]`)
	r := NewRouter(st)

	_, err := r.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	if err == nil {
		t.Fatal("全部被拒必须返回错误")
	}
	if !errors.Is(err, ErrAllRefused) {
		t.Errorf("应能用 errors.Is 判定为 ErrAllRefused，实际: %v", err)
	}
	if m.count("ds") == 0 {
		t.Error("至少应尝试一次")
	}

	// 关键：被内容审核拒绝**不能**推进冷却/降权。
	// 否则「有人在群里说了句敏感词」就会把健康的主力踢进指数退避（5s→5min），
	// 代价和故障完全不成比例。
	for _, tgt := range r.targets {
		tgt.mu.Lock()
		fails, cooling := tgt.h.Fails, tgt.h.Cooling(time.Now())
		tgt.mu.Unlock()
		if fails != 0 {
			t.Errorf("内容被拒不应计入目标失败计数，实际 fails=%d", fails)
		}
		if cooling {
			t.Error("内容被拒不应让目标进入冷却")
		}
	}
}

// 内容被拒时要换目标：不同厂商审核口径不同，换一家可能就过了。
// 这一条是生产上真正救回那条链路的东西。
func TestRouterRefusedFailsOverToNextTarget(t *testing.T) {
	m := newMock()
	m.on("strict", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": prodRefusal},
			}},
		})
	})
	// 宽松的那家正常回答
	m.on("lax", func(w http.ResponseWriter, model string, stream bool) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "说人话"},
			}},
		})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	// strict 给高 priority，确保它被先选中——这条测的是「被拒之后会换人」，
	// 不是「谁先被选」。
	st := testStore(t, `"endpoints":[{"id":"A","name":"A","base_url":"`+srv.URL+`","api_key":"k","enabled":true,"timeout_ms":5000,"models":[`+
		`{"id":"strict","enabled":true,"max_ctx":8000,"max_out":512,"priority":10},`+
		`{"id":"lax","enabled":true,"max_ctx":8000,"max_out":512,"priority":1}]}]`)
	r := NewRouter(st)

	res, err := r.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	if err != nil {
		t.Fatalf("应换到宽松目标成功，实际: %v", err)
	}
	if res.Content != "说人话" {
		t.Errorf("应拿到宽松目标的回答，实际 %q", res.Content)
	}
	if m.count("strict") == 0 {
		t.Error("应先尝试严格目标，再因被拒换到宽松目标")
	}
}
