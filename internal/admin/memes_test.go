package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memepool"
)

// memStorageStub 测试用存储
type memStorageStub struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memStorageStub) Put(k string, d []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[k] = d
	return nil
}

func (m *memStorageStub) Get(k string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[k]
	if !ok {
		return nil, "", memepool.ErrNotFound
	}
	return d, "image/jpeg", nil
}

func (m *memStorageStub) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}

// postJSON 带会话发一个 POST
func postJSON(t *testing.T, h http.Handler, path string, body any, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newPoolWithMemes(t *testing.T, n int) (*memepool.Pool, *memStorageStub) {
	t.Helper()
	st := &memStorageStub{}
	pool := memepool.New(memepool.DefaultConfig(), st)
	for i := 0; i < n; i++ {
		if _, err := pool.Add([]byte("fake-jpeg-"+string(rune('a'+i))), "image/jpeg",
			"无语的猫"+string(rune('a'+i)), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return pool, st
}

// 未启用时列表接口要明确说「没启用」，而不是 500
func TestMemeListDisabled(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/memes", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("未启用也应返回 200，got %d", rec.Code)
	}
	var out struct {
		OK      bool `json:"ok"`
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled {
		t.Error("未挂池子时 enabled 应为 false")
	}
}

// 列表按好感度排序，且带预览地址与「还剩几天淘汰」
func TestMemeListRanked(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	pool, _ := newPoolWithMemes(t, 3)
	srv.SetMemePool(pool, nil)
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/memes", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out struct {
		Enabled bool `json:"enabled"`
		MaxPool int  `json:"max_pool"`
		Memes   []struct {
			ID        int64   `json:"id"`
			Descr     string  `json:"descr"`
			Rank      int     `json:"rank"`
			ExpiresIn int64   `json:"expires_in"`
			Quality   float64 `json:"quality"`
		} `json:"memes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || len(out.Memes) != 3 {
		t.Fatalf("应返回 3 条，got %+v", out)
	}
	if out.MaxPool != 20 {
		t.Errorf("上限应为 20，got %d", out.MaxPool)
	}
	for i, m := range out.Memes {
		if m.Rank != i+1 {
			t.Errorf("第 %d 条的 rank 应为 %d，got %d", i, i+1, m.Rank)
		}
		if m.ExpiresIn <= 0 {
			t.Errorf("应带剩余天数: %+v", m)
		}
	}
}

// 手动删除要同时删掉 MinIO 上的对象并落盘
func TestMemeDeleteRemovesObjectAndPersists(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	pool, st := newPoolWithMemes(t, 2)
	saved := 0
	srv.SetMemePool(pool, func() error { saved++; return nil })

	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	ids := pool.Browse(time.Now())
	victim := ids[0].ID

	rec := postJSON(t, h, "/admin/api/memes/delete", map[string]any{"id": victim}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败: %d %s", rec.Code, rec.Body.String())
	}
	if pool.Len() != 1 {
		t.Errorf("池子应剩 1 张，got %d", pool.Len())
	}
	st.mu.Lock()
	n := len(st.data)
	st.mu.Unlock()
	if n != 1 {
		t.Errorf("MinIO 上的对象也应一起删掉，got %d", n)
	}
	if saved != 1 {
		t.Errorf("删完应立刻落盘一次，got %d", saved)
	}
}

func TestMemeDeleteRejectsMissingID(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	pool, _ := newPoolWithMemes(t, 1)
	srv.SetMemePool(pool, nil)
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	rec := postJSON(t, h, "/admin/api/memes/delete", map[string]any{}, ck)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 id 应 400，got %d", rec.Code)
	}
	if pool.Len() != 1 {
		t.Error("校验失败不该删任何东西")
	}
}

// 保存人设页时前端不回传 MinIO 段（没有表单、含密钥），不能被整包覆盖抹掉
func TestSaveConfigKeepsMinIO(t *testing.T) {
	srv, store := newTestServer(t, func(c *config.Config) {
		c.MemePool.Enabled = true
		c.MemePool.MinIO = config.MinIOConfig{
			Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "qqbot",
			AccessKey: "minioadmin", SecretKey: "minioadmin-secret",
			StateFile: "memes.json",
		}
	})
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	// 模拟前端：只带六个标量，minio 整块不回传
	probe := store.Get()
	probe.MemePool.MinIO = config.MinIOConfig{}

	rec := postJSON(t, h, "/admin/api/config", map[string]any{
		"rev": store.Rev(), "config": probe,
	}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", rec.Code, rec.Body.String())
	}

	after := store.Get().MemePool
	if after.MinIO.AccessKey != "minioadmin" || after.MinIO.SecretKey != "minioadmin-secret" {
		t.Errorf("MinIO 密钥不该被前端回传抹掉: %+v", after.MinIO)
	}
	if after.MinIO.Endpoint != "http://127.0.0.1:9000" || after.MinIO.Bucket != "qqbot" {
		t.Errorf("MinIO 地址不该丢: %+v", after.MinIO)
	}
	if !after.Enabled {
		t.Error("前端回传的开关应当被采纳")
	}
}

// 预览图走同源代理：管理端是 https、MinIO 是 http，直链会被浏览器当混合内容拦掉。
// 2026-10-01 生产实况：右键打开链接是好的，页面里就是一片裂图。
func TestMemeImageProxiedSameOrigin(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	pool, _ := newPoolWithMemes(t, 1)
	srv.SetMemePool(pool, nil)
	h := srv.Handler("/admin")
	ck := authedCookie(t, srv)

	id := pool.Browse(time.Now())[0].ID
	req := httptest.NewRequest(http.MethodGet, "/admin/api/meme/img?id="+itoa(id), nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("取图失败: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type 应是 image/jpeg，got %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("图内容为空")
	}
	// 绝不能把 MinIO 的地址泄给前端去直连
	if strings.Contains(rec.Body.String(), "minio.local") {
		t.Error("响应里不该出现存储直链")
	}
}

// 代理接口同样要鉴权 + 校验参数，不能变成裸奔的图片服务器
func TestMemeImageGuards(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	pool, _ := newPoolWithMemes(t, 1)
	srv.SetMemePool(pool, nil)
	h := srv.Handler("/admin")

	req := httptest.NewRequest(http.MethodGet, "/admin/api/meme/img?id=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未登录应 401，got %d", rec.Code)
	}

	ck := authedCookie(t, srv)
	for _, q := range []string{"", "?id=abc", "?id=0", "?id=999999"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/meme/img"+q, nil)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%q 应拒绝，got 200", q)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
