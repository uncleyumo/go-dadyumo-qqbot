package memepool

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// parseScores 解析优选模型返回的评分。
//
// 格式是 {"1":0.8,"2":0.3,...}。真实模型很爱在 JSON 外面包东西
// （"好的，评分如下：{...}"）、或者用单引号、或者干脆包在 ``` 里，
// 所以这里比决策解析那边宽松得多——解析不出来就整轮放弃，
// 而不是硬凑一个错分数把好图误杀了。
func parseScores(content string, memes []Meme) (map[int64]float64, error) {
	body := extractJSONObject(content)
	if body == "" {
		return nil, fmt.Errorf("优选模型没给出可解析的 JSON: %.80s", content)
	}
	var raw map[string]float64
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("评分 JSON 解析失败: %v", err)
	}
	out := make(map[int64]float64, len(raw))
	for k, v := range raw {
		idx, err := strconv.Atoi(strings.TrimSpace(k))
		if err != nil || idx < 1 || idx > len(memes) {
			continue // 越界的行号直接扔，不让它错位到别的图上
		}
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		out[memes[idx-1].ID] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("评分里没有可用的条目")
	}
	return out, nil
}

// extractJSONObject 从一段自由文本里抠出最外层的 {...}。
//
// 用括号配对而不是正则贪心，因为描述里可能也带大括号。
//
// 关键：从**每一个** '{' 都试一次，而且**必须能解析**才收。
// 模型常写「前面有 {不是 json} 后面 {"1":0.5}」这种话。
// `{不是 json}` 大括号是配平的，但不是合法 JSON——只按配平判会抠到它，
// 然后整轮优选白丢。逐个试、谁能真正解析用谁。
func extractJSONObject(s string) string {
	for start := 0; start < len(s); start++ {
		if s[start] != '{' {
			continue
		}
		obj := matchBraces(s, start)
		if obj == "" {
			continue
		}
		var probe map[string]float64
		if err := json.Unmarshal([]byte(obj), &probe); err == nil {
			return obj
		}
	}
	return ""
}

// matchBraces 从 start 处尝试配平大括号，返回配平的那一段；配不平返回空串。
func matchBraces(s string, start int) string {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
			// depth 变负说明这个起点本来就在一段的中间，
			// 继续往下找只会越走越歪，直接放弃这个起点
			if depth < 0 {
				return ""
			}
		}
	}
	return ""
}
