package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/brain"
	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memory"
	"dadyumo/internal/takeover"
)

// fakeAnchorSource 是 takeover.AnchorSource 的测试桩。
type fakeAnchorSource struct {
	groups map[string][]takeover.AnchorSnapshot
}

func (f *fakeAnchorSource) PickAnchor(group, prefer string) (takeover.AnchorSnapshot, bool, bool) {
	for _, a := range f.groups[group] {
		if !a.Alive {
			continue
		}
		return a, prefer == "" || a.SenderOpenID == prefer, true
	}
	return takeover.AnchorSnapshot{}, false, false
}

func (f *fakeAnchorSource) ListAnchors(group string) ([]takeover.AnchorSnapshot, int) {
	all := append([]takeover.AnchorSnapshot(nil), f.groups[group]...)
	alive := 0
	for _, a := range all {
		if a.Alive {
			alive++
		}
	}
	return all, alive
}

func (f *fakeAnchorSource) AnchorGroups() []string {
	out := make([]string, 0, len(f.groups))
	for g := range f.groups {
		out = append(out, g)
	}
	return out
}

// testSender 只记录发送，不碰网络。
type testSender struct {
	mu    sync.Mutex
	count int
	last  string
}

func (t *testSender) SendGroup(ctx context.Context, groupID, content string) error {
	return t.SendGroupTo(ctx, groupID, content, "")
}

func (t *testSender) SendGroupTo(ctx context.Context, groupID, content, replyTo string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.count++
	t.last = content
	return nil
}

func (t *testSender) SendGroupQuote(ctx context.Context, groupID, content, replyTo string) error {
	return t.SendGroupTo(ctx, groupID, content, replyTo)
}

const (
	tiGroup    = "group-openid-1"
	tiMember   = "member-openid-1"
	tiLease    = "lease-abcdef-1"
	tiLeaseAlt = "lease-abcdef-2"
)

// newTakeoverFixture 构造带真实 Engine + takeover.Store 的管理端。
// Engine 必须显式注入，不能依赖 newTestServer 的 nil engine 做租约/发送测试。
func newTakeoverFixture(t *testing.T) (*Server, *config.Store, *brain.Engine, *memory.Store, *fakeAnchorSource, *testSender, string) {
	t.Helper()
	srv, st := newTestServer(t, nil)

	mem := memory.New(20)
	g := mem.Group(tiGroup, "测试群")
	g.TouchMember(tiMember, "张三")
	g.TouchMember("member-openid-2", "李四")
	g.Append(memory.Line{TS: time.Now(), Role: memory.RoleUser, Name: "张三", OpenID: tiMember, Content: "在吗"}, 50)

	sender := &testSender{}
	eng := brain.NewEngine(st, llm.NewRouter(st), mem, sender)
	tkPath := filepath.Join(t.TempDir(), "takeover.json")
	tk, err := takeover.Open(tkPath)
	if err != nil {
		t.Fatal(err)
	}
	eng.SetTakeoverStore(tk)

	anchors := &fakeAnchorSource{groups: map[string][]takeover.AnchorSnapshot{
		tiGroup: {{
			MsgID: "msg-1", SenderOpenID: tiMember, SenderName: "张三",
			Used: 0, MaxUse: 5, Alive: true, ExpiresAt: time.Now().Add(4 * time.Minute).Unix(),
		}},
	}}
	eng.SetTakeoverAnchors(anchors)

	srv.engine = eng
	srv.mem = mem
	srv.anchors = anchors
	return srv, st, eng, mem, anchors, sender, tkPath
}

// takeoverCall 发一个管理端请求；cookie 为 nil 时不带会话。
func takeoverCall(t *testing.T, srv *Server, cookie *http.Cookie, method, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/admin"+path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler("/admin").ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body.String())
	}
	return m
}

// TestTakeoverRoutesRequireAuth 7 条路由全部套 s.auth。
func TestTakeoverRoutesRequireAuth(t *testing.T) {
	srv, _, _, _, _, _, _ := newTakeoverFixture(t)
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/takeover/mode"},
		{http.MethodPost, "/api/takeover/lease"},
		{http.MethodGet, "/api/takeover/window"},
		{http.MethodGet, "/api/takeover/members"},
		{http.MethodPost, "/api/takeover/send"},
		{http.MethodPost, "/api/takeover/meme"},
		{http.MethodPost, "/api/takeover/end_all"},
	}
	for _, rt := range routes {
		rec := takeoverCall(t, srv, nil, rt.method, rt.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码 = %d，期望 401", rt.method, rt.path, rec.Code)
		}
	}
}

// TestTakeoverModeSwitch 覆盖模式切换、幂等、未知群、c2c 与统一校验。
func TestTakeoverModeSwitch(t *testing.T) {
	srv, _, eng, _, _, _, tkPath := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)

	rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"`+tiGroup+`","mode":"always"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("切 always: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["mode"] != "always" || m["ai_paused"] != true || m["paused_reason"] != "mode_always" {
		t.Fatalf("always 响应不对: %v", m)
	}
	if eng.TakeoverMode(tiGroup) != takeover.ModeAlways {
		t.Fatalf("内存模式 = %q", eng.TakeoverMode(tiGroup))
	}
	// 落盘：重新打开同一个文件应读回 always。
	reopened, _ := takeover.Open(tkPath)
	if reopened.Mode(tiGroup) != takeover.ModeAlways {
		t.Fatalf("重启后模式 = %q，期望 always", reopened.Mode(tiGroup))
	}

	// 同值重复设置不报错、状态不变。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"`+tiGroup+`","mode":"always"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("重复切 always: %d %s", rec.Code, rec.Body.String())
	}

	// 非法模式。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"`+tiGroup+`","mode":"auto"}`, "application/json")
	if rec.Code != http.StatusBadRequest || decodeMap(t, rec)["reason"] != "bad_mode" {
		t.Fatalf("非法模式 = %d %s", rec.Code, rec.Body.String())
	}

	// 未知群。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"unknown-group-xyz","mode":"off"}`, "application/json")
	if rec.Code != http.StatusNotFound || decodeMap(t, rec)["reason"] != "unknown_group" {
		t.Fatalf("未知群 = %d %s", rec.Code, rec.Body.String())
	}

	// c2c 拒绝。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"c2c:ABCDEF123456","mode":"off"}`, "application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "c2c_not_supported" {
		t.Fatalf("c2c = %d %s", rec.Code, rec.Body.String())
	}

	// 非 JSON Content-Type。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"`+tiGroup+`","mode":"off"}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON = %d %s", rec.Code, rec.Body.String())
	}

	// 方法不对。
	rec = takeoverCall(t, srv, cookie, http.MethodGet, "/api/takeover/mode", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET mode = %d", rec.Code)
	}

	// Origin 与 Host 不一致。
	req := httptest.NewRequest(http.MethodPost, "/admin/api/takeover/mode",
		strings.NewReader(`{"openid":"`+tiGroup+`","mode":"off"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler("/admin").ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden || decodeMap(t, rec2)["reason"] != "bad_origin" {
		t.Fatalf("跨站 Origin = %d %s", rec2.Code, rec2.Body.String())
	}
}

// TestTakeoverEndAll end_all 的群并集、计数、epoch 与重复调用幂等。
func TestTakeoverEndAll(t *testing.T) {
	srv, _, eng, mem, anchors, _, _ := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)

	// 第二个记忆群：从没改过模式，也要被 end_all 覆盖。
	mem.Group("group-openid-2", "二群")
	// 只存在于锚点池的群。
	anchors.groups["group-openid-3"] = []takeover.AnchorSnapshot{{
		MsgID: "m3", SenderOpenID: "member-openid-3", SenderName: "王五",
		Used: 1, MaxUse: 5, Alive: true, ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}}
	// 先拿一条租约（window 档才会建），验证 end_all 会清掉。
	if err := eng.AcquireLease(tiGroup, tiLease, false, 0); err != nil {
		t.Fatal(err)
	}

	rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/end_all", `{}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("end_all: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["modes_reset"] != float64(3) {
		t.Fatalf("modes_reset = %v，期望 3（记忆两群 + 锚点群）", m["modes_reset"])
	}
	if m["leases_released"] != float64(1) {
		t.Fatalf("leases_released = %v", m["leases_released"])
	}
	if m["epoch"] != float64(1) {
		t.Fatalf("epoch = %v，期望 1", m["epoch"])
	}
	for _, g := range []string{tiGroup, "group-openid-2", "group-openid-3"} {
		if eng.TakeoverMode(g) != takeover.ModeOff {
			t.Fatalf("%s 模式 = %q，期望 off", g, eng.TakeoverMode(g))
		}
	}
	if eng.AIPaused(tiGroup) {
		t.Fatal("end_all 后 AI 仍在暂停")
	}

	// 重复调用：计数为 0，epoch 继续递增。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/end_all", `{}`, "application/json")
	m = decodeMap(t, rec)
	if rec.Code != http.StatusOK || m["modes_reset"] != float64(0) {
		t.Fatalf("重复 end_all = %d %v", rec.Code, m)
	}
	if m["epoch"] != float64(2) {
		t.Fatalf("重复 end_all epoch = %v，期望 2", m["epoch"])
	}
}

// TestTakeoverLeaseLifecycle acquire / beat / release / 墓碑 / stale epoch。
func TestTakeoverLeaseLifecycle(t *testing.T) {
	srv, st, eng, _, _, _, _ := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)
	_ = st

	// 默认 window 档，无租约 → acquire。
	rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"acquire","visibility":"visible","epoch":0}`,
		"application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["lease_valid"] != true || m["lease_count"] != float64(1) || m["ai_paused"] != true {
		t.Fatalf("acquire 响应: %v", m)
	}

	// beat 续期。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"beat","epoch":0}`,
		"application/json")
	if rec.Code != http.StatusOK || decodeMap(t, rec)["lease_valid"] != true {
		t.Fatalf("beat: %d %s", rec.Code, rec.Body.String())
	}

	// release closing:false → 立即删除，AI 恢复。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"release","closing":false,"epoch":0}`,
		"application/json")
	m = decodeMap(t, rec)
	if rec.Code != http.StatusOK || m["released"] != true || m["lease_count"] != float64(0) || m["ai_paused"] != false {
		t.Fatalf("release: %d %v", rec.Code, m)
	}

	// 重复 release 返回 released:false。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"release","closing":false,"epoch":0}`,
		"application/json")
	if m = decodeMap(t, rec); m["released"] != false {
		t.Fatalf("重复 release = %v", m["released"])
	}

	// off 档下 acquire 被拒。
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"acquire","epoch":0}`,
		"application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "mode_off" {
		t.Fatalf("off acquire = %d %s", rec.Code, rec.Body.String())
	}

	// 回到 window，end_all 后旧 epoch 被拒（先被 mode_off 挡，切回 window 后是 stale_epoch）。
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeWindow); err != nil {
		t.Fatal(err)
	}
	oldEpoch := eng.TakeoverEpoch()
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/end_all", `{}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("end_all: %d", rec.Code)
	}
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeWindow); err != nil {
		t.Fatal(err)
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"acquire","epoch":`+epochStr(oldEpoch)+`}`,
		"application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "stale_epoch" {
		t.Fatalf("stale epoch = %d %s", rec.Code, rec.Body.String())
	}

	// 用新 epoch acquire 成功。
	newEpoch := eng.TakeoverEpoch()
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"acquire","epoch":`+epochStr(newEpoch)+`}`,
		"application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("新 epoch acquire: %d %s", rec.Code, rec.Body.String())
	}

	// final release 写墓碑，同 id 60 秒内不能复活。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"release","final":true,"epoch":`+epochStr(newEpoch)+`}`,
		"application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("final release: %d %s", rec.Code, rec.Body.String())
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/lease",
		`{"openid":"`+tiGroup+`","lease_id":"`+tiLease+`","action":"acquire","epoch":`+epochStr(newEpoch)+`}`,
		"application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "lease_released" {
		t.Fatalf("墓碑 acquire = %d %s", rec.Code, rec.Body.String())
	}
}

// TestTakeoverWindowAndMembers window 只读 DTO 与成员接口。
func TestTakeoverWindowAndMembers(t *testing.T) {
	srv, _, eng, _, _, _, _ := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)
	// window 档下 lease_id 指向存活租约，can_send 才该是 true。
	if err := eng.AcquireLease(tiGroup, tiLease, false, 0); err != nil {
		t.Fatal(err)
	}

	rec := takeoverCall(t, srv, cookie, http.MethodGet,
		"/api/takeover/window?openid="+tiGroup+"&lease_id="+tiLease, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("window: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["mode"] != "window" || m["can_send"] != true || m["can_send_reason"] != "" {
		t.Fatalf("window DTO: %v", m)
	}
	if m["total_quota"] != float64(5) {
		t.Fatalf("total_quota = %v，期望 5", m["total_quota"])
	}
	if sel, ok := m["selected"].(map[string]any); !ok || sel["sender_openid"] != tiMember {
		t.Fatalf("selected = %v", m["selected"])
	}
	if lines, ok := m["last_lines"].([]any); !ok || len(lines) != 1 {
		t.Fatalf("last_lines = %v", m["last_lines"])
	}

	// lease_id 为空时 window 档 can_send_reason = lease_required。
	rec = takeoverCall(t, srv, cookie, http.MethodGet, "/api/takeover/window?openid="+tiGroup, "", "")
	if m = decodeMap(t, rec); m["can_send_reason"] != "lease_required" {
		t.Fatalf("无租约 can_send_reason = %v", m["can_send_reason"])
	}

	// 成员列表与过滤。
	rec = takeoverCall(t, srv, cookie, http.MethodGet, "/api/takeover/members?openid="+tiGroup, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("members: %d %s", rec.Code, rec.Body.String())
	}
	m = decodeMap(t, rec)
	if m["total"] != float64(2) {
		t.Fatalf("members total = %v，期望 2", m["total"])
	}
	rec = takeoverCall(t, srv, cookie, http.MethodGet, "/api/takeover/members?openid="+tiGroup+"&q=张", "", "")
	if m = decodeMap(t, rec); m["total"] != float64(1) {
		t.Fatalf("members 过滤 total = %v", m["total"])
	}

	// 未知群。
	rec = takeoverCall(t, srv, cookie, http.MethodGet, "/api/takeover/window?openid=unknown-group-xyz", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("window 未知群 = %d", rec.Code)
	}
}

// TestTakeoverSendValidation send 的参数校验、409 原因、成功与幂等。
func TestTakeoverSendValidation(t *testing.T) {
	srv, st, eng, _, anchors, sender, _ := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)

	// 先切 always，绕开租约要求。
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		body   string
		code   int
		reason string
	}{
		{"client_msg_id 缺失", `{"openid":"` + tiGroup + `","text":"hi"}`, 400, "bad_client_msg_id"},
		{"空文本", `{"openid":"` + tiGroup + `","text":"  ","client_msg_id":"client-msg-0001"}`, 400, "text_empty"},
		{"bad_at", `{"openid":"` + tiGroup + `","text":"hi","at_openids":["stranger-id-1"],"client_msg_id":"client-msg-0001"}`, 400, "bad_at"},
		{"bad_reply_to", `{"openid":"` + tiGroup + `","text":"hi","reply_to_openid":"stranger-id-1","client_msg_id":"client-msg-0001"}`, 400, "bad_reply_to"},
		{"c2c", `{"openid":"c2c:ABCDEF123456","text":"hi","client_msg_id":"client-msg-0001"}`, 409, "c2c_not_supported"},
		{"未知群", `{"openid":"unknown-group-xyz","text":"hi","client_msg_id":"client-msg-0001"}`, 404, "unknown_group"},
	}
	for _, c := range cases {
		rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send", c.body, "application/json")
		if rec.Code != c.code {
			t.Fatalf("%s 状态码 = %d，期望 %d（%s）", c.name, rec.Code, c.code, rec.Body.String())
		}
		if got := decodeMap(t, rec)["reason"]; got != c.reason {
			t.Fatalf("%s reason = %v，期望 %s", c.name, got, c.reason)
		}
	}

	// 跨站头。
	req := httptest.NewRequest(http.MethodPost, "/admin/api/takeover/send",
		strings.NewReader(`{"openid":"`+tiGroup+`","text":"hi","client_msg_id":"client-msg-0001"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler("/admin").ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site = %d", rec.Code)
	}

	// 群被配置禁用 → group_disabled（只挡 send/meme）。
	if err := st.Update(func(c *config.Config) (bool, error) {
		c.Groups = append(c.Groups, config.GroupConfig{OpenID: tiGroup, Enabled: false})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send",
		`{"openid":"`+tiGroup+`","text":"hi","client_msg_id":"client-msg-0001"}`, "application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "group_disabled" {
		t.Fatalf("group_disabled = %d %s", rec.Code, rec.Body.String())
	}
	// mode 写接口对禁用群仍允许。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/mode",
		`{"openid":"`+tiGroup+`","mode":"always"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("禁用群切模式 = %d %s", rec.Code, rec.Body.String())
	}
	// 恢复启用。
	if err := st.Update(func(c *config.Config) (bool, error) {
		for i := range c.Groups {
			if c.Groups[i].OpenID == tiGroup {
				c.Groups[i].Enabled = true
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	// 成功发送。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send",
		`{"openid":"`+tiGroup+`","text":"你好呀","client_msg_id":"client-msg-0001"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["duplicate"] != false || m["sent_text"] != "你好呀" || m["ok"] != true {
		t.Fatalf("send 响应: %v", m)
	}
	if sender.count != 1 || sender.last != "你好呀" {
		t.Fatalf("sender 调用 = %d %q", sender.count, sender.last)
	}

	// 同 client_msg_id 幂等：返回缓存结果，不重复发送。
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send",
		`{"openid":"`+tiGroup+`","text":"你好呀","client_msg_id":"client-msg-0001"}`, "application/json")
	if m = decodeMap(t, rec); m["duplicate"] != true {
		t.Fatalf("重复发送 duplicate = %v", m["duplicate"])
	}
	if sender.count != 1 {
		t.Fatalf("幂等失效，sender 被调用 %d 次", sender.count)
	}

	// 幂等命中必须排在 can_send 预检之前：第一次已成功但响应丢了，
	// 重试时额度用满 / 模式被切 off，仍应返回 duplicate:true 而不是 409。
	anchors.groups[tiGroup] = []takeover.AnchorSnapshot{{
		MsgID: "msg-1", SenderOpenID: tiMember, SenderName: "张三",
		Used: 5, MaxUse: 5, Alive: true, ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}}
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send",
		`{"openid":"`+tiGroup+`","text":"你好呀","client_msg_id":"client-msg-0001"}`, "application/json")
	if rec.Code != http.StatusOK || decodeMap(t, rec)["duplicate"] != true {
		t.Fatalf("额度用尽后幂等重试 = %d %s", rec.Code, rec.Body.String())
	}
	if sender.count != 1 {
		t.Fatalf("幂等重试又发了一次，sender = %d", sender.count)
	}
	// 复原：后面的 no_anchor 用例需要 always 档且锚点为空。
	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}

	// 锚点全空 → no_anchor。
	anchors.groups[tiGroup] = nil
	if err := st.Update(func(c *config.Config) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	rec = takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/send",
		`{"openid":"`+tiGroup+`","text":"再来一条","client_msg_id":"client-msg-0002"}`, "application/json")
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "no_anchor" {
		t.Fatalf("no_anchor = %d %s", rec.Code, rec.Body.String())
	}
}

// TestTakeoverMemeNotInitialized 表情包池未注入时返回 500，而不是 panic。
func TestTakeoverMemeNotInitialized(t *testing.T) {
	srv, _, _, _, _, _, _ := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)
	rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/takeover/meme",
		`{"openid":"`+tiGroup+`","meme_id":1,"client_msg_id":"client-msg-0001"}`, "application/json")
	if rec.Code != http.StatusInternalServerError || decodeMap(t, rec)["reason"] != "not_initialized" {
		t.Fatalf("meme = %d %s", rec.Code, rec.Body.String())
	}
}

// TestSaveConfigKeepsTakeoverMode 配置整包保存不覆盖接管模式文件。
func TestSaveConfigKeepsTakeoverMode(t *testing.T) {
	srv, st, eng, _, _, _, tkPath := newTakeoverFixture(t)
	cookie := authedCookie(t, srv)

	if err := eng.SetTakeoverMode(tiGroup, takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tkPath)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(st.Get())
	if err != nil {
		t.Fatal(err)
	}
	rec := takeoverCall(t, srv, cookie, http.MethodPost, "/api/config", string(raw), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("保存配置: %d %s", rec.Code, rec.Body.String())
	}
	if eng.TakeoverMode(tiGroup) != takeover.ModeAlways {
		t.Fatalf("保存配置后模式 = %q，期望 always", eng.TakeoverMode(tiGroup))
	}
	after, err := os.ReadFile(tkPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("配置保存改动了 takeover.json")
	}
}

// TestStateWithNilEngine engine=nil 时 /api/state 不 panic，接管字段取零值。
func TestStateWithNilEngine(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	// 群循环里会调用 nil engine 的 AIPauseInfo / TakeoverMode。
	srv.mem.Group("group-openid-1", "测试群")
	cookie := authedCookie(t, srv)

	rec := takeoverCall(t, srv, cookie, http.MethodGet, "/api/state", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["takeover_rev"] != float64(0) || m["epoch"] != float64(0) {
		t.Fatalf("nil engine rev/epoch = %v/%v", m["takeover_rev"], m["epoch"])
	}
	groups, ok := m["groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("groups = %v", m["groups"])
	}
	gv := groups[0].(map[string]any)
	if gv["takeover_mode"] != "window" || gv["ai_paused"] != false {
		t.Fatalf("nil engine 群字段 = %v", gv)
	}
}

// epochStr 把一个 epoch 转成 URL/JSON 里的十进制文本。
func epochStr(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
