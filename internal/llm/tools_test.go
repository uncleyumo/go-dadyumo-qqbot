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

// 工具调用链路的验收标准。
//
// 这套东西存在的唯一理由：让模型能主动要看的东西（表情包池），
// 但**工具轮绝不发送消息**。所以最要紧的两条是：
//   1. 工具轮不能被误判成「模型返回空内容」——那会触发重试，白烧钱
//   2. 工具轮的空 Content 是正常的，不能走到发言路径上去

func testTarget(baseURL, apiType string) *Target {
	return &Target{
		EndpointID: "ep-test",
		BaseURL:    baseURL,
		APIKey:     "test-key",
		APIType:    apiType,
		Model:      "test-model",
		MaxOut:     512,
		Enabled:    true,
		Timeout:    5 * time.Second,
	}
}

func testView(apiType string) TargetView {
	return TargetView{
		Model:   "test-model",
		BaseURL: "http://127.0.0.1",
		APIType: apiType,
		MaxOut:  512,
	}
}

// 实测 deepseek-flash 的形态：output=[reasoning, message, function_call]
const respToolWithMessage = `{
 "status":"completed",
 "output":[
  {"type":"reasoning","summary":[]},
  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"让我看看池子里有啥"}]},
  {"type":"function_call","call_id":"call_abc","name":"browse_meme_pool","arguments":"{\"limit\":5}"}
 ],
 "usage":{"input_tokens":120,"output_tokens":30}
}`

// 实测 mimo-v2.6-flash 的形态：output=[reasoning, function_call]，没有 message
const respToolOnly = `{
 "status":"completed",
 "output":[
  {"type":"reasoning","summary":[]},
  {"type":"function_call","call_id":"call_xyz","name":"browse_meme_pool","arguments":"{}"}
 ],
 "usage":{"input_tokens":98,"output_tokens":22}
}`

func TestResponsesToolCallParsed(t *testing.T) {
	var parsed responsesResponse
	if err := json.Unmarshal([]byte(respToolOnly), &parsed); err != nil {
		t.Fatal(err)
	}
	calls := collectFunctionCalls(&parsed)
	if len(calls) != 1 {
		t.Fatalf("应解析出 1 个工具调用，实际 %d", len(calls))
	}
	if calls[0].Name != "browse_meme_pool" {
		t.Errorf("工具名不对: %q", calls[0].Name)
	}
	if calls[0].ID != "call_xyz" {
		t.Errorf("call_id 不对: %q", calls[0].ID)
	}
	if calls[0].Arguments != "{}" {
		t.Errorf("arguments 应原样保留: %q", calls[0].Arguments)
	}
	// 工具轮的正文本来就不该有内容
	if got := collectResponsesOutput(&parsed); got != "" {
		t.Errorf("工具轮不应有正文，实际 %q", got)
	}
}

// message + function_call 同时存在时，两者都要能取到
func TestResponsesToolAndMessageBothParsed(t *testing.T) {
	var parsed responsesResponse
	if err := json.Unmarshal([]byte(respToolWithMessage), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := collectResponsesOutput(&parsed); got != "让我看看池子里有啥" {
		t.Errorf("正文没解析出来: %q", got)
	}
	if len(collectFunctionCalls(&parsed)) != 1 {
		t.Error("工具调用应被解析出来")
	}
}

// call_id 缺失时合成一个稳定的 id，否则上层没法回填结果
func TestResponsesToolCallIDFallback(t *testing.T) {
	const body = `{"output":[{"type":"function_call","name":"browse_meme_pool","arguments":"{}"}]}`
	var parsed responsesResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	calls := collectFunctionCalls(&parsed)
	if len(calls) != 1 || calls[0].ID == "" {
		t.Fatalf("缺 call_id 时应合成一个: %#v", calls)
	}
	if !strings.HasPrefix(calls[0].ID, "call_") {
		t.Errorf("合成 id 应带前缀便于识别: %q", calls[0].ID)
	}
}

// 没有名字的条目不是可执行调用，跳过而不是报给上层一个空工具
func TestResponsesToolCallWithoutNameSkipped(t *testing.T) {
	const body = `{"output":[{"type":"function_call","arguments":"{}"}]}`
	var parsed responsesResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := collectFunctionCalls(&parsed); len(got) != 0 {
		t.Errorf("无名的条目应被跳过: %#v", got)
	}
}

// 端到端：工具轮必须返回 Result 而不是错误。
// 这是最容易出错的地方——正文为空，先判空就会走 emptyContentErr 触发重试。
func TestToolRoundNotTreatedAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respToolOnly))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, cerr := Call(ctx, testTarget(srv.URL, config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "看图"}},
		Tools: []Tool{{Name: "browse_meme_pool", Description: "浏览表情包池",
			Parameters: map[string]any{"type": "object"}}},
	})
	if cerr != nil {
		t.Fatalf("工具轮不该被当成失败: %v", cerr)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("应带 1 个工具调用，实际 %d", len(res.ToolCalls))
	}
	if res.Content != "" {
		t.Errorf("工具轮正文应为空: %q", res.Content)
	}
}

// 真的空响应（没有工具、也没有正文）仍然必须报错——不能为了放行工具轮把校验放松了
func TestTrulyEmptyStillErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"incomplete","output":[{"type":"reasoning"}]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, cerr := Call(ctx, testTarget(srv.URL, config.APIResponses), Request{
		Messages: []Message{{Role: RoleUser, Content: "在吗"}},
	})
	if cerr == nil {
		t.Fatal("纯空响应必须报错，否则会静默当成模型什么都没说")
	}
}

// 工具定义必须排在 instructions 之后 input 之前 —— 前缀缓存按字节匹配，键顺序变了缓存全废
func TestResponsesToolsKeyOrderForCache(t *testing.T) {
	view := testView(config.APIResponses)
	view.BaseURL = "http://x"
	_, raw, cerr := buildPayload(view, Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "稳定人设前缀"},
			{Role: RoleUser, Content: "动态尾巴"},
		},
		Tools: []Tool{{Name: "t1", Description: "d1", Parameters: map[string]any{"type": "object"}}},
	}, 512)
	if cerr != nil {
		t.Fatal(cerr)
	}
	s := string(raw)
	iInstr := strings.Index(s, `"instructions"`)
	iTools := strings.Index(s, `"tools"`)
	iInput := strings.Index(s, `"input"`)
	if iInstr < 0 || iTools < 0 || iInput < 0 {
		t.Fatalf("报文缺字段: %s", s)
	}
	if !(iInstr < iTools && iTools < iInput) {
		t.Errorf("键顺序必须是 instructions < tools < input，实际 %d/%d/%d\n%s",
			iInstr, iTools, iInput, s)
	}
	// 工具定义要带上 type:function 外壳，否则上游不认
	if !strings.Contains(s, `"type":"function"`) {
		t.Errorf("Responses 方言的 tools 需要 type:function 外壳: %s", s)
	}
}

// 不给工具时不能出现 tools 键——多一个键就多一份字节，白费缓存
func TestNoToolsNoToolsField(t *testing.T) {
	view := testView(config.APIResponses)
	view.BaseURL = "http://x"
	_, raw, cerr := buildPayload(view, Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, 512)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Errorf("无工具时不应出现 tools 键: %s", raw)
	}
}

// 工具结果回填：responses 用 function_call_output，chat 用 role=tool + tool_call_id
func TestToolResultWireShapes(t *testing.T) {
	toolMsg := Message{Role: RoleTool, CallID: "call_abc", Content: `[{"id":"m1","desc":"狗头"}]`}

	r := toolMsg.toResponsesWire()
	if r.Type != "function_call_output" || r.CallID != "call_abc" {
		t.Errorf("responses 方言的工具结果形状不对: %#v", r)
	}

	c := toolMsg.toChatWire()
	if c.Role != "tool" {
		t.Errorf("chat 方言的工具结果 role 应为 tool，实际 %q", c.Role)
	}
}

// 序列化后的字节形状必须严格对。
//
// 这条是 2026-10-01 生产事故换来的：工具轮 100% 400，两处字段都写错——
// function_call_output 带了个空的 "role"，正文写成 content 而规范里叫 output。
// 上游只回一句 "Invalid Responses API request"，光看代码怎么都看不出问题，
// 只有把 JSON 拿出来逐字比才抓得到。所以这里断言的是**序列化结果**，
// 不是 Go 结构体字段——后者对 omitempty 之类的坑是瞎的。
func TestResponsesToolFeedbackSerializedBytes(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "系统提示"},
		{Role: RoleUser, Content: "在吗"},
		{Role: RoleAssistant, CallID: "call_1", ToolName: "browse_meme_pool", Content: `{"mood":"无语"}`},
		{Role: RoleTool, CallID: "call_1", Content: "1. 无语的猫"},
	}
	_, raw, err := buildPayload(
		TargetView{BaseURL: "https://x/v1", Model: "m", APIType: "responses"},
		Request{Messages: msgs, Tools: []Tool{{Name: "browse_meme_pool"}}}, 256)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	for _, bad := range []string{`"role":""`, `"role": "", `, `"content":"{\"mood\"`, `"content":"1. 无语的猫"`} {
		if strings.Contains(body, bad) {
			t.Errorf("请求体里不该出现 %s：%s", bad, body)
		}
	}
	for _, want := range []string{
		`{"type":"function_call","call_id":"call_1","name":"browse_meme_pool","arguments":"{\"mood\":\"无语\"}"}`,
		`{"type":"function_call_output","call_id":"call_1","output":"1. 无语的猫"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s\n实际：%s", want, body)
		}
	}
}

// 普通消息即使正文为空也必须带 content：interface 的 omitempty 判的是 nil，
// 不是空串。这条钉住那个很容易被「顺手优化」掉的细节。
func TestResponsesEmptyContentStillSerialized(t *testing.T) {
	_, raw, err := buildPayload(
		TargetView{BaseURL: "https://x/v1", Model: "m", APIType: "responses"},
		Request{Messages: []Message{{Role: RoleUser, Content: ""}}}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `{"role":"user","content":""}`) {
		t.Errorf("空正文也要带 content：%s", raw)
	}
}

// chat 方言同理：assistant 要带 tool_calls，tool 结果要带 tool_call_id。
// 缺 tool_call_id 的报错是 "tool messages must include a non-empty string tool_call_id"。
func TestChatToolFeedbackSerializedBytes(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "在吗"},
		{Role: RoleAssistant, CallID: "call_1", ToolName: "f", Content: `{"x":"1"}`},
		{Role: RoleTool, CallID: "call_1", Content: "结果"},
	}
	_, raw, err := buildPayload(
		TargetView{BaseURL: "https://x/v1", Model: "m", APIType: "chat_completions"},
		Request{Messages: msgs, Tools: []Tool{{Name: "f"}}}, 256)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":\"1\"}"}}]`,
		`{"role":"tool","content":"结果","tool_call_id":"call_1"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s\n实际：%s", want, body)
		}
	}
}

// 模型发起的那次调用要原样回传，否则它下一轮看不到自己调过什么，会重复调
func TestAssistantToolCallRoundTrip(t *testing.T) {
	m := Message{Role: RoleAssistant, CallID: "call_x", ToolName: "browse_meme_pool",
		Content: `{"limit":5}`}
	r := m.toResponsesWire()
	if r.Type != "function_call" || r.Name != "browse_meme_pool" || r.Arguments != `{"limit":5}` {
		t.Errorf("function_call 回传形状不对: %#v", r)
	}
	// 普通 assistant 消息不能被误当成函数调用
	plain := Message{Role: RoleAssistant, Content: "好的"}.toResponsesWire()
	if plain.Type == "function_call" {
		t.Error("普通 assistant 消息不该被编码成 function_call")
	}
}

func TestChatToolCallsParsed(t *testing.T) {
	const body = `{"choices":[{"message":{"role":"assistant","content":"",
	 "tool_calls":[{"id":"call_1","type":"function",
	 "function":{"name":"browse_meme_pool","arguments":"{\"limit\":3}"}}]}}]}`
	var parsed chatResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	calls := collectChatToolCalls(&parsed.Choices[0])
	if len(calls) != 1 || calls[0].Name != "browse_meme_pool" {
		t.Fatalf("chat 方言工具调用解析失败: %#v", calls)
	}
	if calls[0].Arguments != `{"limit":3}` {
		t.Errorf("arguments 应原样保留: %q", calls[0].Arguments)
	}
}