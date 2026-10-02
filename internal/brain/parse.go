package brain

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"unicode"
)

// ParseIssue 描述这次解析走了哪条路径，用于观察模型的格式服从度
type ParseIssue string

const (
	ParseOK        ParseIssue = "ok"        // 完全符合协议
	ParseRecovered ParseIssue = "recovered" // JSON 不规范但修好了
	ParseFallback  ParseIssue = "fallback"  // 拿不到 JSON，降级当普通发言
	ParseNoise     ParseIssue = "noise"     // 剥完标签剩下的还是协议残渣，闭嘴
)

// ParseDecision 解析模型输出。
// 免费小模型的格式服从度很差，这里必须足够脏：标签缺失、markdown 围栏、
// 前面带废话、JSON 被截断、输出了全角引号等情况都要能捞回来。
func ParseDecision(raw string) (*Decision, ParseIssue) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return &Decision{Act: "quiet"}, ParseFallback
	}

	os := extractTag(text, "os")
	body, issue := extractJSON(text)

	if body != "" {
		// 逐级放宽地重试：原样 → 修尾随逗号/裸换行 → 修全角引号 → 两者都修。
		// 原样那次沿用 extractJSON 判定的 issue，之后几次一律算 recovered。
		fixes := []func(string) string{
			nil,
			repair,
			normalizeQuotes,
			func(s string) string { return normalizeQuotes(repair(s)) },
		}
		for i, fix := range fixes {
			cand := body
			if fix != nil {
				cand = fix(cand)
			}
			var d Decision
			if err := json.Unmarshal([]byte(cand), &d); err != nil {
				continue
			}
			d.OS = os
			normalize(&d)
			if i == 0 {
				return &d, issue
			}
			return &d, ParseRecovered
		}
	}

	// 拿不到合法 JSON：把原文当成它想说的话，但必须先剥掉标签和围栏
	plain := strings.TrimSpace(stripTags(text))
	if plain == "" {
		return &Decision{Act: "quiet"}, ParseFallback
	}
	// 剥干净之后还得像句人话。免费小模型的畸形输出剥完标签剩下的常常
	// 还是协议碎片（<json>{"text": 、 {"act":"say"} 这种），
	// 当发言发出去就是刷屏，宁可闭嘴——engine.go 只在 fallback+say 时告警，
	// 这里返回 quiet 并带上 ParseNoise，debug 日志里能看出是被这道闸拦下的。
	if !looksLikeSpeech(plain) {
		return &Decision{Act: "quiet", OS: os}, ParseNoise
	}
	return &Decision{
		Act:  "say",
		Text: Sanitize(plain, int(maxChars.Load())),
		Tone: "roast",
		OS:   os,
	}, ParseFallback
}

// maxChars 单条发言的字数上限，Persona.MaxChars 的运行时副本。
// 引擎在配置热重载时调 SetMaxChars 写进来，0 表示不限长。
var maxChars atomic.Int64

// SetMaxChars 设置单条发言上限，供引擎在配置变更时调用
func SetMaxChars(n int) { maxChars.Store(int64(n)) }

// looksLikeSpeech 判断剥完标签的残渣像不像一句能发出去的话。
// 判据都是协议层面的：长度不足、纯标点、或还带着 JSON/XML 的结构字符
// （{}"<>)，都说明剥得不干净，剩下的不是人话。
func looksLikeSpeech(s string) bool {
	r := []rune(s)
	if len(r) < 2 {
		return false
	}
	wordy := false
	for _, c := range r {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			wordy = true
			continue
		}
		// 协议结构字符没剥干净
		if strings.ContainsRune(`{}"<>\`, c) {
			return false
		}
	}
	// 全是标点和空白，等于没说话
	return wordy
}

// normalize 规范化字段：补默认值、限制长度
func normalize(d *Decision) {
	d.Act = strings.ToLower(strings.TrimSpace(d.Act))
	if d.Act != "say" && d.Act != "quiet" {
		// 有些模型会写 speak / reply / 说 等
		if strings.TrimSpace(d.Text) != "" || len(d.Blocks) > 0 {
			d.Act = "say"
		} else {
			d.Act = "quiet"
		}
	}
	d.Text = strings.TrimSpace(d.Text)
	d.Tone = strings.ToLower(strings.TrimSpace(d.Tone))
	switch d.Tone {
	case "roast", "warm", "empathy":
	default:
		d.Tone = "roast"
	}
	// 出口统一过一道 Sanitize：剥掉模型没吐干净的标签与控制字符，
	// 并让 persona.max_chars 真正生效
	d.Text = Sanitize(d.Text, int(maxChars.Load()))
	// blocks 里的文字也要过同一道出口清理——
	// 模型照样可能在 c 里塞 <os> 或控制字符。
	for i := range d.Blocks {
		if d.Blocks[i].T == BlockTypeText {
			d.Blocks[i].C = Sanitize(d.Blocks[i].C, int(maxChars.Load()))
		}
		d.Blocks[i].T = strings.ToLower(strings.TrimSpace(d.Blocks[i].T))
	}
	if d.Act == "quiet" {
		d.Text = ""
		d.Blocks = nil
	}
	// collect 与发不发言无关：闭嘴那轮照样可以收图。
	// 但序号必须是正数、描述必须非空，否则收进来的是一张没法挑的图。
	valid := d.Collect[:0]
	for _, c := range d.Collect {
		c.D = Sanitize(strings.TrimSpace(c.D), 40)
		if c.I > 0 && c.D != "" {
			valid = append(valid, c)
		}
	}
	d.Collect = valid
}

// extractTag 抽取 <tag>...</tag> 内容
func extractTag(s, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:j])
}

// stripTags 去掉 <os>/<json> 标签块、markdown 围栏与多余空行
func stripTags(s string) string {
	out := s
	// 剥标签的迭代预算。修好「闭标签在开标签之前」之后，每一轮都在让字符串变短，
	// 正常输入几轮就结束了；这个上限是给畸形输入兜底的死保险，
	// 免得哪天真出现不收敛的输入，把内存吃光。
	// 每轮至少删掉一对标签（最短的 <os></os> 有 7 字节），所以按长度给预算就够了。
	budget := len(s)/7 + 8
	for _, tag := range []string{"os", "json", "think", "thinking"} {
		open := "<" + tag + ">"
		close := "</" + tag + ">"
		for budget > 0 {
			i := strings.Index(out, open)
			if i < 0 {
				break
			}
			// 闭标签必须在开标签之后。
			// 这里曾经是从串首独立搜 j 的，闭标签出现在开标签之前时
			// （免费小模型很常见的 </json> 哦 <json>{...}），
			// out[:i] + out[j+len(close):] 会把中间那段原样复制回来，字符串净增长，
			// 下一轮又找到同样的 i/j——38 字节的输入能涨到 800MB 再也退不出来。
			rest := out[i+len(open):]
			j := strings.Index(rest, close)
			if j < 0 {
				// 没闭合：开标签往后都是残渣，直接截断
				out = out[:i]
				break
			}
			out = out[:i] + rest[j+len(close):]
			budget--
		}
		if budget <= 0 {
			break
		}
	}
	out = strings.ReplaceAll(out, "```json", "")
	out = strings.ReplaceAll(out, "```", "")
	lines := strings.Split(out, "\n")
	var keep []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if t = cutStructure(t); t == "" {
			continue
		}
		keep = append(keep, t)
	}
	return strings.Join(keep, "\n")
}

// cutStructure 剥掉一行开头的 JSON 结构碎片，只留下后面的人话。
// 原来整行只要以 { 或 } 开头就丢掉，可模型很爱写成
// {"act":"say"} 顺便说句真心话——整行丢掉等于把唯一能发的一句也扔了。
// 切完剩下的还是结构（切不动）就返回空串，让调用方丢掉整行。
func cutStructure(t string) string {
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "}") {
		return t
	}
	var rest string
	if b, ok := braceMatch(t); ok {
		// 配对上了：碎片就是它，后面的都是人话
		rest = t[len(b):]
	} else {
		// 碎片本身是残的（被截断的 JSON）：只能砍掉行首连续的括号与标点，
		// 砍完剩下的要是 {"a": 这种光秃秃的键名，就当它还是结构。
		// 真正的中文发言不会没有汉字，也不会一个空格都没有。
		rest = strings.TrimSpace(strings.TrimLeft(t, "{}[](),:\" \t"))
		if !hasWord(rest) || (!hasHan(rest) && !strings.ContainsAny(rest, " \t")) {
			return ""
		}
	}
	rest = strings.TrimSpace(rest)
	if !hasWord(rest) {
		return ""
	}
	return rest
}

// hasHan 判断有没有汉字
func hasHan(s string) bool {
	for _, c := range s {
		if unicode.Is(unicode.Han, c) {
			return true
		}
	}
	return false
}

// hasWord 判断有没有真正的词（字母或数字），纯标点不算
func hasWord(s string) bool {
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			return true
		}
	}
	return false
}

// extractJSON 从文本中捞出一个 JSON 对象。
// 优先 <json> 标签，其次做括号配对；被截断时尽力补成闭合。
func extractJSON(s string) (string, ParseIssue) {
	if body := extractTag(s, "json"); body != "" {
		body = cleanFence(body)
		if b, ok := braceMatch(body); ok {
			return b, ParseOK
		}
		if b, ok := braceMatch(body + "}"); ok {
			return b, ParseRecovered
		}
		return body, ParseRecovered
	}

	// 没有标签：从第一个 { 开始配对
	start := strings.Index(s, "{")
	if start < 0 {
		return "", ParseFallback
	}
	candidate := cleanFence(s[start:])
	if b, ok := braceMatch(candidate); ok {
		return b, ParseRecovered
	}
	// 大概率是被截断了，补一个 }
	if b, ok := braceMatch(candidate + "}"); ok {
		return b, ParseRecovered
	}
	return "", ParseFallback
}

// cleanFence 去掉 markdown 代码围栏
//
// 这里刻意不碰引号。模型爱写 {"text":"他说“没事”"}，而全角引号在 JSON 里
// 本来就是合法的普通字符——无条件替换成 ASCII " 会把字符串提前闭合，
// 整条决策解析失败后被静默丢掉（引擎只在 fallback+say 时告警，压根不响）。
// 引号留给 normalizeQuotes 在解析失败之后做。
func cleanFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// normalizeQuotes 把 JSON 结构位置上的全角引号换成 ASCII 双引号。
// 只动字符串外面的：字符串内部的引号是内容的一部分，换掉就是改句子。
// 靠 ASCII 双引号判断在不在字符串里，所以 {"text":"他说“没事”"} 这种
// 已经合法的原文扫不出变化，{“act”:“say”} 这种整体失守的才被修好。
func normalizeQuotes(s string) string {
	if !strings.ContainsAny(s, "“”‘’") {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	inStr := false
	// 按 rune 走：全角引号是三字节的，逐字节替换会把后面的续字节当普通字符拷出去
	for _, c := range s {
		if inStr {
			sb.WriteRune(c)
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			sb.WriteRune(c)
		case '“', '”', '‘', '’':
			sb.WriteByte('"')
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

// braceMatch 从字符串起始处做花括号配对，返回第一个完整对象
func braceMatch(s string) (string, bool) {
	start := strings.Index(s, "{")
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
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
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// repair 修补常见的 JSON 坏味道：尾随逗号、换行未转义
func repair(s string) string {
	s = strings.TrimSpace(s)
	// 去掉对象/数组尾部的逗号
	var sb strings.Builder
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			sb.WriteByte(c)
			continue
		}
		if c == '"' {
			inStr = true
			sb.WriteByte(c)
			continue
		}
		if c == ',' {
			// 向后看，若下一个非空字符是 } 或 ] 则丢弃这个逗号
			j := i + 1
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		// 字符串外的裸换行必须转义
		if c == '\n' || c == '\r' {
			sb.WriteString("\\n")
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// Sanitize 发送前的最后一道清理：去掉残留标签、控制字符与过长内容
func Sanitize(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	s = stripTags(s)
	// 去掉控制字符，保留换行
	var sb strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || unicode.IsPrint(r) {
			sb.WriteRune(r)
		}
	}
	s = strings.TrimSpace(sb.String())
	if maxChars > 0 {
		r := []rune(s)
		if len(r) > maxChars {
			s = string(r[:maxChars]) + "…"
		}
	}
	return s
}
