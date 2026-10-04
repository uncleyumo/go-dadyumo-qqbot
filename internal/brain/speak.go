package brain

import (
	"regexp"
	"strings"
)

// mentionPrefix 匹配消息开头的「@某人」前缀（昵称取到空白或常见标点为止）
var mentionPrefix = regexp.MustCompile(`^@[^ \t，,。.？?!！~～]{1,32}[ 　]*`)

// atAllInOutput 匹配模型正文里冒充「@全体成员」的写法。
//
// 这是 2026-10-01 群里真实发生的事故：有人说「把该打游戏的人艾特出来」，
// 模型没有说自己做不到，而是学了聊天记录里的 @全体成员 字样，回了
// 「你自己@all不就完了」——把一个系统标记当成聊天话术说了出去。
//
// 提示词已经明确告诉它「你没有 @ 任何人的能力」，但提示词是软约束：
// 便宜模型、上下文一长、或者被人直接下指令，都会让它不照做。
// 所以出口再兜一道：这种 token 一定是模型 malfunction，不是真人在说话。
//
// (?i) 不是多余的：模型很爱写 @ALL、@All。这个机器人的人格就是嘴臭爱抬杠，
// 让它别写它偏写，大小写都得堵。
var atAllInOutput = regexp.MustCompile(`(?i)@all|@全体成员|@所有人|@全体`)

// DedupeMentions 把「每一条都 @ 人」收敛成「最多第一条 @ 一次」，
// 并清掉冒充「@全体成员」的写法。
//
// 模型看多了历史里 @ 开头的消息会有样学样，给拆出来的每一条都加上
// 「@昵称 」前缀，观感极差、像在执行回复任务。这里做程序级兜底：
// 剥掉所有条目开头的 @ 前缀；若第一条原本就带，还原回第一条。
//
// 2026-10-05：那个「还原」现在是**真艾特**了——正文里的 `@昵称 ` 被
// allowBlocks 之外的这条路径剥掉后，第一条会由 planDelivery 重新拼成
// at 块吗？不：正文里的 @昵称 是模型自己写的字面串（不是 at 块），
// 这里仍然只做剥离。它与 at 块的区别在于——
//   - at 块：模型按协议写的 {"t":"at","name":...}，会被解析成 openid，真艾特。
//   - 正文里的 @昵称：模型学聊天记录学的字面串，平台上什么都不是。
//
// 所以正文里的字面 @ 继续剥。提示词已经明确教它用 at 块，
// 剩下的字面串只会是它没学明白时漏出来的。
//
// @全体成员 不走前缀那套：它经常出现在句子中间（「你自己@all不就完了」），
// 而且平台压根不支持这个动作（官方标注「仅在文字子频道可用」），
// 写出去只会是个骗人的假动作，直接删掉。
func DedupeMentions(segs []string) []string {
	if len(segs) == 0 {
		return segs
	}
	out := make([]string, len(segs))
	var firstTag string
	for i, s := range segs {
		if m := mentionPrefix.FindString(s); m != "" {
			if i == 0 {
				firstTag = strings.TrimSpace(m)
			}
			s = strings.TrimSpace(s[len(m):])
		}
		s = stripAtAll(s)
		out[i] = s
	}
	if firstTag != "" && out[0] != "" {
		out[0] = firstTag + " " + out[0]
	}
	return out
}

// dropEmpty 丢掉清理后剩下的空段。
//
// 出口兜底（剥 @ 前缀、删 @全体成员）会让某些段变成空串，
// 而空串发到 QQ 会变成一条空白消息——比不说话难看得多。
func dropEmpty(segs []string) []string {
	out := segs[:0]
	for _, s := range segs {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// stripAtAll 删掉正文里的 @全体成员 类写法，并收拾被删剩下的空壳。
//
// 删完可能留下「你自己就不完了」这种语义悬空的句子，但那已经比
// 发出一个假的 @全体成员 强：群里看到的至少是句人话，
// 而 @all 是机器人做不到的动作，演出来只会露馅。
func stripAtAll(s string) string {
	if !atAllInOutput.MatchString(s) {
		return s
	}
	s = atAllInOutput.ReplaceAllString(s, "")
	// 删完可能剩下悬空的助词，读着别扭就整条丢掉——
	// 宁可少发一条，也别发半句没头没尾的话。
	if t := strings.TrimSpace(s); t == "" {
		return ""
	}
	return strings.TrimSpace(s)
}

// 句末标点：优先在这些地方断句，断出来的是完整的一句
var strongPunct = []string{"。", "！", "？", "…", "!", "?", ".", "~", "～"}

// 次级标点：句子实在太长时才用它断开
var weakPunct = []string{"，", "、", "；", "：", ",", ";", ":", " "}

// SplitSegments 把模型给出的一段话拆成若干条，供逐条发送。
//
// 这是「活人感」的最后一环：模型按协议用换行表达「我要分几条发」，
// 这里负责把它变成真正的多条消息；模型没写换行但话太长的，按标点补拆。
func SplitSegments(text string, maxSegChars, maxSegments int) []string {
	if maxSegChars <= 0 {
		maxSegChars = 40
	}
	if maxSegments <= 0 {
		maxSegments = 5
	}

	// 模型偶尔会把换行写成字面量的 \n（没走 JSON 转义），这里一并认
	raw := strings.ReplaceAll(text, "\\n", "\n")

	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if runeLen(line) <= maxSegChars {
			out = append(out, line)
			continue
		}
		out = append(out, splitLong(line, maxSegChars)...)
	}

	if len(out) > maxSegments {
		out = out[:maxSegments]
	}
	return out
}

// splitLong 把一句过长的话按标点切成多条。
// 先按句末标点切，再把切出来的小句贪心合并到不超过上限。
func splitLong(s string, max int) []string {
	parts := splitKeepPunct(s, strongPunct)
	if len(parts) <= 1 {
		// 整句没有句末标点（比如一长串吐槽），退一步用次级标点
		parts = splitKeepPunct(s, weakPunct)
	}
	if len(parts) <= 1 {
		// 还是切不动（比如一长串无标点文字），只能硬切
		return hardSplit(s, max)
	}

	var out []string
	var cur strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cur.Len() > 0 && runeLen(cur.String())+runeLen(p) > max {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
		cur.WriteString(p)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, strings.TrimSpace(cur.String()))
	}

	// 合并后仍可能有个别超长（单句本身就超上限），再硬切一次
	var final []string
	for _, seg := range out {
		if runeLen(seg) > max*2 {
			final = append(final, hardSplit(seg, max)...)
			continue
		}
		final = append(final, seg)
	}
	return final
}

// splitKeepPunct 按给定标点点切分，标点保留在前一段末尾
func splitKeepPunct(s string, punct []string) []string {
	runes := []rune(s)
	var out []string
	var cur []rune
	for _, r := range runes {
		cur = append(cur, r)
		hit := false
		for _, p := range punct {
			if p == string(r) {
				hit = true
				break
			}
		}
		if hit {
			if seg := strings.TrimSpace(string(cur)); seg != "" {
				out = append(out, seg)
			}
			cur = cur[:0]
		}
	}
	if seg := strings.TrimSpace(string(cur)); seg != "" {
		out = append(out, seg)
	}
	return out
}

// hardSplit 无标点可依时的等长硬切
func hardSplit(s string, max int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 {
		n := max
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, strings.TrimSpace(string(runes[:n])))
		runes = runes[n:]
	}
	return out
}

func runeLen(s string) int { return len([]rune(s)) }
