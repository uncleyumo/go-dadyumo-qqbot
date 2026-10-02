package webhook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
)

// mockHandler 线程安全地记录收到的事件：后台队列 worker 与测试 goroutine 并发访问它
type mockHandler struct {
	mu      sync.Mutex
	group   []*GroupMessage
	atMe    []bool
	c2c     []*C2CMessage
	robot   []string // 事件类型
	robotEv []*GroupRobotEvent
}

func (m *mockHandler) OnGroupMessage(ev *GroupMessage, atMe bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.group = append(m.group, ev)
	m.atMe = append(m.atMe, atMe)
}

func (m *mockHandler) OnC2CMessage(ev *C2CMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.c2c = append(m.c2c, ev)
}

func (m *mockHandler) OnGroupRobotEvent(eventType string, ev *GroupRobotEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.robot = append(m.robot, eventType)
	m.robotEv = append(m.robotEv, ev)
}

// snapshot 读出当前已收到的计数，测试断言用
func (m *mockHandler) snapshot() (group, c2c, robot int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.group), len(m.c2c), len(m.robot)
}

// waitCount 轮询等待某类事件到达，避免测试靠 sleep 猜时间
func (m *mockHandler) waitCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g, _, _ := m.snapshot(); g >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	g, _, _ := m.snapshot()
	t.Fatalf("等待群消息 %d 条超时，实际 %d 条", want, g)
}

// testSecret 官方 DEMO 的 secret，同时用于锁定公钥推导
const testSecret = "naOC0ocQE3shWLAfffVLB1rhYPG7"

// nowTs 取当前时间戳：验签会卡 ±5 分钟新鲜度，写死的常量 ts 一定过不了
func nowTs() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func newTestStore(t *testing.T) *config.Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{
		"qq": {"app_id":"123456","app_secret":"naOC0ocQE3shWLAfffVLB1rhYPG7","require_signature":true,"sandbox":true}
	}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(p)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return st
}

func signReq(t *testing.T, secret, ts string, body []byte) *http.Request {
	t.Helper()
	var msg bytes.Buffer
	msg.WriteString(ts)
	msg.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", Sign(secret, msg.Bytes()))
	req.Header.Set("X-Signature-Timestamp", ts)
	return req
}

// post 造一个验签通过的回调请求
func post(t *testing.T, body string) *http.Request {
	t.Helper()
	return signReq(t, testSecret, nowTs(), []byte(body))
}

func TestServerRejectsBadSignature(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	body := []byte(`{"op":0,"d":{},"t":"GROUP_AT_MESSAGE_CREATE"}`)
	req := httptest.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", "deadbeef")
	req.Header.Set("X-Signature-Timestamp", "1725442341")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无效应签应返回 401，实际 %d", w.Code)
	}
	if len(h.group) != 0 {
		t.Fatal("验签失败的事件不应被分发")
	}
}

func TestServerHandlesValidation(t *testing.T) {
	st := newTestStore(t)
	srv := New(st, &mockHandler{})
	ts := nowTs()
	body := []byte(`{"d":{"plain_token":"Arq0D5A61EgUu4OxUvOp","event_ts":"1725442341"},"op":13}`)
	req := signReq(t, testSecret, ts, body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("验证请求应返回 200，实际 %d", w.Code)
	}
	var rsp ValidationRsp
	if err := json.Unmarshal(w.Body.Bytes(), &rsp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if rsp.PlainToken != "Arq0D5A61EgUu4OxUvOp" || rsp.Signature == "" {
		t.Fatalf("验证响应不完整: %+v", rsp)
	}
	// 用平台侧公钥反向验签
	var msg bytes.Buffer
	msg.WriteString("1725442341")
	msg.WriteString("Arq0D5A61EgUu4OxUvOp")
	sig, err := hex.DecodeString(rsp.Signature)
	if err != nil {
		t.Fatalf("签名不是合法 hex: %v", err)
	}
	pub := PublicKey(testSecret)
	if !ed25519.Verify(pub, msg.Bytes(), sig) {
		t.Fatal("返回的签名无法被平台公钥验证")
	}
}

func TestServerDispatchesGroupMessage(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	payload := `{
		"id":"evt-1","op":0,"s":1,"t":"GROUP_MESSAGE_CREATE",
		"d":{"id":"ROBOT1.0_abc","author":{"id":"A1B2","member_openid":"A1B2","member_role":"owner","username":"小明","bot":false},
		     "content":"大家早上好呀","group_openid":"G1","message_type":0,"timestamp":"2026-07-21T08:00:00+08:00"}
	}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatalf("正常事件应返回 200，实际 %d", w.Code)
	}
	if len(h.group) != 1 {
		t.Fatalf("应分发 1 条群消息，实际 %d", len(h.group))
	}
	ev := h.group[0]
	if ev.Content != "大家早上好呀" || ev.GroupOpenID != "G1" || ev.ID != "ROBOT1.0_abc" {
		t.Fatalf("事件解析有误: %+v", ev)
	}
	if ev.Author == nil || ev.Author.Username != "小明" || ev.Author.MemberRole != "owner" {
		t.Fatalf("作者信息解析有误: %+v", ev.Author)
	}
	if h.atMe[0] {
		t.Fatal("GROUP_MESSAGE_CREATE 不应标记为 @ 消息")
	}
}

func TestServerMarksAtMessage(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	payload := `{"id":"evt-2","op":0,"t":"GROUP_AT_MESSAGE_CREATE",
		"d":{"id":"ROBOT1.0_def","author":{"id":"C3D4","username":"小红","bot":false},
		     "content":"老王在吗","group_openid":"G1"}}`
	srv.ServeHTTP(httptest.NewRecorder(), post(t, payload))
	if len(h.group) != 1 || !h.atMe[0] {
		t.Fatal("GROUP_AT_MESSAGE_CREATE 应标记为 @ 消息")
	}
}

// ---------- A. GroupRobotEvent.timestamp 类型兼容 ----------

// 平台对机器人生命周期四事件推的 timestamp 是 JSON number，早先声明成 string 导致
// 解析失败率 3/3，OnGroupRobotEvent 是死代码。
func TestGroupRobotEventTimestampAcceptsNumber(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	payload := `{"id":"evt-add","op":0,"t":"GROUP_ADD_ROBOT",
		"d":{"timestamp":1725442341,"group_openid":"G_1","opd_id":"OP_1"}}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatalf("正常事件应返回 200，实际 %d", w.Code)
	}
	_, _, robot := h.snapshot()
	if robot != 1 {
		t.Fatalf("数字 timestamp 应被解析并分发，实际分发 %d 条", robot)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := h.robotEv[0]
	if int64(ev.Timestamp) != 1725442341 {
		t.Fatalf("timestamp 解析错误: %d", ev.Timestamp)
	}
	if ev.GroupOpenID != "G_1" || ev.OpdID != "OP_1" {
		t.Fatalf("事件字段解析有误: %+v", ev)
	}
}

func TestGroupRobotEventTimestampAcceptsString(t *testing.T) {
	var ev GroupRobotEvent
	// RFC3339 字符串
	if err := json.Unmarshal([]byte(`{"timestamp":"2024-09-03T10:12:00+08:00"}`), &ev); err != nil {
		t.Fatalf("RFC3339 字符串 timestamp 应可解析: %v", err)
	}
	want := time.Date(2024, 9, 3, 10, 12, 0, 0, time.FixedZone("CST", 8*3600)).Unix()
	if int64(ev.Timestamp) != want {
		t.Fatalf("RFC3339 解析结果不对: got %d want %d", ev.Timestamp, want)
	}
	// 纯数字字符串
	if err := json.Unmarshal([]byte(`{"timestamp":"1725442341"}`), &ev); err != nil {
		t.Fatalf("数字字符串 timestamp 应可解析: %v", err)
	}
	if int64(ev.Timestamp) != 1725442341 {
		t.Fatalf("数字字符串解析结果不对: %d", ev.Timestamp)
	}
	// 浮点秒
	if err := json.Unmarshal([]byte(`{"timestamp":1725442341.0}`), &ev); err != nil {
		t.Fatalf("浮点 timestamp 应可解析: %v", err)
	}
	if int64(ev.Timestamp) != 1725442341 {
		t.Fatalf("浮点解析结果不对: %d", ev.Timestamp)
	}
	// null 与缺省都要给出零值而不是报错
	if err := json.Unmarshal([]byte(`{"timestamp":null}`), &ev); err != nil {
		t.Fatalf("null timestamp 不应报错: %v", err)
	}
	if ev.Timestamp != 0 || ev.Timestamp.String() != "" {
		t.Fatalf("null timestamp 应为零值: %+v", ev)
	}
	// 真正读不懂的必须报错，不能悄悄吞成 0
	if err := json.Unmarshal([]byte(`{"timestamp":"昨天"}`), &ev); err == nil {
		t.Fatal("无法解析的 timestamp 应返回错误")
	}
}

// 四个生命周期事件各走各的分支，都能到 handler（早先共用一条 warn，定位只能翻源码）
func TestGroupRobotEventFourBranches(t *testing.T) {
	cases := []struct {
		typ string
		id  string
	}{
		{EventGroupAddRobot, "evt-a"},
		{EventGroupDelRobot, "evt-b"},
		{EventGroupMsgRecv, "evt-c"},
		{EventGroupMsgReject, "evt-d"},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			st := newTestStore(t)
			h := &mockHandler{}
			srv := New(st, h)
			payload := `{"id":"` + c.id + `","op":0,"t":"` + c.typ + `",
				"d":{"timestamp":1725442341,"group_openid":"G_9","opd_id":"OP_9"}}`
			srv.ServeHTTP(httptest.NewRecorder(), post(t, payload))
			_, _, robot := h.snapshot()
			if robot != 1 {
				t.Fatalf("%s 应分发 1 条，实际 %d 条", c.typ, robot)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.robot[0] != c.typ {
				t.Fatalf("事件类型不对: got %s want %s", h.robot[0], c.typ)
			}
		})
	}
}

// ---------- C. 请求体上限 ----------

func TestBodyOverLimitRejected(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	// 造一个超过 1 MiB 的合法信封
	big := strings.Repeat("x", maxBodyBytes+1024)
	payload := `{"id":"evt-big","op":0,"t":"GROUP_MESSAGE_CREATE","d":{"content":"` + big + `"}}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应返回 413，实际 %d", w.Code)
	}
	if g, _, _ := h.snapshot(); g != 0 {
		t.Fatal("被限流的请求不应被分发")
	}
}

// ---------- D. 签名时间戳新鲜度 ----------

func TestSignatureStaleTimestampRejected(t *testing.T) {
	const secret = testSecret
	body := []byte(`{"op":0,"d":{},"t":"GROUP_AT_MESSAGE_CREATE"}`)

	// 6 分钟前：签名完全合法，但太旧了
	stale := strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10)
	req := signReq(t, secret, stale, body)
	if VerifyRequest(secret, req, body) {
		t.Fatal("超过 5 分钟的签名包应被拒绝（可无限重放）")
	}
	// 未来 6 分钟同理，防止有人往回拨时钟
	future := strconv.FormatInt(time.Now().Add(6*time.Minute).Unix(), 10)
	req2 := signReq(t, secret, future, body)
	if VerifyRequest(secret, req2, body) {
		t.Fatal("来自未来的时间戳应被拒绝")
	}
	// 窗口内必须放行
	ok := signReq(t, secret, nowTs(), body)
	if !VerifyRequest(secret, ok, body) {
		t.Fatal("新鲜签名应通过校验")
	}
	// 非数字时间戳拒绝
	req3 := signReq(t, secret, "not-a-number", body)
	if VerifyRequest(secret, req3, body) {
		t.Fatal("非数字时间戳应被拒绝")
	}
}

// 端到端：过期签名的回调应拿到 401 且不入队
func TestServerRejectsStaleSignature(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	dir := t.TempDir()
	srv := NewWithQueue(st, h, dir)
	payload := `{"id":"evt-replay","op":0,"t":"GROUP_MESSAGE_CREATE","d":{"content":"重放"}}`
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signReq(t, testSecret, stale, []byte(payload)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("过期签名应返回 401，实际 %d", w.Code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("过期签名的事件不应落盘，目录里有 %d 个文件", len(entries))
	}
}

// ---------- B. 落盘队列 ----------

func queueFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 启用队列后 HTTP 线程只做「验签 → 落盘 → 200」，业务处理交给后台 worker
func TestQueueDurableThenConsumed(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	dir := t.TempDir()
	srv := NewWithQueue(st, h, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("队列启动失败: %v", err)
	}

	payload := `{"id":"evt-q1","op":0,"t":"GROUP_MESSAGE_CREATE",
		"d":{"id":"ROBOT1.0_q1","content":"落盘队列","group_openid":"G1"}}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatalf("落盘成功应返回 200，实际 %d", w.Code)
	}
	if body := w.Body.String(); body != `{"op":12}` {
		t.Fatalf("ACK 内容不对: %q", body)
	}
	h.waitCount(t, 1, 3*time.Second)
	// 消费成功后文件应被删除
	deadline := time.Now().Add(2 * time.Second)
	for len(queueFiles(t, dir)) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fs := queueFiles(t, dir); len(fs) != 0 {
		t.Fatalf("消费成功后队列应清空，残留 %v", fs)
	}
}

// 同一 MsgID 重复推送只入队一次
func TestQueueDedupeByMsgID(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	srv := NewWithQueue(st, &mockHandler{}, dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"id":"evt-dup","op":0,"t":"GROUP_MESSAGE_CREATE","d":{"content":"重复"}}`)
	for i := 0; i < 5; i++ {
		if err := srv.enqueue(payload, "evt-dup"); err != nil {
			t.Fatalf("第 %d 次入队失败: %v", i, err)
		}
	}
	if fs := queueFiles(t, dir); len(fs) != 1 {
		t.Fatalf("同一 MsgID 应只入队一次，实际 %d 个文件: %v", len(fs), fs)
	}
	// 无 MsgID 时每条都要留下独立文件，不能互相覆盖
	noID := []byte(`{"op":0,"t":"GROUP_MESSAGE_CREATE","d":{"content":"无ID"}}`)
	for i := 0; i < 3; i++ {
		if err := srv.enqueue(noID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if fs := queueFiles(t, dir); len(fs) != 4 {
		t.Fatalf("无 MsgID 的三条应各自留文件，合计应为 4，实际 %d: %v", len(fs), fs)
	}
}

// 队列目录不可写时降级同步处理，且处理失败必须回 5xx 而不是骗平台成功
func TestQueueDegradesToSyncWhenDirUnusable(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	// 用一个「本该是目录却是普通文件」的位置把队列目录顶掉，enqueue 必然失败 → 走同步降级
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewWithQueue(st, h, filepath.Join(blocker, "queue"))
	payload := `{"id":"evt-sync","op":0,"t":"GROUP_MESSAGE_CREATE","d":{"id":"ROBOT1.0_s","content":"同步","group_openid":"G1"}}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatalf("降级同步处理成功应返回 200，实际 %d", w.Code)
	}
	if g, _, _ := h.snapshot(); g != 1 {
		t.Fatalf("降级后 handler 应收到事件，实际 %d 条", g)
	}
}

// 降级同步处理里解析失败 → 5xx，让平台重投，而不是像以前那样回 200 把事件丢了
func TestSyncFailureReturns5xx(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h) // 不启用队列，直接同步
	// d 是个字符串而不是对象，解不出 GroupMessage
	payload := `{"id":"evt-bad","op":0,"t":"GROUP_MESSAGE_CREATE","d":"不是对象"}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("同步处理失败应返回 5xx 让平台重投，实际 %d", w.Code)
	}
	if g, _, _ := h.snapshot(); g != 0 {
		t.Fatal("解析失败不应进 handler")
	}
}

// 处理失败时事件不丢失：文件留在盘上重试，重试到上限才进 failed/
func TestQueueKeepsEventWhenDispatchFails(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	dir := t.TempDir()
	srv := NewWithQueue(st, h, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// d 解析失败 → dispatch 返回 error
	payload := `{"id":"evt-poison","op":0,"t":"GROUP_MESSAGE_CREATE","d":"不是对象"}`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := srv.enqueue([]byte(payload), "evt-poison"); err != nil {
		t.Fatal(err)
	}
	// 观察若干轮：文件既不该被删掉，也不该消失
	for i := 0; i < 4; i++ {
		time.Sleep(250 * time.Millisecond)
		inQueue := len(queueFiles(t, dir))
		inFailed := len(queueFiles(t, filepath.Join(dir, "failed")))
		if inQueue == 0 && inFailed == 0 {
			t.Fatal("处理失败的事件被丢掉了，既不在队列也不在 failed 目录")
		}
	}
	if g, _, _ := h.snapshot(); g != 0 {
		t.Fatal("解析失败不应进 handler")
	}
	// 重试到上限后应转入 failed 目录留证据
	deadline := time.Now().Add(10 * time.Second)
	for len(queueFiles(t, filepath.Join(dir, "failed"))) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if fs := queueFiles(t, filepath.Join(dir, "failed")); len(fs) != 1 {
		t.Fatalf("重试到上限后应转入 failed 目录，实际 %v", fs)
	}
}

// 重启后队列能被消费：文件就是持久化，换个 Server 实例接着干
func TestQueueSurvivesRestart(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()

	// 第一个「进程」：只落盘、不启动 worker，等价于「收到就崩了」
	h1 := &mockHandler{}
	srv1 := NewWithQueue(st, h1, dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `{"id":"evt-restart","op":0,"t":"GROUP_MESSAGE_CREATE",
		"d":{"id":"ROBOT1.0_r","content":"重启后还在","group_openid":"G1"}}`
	w := httptest.NewRecorder()
	srv1.ServeHTTP(w, post(t, payload))
	if g, _, _ := h1.snapshot(); g != 0 {
		t.Fatal("没启动 worker 时事件不应被处理")
	}
	if fs := queueFiles(t, dir); len(fs) != 1 {
		t.Fatalf("事件应已落盘待消费，实际文件 %v", fs)
	}

	// 第二个「进程」：重新 Start，同一个目录
	h2 := &mockHandler{}
	srv2 := NewWithQueue(st, h2, dir)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := srv2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	h2.waitCount(t, 1, 3*time.Second)
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if h2.group[0].Content != "重启后还在" {
		t.Fatalf("重启后消费到的内容不对: %+v", h2.group[0])
	}
}

// 进程崩在写盘中途留下的半截临时文件，启动时应被清掉
func TestQueueSweepsStaleTempFiles(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "evt-x.1725442341abcd.json.tmp")
	if err := os.WriteFile(stale, []byte(`{"op":0,`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := NewWithQueue(st, &mockHandler{}, dir).Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("残留的 .tmp 半截文件应被清理")
	}
}

// 未识别事件不是「丢失」，但必须留痕（早先是 Debug，生产环境完全静默）
func TestUnknownEventIsAckedNotLost(t *testing.T) {
	st := newTestStore(t)
	h := &mockHandler{}
	srv := New(st, h)
	payload := `{"id":"evt-unknown","op":0,"t":"FRIEND_ADD","d":{"openid":"X1"}}`
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatalf("未订阅事件应正常 ACK，实际 %d", w.Code)
	}
	if g, c, r := h.snapshot(); g+c+r != 0 {
		t.Fatal("未订阅事件不应进 handler")
	}
}
