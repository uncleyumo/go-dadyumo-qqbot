package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

// compactTimeout 转写压缩的单次超时。
// 压缩调用就几百 token，10 秒足够；超时降级为硬截断，不拖主流程。
const compactTimeout = 10 * time.Second

// compactTranscript 压缩超长语音/视频转写。
//
// 两分钟的内容能转出上千字，原样塞进提示词既烧 token 又稀释重点，
// 所以超过 ThresholdChars 就交给一个极致便宜的纯文本小模型压成 TargetChars
// 字左右的要点。这个模型在独立的 compact 配置段里，**不进中央调度器**。
// 任何失败都降级为硬截断：压缩只是锦上添花，不能拦主流程。
// 短于阈值、没配 key 时原样返回，一次 HTTP 都不发。
func compactTranscript(ctx context.Context, cfg config.CompactConfig, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if cfg.APIKey == "" || cfg.BaseURL == "" || len([]rune(text)) <= cfg.ThresholdChars {
		return text
	}
	out, err := callCompact(ctx, cfg, text)
	if err != nil {
		logx.Debug("转写压缩失败，硬截断兜底", "err", err.Error())
		return truncate(text, cfg.ThresholdChars)
	}
	logx.Debug("转写已压缩", "原字数", len([]rune(text)), "压缩后", len([]rune(out)))
	return out
}

// callCompact 调一次 OpenAI 风格的 chat completions 做压缩。
// qwen3.5-flash 这类模型可能带思维链，只取最终 content，reasoning 不当输出。
func callCompact(ctx context.Context, cfg config.CompactConfig, text string) (string, error) {
	sys := fmt.Sprintf("把下面的语音转写原文压缩成不超过 %d 字的口语要点。"+
		"保留谁在说什么事、关键的名字/数字/时间/结论，丢掉语气词、重复和客套。"+
		"直接输出压缩后的内容，不要加任何前缀，不要评价。", cfg.TargetChars)
	reqBody, err := json.Marshal(map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": text},
		},
		"max_tokens":  cfg.MaxOutTokens,
		"temperature": 0.3,
	})
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	c, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	rsp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("压缩请求失败: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(rsp.Body, 1<<20))
	if rsp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("压缩 HTTP %d: %s", rsp.StatusCode, truncate(string(body), 120))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("压缩响应解析失败: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("压缩响应无 choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("压缩响应为空")
	}
	// 模型不守规矩说太多时硬截断，给目标字数两倍余量
	return truncate(content, cfg.TargetChars*2), nil
}
