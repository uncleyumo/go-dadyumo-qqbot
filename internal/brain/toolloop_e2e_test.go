package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
)

// 端到端跑工具循环。
//
// 这是整套设计里最要紧的一条：**工具轮绝不发送任何消息**。
// 工具轮发出去的东西在最终轮失败时无法回滚，重试就会重复发言——
// 「内容对但发了两次」在群里比「这次没说话」糟得多。
//
// 所以这里用假上游精确控制模型每一轮返回什么，然后断言发送通道的记录。

// fakeUpstream 按顺序返回预设响应，并记录收到的请求
type fakeUpstream struct {
	mu     sync.Mutex
	steps  []fakeStep
	bodies []string
	hits   int
}

type fakeStep struct {
	toolCalls []map[string]any
	content   string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	i := f.hits
	f.hits++
	if i >= len(f.steps) {
		f.mu.Unlock()
		w.WriteHeader(500)
		return
	}
	step := f.steps[i]
	f.mu.Unlock()

	// 记录原始请求体。
	//
	// 不能反序列化成 llm.Request 再断言：Responses 方言的报文形状是
	// {instructions, input, tools}，input 里的条目有 function_call /
	// function_call_output 两种类型，llm.Request 表达不了。
	// 直接存原始字节，用字符串包含来判断。
	if body, err := io.ReadAll(r.Body); err == nil {
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
	}

	var sb strings.Builder
	sb.WriteString(`{"status":"completed","output":[`)
	parts := []string{`{"type":"reasoning","summary":[]}`}
	for j, tc := range step.toolCalls {
		if j > 0 {
			// 多个工具调用之间不需要逗号陷阱，逐个拼
		}
		b, _ := json.Marshal(map[string]any{
			"type":    "function_call",
			"call_id": tc["id"], "name": tc["name"], "arguments": tc["args"],
		})
		parts = append(parts, string(b))
	}
	if step.content != "" {
		b, _ := json.Marshal(map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]string{{"type": "output_text", "text": step.content}},
		})
		parts = append(parts, string(b))
	}
	sb.WriteString(strings.Join(parts, ","))
	sb.WriteString(`],"usage":{"input_tokens":100,"output_tokens":50}}`)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(sb.String()))
}

// body 返回第 i 次请求的原始报文
func (f *fakeUpstream) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.bodies) {
		return ""
	}
	return f.bodies[i]
}

// newEngineWithUpstream 组一个指向假上游的引擎。
// srvURL 是那个假上游的地址。
func newEngineWithUpstream(t *testing.T, srvURL string, sender Sender, pool *memepool.Pool, tweak func(*config.Config)) *Engine {
	t.Helper()
	store := config.NewStoreFrom(func(c *config.Config) {
		c.MemePool.Enabled = pool != nil
		c.LLM.MaxAttempts = 1
		c.LLM.Endpoints = []config.Endpoint{{
			ID: "fake", Name: "fake", BaseURL: srvURL, APIKey: "k",
			APIType: config.APIResponses, Enabled: true, TimeoutMS: 5000,
			Models: []config.Model{{
				ID: "fake-model", Label: "fake-model", Enabled: true,
				MaxCtx: 32000, MaxOut: 512,
			}},
		}}
		if tweak != nil {
			tweak(c)
		}
	})
	return &Engine{
		store:  store,
		router: llm.NewRouter(store),
		mem:    memory.New(50),
		sender: sender,
		memes:  pool,
	}
}

// startFakeUpstream 起一个假上游
func startFakeUpstream(t *testing.T, steps []fakeStep) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	up := &fakeUpstream{steps: steps}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return srv, up
}

func TestToolRoundNeverSendsMessages(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		// 第 1 轮：模型要看表情包池
		{toolCalls: []map[string]any{
			{"id": "call_1", "name": "browse_meme_pool", "args": `{"mood":"无语"}`},
		}},
		// 第 2 轮：交付轮
		{content: `<json>{"act":"say","to":"","blocks":[{"t":"text","c":"无语"}],"mem":[]}</json>`},
	})
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "无语的猫", time.Now())

	sender := &recordingSender{}
	e := newEngineWithUpstream(t, srv.URL, sender, pool, nil)
	e.imgSender = sender

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, rounds, err := e.chatWithTools(ctx, e.store.Get(), "系统提示", "用户消息", nil, nil)
	if err != nil {
		t.Fatalf("工具循环不该失败: %v", err)
	}
	if rounds != 2 {
		t.Errorf("应跑了两轮（1 工具 + 1 交付），got %d", rounds)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("交付轮不该再带工具调用: %+v", res.ToolCalls)
	}
	// 关键：整个过程中一条消息都没发出去
	if texts, imgs := sender.count(); texts != 0 || imgs != 0 {
		t.Errorf("工具循环期间绝不能发送，texts=%v imgs=%d", texts, imgs)
	}
}

// 工具结果必须被回传给模型，否则它下一轮看不到自己调过什么，会重复调
func TestToolResultFedBack(t *testing.T) {
	srv, up := startFakeUpstream(t, []fakeStep{
		{toolCalls: []map[string]any{
			{"id": "call_1", "name": "browse_meme_pool", "args": `{"mood":"无语"}`},
		}},
		{content: `<json>{"act":"quiet","mem":[]}</json>`},
	})
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "无语的猫", time.Now())

	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, pool, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := e.chatWithTools(ctx, e.store.Get(), "系统", "用户", nil, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	second := up.body(1)
	if second == "" {
		t.Fatal("应发出第二次请求")
	}
	// 第二次请求里必须带上工具结果——池子列表内容
	if !strings.Contains(second, "无语的猫") {
		t.Errorf("第二次请求应带上工具结果（列表内容），got %s", truncate(second, 300))
	}
	// 并且带上模型自己那次调用（function_call），否则它下一轮
	// 看不到自己调过什么，会重复调同一个工具
	if !strings.Contains(second, "function_call") {
		t.Errorf("第二次请求应回传模型发起的那次调用，got %s", truncate(second, 300))
	}
	if !strings.Contains(second, "function_call_output") {
		t.Errorf("第二次请求应有工具结果条目，got %s", truncate(second, 300))
	}
}

// 轮次上限：模型发癫反复调工具时，必须停下来而不是无限循环
func TestToolRoundLimitStopsRunaway(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{toolCalls: []map[string]any{
			{"id": "c1", "name": "browse_meme_pool", "args": `{}`},
		}},
		{toolCalls: []map[string]any{
			{"id": "c2", "name": "browse_meme_pool", "args": `{}`},
		}},
		{toolCalls: []map[string]any{
			{"id": "c3", "name": "browse_meme_pool", "args": `{}`},
		}},
		{toolCalls: []map[string]any{
			{"id": "c4", "name": "browse_meme_pool", "args": `{}`},
		}},
	})
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "无语的猫", time.Now())

	sender := &recordingSender{}
	e := newEngineWithUpstream(t, srv.URL, sender, pool, func(c *config.Config) {
		c.MemePool.MaxToolRounds = 3
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, rounds, err := e.chatWithTools(ctx, e.store.Get(), "系统", "用户", nil, nil)
	if err != nil {
		t.Fatalf("不该报错，应按上限截断: %v", err)
	}
	if rounds > 3 {
		t.Errorf("轮数必须受上限约束，got %d", rounds)
	}
	// 无论怎么发癫，一条消息都不能发出去
	if texts, imgs := sender.count(); texts != 0 || imgs != 0 {
		t.Errorf("工具轮绝不能发送，texts=%v imgs=%d", texts, imgs)
	}
	_ = res
}

// 没有工具时就是单轮：直接就是交付轮
func TestNoToolsMeansSingleRound(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{content: `<json>{"act":"say","blocks":[{"t":"text","c":"在"}],"mem":[]}</json>`},
	})
	sender := &recordingSender{}
	e := newEngineWithUpstream(t, srv.URL, sender, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, rounds, err := e.chatWithTools(ctx, e.store.Get(), "系统", "用户", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rounds != 1 {
		t.Errorf("无工具时应只跑一轮，got %d", rounds)
	}
	if len(res.ToolCalls) != 0 {
		t.Error("无工具时不该有工具调用")
	}
}
