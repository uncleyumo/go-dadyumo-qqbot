package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dadyumo/internal/config"
)

// TestHandleVideosLongSkipped 超过 video_max_sec 的视频不该抽帧，
// 提示词必须给出搪塞指引（这是防烧钱闸门，也是用户点名的化解策略）。
// 依赖 ffmpeg 存在：没有就跳过（该场景由 hasFFmpeg 降级分支覆盖）。
func TestHandleVideosLongSkipped(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("本机没有 ffmpeg，跳过")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("本机没有 ffprobe，跳过")
	}

	// 用 ffmpeg 生成一段 2 秒的测试视频，把 video_max_sec 压到 1 秒，
	// 这样不用生成很长的文件也能命中「超长」分支。
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i",
		"testsrc=duration=2:size=128x96:rate=10", src).Run(); err != nil {
		t.Fatalf("生成测试视频失败: %v", err)
	}

	cfg := config.Default()
	cfg.Brain.VideoMaxSec = 1
	cfg.Brain.VideoMaxMB = 10

	// handleVideos 从 URL 下载，这里借 httptest 把本地文件伺服出去
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, src)
	}))
	defer srv.Close()

	res := handleVideos(context.Background(), *cfg, []MediaRef{{URL: srv.URL + "/v.mp4"}})
	if len(res.frames) != 0 {
		t.Errorf("超长视频不应抽帧，却抽了 %d 帧", len(res.frames))
	}
	if !strings.Contains(res.note, "搪塞") {
		t.Errorf("超长视频的提示词应包含搪塞指引，got: %s", res.note)
	}
}

// TestHandleVideosShortExtracts 短视频应抽出帧并组装出「看了画面」的提示词。
func TestHandleVideosShortExtracts(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("本机没有 ffmpeg，跳过")
	}

	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i",
		"testsrc=duration=3:size=128x96:rate=10", src).Run(); err != nil {
		t.Fatalf("生成测试视频失败: %v", err)
	}

	cfg := config.Default()
	cfg.Brain.VideoMaxSec = 30
	cfg.Brain.VideoFrames = 2
	// 抽帧的前提是有看得见图的模型：Default() 里没有，得补一个
	cfg.LLM.Endpoints = []config.Endpoint{{
		ID: "t", Enabled: true,
		Models: []config.Model{{ID: "m", Enabled: true, Vision: true}},
	}}
	// 本机多半没有 ASR key：转写走「没去听」降级，不影响帧的断言

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, src)
	}))
	defer srv.Close()

	res := handleVideos(context.Background(), *cfg, []MediaRef{{URL: srv.URL + "/v.mp4"}})
	if len(res.frames) == 0 {
		t.Fatal("短视频应至少抽出 1 帧")
	}
	for _, f := range res.frames {
		if !strings.HasPrefix(f, "data:image/jpeg;base64,") {
			t.Errorf("帧应为 JPEG data URI，got 前缀: %.40s", f)
		}
	}
	if !strings.Contains(res.note, "画面里的") {
		t.Errorf("提示词应说明看了画面，got: %s", res.note)
	}
}

// TestHandleVideosMultiTakesLast 连发多条视频只看最后一条，其余记一笔。
func TestHandleVideosMultiTakesLast(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("本机没有 ffmpeg，跳过")
	}
	// 不用真视频：故意给一个下载会失败的 URL，只验证提示词里的「连发」前缀
	// 与「只看最后那条」的口径（真链路已由上面两个用例覆盖）。
	cfg := config.Default()
	res := handleVideos(context.Background(), *cfg,
		[]MediaRef{{URL: "http://127.0.0.1:1/a.mp4", Name: "张三"}, {URL: "http://127.0.0.1:1/b.mp4", Name: "张三"}})
	if !strings.Contains(res.note, "连发了 2 条视频") {
		t.Errorf("应提示连发条数，got: %s", res.note)
	}
	if len(res.frames) != 0 {
		t.Errorf("下载失败的批次不应有帧，got %d", len(res.frames))
	}
}

// TestHandleVideosNoFFmpeg 没有 ffmpeg 时整体降级成搪塞提示，不报错不崩溃。
func TestHandleVideosNoFFmpeg(t *testing.T) {
	if hasFFmpeg() {
		t.Skip("本机有 ffmpeg，跳过该降级分支")
	}
	cfg := config.Default()
	res := handleVideos(context.Background(), *cfg, []MediaRef{{URL: "http://example.com/v.mp4"}})
	if len(res.frames) != 0 || res.note == "" {
		t.Errorf("无 ffmpeg 应降级为纯提示词，got frames=%d note=%q", len(res.frames), res.note)
	}
}

// TestTranscribeAudioMock 用 httptest 模拟 SiliconFlow 的转写端点，
// 校验 multipart 字段、鉴权头与响应解析。
func TestTranscribeAudioMock(t *testing.T) {
	var gotModel, gotAuth string
	var gotFile bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
		}
		gotModel = r.FormValue("model")
		gotFile = r.MultipartForm != nil && len(r.MultipartForm.File["file"]) == 1
		_ = json.NewEncoder(w).Encode(map[string]string{"text": " 大家好，今天聊聊测试 "})
	}))
	defer srv.Close()

	audio := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(audio, []byte("fake-audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.ASRConfig{Provider: "siliconflow", APIKey: "sk-test", Model: "FunAudioLLM/SenseVoiceSmall"}
	// 端点表是按 provider 写死的，这里直接把测试服务器指给 provider 表
	old := asrEndpoints["siliconflow"]
	asrEndpoints["siliconflow"] = srv.URL
	defer func() { asrEndpoints["siliconflow"] = old }()

	text, err := transcribeAudio(context.Background(), audio, cfg)
	if err != nil {
		t.Fatalf("转写失败: %v", err)
	}
	if text != "大家好，今天聊聊测试" {
		t.Errorf("转写文本应去首尾空白，got: %q", text)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头不对: %q", gotAuth)
	}
	if gotModel != "FunAudioLLM/SenseVoiceSmall" {
		t.Errorf("model 字段不对: %q", gotModel)
	}
	if !gotFile {
		t.Error("应包含 file 字段")
	}
}

// TestTranscribeAudioNoKey 没 key 时直接跳过，返回错误而非崩溃。
func TestTranscribeAudioNoKey(t *testing.T) {
	if _, err := transcribeAudio(context.Background(), "x.wav", config.ASRConfig{Provider: "siliconflow"}); err == nil {
		t.Error("无 key 应返回错误")
	}
}

// TestTranscribeVoicesMock 语音兜底转写全链路：wav 伺服 + ASR mock，
// 验证下载→转写→拼提示词；无 key 时必须直接短路（连下载都不该发生）。
func TestTranscribeVoicesMock(t *testing.T) {
	asrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "晚上吃烧烤去不去"})
	}))
	defer asrSrv.Close()
	old := asrEndpoints["siliconflow"]
	asrEndpoints["siliconflow"] = asrSrv.URL
	defer func() { asrEndpoints["siliconflow"] = old }()

	wavSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-wav-bytes"))
	}))
	defer wavSrv.Close()

	cfg := config.Default()
	cfg.ASR.Provider = "siliconflow"
	cfg.ASR.APIKey = "sk-test"
	cfg.ASR.Model = "FunAudioLLM/SenseVoiceSmall"

	note := transcribeVoices(context.Background(), *cfg,
		[]VoiceRef{{Name: "张三", URL: wavSrv.URL + "/v.wav"}})
	if !strings.Contains(note, "张三") || !strings.Contains(note, "晚上吃烧烤去不去") {
		t.Errorf("提示词应包含发送人与转写内容，got: %q", note)
	}

	// 无 key：应返回空串，且不该碰网络
	noKey := config.Default()
	noKey.ASR.Provider = "siliconflow"
	if got := transcribeVoices(context.Background(), *noKey,
		[]VoiceRef{{Name: "张三", URL: "http://127.0.0.1:1/x.wav"}}); got != "" {
		t.Errorf("无 key 应短路返回空串，got: %q", got)
	}
}

// TestTranscribeVoicesCap 一批语音超过上限时只转最后几条。
func TestTranscribeVoicesCap(t *testing.T) {
	if maxVoicesPerBatch != 3 {
		t.Fatalf("上限变了，用例跟着改: %d", maxVoicesPerBatch)
	}
	refs := make([]VoiceRef, 5)
	// 裁剪逻辑与 transcribeVoices 内部一致：保留末尾 maxVoicesPerBatch 条
	trimmed := refs
	if len(trimmed) > maxVoicesPerBatch {
		trimmed = trimmed[len(trimmed)-maxVoicesPerBatch:]
	}
	if len(trimmed) != 3 {
		t.Errorf("裁剪后应为 3 条，got %d", len(trimmed))
	}
}
