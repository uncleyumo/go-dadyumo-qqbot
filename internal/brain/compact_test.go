package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dadyumo/internal/config"
)

func testCompactCfg(url string) config.CompactConfig {
	return config.CompactConfig{
		BaseURL:        url,
		APIKey:         "sk-test",
		Model:          "qwen3.5-flash",
		ThresholdChars: 300,
		TargetChars:    150,
		MaxOutTokens:   300,
	}
}

// TestCompactShortPassesThrough 短于阈值的转写原样返回，一次 HTTP 都不该发。
func TestCompactShortPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("短文本不该发起压缩请求")
	}))
	defer srv.Close()

	short := "晚上吃烧烤去不去"
	if got := compactTranscript(context.Background(), testCompactCfg(srv.URL), short); got != short {
		t.Errorf("短文本应原样返回，got %q", got)
	}
	// 没配 key：再长也不该发请求
	long := strings.Repeat("啊", 1000)
	bare := testCompactCfg(srv.URL)
	bare.APIKey = ""
	if got := compactTranscript(context.Background(), bare, long); got != long {
		t.Errorf("无 key 应原样返回，got 长度 %d", len([]rune(got)))
	}
}

// TestCompactLongCompressed 超长转写走压缩模型，请求体里要带对模型名。
func TestCompactLongCompressed(t *testing.T) {
	var gotModel string
	var gotMaxTokens float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string  `json:"model"`
			MaxTokens float64 `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel, gotMaxTokens = req.Model, req.MaxTokens
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "张三约大家明晚八点老地方吃烧烤，李四说加班可能迟到"}},
			},
		})
	}))
	defer srv.Close()

	long := strings.Repeat("那个就是说", 100) // 500 字 > 阈值 300
	got := compactTranscript(context.Background(), testCompactCfg(srv.URL), long)
	if !strings.Contains(got, "烧烤") {
		t.Errorf("应返回压缩结果，got %q", got)
	}
	if gotModel != "qwen3.5-flash" {
		t.Errorf("模型名不对: %q", gotModel)
	}
	if gotMaxTokens != 300 {
		t.Errorf("max_tokens 不对: %v", gotMaxTokens)
	}
}

// TestCompactFailureFallsBackToTruncate 压缩接口挂了就硬截断，不能让主流程失败。
func TestCompactFailureFallsBackToTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	long := strings.Repeat("字", 500)
	got := compactTranscript(context.Background(), testCompactCfg(srv.URL), long)
	// truncate 会追加省略号，阈值 300 → 截断后是 301 字
	if len([]rune(got)) != 301 {
		t.Errorf("失败应硬截断到阈值 300 字，got %d 字", len([]rune(got)))
	}

	// 响应为空 content 也要兜住
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": ""}},
			},
		})
	}))
	defer emptySrv.Close()
	got = compactTranscript(context.Background(), testCompactCfg(emptySrv.URL), long)
	if len([]rune(got)) != 301 {
		t.Errorf("空响应应硬截断到阈值 300 字，got %d 字", len([]rune(got)))
	}
}
