package brain

import (
	"dadyumo/internal/memory"
)

// EstimateTokens 粗估一段文本占用多少 token。
//
// 为什么必须估：免费的、便宜的模型上下文可能只有 32K 甚至更小，
// 而一个 20 人的群一天能攒出上千条消息。不裁剪的话，
// 要么请求被上游直接拒绝，要么单次调用就吃掉大几千 token 的额度。
//
// 估算口径（偏保守，宁可多算不可少算）：
//   - 中日韩文字：约 1 token/字
//   - 其他字符（拉丁字母、数字、标点、空格）：约 3.5 字符/token
//   - 整体上浮 10% 作为安全边际
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	n := cjk + other/3 + 1
	return n + n/10
}

// isCJK 判断是否为中日韩文字或全角符号。
// 用 unicode 的区间表覆盖不全（比如全角标点在 HalfwidthAndFullwidthForms），
// 这里直接用码点区间判断，简单且够准。
func isCJK(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11FF: // 韩文音节
		return true
	case r >= 0x2E80 && r <= 0x303F: // CJK 部首、标点
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // 中日韩统一表意文字
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容表意文字
		return true
	case r >= 0xFF00 && r <= 0xFF60: // 全角形式
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
		return true
	}
	return false
}

// TrimHistory 按 token 预算裁剪历史，保留最新的若干条。
//
// 从最新往回累加，一旦超预算就停。这样保证「最近发生了什么」永远完整，
// 被牺牲的总是更久远的上下文——这正是滑动窗口该有的行为。
func TrimHistory(lines []memory.Line, budget int) []memory.Line {
	if budget <= 0 || len(lines) == 0 {
		return nil
	}
	kept := make([]memory.Line, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		cost := EstimateTokens(lines[i].Content) + EstimateTokens(lines[i].Name) + 8
		if used+cost > budget && len(kept) > 0 {
			break
		}
		used += cost
		kept = append(kept, lines[i])
	}
	// 上面是倒序收集的，翻回正序
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// ContextBudget 计算这次请求能给历史留多少 token。
//
// 取三个上限里的最小值：
//   - 配置里的硬预算（brain.max_ctx_tokens）
//   - 模型标称上下文扣掉输出预留与安全余量
//   - 兜底最小值，防止算出个负数
func ContextBudget(maxCtxTokens, modelMaxCtx, maxOutTokens int) int {
	budget := maxCtxTokens
	if budget <= 0 {
		budget = 6000
	}
	if modelMaxCtx > 0 {
		// 输出 + 系统提示词 + 余量，至少留 1/4 给它们
		reserve := maxOutTokens
		if reserve <= 0 {
			reserve = 512
		}
		avail := modelMaxCtx - reserve - modelMaxCtx/4
		if avail < budget {
			budget = avail
		}
	}
	if budget < 800 {
		budget = 800
	}
	return budget
}
