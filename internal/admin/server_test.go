package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/memory"
	"dadyumo/internal/statsdb"
)

// newTestServer 构造一个带真实 Store + Router 的最小管理端（同包可触私有字段）。
// initCredentials 会生成会话密钥与初始密码，正好覆盖测试需要的 admin 字段。
func newTestServer(t *testing.T, mutate func(*config.Config)) (*Server, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	base := config.Default()
	base.QQ.AppID = "1905000000"
	base.QQ.AppSecret = "test-secret"
	base.LLM.Endpoints = []config.Endpoint{{
		ID: "ep-a", Name: "测试A", BaseURL: "https://a.example/v1", APIKey: "sk-real-key-aaaa",
		APIType: config.APIChatCompletions, Enabled: true,
		Models: []config.Model{{ID: "m1", Enabled: true, MaxCtx: 131072, MaxOut: 1024}},
	}}
	base.Admin.Username = "admin"
	base.Admin.PasswordPlain = "test-pass-123"
	base.Master.BindToken = "bind-token-xyz"
	base.ASR.Provider = "siliconflow"
	base.ASR.APIKey = "sk-asr-secret"
	base.Compact.BaseURL = "https://c.example/v1"
	base.Compact.APIKey = "sk-compact-secret"
	if mutate != nil {
		mutate(base)
	}
	b, _ := json.Marshal(base)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// mem 接真实实例：群记忆/别名/facts 相关 handler 需要它，
	// 现有 handler 都自带 s.mem != nil 守卫，接上不影响旧用例。
	srv, err := New(st, llm.NewRouter(st), nil, memory.New(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv, st
}

// authedCookie 登录拿到会话 cookie（handleState 等有 auth 中间件）
func authedCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	h := srv.Handler("/admin")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"user":"admin","pass":"test-pass-123"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("登录响应没有会话 cookie")
	return nil
}

// TestStateMasksAllSecrets B1：/api/state 不得明文下发任何密钥/口令/哈希
func TestStateMasksAllSecrets(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.AddCookie(authedCookie(t, srv))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 状态码: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{
		"sk-real-key-aaaa", "sk-asr-secret", "sk-compact-secret", "bind-token-xyz", "test-pass-123",
		"test-secret", // QQ AppSecret：曾经漏掩码，夹具里设了却从没断言过，测试假绿
	} {
		if strings.Contains(body, secret) {
			t.Errorf("state 泄露了敏感值 %q", secret)
		}
	}
	// bcrypt 哈希也不该出现（$2a$ 前缀）
	if strings.Contains(body, "$2a$") {
		t.Error("state 泄露了密码 bcrypt 哈希")
	}
}

// TestSaveConfigKeepsKeyWhenIDRenamed B2：前端改 endpoint id 后保存，掩码 key 必须按下标回退还原
func TestSaveConfigKeepsKeyWhenIDRenamed(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	// 拿到掩码版配置，改 id 后回传（模拟前端改标识）
	cfg := st.Get()
	cfg.LLM.Endpoints[0].APIKey = maskKey(cfg.LLM.Endpoints[0].APIKey)
	cfg.LLM.Endpoints[0].ID = "ep-a-renamed"
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}
	got := st.Get().LLM.Endpoints[0].APIKey
	if got != "sk-real-key-aaaa" {
		t.Fatalf("改 id 后真实 key 被覆盖: %q", got)
	}
	// 磁盘上也得是真 key
	st2, err := config.Load(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Get().LLM.Endpoints[0].APIKey != "sk-real-key-aaaa" {
		t.Fatal("落盘的 key 不是真实值")
	}
}

// TestSaveConfigKeepsWeather 天气三件套没有管理端表单，整包保存时前端按零值回传。
// 曾经踩过的坑：normalize() 会回填默认坐标，所以清零还能自愈；改成不回填后，
// 一旦这里放行清零，用户的天气就永久失效且没有任何自愈路径，只能手改配置文件。
func TestSaveConfigKeepsWeather(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	// 模拟用户手工在 config.json 里写了天气（这是唯一的配置入口）
	if err := st.Update(func(c *config.Config) (bool, error) {
		c.Brain.WeatherLat = 31.87
		c.Brain.WeatherLon = 120.55
		c.Brain.WeatherPlace = "示例市"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	// 前端整包保存：天气字段原样回传（掩码/未渲染时可能是零值，这里两种都验）
	cfg := st.Get()
	cfg.Brain.WeatherLat = 0
	cfg.Brain.WeatherLon = 0
	cfg.Brain.WeatherPlace = ""
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}

	got := st.Get().Brain
	if got.WeatherPlace != "示例市" {
		t.Errorf("保存后地名被清空: %q", got.WeatherPlace)
	}
	if got.WeatherLat == 0 || got.WeatherLon == 0 {
		t.Errorf("保存后坐标被清空: lat=%v lon=%v", got.WeatherLat, got.WeatherLon)
	}
}

// TestSaveConfigOptimisticLock B7：基于过期 rev 的保存必须 409 拒绝
func TestSaveConfigOptimisticLock(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	staleRev := st.Rev()
	// 另一处先改一次配置，rev 前进
	if _, err := st.BindMaster("OPENID-X"); err != nil {
		t.Fatal(err)
	}

	cfg := st.Get()
	payload, _ := json.Marshal(map[string]any{"rev": staleRev, "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("过期 rev 应 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 旧格式（裸 config，无 rev）兼容放行
	payload2, _ := json.Marshal(st.Get())
	req2 := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload2)))
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("裸 config 应兼容放行，实际 %d %s", rec2.Code, rec2.Body.String())
	}
}

// TestSaveConfigKeepsASRCompactMasked 掩码回传时 ASR/Compact 配置必须保留（B1 配套）
func TestSaveConfigKeepsASRCompactMasked(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	cfg := st.Get()
	cfg.ASR.APIKey = maskKey(cfg.ASR.APIKey)
	cfg.Compact.APIKey = maskKey(cfg.Compact.APIKey)
	cfg.Master.BindToken = maskKey(cfg.Master.BindToken)
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}
	got := st.Get()
	if got.ASR.APIKey != "sk-asr-secret" {
		t.Errorf("ASR key 丢失: %q", got.ASR.APIKey)
	}
	if got.Compact.APIKey != "sk-compact-secret" {
		t.Errorf("Compact key 丢失: %q", got.Compact.APIKey)
	}
	if got.Master.BindToken != "bind-token-xyz" {
		t.Errorf("BindToken 丢失: %q", got.Master.BindToken)
	}
}

// TestGroupAliasWritesConfigAndMemory 别名必须同时写进 config.groups 并刷新记忆群名
func TestGroupAliasWritesConfigAndMemory(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/group/alias",
		strings.NewReader(`{"openid":"G123","name":"测试群三号"}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("设置别名失败: %d %s", rec.Code, rec.Body.String())
	}
	cfg := st.Get()
	found := false
	for _, g := range cfg.Groups {
		if g.OpenID == "G123" {
			found = true
			if g.Name != "测试群三号" {
				t.Errorf("config 里的别名不对: %q", g.Name)
			}
			if !g.Enabled {
				t.Error("新建别名条目默认应启用")
			}
		}
	}
	if !found {
		t.Fatal("config.groups 里没有写入别名")
	}
	// 再改一次：应该更新而不是重复追加
	req2 := httptest.NewRequest(http.MethodPost, "/admin/api/group/alias",
		strings.NewReader(`{"openid":"G123","name":"新名字"}`))
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("更新别名失败: %d", rec2.Code)
	}
	cnt := 0
	for _, g := range st.Get().Groups {
		if g.OpenID == "G123" {
			cnt++
			if g.Name != "新名字" {
				t.Errorf("别名未更新: %q", g.Name)
			}
		}
	}
	if cnt != 1 {
		t.Errorf("同一 openid 出现 %d 条配置, want 1", cnt)
	}
}

// TestSaveConfigKeepsGroupsWhenNil 整包保存时 groups 为 null 不得清空别名表
func TestSaveConfigKeepsGroupsWhenNil(t *testing.T) {
	srv, st := newTestServer(t, func(c *config.Config) {
		c.Groups = []config.GroupConfig{{OpenID: "G1", Name: "群一", Enabled: true}}
	})
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	cfg := st.Get()
	cfg.Groups = nil // 模拟前端没回传
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := st.Get().Groups; len(got) != 1 || got[0].Name != "群一" {
		t.Errorf("groups 被整包保存清空: %+v", got)
	}
}

// TestUsageSeriesDisabledWithoutDB 统计库降级时 usage_series 必须明确报未启用而不是报错
func TestUsageSeriesDisabledWithoutDB(t *testing.T) {
	srv, _ := newTestServer(t, nil) // newTestServer 不传 stats，s.stats == nil
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage_series?range=today", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码: %d", rec.Code)
	}
	var body struct {
		OK      bool `json:"ok"`
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Enabled {
		t.Errorf("降级时应返回 ok+enabled=false: %s", rec.Body.String())
	}
}

// TestGroupFactSetDelete 长期要点的增/改/删要走通，且改动后要触发即时落盘
func TestGroupFactSetDelete(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	persists := 0
	srv.SetMemoryPersist(func() error { persists++; return nil })

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 新增
	rec := post("/admin/api/group/fact/set", `{"openid":"G1","key":"群主","value":"小明"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set 失败: %d %s", rec.Code, rec.Body.String())
	}
	if persists != 1 {
		t.Errorf("写入要点后应触发一次落盘, got %d", persists)
	}

	// 改（同 key 覆盖）
	if rec := post("/admin/api/group/fact/set", `{"openid":"G1","key":"群主","value":"小明，杭州人"}`); rec.Code != http.StatusOK {
		t.Fatalf("覆盖失败: %d %s", rec.Code, rec.Body.String())
	}
	list := srv.mem.Group("G1", "").FactsList()
	if len(list) != 1 || list[0].Value != "小明，杭州人" {
		t.Fatalf("覆盖未生效: %+v", list)
	}

	// 空 key / 空 value 必须 400
	if rec := post("/admin/api/group/fact/set", `{"openid":"G1","key":"","value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("空 key 应 400, got %d", rec.Code)
	}
	if rec := post("/admin/api/group/fact/set", `{"openid":"G1","key":"k","value":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("空 value 应 400, got %d", rec.Code)
	}
	// 缺 openid 必须 400
	if rec := post("/admin/api/group/fact/set", `{"key":"k","value":"v"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 openid 应 400, got %d", rec.Code)
	}

	// 删除
	if rec := post("/admin/api/group/fact/delete", `{"openid":"G1","key":"群主"}`); rec.Code != http.StatusOK {
		t.Fatalf("delete 失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := srv.mem.Group("G1", "").FactsList(); len(got) != 0 {
		t.Errorf("删除后应为空: %+v", got)
	}
	if rec := post("/admin/api/group/fact/delete", `{"openid":"G1","key":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("空 key 删除应 400, got %d", rec.Code)
	}
}

// TestGroupFactSetReportsEviction 满容量写新要点时，响应里要带回被挤掉的 key
func TestGroupFactSetReportsEviction(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/group/fact/set", strings.NewReader(body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < memory.MaxFacts; i++ {
		if rec := post(fmt.Sprintf(`{"openid":"G1","key":"k%02d","value":"v"}`, i)); rec.Code != http.StatusOK {
			t.Fatalf("填充第 %d 条失败: %d", i, rec.Code)
		}
	}
	rec := post(`{"openid":"G1","key":"kNEW","value":"v"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("第 %d 条失败: %d", memory.MaxFacts+1, rec.Code)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Evicted string `json:"evicted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Evicted == "" {
		t.Errorf("满容量写入应在 evicted 里报告被淘汰的 key: %s", rec.Body.String())
	}
	if got := len(srv.mem.Group("G1", "").FactsList()); got != memory.MaxFacts {
		t.Errorf("总数应保持 %d, got %d", memory.MaxFacts, got)
	}
}

// TestStateExposesFactsItems handleState 要下发结构化长期要点（前端要拿它渲染编辑区）
func TestStateExposesFactsItems(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.mem.Group("G1", "群一").SetFact("群主", "小明")
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 失败: %d", rec.Code)
	}
	var out struct {
		Groups []struct {
			OpenID     string `json:"openid"`
			FactsItems []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"facts_items"`
			FactsMax int `json:"facts_max"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("应有 1 个群, got %d", len(out.Groups))
	}
	g := out.Groups[0]
	if g.FactsMax != memory.MaxFacts {
		t.Errorf("facts_max 应为 %d, got %d", memory.MaxFacts, g.FactsMax)
	}
	if len(g.FactsItems) != 1 || g.FactsItems[0].Key != "群主" || g.FactsItems[0].Value != "小明" {
		t.Errorf("facts_items 结构不对: %+v", g.FactsItems)
	}
}

// TestUsageSeriesRejectsUnknownRange range 白名单要收紧，别再接受任意值。
// 注意必须给 server 接一个真实统计库：stats=nil 时接口会在 range 判断之前
// 就短路成「未启用」，永远走不到 400 那条分支。
func TestUsageSeriesRejectsUnknownRange(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	db, err := statsdb.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	srv.stats = db

	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage_series?range=bogus", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知 range 应 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestUsageSeries60mShape 分钟档要返回 60 个桶（前端直接画，不补缺口）
func TestUsageSeries60mShape(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	db, err := statsdb.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	srv.stats = db

	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage_series?range=60m", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Enabled bool   `json:"enabled"`
		Range   string `json:"range"`
		Buckets []struct {
			Label  string `json:"label"`
			Minute int    `json:"minute"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || !body.Enabled || body.Range != "60m" {
		t.Fatalf("返回不对: %s", rec.Body.String())
	}
	if len(body.Buckets) != 60 {
		t.Fatalf("应返回 60 个分钟桶, got %d", len(body.Buckets))
	}
	if body.Buckets[0].Label == "" {
		t.Error("分钟桶应有 HH:MM 标签")
	}
}

// TestUsageSeriesAndRecentCallsDegradeWithoutDB 统计库缺失时两个接口都要「明确说未启用」而不是报错
func TestUsageSeriesAndRecentCallsDegradeWithoutDB(t *testing.T) {
	srv, _ := newTestServer(t, nil) // newTestServer 不传 stats
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	for _, path := range []string{"/admin/api/usage_series?range=60m", "/admin/api/recent_calls?limit=10"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码: %d", path, rec.Code)
			continue
		}
		var body struct {
			OK      bool `json:"ok"`
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.OK || body.Enabled {
			t.Errorf("%s 降级时应返回 ok+enabled=false: %s", path, rec.Body.String())
		}
	}
}

// TestStateExposesChain handleState 必须下发「实际调度顺序」，
// 否则概览面板拿不到真实命中链路
func TestStateExposesChain(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 失败: %d", rec.Code)
	}
	var out struct {
		Chain struct {
			Chain []struct {
				Rank  int    `json:"rank"`
				Model string `json:"model"`
			} `json:"chain"`
			MaxAttempts int     `json:"max_attempts"`
			ExploreRate float64 `json:"explore_rate"`
		} `json:"chain"`
		Targets []struct {
			Model string `json:"model"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// 夹具里配了 ep-a / m1，链路应该正好列出它
	if len(out.Chain.Chain) != 1 {
		t.Fatalf("链路应有 1 个目标, got %+v", out.Chain)
	}
	if out.Chain.Chain[0].Rank != 1 || out.Chain.Chain[0].Model != "m1" {
		t.Errorf("链路内容不对: %+v", out.Chain.Chain[0])
	}
	if out.Chain.MaxAttempts <= 0 || out.Chain.ExploreRate <= 0 {
		t.Errorf("max_attempts / explore_rate 应透出: %+v", out.Chain)
	}
	// targets 仍然照常下发（健康度表用）
	if len(out.Targets) != 1 || out.Targets[0].Model != "m1" {
		t.Errorf("targets 不对: %+v", out.Targets)
	}
}

// TestSaveConfigKeepsBaseURL base_url 不允许从管理端改：
// 改了它，/api/test 会拿真实 API Key 去请求攻击者指定的地址（SSRF + 密钥外发通道）。
func TestSaveConfigKeepsBaseURL(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	cfg := st.Get()
	cfg.LLM.Endpoints[0].BaseURL = "http://169.254.169.254/latest/meta-data"
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := st.Get().LLM.Endpoints[0].BaseURL; got != "https://a.example/v1" {
		t.Fatalf("base_url 被管理端改了，构成 SSRF 通道: %q", got)
	}
}

// TestOversizedBodyRejected 请求体必须封顶，否则公开的登录接口几个大 body 就能 OOM 掉进程
func TestOversizedBodyRejected(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")

	huge := `{"user":"admin","pass":"` + strings.Repeat("x", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(huge))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("超大登录体应 400, got %d", rec.Code)
	}

	cookie := authedCookie(t, srv)
	cfg := srv.store.Get()
	cfg.Persona.Background = strings.Repeat("y", 8<<20)
	payload, _ := json.Marshal(map[string]any{"rev": -1, "config": cfg})
	req2 := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("超大配置体应 400, got %d", rec2.Code)
	}
}

// TestSessionCookieSecure 会话 cookie 必须带 Secure（全站 HTTPS）
func TestSessionCookieSecure(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"user":"admin","pass":"test-pass-123"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.Secure {
				t.Error("会话 cookie 缺 Secure")
			}
			if !c.HttpOnly {
				t.Error("会话 cookie 缺 HttpOnly")
			}
			return
		}
	}
	t.Fatal("没有拿到会话 cookie")
}

// TestMuteZeroUnmutes 守住「取消静默」这条指令。
//
// 原来 body.Minutes 是 int，0 被当成「没给」→ 重新静默 30 分钟，
// 于是界面上想取消反而取消不掉，只能等它自己到期。
// 现在 Minutes 是指针：nil = 没给（默认 30），0 = 明确取消。
func TestMuteZeroUnmutes(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)
	const gid = "g-mute-1"

	post := func(body string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/mute", strings.NewReader(body))
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s -> %d %s", body, rec.Code, rec.Body.String())
		}
		var j map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		return j
	}

	// 先静默，确认真的静默了
	j := post(`{"openid":"` + gid + `","minutes":30}`)
	if j["muted"] != true {
		t.Fatalf("minutes=30 期望 muted=true，实际 %v", j["muted"])
	}
	until, _ := j["until"].(float64)
	if until <= 0 {
		t.Fatalf("minutes=30 期望返回非零 until，实际 %v", j["until"])
	}
	if got := srv.mem.Group(gid, "").MutedUntil(); !got.After(time.Now()) {
		t.Fatalf("内存里没处于静默状态，mutedUntil=%v", got)
	}

	// minutes=0 必须取消，而不是又静默 30 分钟
	j = post(`{"openid":"` + gid + `","minutes":0}`)
	if j["muted"] != false {
		t.Fatalf("minutes=0 期望 muted=false，实际 %v", j["muted"])
	}
	if got := srv.mem.Group(gid, "").MutedUntil(); !got.IsZero() {
		t.Fatalf("取消后 mutedUntil 应为零值，实际 %v", got)
	}

	// 完全不传 minutes 走默认 30（这是指针方案要保住的老行为）
	j = post(`{"openid":"` + gid + `"}`)
	if j["muted"] != true {
		t.Fatalf("不传 minutes 期望默认静默，muted=%v", j["muted"])
	}
}

// TestMuteDeadlineHidesExpired 已过期的静默必须报 0，
// 否则界面会算出「剩 -3 分钟」。
func TestMuteDeadlineHidesExpired(t *testing.T) {
	if got := muteDeadline(time.Now().Add(-time.Hour)); got != 0 {
		t.Fatalf("已过期应返回 0，实际 %d", got)
	}
	if got := muteDeadline(time.Time{}); got != 0 {
		t.Fatalf("零值应返回 0，实际 %d", got)
	}
	if got := muteDeadline(time.Now().Add(10 * time.Minute)); got <= 0 {
		t.Fatalf("未过期应返回正的 Unix 秒，实际 %d", got)
	}
}
