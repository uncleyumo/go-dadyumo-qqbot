package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dadyumo/internal/config"
)

func postPause(t *testing.T, h http.Handler, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/pause", strings.NewReader(string(b)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPauseTogglePersists 总开关要落盘。
//
// 「跨重启保持」是用户明确选的语义：关掉之后重新部署二进制、systemctl restart、
// 机器重启，它都不许自己开口。所以只改内存（st.Get() 变了）不算通过——
// 必须能从磁盘重新读出来。
func TestPauseTogglePersists(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	if rec := postPause(t, h, cookie, map[string]any{"on": true}); rec.Code != http.StatusOK {
		t.Fatalf("关闭失败: %d %s", rec.Code, rec.Body.String())
	}
	if !st.Get().Paused {
		t.Fatal("关闭后 store 里没有生效")
	}
	onDisk, err := config.Load(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Get().Paused {
		t.Fatal("关闭没落盘——一次重启就会自己开机，这正是用户最担心的场景")
	}

	if rec := postPause(t, h, cookie, map[string]any{"on": false}); rec.Code != http.StatusOK {
		t.Fatalf("启动失败: %d %s", rec.Code, rec.Body.String())
	}
	if st.Get().Paused {
		t.Fatal("启动后仍然是关闭")
	}
	onDisk, err = config.Load(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Get().Paused {
		t.Fatal("启动没落盘")
	}
}

// TestPauseRequiresExplicitOn on 字段缺失必须报错，不能被当成 false。
//
// 写成 *bool 就是为了这个：没有它，`{}` 会解码成 on=false，
// 于是一次手滑的空请求就能把机器人**悄悄开机**。
func TestPauseRequiresExplicitOn(t *testing.T) {
	srv, st := newTestServer(t, func(c *config.Config) { c.Paused = true })
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	rec := postPause(t, h, cookie, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 on 字段应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !st.Get().Paused {
		t.Fatal("缺字段的请求把机器人开机了")
	}
}

// TestSaveConfigCannotUnpause 从配置页保存整份配置，不许把总开关带走。
//
// 这是本次最容易埋雷的地方，且失败时**没有任何报错**：
// 前端 CFG 是页面加载时的快照，用户关停之后回配置页随手点一次「保存并生效」，
// 那份旧快照里的 paused:false 就会覆盖服务端的 true——
// 现场表现是「我明明关了它怎么又说话了」，翻日志只能看到一条正常的
// 「管理端已更新配置并重载调用器」。
func TestSaveConfigCannotUnpause(t *testing.T) {
	srv, st := newTestServer(t, func(c *config.Config) { c.Paused = true })
	h := srv.Handler("/admin")
	cookie := authedCookie(t, srv)

	// 模拟页面里的旧快照：paused 是 false
	cfg := st.Get()
	cfg.Paused = false
	payload, _ := json.Marshal(map[string]any{"rev": st.Rev(), "config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(payload)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}

	if !st.Get().Paused {
		t.Fatal("保存配置把总开关改回了 false——机器人会在用户以为它关着的时候开口")
	}
	onDisk, err := config.Load(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Get().Paused {
		t.Fatal("落盘的总开关被配置保存改掉了")
	}
}
