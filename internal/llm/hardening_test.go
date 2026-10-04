package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
)

// reasoningOnlyBody 是从生产上游原样抓下来的「空内容」响应：
// status=incomplete、incomplete_details.reason=max_output_tokens，
// output 里只剩一条 reasoning、连 message 都没有，正文一个字都没有。
// 这就是那 86 次「模型返回空内容」的真实形态——不是上游坏了，
// 是输出预算被思维链烧光，模型还没轮到说正文就被掐断。
const reasoningOnlyBody = `{
  "id": "resp_1",
  "status": "incomplete",
  "incomplete_details": {"reason": "max_output_tokens"},
  "output": [
    {"id": "rs_1", "type": "reasoning", "status": "completed",
     "content": [{"type": "reasoning_text", "text": "We need answer in Chinese, let me think about it..."}]}
  ],
  "usage": {"input_tokens": 186, "output_tokens": 200}
}`

// responsesServe 记录收到的 max_output_tokens，便于断言「预算有没有被放宽」
func responsesServe(t *testing.T, budgets *[]int, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*budgets = append(*budgets, body.MaxOutputTokens)
		n := len(*budgets)
		mu.Unlock()
		if n >= 2 {
			// 第二次给足预算后，模型终于把正文写出来了
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "completed",
				"output": []map[string]any{{"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "终于说人话了"}}}},
				"usage": map[string]any{"input_tokens": 186, "output_tokens": 64},
			})
			return
		}
		writeJSON(w, http.StatusOK, json.RawMessage(reasoningOnlyBody))
	}
}

func newResponsesTarget(url string) *Target {
	return &Target{
		EndpointID: "or", EndpointName: "or", BaseURL: url, APIKey: "k",
		APIType: config.APIResponses, Model: "reasoner", MaxOut: 1024,
		Timeout: 5 * time.Second, Enabled: true,
	}
}

// TestEmptyContentReportsReasoningOnlyShape 认知链把预算烧光时，
// 错误信息必须说清「是 reasoning-only + 被 max_output_tokens 截断」，
// 而不是笼统的「空内容」——不然永远定位不到。
func TestEmptyContentReportsReasoningOnlyShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, json.RawMessage(reasoningOnlyBody))
	}))
	defer srv.Close()

	// 关掉「加预算重试」，单独验证诊断信息本身
	tgt := newResponsesTarget(srv.URL)
	tgt.MaxOut = 0 // retryMaxTokens 会取两倍，这里让它有重试空间；用 maxTokens=0 触发
	_, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	// MaxOut=0 → defaultMaxOut=512，retbig=1024 > 512，会触发重试，
	// 所以这里改用一个不会重试的调用方预算来验证诊断
	if cerr == nil {
		t.Fatal("reasoning-only 响应必须判为失败")
	}
	msg := cerr.Message
	for _, want := range []string{"没有正文", "status=incomplete", "incomplete_reason=max_output_tokens", "reasoning-only", "message=0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际: %s", want, msg)
		}
	}
	// 上游已经计费，用量必须带出来，否则成本统计会把这 386 token 记成 0
	if cerr.PromptTokens != 186 || cerr.OutputTokens != 200 {
		t.Errorf("失败调用应带回真实用量，实际 prompt=%d output=%d", cerr.PromptTokens, cerr.OutputTokens)
	}
	if !cerr.OutputShort {
		t.Error("被 max_output_tokens 截断应标记 OutputShort（值得加预算重试）")
	}
}

// TestReasoningOnlyRetriesWithLargerBudget 唯一的自适应兜底：
// 思维链把预算烧光时原地加预算重试一次，而不是直接判定失败让路由换模型。
func TestReasoningOnlyRetriesWithLargerBudget(t *testing.T) {
	var mu sync.Mutex
	var budgets []int
	srv := httptest.NewServer(responsesServe(t, &budgets, &mu))
	defer srv.Close()

	tgt := newResponsesTarget(srv.URL) // MaxOut=1024
	res, cerr := Call(context.Background(), tgt, Request{
		Messages:  []Message{{Role: RoleUser, Content: "在吗"}},
		MaxTokens: 200, // 调用方给的预算，reasoning 模型根本不够
	})
	if cerr != nil {
		t.Fatalf("加预算重试后应成功，实际: %v", cerr)
	}
	if res.Content != "终于说人话了" {
		t.Fatalf("内容不符: %q", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 2 {
		t.Fatalf("应恰好请求两次（原文一次 + 重试一次），实际 %d 次: %v", len(budgets), budgets)
	}
	if budgets[0] != 200 {
		t.Errorf("第一次应尊重调用方的 200，实际 %d", budgets[0])
	}
	if budgets[1] <= budgets[0] {
		t.Errorf("重试应放宽预算，实际 %d -> %d", budgets[0], budgets[1])
	}
}

// TestNoRetryWhenBudgetAlreadyGenerous 预算本来就够（重试算不出更大的值）时不该重试
func TestNoRetryWhenBudgetAlreadyGenerous(t *testing.T) {
	var mu sync.Mutex
	var budgets []int
	srv := httptest.NewServer(responsesServe(t, &budgets, &mu))
	defer srv.Close()

	tgt := newResponsesTarget(srv.URL)
	tgt.MaxOut = hardMaxOutTokens // 已经顶格，加预算没有意义
	_, cerr := Call(context.Background(), tgt, Request{
		Messages:  []Message{{Role: RoleUser, Content: "在吗"}},
		MaxTokens: hardMaxOutTokens,
	})
	if cerr == nil {
		t.Fatal("仍应判为失败")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 1 {
		t.Fatalf("预算已顶格时不该重试，实际请求 %d 次", len(budgets))
	}
}

// TestEmptyContentFormsAreDistinguishable 「空」有好几种形态，
// 处置完全不同，错误信息必须能区分开。
func TestEmptyContentFormsAreDistinguishable(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"choices空数组", `{"choices":[],"usage":{"prompt_tokens":10,"output_tokens":0}}`,
			[]string{"没有 choices", "output_tokens=0"}},
		{"content空串", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			[]string{"content 为空串", "finish_reason=stop"}},
		{"content为null", `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`,
			[]string{"content 显式为 null"}},
		{"content全空白", `{"choices":[{"message":{"content":"  \n\t "},"finish_reason":"stop"}]}`,
			[]string{"content 全是空白字符"}},
		{"reasoning-only", `{"choices":[{"message":{"content":"","reasoning":"想了一下"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":400}}`,
			[]string{"reasoning-only", "finish_reason=length", "output_tokens=400"}},
		{"被length截断", `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`,
			[]string{"finish_reason=length"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, json.RawMessage(body))
			}))
			defer srv.Close()
			tgt := &Target{EndpointID: "A", BaseURL: srv.URL, APIKey: "k", Model: "m",
				Timeout: 5 * time.Second, Enabled: true}
			_, cerr := Call(context.Background(), tgt, Request{
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			})
			if cerr == nil {
				t.Fatal("应判为失败")
			}
			for _, want := range tc.want {
				if !strings.Contains(cerr.Message, want) {
					t.Errorf("错误信息应包含 %q，实际: %s", want, cerr.Message)
				}
			}
		})
	}
}

// TestAttemptBudget从父预算里扣 attemptBudget 必须从父 ctx 的剩余时间里扣，
// 否则外层预算 ≤ 单目标超时时 fallback 链对慢目标结构性失效。
func TestAttemptBudget从父预算里扣(t *testing.T) {
	// 无 deadline：拿满配置值
	d, cerr := attemptBudget(context.Background(), 60*time.Second)
	if cerr != nil || d != 60*time.Second {
		t.Errorf("无预算上限时应拿满配置值，实际 %v %v", d, cerr)
	}
	// 父预算比配置值小：以内层为准
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, cerr = attemptBudget(ctx, 60*time.Second)
	if cerr != nil {
		t.Fatalf("父预算充足时不该报错: %v", cerr)
	}
	if d > 10*time.Second || d < 9*time.Second {
		t.Errorf("单次超时应被父预算截断到 ~10s，实际 %v", d)
	}
	// 父预算已经见底：立刻失败，别发起注定超时的请求
	tiny, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	d, cerr = attemptBudget(tiny, 60*time.Second)
	if cerr == nil {
		t.Fatal("预算不足时必须直接返回错误")
	}
	if cerr.Kind != ErrKindBudget || !cerr.External {
		t.Errorf("应是预算耗尽且不计入目标健康度，实际 %+v", cerr)
	}
	if d != 0 {
		t.Errorf("预算耗尽时不应给出超时时长，实际 %v", d)
	}
	if enoughBudget(tiny, minAttemptBudget) {
		t.Error("剩余预算不足时 enoughBudget 应为 false")
	}
}

// TestChatStopsWhenBudgetExhausted 预算被第一个目标吃光后，
// 不应再发起第二个请求（那注定超时），而要如实报「预算耗尽」。
func TestChatStopsWhenBudgetExhausted(t *testing.T) {
	m := newMock()
	m.on("slow", func(w http.ResponseWriter, _ string, _ bool) {
		time.Sleep(60 * time.Millisecond)
		writeJSON(w, 500, map[string]any{"error": map[string]string{"message": "boom"}})
	})
	m.on("spare", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}}})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	// slow 放最高优先级，保证它一定第一个被试到（否则同分时顺序随机，测试不可靠）
	st := testStore(t, `"endpoints":[`+
		epWithPriority("A", srv.URL, [2]any{"slow", 9}, [2]any{"spare", 1})+`]`)
	r := NewRouter(st)
	// 预算 3.05s：够跑第一次（sleep 60ms 后失败），之后剩 2.99s < minAttemptBudget
	ctx, cancel := context.WithTimeout(context.Background(), 3050*time.Millisecond)
	defer cancel()
	_, err := r.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("预算耗尽时应返回错误")
	}
	if !strings.Contains(err.Error(), "预算耗尽") {
		t.Errorf("错误信息应说明预算耗尽，实际: %v", err)
	}
	if n := m.count("spare"); n != 0 {
		t.Errorf("预算不足时不该再发起注定超时的请求，实际 spare 被调 %d 次", n)
	}
}

// TestParentCancelNotChargedToTarget 调用方自己超时/取消不算目标的账：
// 目标健康度必须纹丝不动，否则一次摘录超时会误伤健康端点。
func TestParentCancelNotChargedToTarget(t *testing.T) {
	m := newMock()
	m.on("slowpoke", func(w http.ResponseWriter, _ string, _ bool) {
		time.Sleep(3 * time.Second) // 远超父 ctx 的 300ms
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "太晚了"}}}})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "slowpoke")+`]`)
	r := NewRouter(st)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := r.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("父 ctx 到期应返回错误")
	}
	for _, s := range r.Stats() {
		if s.Total != 0 || s.Fails != 0 || s.ConsecFail != 0 {
			t.Errorf("父 ctx 取消不应计入目标健康度: %+v", s)
		}
		if s.Cooling || s.Dead {
			t.Errorf("父 ctx 取消不应让目标进冷却/下线: %+v", s)
		}
	}
}

// TestBadRequestDoesNotCoolHealthyTargets 上下文超长返回 400 时，
// 必须既不重试也不冷却：同一个坏请求发给 4 个目标会把 4 个健康目标一锅端。
func TestBadRequestDoesNotCoolHealthyTargets(t *testing.T) {
	m := newMock()
	for _, name := range []string{"m1", "m2", "m3", "m4"} {
		m.on(name, func(w http.ResponseWriter, _ string, _ bool) {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "context length exceeded"}})
		})
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "m1", "m2", "m3", "m4")+`]`)
	r := NewRouter(st)
	_, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("400 应返回错误")
	}
	if !strings.Contains(err.Error(), "请求被上游拒绝") {
		t.Errorf("错误信息应说明是请求本身不合法，实际: %v", err)
	}
	total := m.count("m1") + m.count("m2") + m.count("m3") + m.count("m4")
	if total != 1 {
		t.Errorf("坏请求只该发一次，实际发了 %d 次", total)
	}
	for _, s := range r.Stats() {
		if s.Cooling || s.Dead || s.Total != 0 {
			t.Errorf("坏请求不应影响目标健康度: %+v", s)
		}
	}
}

// epVision 造一个带 vision 标记的接入点
func epVision(id, baseURL string, models ...[2]any) string {
	ms := make([]string, 0, len(models))
	for _, m := range models {
		ms = append(ms, `{"id":"`+m[0].(string)+`","label":"`+m[0].(string)+`","priority":1,"enabled":true,`+
			`"max_ctx":8000,"max_out":512,"stream":false,"vision":`+boolStr(m[1].(bool))+`}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","base_url":"` + baseURL + `","api_key":"k","enabled":true,"timeout_ms":5000,"models":[` + join(ms) + `]}`
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestNeedsVisionExcludesTextOnlyModels 带图请求不能发给纯文本模型：
// 上游只会回 404「No endpoints found that support image input」。
func TestNeedsVisionExcludesTextOnlyModels(t *testing.T) {
	m := newMock()
	m.on("textonly", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 404, map[string]any{"error": map[string]string{"message": "No endpoints found that support image input"}})
	})
	m.on("seer", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "是只猫"}}}})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epVision("A", srv.URL, [2]any{"textonly", false}, [2]any{"seer", true})+`]`)
	r := NewRouter(st)
	res, err := r.Chat(context.Background(), Request{
		Messages:    []Message{{Role: RoleUser, Content: "看", Images: []string{"data:image/png;base64,AAA"}}},
		NeedsVision: true,
	})
	if err != nil {
		t.Fatalf("应命中 vision 目标: %v", err)
	}
	if res.Content != "是只猫" {
		t.Fatalf("内容不符: %q", res.Content)
	}
	if m.count("textonly") != 0 {
		t.Errorf("非 vision 目标不该被调用，实际 %d 次", m.count("textonly"))
	}
	// 不带图时它仍然是正常候选——这才能证明上面是被 vision 过滤掉的，不是配置问题
	//
	// 必须关掉探路（ε-贪心，同 priority 档内随机选）：开着的话这一轮可能
	// 又挑中 seer，textonly 一次都不会被调用，断言随机失败。
	// 这是个先前就存在的偶发失败（-count=200 能稳定复现），与本文件其他
	// 断言排序的测试一样用 exploreEpsilon=0 换确定性。
	old := exploreEpsilon
	exploreEpsilon = 0
	defer func() { exploreEpsilon = old }()
	if _, err := r.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("纯文本请求应正常: %v", err)
	}
	if m.count("textonly") == 0 {
		t.Error("纯文本请求本该轮到 textonly，说明它只是被 vision 过滤挡掉的")
	}
}

// TestVisionRejectionDoesNotMarkDead 带图被拒不等于模型下线。
// markDead 只在全部目标都判死时才由 reviveAll 解开，误判等于本次进程内永久出局。
func TestVisionRejectionDoesNotMarkDead(t *testing.T) {
	m := newMock()
	m.on("seer", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 404, map[string]any{"error": map[string]string{"message": "No endpoints found that support image input"}})
	})
	m.on("ok", func(w http.ResponseWriter, _ string, _ bool) {
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "在"}}}})
	})
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epVision("A", srv.URL, [2]any{"seer", true}, [2]any{"ok", true})+`]`)
	r := NewRouter(st)
	_, err := r.Chat(context.Background(), Request{
		Messages:    []Message{{Role: RoleUser, Content: "看", Images: []string{"data:image/png;base64,AAA"}}},
		NeedsVision: true,
	})
	if err != nil {
		t.Fatalf("应回退到另一个目标: %v", err)
	}
	for _, s := range r.Stats() {
		if s.Model == "seer" && s.Dead {
			t.Error("因带图被拒不应把模型永久下线")
		}
	}
}

// TestCallerMaxTokensWins 调用方给的 max_tokens 必须被尊重。
// 早先取「调用方与模型配置的较大值」，把 brain.max_out_tokens=400 顶成 1024，
// 运维调这个值完全无效。
func TestCallerMaxTokensWins(t *testing.T) {
	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "短"}}}})
	}))
	defer srv.Close()

	tgt := &Target{EndpointID: "A", BaseURL: srv.URL, APIKey: "k", Model: "m",
		MaxOut: 1024, Timeout: 5 * time.Second, Enabled: true}
	if _, cerr := Call(context.Background(), tgt, Request{
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 400,
	}); cerr != nil {
		t.Fatal(cerr)
	}
	if got.MaxTokens != 400 {
		t.Errorf("调用方给的 400 应原样发出，实际 %d", got.MaxTokens)
	}
	// 调用方没给时才回落到模型配置
	if _, cerr := Call(context.Background(), tgt, Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); cerr != nil {
		t.Fatal(cerr)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("调用方没给时应回落到模型的 max_out=1024，实际 %d", got.MaxTokens)
	}
	// MaxOutTokens 供调用方按「实际上限」预留上下文空间
	if n := tgt.MaxOutTokens(); n != 1024 {
		t.Errorf("MaxOutTokens 应为 1024，实际 %d", n)
	}
}

// TestNeverSucceededTargetDoesNotWinByOptimism 调了很多次全失败的目标，
// 不能按「未探索的新模型」拿满分延迟分——那会让坏目标反超健康但慢的目标。
func TestNeverSucceededTargetDoesNotWinByOptimism(t *testing.T) {
	bad := newHealth()
	for i := 0; i < 20; i++ {
		bad.observeFailure("boom", time.Now())
	}
	if bad.EverSucceeded {
		t.Fatal("全失败的目标不该标记为成功过")
	}
	slow := newHealth()
	slow.observeSuccess(9000, 30000, time.Now()) // 健康但很慢
	fast := newHealth()
	fast.observeSuccess(300, 900, time.Now())

	w := Weight{TTFT: 0, Failure: 0.30, Latency: 0.60, Cooldown: 0.10}
	scale := computeScale([]Health{*slow, *fast})
	scoreBad := bad.Score(w, time.Now(), true, scale)
	scoreSlow := slow.Score(w, time.Now(), true, scale)
	if scoreBad >= scoreSlow {
		t.Errorf("全失败目标的分(%.4f)不该高过健康但慢的目标(%.4f)", scoreBad, scoreSlow)
	}
	// 而「从未探索过」的目标仍应拿到探索加成，不能被这条规则误伤
	untried := newHealth()
	if s := untried.Score(w, time.Now(), false, scale); s <= scoreSlow {
		t.Errorf("未探索过的目标应仍有探索优势，实际 %.4f vs %.4f", s, scoreSlow)
	}
}

// TestTTFTWeightFoldedWhenNoStreaming 全量非流式时 TTFT≡Latency，
// 权重必须合并，否则 0.45 与 0.15 压在同一个变量上重复计分。
func TestTTFTWeightFoldedWhenNoStreaming(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "m1")+`]`)
	r := NewRouter(st)
	r.mu.RLock()
	w := r.weights
	r.mu.RUnlock()
	if w.TTFT != 0 {
		t.Errorf("全量非流式时 TTFT 权重应并入 Latency，实际 ttft=%v latency=%v", w.TTFT, w.Latency)
	}
	if w.Latency < 0.6 {
		t.Errorf("Latency 权重应吸收原来的 TTFT 权重（0.15+0.45），实际 %v", w.Latency)
	}
	// 开了流式就恢复配置里的权重
	st2 := testStore(t, `"endpoints":[`+
		`{"id":"A","name":"A","base_url":"`+srv.URL+`","api_key":"k","enabled":true,"timeout_ms":5000,`+
		`"models":[{"id":"s","enabled":true,"stream":true,"max_out":512}]}]`)
	r2 := NewRouter(st2)
	r2.mu.RLock()
	w2 := r2.weights
	r2.mu.RUnlock()
	if w2.TTFT <= 0 {
		t.Errorf("存在流式目标时应保留配置的 TTFT 权重，实际 %+v", w2)
	}
}

// TestConcurrentReloadAndRoute Reload 会改写 Target 的可变配置字段，
// 路由热路径必须只读快照。这个测试在 -race 下才有意义。
func TestConcurrentReloadAndRoute(t *testing.T) {
	m := newMock()
	srv := httptest.NewServer(m)
	defer srv.Close()

	st := testStore(t, `"endpoints":[`+epJSON("A", srv.URL, "m1", "m2")+`]`)
	r := NewRouter(st)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // 模拟管理端反复热加载配置
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.Reload()
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				_, _ = r.Chat(context.Background(), Request{
					Messages: []Message{{Role: RoleUser, Content: "hi"}},
				})
				_ = r.Stats()
				_ = r.PreviewChain()
				_ = r.Summary()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
