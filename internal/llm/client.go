// Package llm 实现中央调用器：把多个「接入点 × 模型」封装成统一的调用池，
// 按首字延迟、成功率、端到端延迟与冷却状态动态排序，保证请求尽可能被消费。
//
// 协议方言：OpenAI 生态现在同时存在 chat/completions 与 responses 两套接口，
// 路径与报文结构都不同（一个用 messages、一个用 input+instructions）。
// 这里按 Target.APIType 分流，让上层只面对一种 Request。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

// Role 消息角色
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool" // 工具执行结果回填
)

// defaultMaxOut 调用方与模型配置都没给 max_tokens 时的兜底输出上限
const defaultMaxOut = 512

// hardMaxOutTokens 「输出预算被思维链吃光」后原地重试时的预算封顶。
// 不封顶的话一次重试可能变成一次几分钟的长跑，比失败还糟。
const hardMaxOutTokens = 8192

// minAttemptBudget 一次尝试至少要有的预算。低于这个数模型连首字都吐不出来，
// 发起请求只是白白消耗一轮重试机会，不如立刻把「预算耗尽」告诉调用方。
const minAttemptBudget = 3 * time.Second

// Message 对话消息。
// Images 非空时表示这条消息带图（data URI），序列化时按协议方言转成多模态 parts。
type Message struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"-"` // data:image/...;base64,...
	// CallID 非空表示这条与某次工具调用相关：role=tool 时它是「这次调用的结果」，
	// role=assistant 时它是「模型发起的那次调用」。
	CallID string `json:"-"`
	// ToolName 只在回填模型发起的调用时用（responses 方言的 function_call 条目需要 name）。
	ToolName string `json:"-"`
}

// Tool 一个可调用的工具声明。
//
// Parameters 是 JSON Schema 对象，原样透传给上游，不做校验——
// 写错了是提示词层面的事，在这里拦只会掩盖问题。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall 模型请求调用某个工具。
//
// Arguments 是 JSON 字符串（上游原样给字符串），调用方自行反序列化。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Request 一次对话请求（与具体接入点无关）
type Request struct {
	Messages    []Message
	Temperature float64
	// MaxTokens 调用方给的输出预算。只作「默认值」语义：<=0 时才回落到
	// 目标的 max_out 配置，>0 时以调用方为准（见 Call）。
	MaxTokens int
	// NeedsVision 本次请求带图。带图时只派发给声明支持多模态的目标，
	// 否则上游会回 400/404（OpenRouter 原话是 "No endpoints found that support
	// image input"），白白赔掉一次调用和一次目标健康度。
	NeedsVision bool
	// Tools 本次请求允许模型调用的工具。空表示不给工具，
	// 模型就只能直接回答（等价于改动前的行为）。
	Tools []Tool
}

// 两种方言的多模态/工具报文形状并不一样，各用一个类型。
//
// chat/completions 的图片是 {"type":"image_url","image_url":{"url":...}}；
// responses 的是 {"type":"input_image","image_url":...}。
// 工具回填差得更远：responses 的 function_call_output 正文字段叫 **output**
// 而不是 content，而且整条都不能带 role —— 上游对多余字段一律 400，
// 报错还只是 "Invalid Responses API request"，一个字都不告诉你错在哪。
// （2026-10-01 生产实况：这两个字段都写错，工具轮次 100% 失败。）

// chatMsg 是 chat/completions 的 messages 条目
type chatMsg struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content,omitempty"`
	// 工具回填专用。chat 方言里 assistant 要带 tool_calls，
	// tool 结果要带 tool_call_id —— 缺了直接报
	// "tool messages must include a non-empty string tool_call_id"。
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// respMsg 是 responses 的 input 条目
type respMsg struct {
	Role string `json:"role,omitempty"`
	// Content 声明为 interface{} + omitempty：interface 的「空」判的是 nil，
	// 所以正文为 "" 的消息照样会带上 content，而工具条目（Content 保持 nil）
	// 才真的不会多出这个键。
	Content interface{} `json:"content,omitempty"`
	Type    string      `json:"type,omitempty"`
	CallID  string      `json:"call_id,omitempty"`
	Name    string      `json:"name,omitempty"`
	// Arguments 属于 function_call，Output 属于 function_call_output。
	// 这两个名字写错任意一个，上游都只会回一句没头没尾的 invalid_prompt。
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// toChatWire 转成 chat/completions 方言
func (m Message) toChatWire() chatMsg {
	if m.Role == RoleTool {
		return chatMsg{Role: RoleTool, Content: m.Content, ToolCallID: m.CallID}
	}
	if m.Role == RoleAssistant && m.CallID != "" {
		// 工具轮里模型自己那条「我要调工具」的发言必须原样回传，
		// 否则下一轮它看不到自己调过什么，会重复调用同一个工具。
		var tc chatToolCall
		tc.ID = m.CallID
		tc.Type = "function"
		tc.Function.Name = m.ToolName
		tc.Function.Arguments = m.Content
		return chatMsg{Role: RoleAssistant, ToolCalls: []chatToolCall{tc}}
	}
	return chatMsg{Role: m.Role, Content: m.contentParts("text", "image_url", false)}
}

// toResponsesWire 转成 responses 方言
func (m Message) toResponsesWire() respMsg {
	if m.Role == RoleTool {
		return respMsg{Type: "function_call_output", CallID: m.CallID, Output: m.Content}
	}
	if m.Role == RoleAssistant && m.CallID != "" {
		return respMsg{Type: "function_call", CallID: m.CallID,
			Name: m.ToolName, Arguments: m.Content}
	}
	return respMsg{Role: m.Role, Content: m.contentParts("input_text", "input_image", true)}
}

// contentParts 组装多模态正文。textType/imgType 由方言决定，
// nestImage 为真时图片是裸 url 字符串（responses），否则要包一层对象（chat）。
func (m Message) contentParts(textType, imgType string, flatImage bool) interface{} {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": textType, "text": m.Content})
	}
	for _, img := range m.Images {
		if flatImage {
			parts = append(parts, map[string]any{"type": imgType, "image_url": img})
		} else {
			parts = append(parts, map[string]any{
				"type": imgType, "image_url": map[string]string{"url": img},
			})
		}
	}
	return parts
}

// Result 调用结果
type Result struct {
	Content      string  // 完整回复文本
	TTFTMS       float64 // 首字延迟
	LatencyMS    float64 // 端到端延迟
	Model        string  // 实际使用的模型
	Endpoint     string  // 实际使用的接入点
	PromptTokens int     // 上报的输入 token（可能为 0，此时上层按估算值统计）
	OutputTokens int     // 上报的输出 token
	// ToolCalls 本次模型请求调用的工具。非空表示这一轮是「工具轮」，
	// 上层执行完、把结果回填后需要再调一次；空则表示这是「交付轮」。
	ToolCalls []ToolCall
}

// ErrorKind 错误分类，决定冷却策略
type ErrorKind int

const (
	ErrKindNone ErrorKind = iota
	ErrKindAuth
	ErrKindNotFound // 模型不存在 / 已下线
	ErrKindRateLimit
	ErrKindServer
	ErrKindNetwork
	ErrKindTimeout
	ErrKindFormat
	// ErrKindBadRequest 请求本身不合法（上下文超长、字段非法）。
	// 换目标也是同样的坏请求，所以既不计入健康度也不触发冷却，直接交回调用方。
	ErrKindBadRequest
	// ErrKindBudget 剩余预算不足以再跑一次有意义的尝试。
	ErrKindBudget
	// ErrKindRefused 上游以 HTTP 200 的形式拒绝了这次请求——
	// 正文不是模型的回答，而是一句「你的提示词含敏感词」之类的说明。
	//
	// 单独一类，因为它既不是目标坏了，也不是请求本身不合法：
	//   - 不冷却、不降权：目标健康得很，是这次的内容踩了它的审核线
	//   - 但要换目标：不同厂商的审核口径不同，换一个可能就过了
	ErrKindRefused
)

func (k ErrorKind) String() string {
	switch k {
	case ErrKindAuth:
		return "鉴权失败"
	case ErrKindNotFound:
		return "模型不存在"
	case ErrKindRateLimit:
		return "限流"
	case ErrKindServer:
		return "服务端错误"
	case ErrKindNetwork:
		return "网络错误"
	case ErrKindTimeout:
		return "超时"
	case ErrKindFormat:
		return "响应格式异常"
	case ErrKindBadRequest:
		return "请求不合法"
	case ErrKindBudget:
		return "预算耗尽"
	case ErrKindRefused:
		return "内容被上游拒绝"
	}
	return "未知"
}

// CallError 调用错误
type CallError struct {
	Status  int
	Kind    ErrorKind
	Message string

	// External 标记「这次中断是调用方掐的（父 ctx 取消/超预算），不是目标的错」。
	// Router 据此跳过健康度回写：否则一个健康目标会因为上游 45s 的摘要超时
	// 被推进指数退避（5s→…→5min），白降权重。
	External bool

	// PromptTokens/OutputTokens 是从响应里解析出来的用量。
	// HTTP 200 之后才判定的失败（空内容、被截断）上游同样计费，
	// 把它们带出去，失败调用才不会在成本统计里凭空归零（丢了近三成）。
	PromptTokens int
	OutputTokens int

	// OutputShort 标记「正文没拿到，是因为输出预算被思维链烧光」。
	// 只有这一种形态值得原地加预算重试；其它空内容形态重试只是重复浪费。
	OutputShort bool
}

func (e *CallError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Kind.String(), e.Message)
}

// ctxErrFn 判定「这次中断该记在谁头上」。
// 闭包在 callOnce 里造好后传给各个读取分支，省得每层都背一遍 ctx+timeout 参数。
type ctxErrFn func() *CallError

// ---- chat/completions 报文 ----

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []chatMsg `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// contentText 解析 content 字段。绝大多数上游给字符串，但有两处必须分开：
//   - 显式 null：上游确实没给正文，和「空串」是不同的信号
//   - 分片数组：部分实现会返回 [{"type":"text","text":...}]
//
// 合成一个 string 会把这三种形态压成同一个值，排查时看不出区别。
type contentText struct {
	Text  string // 实际文本（空串 / 空白 / null 都可能是空）
	Null  bool   // 上游显式给了 null
	Weird string // 既不是字符串也不是分片数组时的类型名，用于报错信息
}

func (c *contentText) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		c.Null = true
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		c.Text = s
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		c.Text = sb.String()
		return nil
	}
	var v any
	_ = json.Unmarshal(trimmed, &v)
	c.Weird = fmt.Sprintf("%T", v)
	return nil
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type chatChoice struct {
	Message struct {
		Role    string      `json:"role"`
		Content contentText `json:"content"`
		// 思维链模型（OpenRouter 上的 reasoning 模型、deepseek thinking 等）
		// 会把思考放进这两个键之一、正文留在 content。两个都要认：
		// 各家上游给的键名并不统一。
		ReasoningContent string `json:"reasoning_content"`
		Reasoning        string `json:"reasoning"`
		// ToolCalls 是这个 dialect 的工具调用载体。
		// Arguments 上游给的是 JSON **字符串**，不在这里解——解了还得保证
		// 不破坏原文，交给上层按需解析更稳。
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Delta struct {
		Content string `json:"content"`
		Role    string `json:"role"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
	Error   *chatError   `json:"error"`
}

// tokens 返回已解析出的用量。空壳响应里也可能有，必须照样取出来。
func (p *chatResponse) tokens() (int, int) {
	if p.Usage == nil {
		return 0, 0
	}
	return p.Usage.PromptTokens, p.Usage.CompletionTokens
}

func (p *chatResponse) fillUsage(res *Result) {
	res.PromptTokens, res.OutputTokens = p.tokens()
}

// reasoning 取思维链文本，兼容 reasoning_content / reasoning 两种键名
func (c *chatChoice) reasoning() string {
	if c.Message.ReasoningContent != "" {
		return c.Message.ReasoningContent
	}
	return c.Message.Reasoning
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *chatError `json:"error"`
}

// ---- responses 报文 ----

type responsesRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions,omitempty"` // system 提示词在 Responses API 里单独放这里
	// Tools 放在 Instructions 之后不是随便排的：上游的 prompt 缓存按
	// **序列化后的字节前缀**匹配，键顺序一变前缀就变了，缓存全部失效。
	// 工具定义必须待在稳定前缀的末尾、input 之前这个位置。
	Tools           []map[string]any `json:"tools,omitempty"`
	Input           []respMsg        `json:"input"`
	Temperature     float64          `json:"temperature,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
}

type responsesError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

type responsesResponse struct {
	// 多数实现会直接给一个拼接好的 output_text；也有实现只给 output 数组，两者都要兼容。
	// 实测 OpenRouter 的 /responses 根本不给 output_text 这个键，必须靠 output 数组。
	OutputText string `json:"output_text"`
	Output     []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Status  string `json:"status"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		// 工具调用条目：type=function_call 时这三个字段有值。
		// 实测 deepseek-flash / mimo-v2.6-flash 都把 call_id 放在 call_id 上，
		// 也有实现放在 item_id 上，两个都认。
		CallID    string `json:"call_id"`
		ItemID    string `json:"item_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	// Status + IncompleteDetails 表达「没写完」。
	// 实测：reasoning 模型把 max_output_tokens 烧光时，返回的是
	// status=incomplete + incomplete_details.reason=max_output_tokens，
	// 且 output 里只剩一条 reasoning、连 message 都没有——正文一个字都没有。
	// 这就是生产上「模型返回空内容」的真实形态。
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *responsesError `json:"error"`
}

// responsesChunk 流式响应的通用壳：只关心 delta / text / error，其余字段忽略。
// Responses API 的事件类型很多（created、output_item.added、output_text.delta…），
// 逐个定义结构体既冗余又容易漏，这里用「按需取值」的方式更稳。
type responsesChunk struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Text  string `json:"text"`
	Item  *struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response *responsesResponse `json:"response"`
	Error    *responsesError    `json:"error"`
}

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Call 对单个目标发起一次对话。无论成败都由调用方回写健康度。
func Call(ctx context.Context, t *Target, req Request) (*Result, *CallError) {
	if t == nil {
		return nil, &CallError{Kind: ErrKindNetwork, Message: "目标为空"}
	}
	view := t.snap()
	timeout, cerr := attemptBudget(ctx, view.Timeout)
	if cerr != nil {
		return nil, cerr
	}

	maxTokens := req.MaxTokens
	// max_tokens 只作默认值用：调用方显式给了就尊重调用方。
	// 曾经反过来取「调用方与模型配置的较大值」，把 brain.max_out_tokens=400
	// 顶成 1024，运维调这个值完全无效（生产里有 4 次 output_tokens 精确顶格 1024）。
	// 思维链模型预算不够的情况改由下面的「识别截断 → 加预算重试一次」兜住。
	if maxTokens <= 0 {
		maxTokens = view.MaxOut
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxOut
	}

	res, cerr := callOnce(ctx, view, req, maxTokens, timeout)
	if cerr == nil || !cerr.OutputShort {
		return res, cerr
	}
	// 唯一值得原地重试的失败形态：输出预算被思维链吃光，正文还没轮到。
	// 换模型要重跑一整轮生成（更贵，而且未必有中文/视觉能力），
	// 放宽同一个模型的上限只是让它把话说完，成功率高得多。
	bigger := retryMaxTokens(maxTokens, view.MaxOut)
	if bigger <= maxTokens || !enoughBudget(ctx, minAttemptBudget) {
		return nil, cerr
	}
	logx.Warn("输出预算被思维链吃光，加预算原地重试一次",
		"endpoint", view.EndpointName, "model", view.Model,
		"原预算", maxTokens, "新预算", bigger, "原因", cerr.Message)
	return callOnce(ctx, view, req, bigger, timeout)
}

// attemptBudget 从调用方 ctx 的剩余时间里扣出本次尝试的超时。
// 外层预算是整条 fallback 链的总预算，内层超时只是它的上限：
// 两者各拿满配置值时，第 2 个目标永远等不到（第一个一慢，预算就见底了）。
func attemptBudget(ctx context.Context, configured time.Duration) (time.Duration, *CallError) {
	if configured <= 0 {
		configured = 60 * time.Second
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return configured, nil
	}
	remain := time.Until(dl)
	if remain < minAttemptBudget {
		return 0, &CallError{
			Kind:     ErrKindBudget,
			External: true,
			Message: fmt.Sprintf("剩余预算 %s 不足一次尝试（至少需要 %s）",
				remain.Round(time.Millisecond), minAttemptBudget),
		}
	}
	if remain < configured {
		return remain, nil
	}
	return configured, nil
}

// enoughBudget 剩余预算是否还够再跑一次尝试。宁可如实失败，
// 也不要发起一个注定超时的请求把预算耗光。
func enoughBudget(ctx context.Context, need time.Duration) bool {
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(dl) >= need
}

// retryMaxTokens 截断后第二轮给多少预算：取「模型自己配的上限」与「原值两倍」
// 里较大的一个。max_out 通常是模型作者验证过的够用值，两倍则兜住没配 max_out 的接入点。
func retryMaxTokens(cur, configured int) int {
	bigger := cur * 2
	if configured > bigger {
		bigger = configured
	}
	if bigger > hardMaxOutTokens {
		bigger = hardMaxOutTokens
	}
	return bigger
}

// callOnce 发一次 HTTP 请求并解析响应。maxTokens/timeout 决定这一次的报文与时限。
func callOnce(ctx context.Context, view TargetView, req Request, maxTokens int, timeout time.Duration) (*Result, *CallError) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 父 ctx 掐断与本目标超时都是 context 错误，必须分开记：
	// 前者不该算目标的账（见 ctxErrOf）。
	ctxErr := func() *CallError { return ctxErrOf(callCtx, ctx, timeout) }

	url, raw, cerr := buildPayload(view, req, maxTokens)
	if cerr != nil {
		return nil, cerr
	}

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, &CallError{Kind: ErrKindFormat, Message: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+view.APIKey)
	// OpenRouter 用这两个头做排行榜归属，非必填，填上能让请求看起来更正常
	if strings.Contains(view.BaseURL, "openrouter.ai") {
		// 用仓库自身的 URL（可在此处改成你自己的 fork），OpenRouter 据此归属排行榜
		httpReq.Header.Set("HTTP-Referer", "https://github.com/uncleyumo/go-dadyumo-qqbot")
		httpReq.Header.Set("X-Title", "dadyumo-qqbot")
	}

	start := time.Now()
	rsp, err := httpClient.Do(httpReq)
	if err != nil {
		if cerr := ctxErr(); cerr != nil {
			return nil, cerr
		}
		return nil, &CallError{Kind: ErrKindNetwork, Message: err.Error()}
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(rsp.Body, 4096))
		return nil, classifyHTTP(rsp.StatusCode, string(b))
	}

	isStream := view.Stream && strings.Contains(rsp.Header.Get("Content-Type"), "event-stream")
	if view.APIType == config.APIResponses {
		if isStream {
			return readStreamResponses(ctxErr, rsp.Body, start, view)
		}
		return readFullResponses(ctxErr, rsp.Body, start, view)
	}
	if isStream {
		return readStream(ctxErr, rsp.Body, start, view)
	}
	return readFull(ctxErr, rsp.Body, start, view)
}

// ctxErrOf 区分「这个目标自己太慢」与「调用方把预算掐了」。
// 两者都是 context 错误，但后者不是目标的毛病：混在一起记，
// 会让健康目标因为上游 45s 的摘录超时进指数退避，还被写成「超过 1m0s 未响应」。
func ctxErrOf(callCtx, parent context.Context, timeout time.Duration) *CallError {
	if parent.Err() != nil {
		return &CallError{
			Kind:     ErrKindTimeout,
			External: true,
			Message:  fmt.Sprintf("调用方上下文已结束（%v），非目标超时", context.Cause(parent)),
		}
	}
	if callCtx.Err() != nil {
		return &CallError{Kind: ErrKindTimeout, Message: fmt.Sprintf("超过 %s 未响应", timeout)}
	}
	return nil
}

// buildPayload 按协议方言生成 URL 与请求体
func buildPayload(t TargetView, req Request, maxTokens int) (string, []byte, *CallError) {
	base := strings.TrimRight(t.BaseURL, "/")
	var payload any
	var url string

	if t.APIType == config.APIResponses {
		// system 消息在 Responses API 里不进 input，单独放 instructions
		var instr string
		input := make([]respMsg, 0, len(req.Messages))
		for _, m := range req.Messages {
			if m.Role == RoleSystem {
				if instr != "" {
					instr += "\n\n"
				}
				instr += m.Content
				continue
			}
			input = append(input, m.toResponsesWire())
		}
		if len(input) == 0 {
			// 没有用户侧内容时塞一句占位，避免上游直接 400
			input = []respMsg{{Role: RoleUser, Content: "（继续）"}}
		}
		payload = responsesRequest{
			Model:           t.Model,
			Instructions:    instr,
			Tools:           responsesTools(req.Tools),
			Input:           input,
			Temperature:     req.Temperature,
			MaxOutputTokens: maxTokens,
			Stream:          t.Stream,
		}
		url = base + "/responses"
	} else {
		wire := make([]chatMsg, 0, len(req.Messages))
		for _, m := range req.Messages {
			wire = append(wire, m.toChatWire())
		}
		payload = chatRequest{
			Model:       t.Model,
			Messages:    wire,
			Tools:       req.Tools, // chat 方言的 tools 不用包 type 字段，直接就是函数声明
			Temperature: req.Temperature,
			MaxTokens:   maxTokens,
			Stream:      t.Stream,
		}
		url = base + "/chat/completions"
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, &CallError{Kind: ErrKindFormat, Message: err.Error()}
	}
	return url, raw, nil
}

// responsesTools 把内部 Tool 转成 Responses API 要的形状。
//
// chat/completions 的 tools 数组里直接就是函数声明，Responses 多一层
// {"type":"function"} 壳——两边不通用，所以这里转一次而不是共用 Tool。
func responsesTools(tools []Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	return out
}

// result 统一包装结果，避免每个分支重复写一遍字段。
// 空内容的判定上移到各读取分支：不同协议、不同失败形态必须给出不同的信息。
func result(content string, start time.Time, ttft float64, view TargetView) *Result {
	latency := msSince(start)
	if ttft <= 0 {
		// 非流式拿不到真实首字时间，只能拿总耗时顶上，于是 TTFT≡Latency。
		// 这个恒等关系让排序权重把同一个变量算了两遍，见 health.go 的处理。
		ttft = latency
	}
	return &Result{
		Content:   content,
		TTFTMS:    ttft,
		LatencyMS: latency,
		Model:     view.Model,
		Endpoint:  view.EndpointID,
	}
}

// emptyContentErr 把「HTTP 200、报文也解析成功、但没拿到正文」包装成错误。
// 现场曾经只有一句「模型返回空内容」，86 次全都定位不了，就是因为
// 好几种形态共用了一个说法，而它们的处置完全不同：
// 预算被思维链吃光 → 加预算重试；上游返回空壳 → 换目标；content 显式 null → 找中转站。
func emptyContentErr(form string, view TargetView, promptTokens, outputTokens int, outputShort bool) *CallError {
	return &CallError{
		Kind: ErrKindFormat,
		Message: fmt.Sprintf("%s（model=%s endpoint=%s prompt_tokens=%d output_tokens=%d）",
			form, view.Model, view.EndpointID, promptTokens, outputTokens),
		PromptTokens: promptTokens,
		OutputTokens: outputTokens,
		OutputShort:  outputShort,
	}
}

// noChoicesErr choices 为空数组。HTTP 200 但一个候选都没给——
// 中转站在上游 502/被限流时会返回这种空壳，和「content 为空」必须分开报。
func noChoicesErr(p *chatResponse, view TargetView) *CallError {
	pt, ot := p.tokens()
	form := "响应中没有 choices（上游只给了 usage 的空壳）"
	if pt == 0 && ot == 0 {
		form = "响应中没有 choices，且没有 usage（上游返回了空壳）"
	}
	return emptyContentErr(form, view, pt, ot, false)
}

// chatEmptyForm 描述 chat/completions 这次为什么没有正文。
// 返回可读形态 + 是否属于「输出预算被烧光」（后者才值得加预算重试）。
func chatEmptyForm(p *chatResponse, ch *chatChoice) (string, bool) {
	var b strings.Builder
	switch {
	case ch.Message.Content.Weird != "":
		b.WriteString("content 字段类型异常：" + ch.Message.Content.Weird)
	case ch.Message.Content.Null:
		b.WriteString("content 显式为 null")
	case ch.Message.Content.Text != "":
		b.WriteString(fmt.Sprintf("content 全是空白字符（%d 字节）", len(ch.Message.Content.Text)))
	default:
		b.WriteString("content 为空串")
	}
	reasoning := ch.reasoning()
	if reasoning != "" {
		b.WriteString(fmt.Sprintf("；思维链字段有 %d 字节但没有正文（reasoning-only）", len(reasoning)))
	}
	if ch.FinishReason != "" {
		b.WriteString("；finish_reason=" + ch.FinishReason)
	}
	_, ot := p.tokens()
	if ot > 0 {
		b.WriteString(fmt.Sprintf("；本次已产出 %d token", ot))
	}
	// finish_reason=length、或「有思维链却没正文」，都指向同一件事：输出预算不够它想完。
	short := ch.FinishReason == "length" || reasoning != ""
	return b.String(), short
}

func readFull(ctxErr ctxErrFn, r io.Reader, start time.Time, view TargetView) (*Result, *CallError) {
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		if cerr := ctxErr(); cerr != nil {
			return nil, cerr
		}
		return nil, &CallError{Kind: ErrKindNetwork, Message: "读取响应失败: " + err.Error()}
	}
	var parsed chatResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, &CallError{Kind: ErrKindFormat, Message: "响应解析失败: " + string(truncateBytes(b, 200))}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		pt, ot := parsed.tokens()
		return nil, &CallError{Kind: ErrKindServer, Status: parsed.Error.Code, Message: parsed.Error.Message,
			PromptTokens: pt, OutputTokens: ot}
	}
	if len(parsed.Choices) == 0 {
		return nil, noChoicesErr(&parsed, view)
	}
	ch := &parsed.Choices[0]
	content := ch.Message.Content.Text
	if strings.TrimSpace(content) == "" && ch.Delta.Content != "" {
		content = ch.Delta.Content
	}
	res := result(content, start, 0, view)
	parsed.fillUsage(res)
	// 和 responses 一样：工具轮正文本来就是空的，必须先判工具再判空。
	res.ToolCalls = collectChatToolCalls(ch)
	if len(res.ToolCalls) > 0 {
		return res, nil
	}
	if strings.TrimSpace(content) == "" {
		form, short := chatEmptyForm(&parsed, ch)
		pt, ot := parsed.tokens()
		return nil, emptyContentErr(form, view, pt, ot, short)
	}
	// 正文非空但其实是上游的拒绝说明：必须判在「成功」之前。
	// 顺序很重要——放这里而不是最前面，是为了先让工具轮和空内容走各自的分支。
	if looksLikeRefusal(content) {
		pt, ot := parsed.tokens()
		return nil, refusalErr(content, view, pt, ot)
	}
	return res, nil
}

// collectChatToolCalls 提取 chat/completions 方言的工具调用。
// 那边没有独立的 output 数组，调用挂在 message.tool_calls 上。
func collectChatToolCalls(ch *chatChoice) []ToolCall {
	if len(ch.Message.ToolCalls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(ch.Message.ToolCalls))
	for _, tc := range ch.Message.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		id := tc.ID
		if id == "" {
			id = "call_" + tc.Function.Name
		}
		out = append(out, ToolCall{ID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out
}

func readStream(ctxErr ctxErrFn, r io.Reader, start time.Time, view TargetView) (*Result, *CallError) {
	var sb strings.Builder
	var ttft float64
	finish := ""
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					break
				}
				var chunk streamChunk
				if err2 := json.Unmarshal([]byte(data), &chunk); err2 == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return nil, &CallError{Kind: ErrKindServer, Status: chunk.Error.Code, Message: chunk.Error.Message}
					}
					if len(chunk.Choices) > 0 {
						if fr := chunk.Choices[0].FinishReason; fr != "" {
							finish = fr
						}
						delta := chunk.Choices[0].Delta.Content
						if delta != "" {
							if ttft == 0 {
								ttft = msSince(start)
							}
							sb.WriteString(delta)
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if cerr := ctxErr(); cerr != nil {
				return nil, cerr
			}
			return nil, &CallError{Kind: ErrKindNetwork, Message: "流式读取失败: " + err.Error()}
		}
	}
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		form := fmt.Sprintf("流式响应没收到任何文本片段（收到 %d 字节，finish_reason=%q）", len(content), finish)
		return nil, emptyContentErr(form, view, 0, 0, finish == "length")
	}
	return result(content, start, ttft, view), nil
}

func readFullResponses(ctxErr ctxErrFn, r io.Reader, start time.Time, view TargetView) (*Result, *CallError) {
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		if cerr := ctxErr(); cerr != nil {
			return nil, cerr
		}
		return nil, &CallError{Kind: ErrKindNetwork, Message: "读取响应失败: " + err.Error()}
	}
	var parsed responsesResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, &CallError{Kind: ErrKindFormat, Message: "响应解析失败: " + string(truncateBytes(b, 200))}
	}
	pt, ot := 0, 0
	if parsed.Usage != nil {
		pt, ot = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, &CallError{Kind: ErrKindServer, Message: parsed.Error.Message,
			PromptTokens: pt, OutputTokens: ot}
	}
	content := parsed.OutputText
	if strings.TrimSpace(content) == "" {
		content = collectResponsesOutput(&parsed)
	}
	res := result(content, start, 0, view)
	res.PromptTokens, res.OutputTokens = pt, ot

	// 工具轮判定必须在「正文为空」判定**之前**：
	// 模型决定调工具时正文本来就是空的（实测 output 只有 [reasoning, function_call]），
	// 先判空会把正常的工具轮误报成「模型返回空内容」并触发重试。
	res.ToolCalls = collectFunctionCalls(&parsed)
	if len(res.ToolCalls) > 0 {
		return res, nil
	}
	if strings.TrimSpace(content) == "" {
		form, short := responsesEmptyForm(&parsed)
		return nil, emptyContentErr(form, view, pt, ot, short)
	}
	// 同 readFull：正文非空不等于拿到回答，上游会用 200 递一句拒绝说明。
	if looksLikeRefusal(content) {
		return nil, refusalErr(content, view, pt, ot)
	}
	return res, nil
}

// collectFunctionCalls 从 output[] 里挑出工具调用条目。
//
// 实测 deepseek-flash 与 mimo-v2.6-flash 都是 output=[reasoning, function_call]
// 或 [reasoning, message, function_call] 两种形态，call_id 字段名也见过 item_id 的变体。
func collectFunctionCalls(p *responsesResponse) []ToolCall {
	var out []ToolCall
	for _, item := range p.Output {
		if item.Type != "function_call" {
			continue
		}
		if item.Name == "" {
			continue // 没有名字的条目不是可执行的调用，跳过而不是报给上层一个空工具
		}
		id := item.CallID
		if id == "" {
			id = item.ItemID
		}
		if id == "" {
			// 没有 id 就没法回填结果，只能就地合成一个稳定的：
			// 上层会把 function_call 原样回传，靠这个 id 配对。
			id = "call_" + item.Name
		}
		out = append(out, ToolCall{ID: id, Name: item.Name, Arguments: item.Arguments})
	}
	return out
}

// collectResponsesOutput 从 output[] 里把所有文本片段拼出来。
// 只认 type=message 的条目：reasoning 摘要不该被当成它要说的话发出去。
func collectResponsesOutput(p *responsesResponse) string {
	var sb strings.Builder
	for _, item := range p.Output {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Text == "" {
				continue
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// responsesEmptyForm 描述 /responses 这次为什么没有正文。
// 重点是把 output 数组的构成说清楚：只有 reasoning 条目（reasoning-only，
// 正文被输出预算挤没了）还是压根什么都没有（上游空壳）——两者的处置完全不同。
func responsesEmptyForm(p *responsesResponse) (string, bool) {
	var b strings.Builder
	b.WriteString("responses 响应里没有正文")
	if p.Status != "" {
		b.WriteString("；status=" + p.Status)
	}
	if p.IncompleteDetails != nil && p.IncompleteDetails.Reason != "" {
		b.WriteString("；incomplete_reason=" + p.IncompleteDetails.Reason)
	}
	var msgItems, reasoningItems, textItems int
	var reasoningBytes int
	for _, item := range p.Output {
		if item.Type == "reasoning" {
			reasoningItems++
			for _, c := range item.Content {
				reasoningBytes += len(c.Text)
			}
			continue
		}
		if item.Type != "" && item.Type != "message" {
			continue
		}
		msgItems++
		for _, c := range item.Content {
			if strings.TrimSpace(c.Text) != "" {
				textItems++
			}
		}
	}
	b.WriteString(fmt.Sprintf("；output 条目: message=%d（其中有文本 %d）reasoning=%d（思维链 %d 字节）",
		msgItems, textItems, reasoningItems, reasoningBytes))
	truncated := p.IncompleteDetails != nil && p.IncompleteDetails.Reason == "max_output_tokens"
	if reasoningItems > 0 && msgItems == 0 {
		b.WriteString("（reasoning-only：模型只写了思维链就被截断，没产出 message）")
	}
	// 「有思维链但没有正文」或上游明说被 max_output_tokens 截断，
	// 都指向同一件事：这次给的输出预算不够它想完，值得加预算重试。
	short := truncated || (reasoningItems > 0 && msgItems == 0)
	return b.String(), short
}

func readStreamResponses(ctxErr ctxErrFn, r io.Reader, start time.Time, view TargetView) (*Result, *CallError) {
	var sb strings.Builder
	var ttft float64
	var finalText string
	gotDelta := false // 是否已经收过增量片段，用于避免 item 与 delta 重复拼接
	status, incomplete := "", ""
	br := bufio.NewReader(r)

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				break
			}
			if data != "" {
				var chunk responsesChunk
				if err2 := json.Unmarshal([]byte(data), &chunk); err2 == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return nil, &CallError{Kind: ErrKindServer, Message: chunk.Error.Message}
					}
					// 完成事件里通常带着完整的 output，留作兜底
					if chunk.Response != nil {
						if chunk.Response.Status != "" {
							status = chunk.Response.Status
						}
						if chunk.Response.IncompleteDetails != nil && chunk.Response.IncompleteDetails.Reason != "" {
							incomplete = chunk.Response.IncompleteDetails.Reason
						}
						if txt := chunk.Response.OutputText; txt != "" {
							finalText = txt
						} else if txt := collectResponsesOutput(chunk.Response); txt != "" {
							finalText = txt
						}
					}
					// 有些实现不发 delta，只在 output_item.added 里给整段文本
					if chunk.Item != nil && !gotDelta {
						for _, c := range chunk.Item.Content {
							if c.Text != "" {
								sb.WriteString(c.Text)
							}
						}
					}
					if chunk.Delta != "" {
						if !gotDelta {
							gotDelta = true
							sb.Reset()
						}
						if ttft == 0 {
							ttft = msSince(start)
						}
						sb.WriteString(chunk.Delta)
					} else if chunk.Text != "" && strings.Contains(chunk.Type, "delta") {
						if ttft == 0 {
							ttft = msSince(start)
						}
						sb.WriteString(chunk.Text)
					}
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if cerr := ctxErr(); cerr != nil {
				return nil, cerr
			}
			return nil, &CallError{Kind: ErrKindNetwork, Message: "流式读取失败: " + err.Error()}
		}
	}

	content := sb.String()
	if strings.TrimSpace(content) == "" {
		content = finalText
	}
	if strings.TrimSpace(content) == "" {
		form := fmt.Sprintf("流式 responses 没收到任何文本（status=%q incomplete_reason=%q）", status, incomplete)
		return nil, emptyContentErr(form, view, 0, 0, incomplete == "max_output_tokens")
	}
	return result(content, start, ttft, view), nil
}

// classifyHTTP 把 HTTP 状态码映射成错误分类。
// 4xx 里必须把「请求本身不合法」单列出来：上下文超长返回的 400 换目标也是同样的
// 坏请求，早先它被归成服务端错误，于是 maxTry=4 把同一个坏请求依次发给 4 个目标，
// 4 个健康目标全被冷却，机器人短时间完全失语。
func classifyHTTP(status int, body string) *CallError {
	msg := truncateBytes([]byte(strings.ReplaceAll(body, "\n", " ")), 300)
	switch {
	case status == 401 || status == 403:
		return &CallError{Status: status, Kind: ErrKindAuth, Message: string(msg)}
	case status == 404:
		return &CallError{Status: status, Kind: ErrKindNotFound, Message: string(msg)}
	case status == 429:
		return &CallError{Status: status, Kind: ErrKindRateLimit, Message: string(msg)}
	case status == 400 || status == 413 || status == 414 || status == 415 || status == 422:
		return &CallError{Status: status, Kind: ErrKindBadRequest, Message: string(msg)}
	case status >= 500:
		return &CallError{Status: status, Kind: ErrKindServer, Message: string(msg)}
	default:
		return &CallError{Status: status, Kind: ErrKindServer, Message: string(msg)}
	}
}

// msSince 返回毫秒数（保留小数）。
// 不能用 time.Since().Milliseconds() 的整数截断：极快的响应会被记成 0，
// 与「从未测量」撞成同一个值，排序就分不清谁快了。
func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
