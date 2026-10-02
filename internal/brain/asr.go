package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

// asrTimeout 单次转写请求的超时。
// SenseVoiceSmall 对一两分钟的音频通常也就几秒，40 秒非常宽裕；
// 超时直接放弃，不值得为一句背景音拖住整轮决策。
const asrTimeout = 40 * time.Second

// asrEndpoints 各 ASR 提供商的转写端点。都是 OpenAI 风格的 multipart 上传，
// 响应都是 {"text": "..."}，所以一个函数就能覆盖。
var asrEndpoints = map[string]string{
	"siliconflow": "https://api.siliconflow.cn/v1/audio/transcriptions",
}

// transcribeAudio 把音频文件交给配置的 ASR 服务转成文字。
//
// 失败一律返回空串 + error，调用方降级为「没听清里面说了啥」——
// ASR 挂了不值得让整轮决策失败。这里也刻意不重试：免费额度的接口，
// 重试只会把偶发故障放大成配额烧穿。
func transcribeAudio(ctx context.Context, audioPath string, cfg config.ASRConfig) (string, error) {
	endpoint, ok := asrEndpoints[strings.ToLower(strings.TrimSpace(cfg.Provider))]
	if !ok {
		return "", fmt.Errorf("未知的 ASR provider: %s", cfg.Provider)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", errors.New("未配置 ASR api_key，跳过语音转写")
	}

	f, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("打开音频失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	pr := multipart.NewWriter(&buf)
	fw, err := pr.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", fmt.Errorf("读取音频失败: %w", err)
	}
	if err := pr.WriteField("model", cfg.Model); err != nil {
		return "", err
	}
	if err := pr.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", pr.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: asrTimeout}
	rsp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ASR 请求失败: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(rsp.Body, 1<<20))
	if rsp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ASR HTTP %d: %s", rsp.StatusCode, truncate(string(body), 120))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("ASR 响应解析失败: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// VoiceRef 一条待兜底转写的语音消息：谁发的、平台给的音频下载地址。
type VoiceRef struct {
	// OpenID 是发语音的人的 openid。
	// 转写出来的字要能对上人，否则模型只会说「有人说了一句」，不知道是谁。
	OpenID string
	Name   string
	URL    string
}

// maxVoicesPerBatch 一个攒批窗口最多兜底转写几条语音。
// 转写只在决策真要发生时才做，但一个窗口里连发一串语音的场面并不罕见，
// 每条都是「下载 + 一次 ASR」，不给上限既拖时间又放大失败面。
const maxVoicesPerBatch = 3

// transcribeVoices 把攒批窗口里平台没给转写文本的语音兜底转成文字。
// 返回拼好的提示词片段（可为空串）；单条失败静默跳过——
// 一条语音没转出来不值得让整轮决策失败。
func transcribeVoices(ctx context.Context, cfg config.Config, refs []VoiceRef) string {
	if len(refs) == 0 || strings.TrimSpace(cfg.ASR.APIKey) == "" {
		return ""
	}
	if len(refs) > maxVoicesPerBatch {
		refs = refs[len(refs)-maxVoicesPerBatch:]
	}
	var sb strings.Builder
	for _, ref := range refs {
		text := transcribeVoiceOne(ctx, cfg, ref.URL)
		if text == "" {
			continue
		}
		// 超长转写先过一道便宜模型压缩（短于阈值时原样返回，不发请求）
		fmt.Fprintf(&sb, "%s发了条语音，说：「%s」。", ref.Name, compactTranscript(ctx, cfg.Compact, text))
	}
	return sb.String()
}

// transcribeVoiceOne 下载一条语音音频并转写，失败返回空串。
// 平台给的音频地址通常是 wav；就算不是，转写服务端自己解容器格式，
// 不用本地再过一道 ffmpeg。
func transcribeVoiceOne(ctx context.Context, cfg config.Config, url string) string {
	f, err := os.CreateTemp("", "dadyumo-voice-*")
	if err != nil {
		return ""
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()

	// 复用视频的下载器：两分钟的语音也就 4MB 上下，12MB 上限给足了余量
	if err := downloadVideo(ctx, url, path, 12); err != nil {
		logx.Debug("语音音频下载失败", "err", err.Error())
		return ""
	}
	text, err := transcribeAudio(ctx, path, cfg.ASR)
	if err != nil {
		logx.Debug("语音兜底转写失败", "err", err.Error())
		return ""
	}
	// 纯标点当作「没听清」处理，别拿一串省略号去骗模型
	if strings.Trim(text, "。，！？、,!? ") == "" {
		return ""
	}
	return text
}
