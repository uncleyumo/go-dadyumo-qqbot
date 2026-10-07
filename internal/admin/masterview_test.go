package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dadyumo/internal/config"
)

// TestStateCarriesBindEnabled /api/state 必须下发「允许口令绑定」开关。
//
// 前端 renderMaster 用 `!!m.bind_enabled` 回填复选框，字段缺席时读到的是
// undefined → 勾永远被抹成未勾。现场表现是「在群里勾了保存没用、勾不上」，
// 而保存其实**成功了**：config.json 是对的、`#认主` 也是通的，
// 只有界面在说谎——所以用户会把火撒在保存按钮上，查半天查不出所以然。
// 更坏的是再保存一次会把 unchecked 回传，等于悄悄关掉刚开的开关。
//
// 断言用 *bool 而不是 bool：这一条要抓的正是「字段根本没下发」，
// 解成零值 false 就看不出缺席与 false 的区别，测试会假绿。
func TestStateCarriesBindEnabled(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) {
		c.Master.BindEnabled = true
	})
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 返回 %d", rec.Code)
	}

	var out struct {
		Master struct {
			BindEnabled *bool  `json:"bind_enabled"`
			DevEnabled  *bool  `json:"dev_enabled"`
			Nickname    string `json:"nickname"`
		} `json:"master"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Master.BindEnabled == nil {
		t.Fatal("/api/state 的 master 里没有 bind_enabled——控制台那个勾选框会永远勾不上")
	}
	if !*out.Master.BindEnabled {
		t.Fatal("配置里 bind_enabled=true，下发给前端的却是 false")
	}
	// 同一个视图里的另一个开关作为对照：它一直都在，所以从没人报过它的 bug
	if out.Master.DevEnabled == nil {
		t.Fatal("dev_enabled 也丢了——整个 masterView 都没下发？")
	}
}
