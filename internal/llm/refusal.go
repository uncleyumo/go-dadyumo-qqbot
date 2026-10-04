package llm

import (
	"fmt"
	"strings"
)

// 上游把「拒绝」塞在正文里。
//
// 生产实况（2026-10-04，Packy 中转）：
//
//	23:01:28 decision  未解析到合法 JSON，已降级为直接发言  raw="The prompt could not be submitted…"
//	23:01:45 speak      已发言  <同一句英文，一字不差>
//
// 形态是 **HTTP 200 + content 里一句英文说明**。它绕过了三层闸：
//   - 状态码是 200，readFull 的 parsed.Error 分支看不到东西
//   - content 非空，emptyContentErr 判不出来
//   - 拿到上层后 parse.go 的 looksLikeSpeech 放行（确实是字母、确实没有 {}"<>），
//     于是「拿不到 JSON 就把原文当发言」那条降级路径把它原样发进了群
//
// 后果不止是发错一句：agent.recordSent 把这句话写回记忆，
// 于是**每一轮后续对话的上下文里都带着这句含「sensitive words」的英文**，
// 下一轮再次触发拒绝，再次写回——自我复制，直到被 30 条消息挤出窗口。
// 这个循环 2026-10-04 在生产上真实发生过（memory.json 里 12 行有 8 行是这个）。
//
// 所以它必须在 client 层就变成错误，而不是留给上层去猜。

// refusalMarkers 是「这句话不是模型回答」的判据。
//
// 只收**上游系统口吻**的标记，不收「敏感词」这种中性字眼——
// 模型自己在正常聊天里提到某个词是合法的，把它们当拒绝判据会误杀正常发言。
// 每条都取自上游原话或各家已知的标准说法，不是猜的。
var refusalMarkers = []string{
	"could not be submitted", // 实测原话：The prompt could not be submitted.
	"prompt contains sensitive words",
	"prompt was blocked",
	"blocked by our content policy",
	"content policy violation",
	"prohibited usage",           // Google's [Generative AI Prohibited Usage] policy
	"prohibited_use",             // OpenRouter 的 block_reason 枚举
	"safety system",              //
	"content_filter",             // Azure 的 finish_reason
	"content was filtered", //
	"violates our usage policies", // OpenAI
	"responsible ai",             // Gemini 的 finishReason
	"candidate was blocked",     //
	"blocked by moderation",      //
	"输入内容不符合",                  // 国内中转的常见中文说法
	"内容涉及违规",                    //
	"敏感词",                        //
}

// looksLikeRefusal 判断正文是不是上游的拒绝说明而非模型回答。
//
// 额外加一道结构性兜底：**整段没有一个汉字**。
// 实测 450 条发言里只有 3 条不含汉字，且全部是 `6`、`？`、`...`
// 这类短促的真人回复——这个机器人的人设是中文闲聊，一条像样的回答不可能没有汉字。
// 单靠标记列表怕漏掉没见过的措辞，配上这道约束就稳得多：
// 漏判要付出「把审核提示发进群 + 写进记忆自我复制」的代价，宁可误杀长英文。
func looksLikeRefusal(content string) bool {
	s := strings.TrimSpace(content)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, m := range refusalMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	// 汉字之外还剩不少字母（排除数字/标点/空白），且一个汉字都没有 → 不像人话
	if hasCJK(s) {
		return false
	}
	// 带协议结构字符的**不是**拒绝说明，是模型自己在讲协议。
	//
	// 这条是被测试逼出来的：决策的合法输出是
	// `<json>{"act":"quiet","mem":[]}\n</json>`——纯 ASCII、三十多个字母，
	// 长得极像一句英文系统提示，但它一个字汉字都没有是**正常的**，
	// 而把它判成拒绝会让每一次「模型选择闭嘴」都变成一次调用失败。
	//
	// 顺序上放在标记匹配之后：万一哪天真有上游把拒绝说明包在 JSON 里，
	// 标记那条路仍然能抓住它。
	if strings.ContainsAny(s, "{}<>") {
		return false
	}
	return countLetters(s) >= 12
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func countLetters(s string) int {
	n := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			n++
		}
	}
	return n
}

// refusalErr 把被拒绝的正文包装成错误。
//
// 带 token 数：上游在 HTTP 200 之后才判定的失败同样计费，
// 不带的话成本统计会凭空少记（与 emptyContentErr 同一个理由）。
func refusalErr(content string, view TargetView, pt, ot int) *CallError {
	return &CallError{
		Kind:    ErrKindRefused,
		Message: fmt.Sprintf("上游以正文形式拒绝了这次请求（model=%s endpoint=%s）: %s",
			view.Model, view.EndpointID, string(truncateBytes([]byte(content), 120))),
		PromptTokens: pt,
		OutputTokens: ot,
	}
}