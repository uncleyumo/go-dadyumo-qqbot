package brain

import (
	"time"

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
		cost := lineCost(lines, i)
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

// lineOverheadBase 每条消息的固定渲染开销：「· 名字：内容」里的分隔符、
// 可能的开发者标签、换行。历史里有大量「嗯」「在」这种一两个字的短消息，
// 正文估不出来但前缀是实打实的 token。
const lineOverheadBase = 8

// 两档时间标记的渲染开销。
//
// 超窗只出时刻（"[14:00] "），比带间隔的（"[14:02 隔了3分钟] "）短一截，
// 所以开销也低。分两档算而不是一律取最大值：真实群里超窗标记
// （跨夜那种）零星出现，一律按最贵的算会让估算比实际高出一截，
// 长期看就是把预算花在了不存在的东西上。
//
// 跨天多出的 "10-05 " 不单列：估算侧拿不到 now 判断是否跨天，
// 而那 4 个字符相对 lineOverheadBase 本就微不足道。
// 漏算它的后果是这几行略微超预算——由 lineOverheadBase 的余量吸收。
const (
	lineOverheadTimeClock = 6  // "[14:00] "
	lineOverheadTimeGap   = 11 // "[14:02 隔了3分钟] "
)

// lineCost 估算第 i 条渲染后的开销。
//
// 必须把时间标记算进去，否则这里会系统性低估：静过一阵的群里
// 每条都挂标记，30 条就是几百 token。低估的实际后果不是「超预算被拒」
// （context_length_exceeded），而是这一轮白花钱直到被上游截断，
// 且现象随群的活跃度剧烈波动，很难归因。
//
// 间隔按「与上一条比」，与 renderLines 的 timeMarkerAt 口径一致。
// 首行没有上一条，用「距今很久」这个保守假设（按有标记计）。
func lineCost(lines []memory.Line, i int) int {
	n := EstimateTokens(lines[i].Content) + EstimateTokens(lines[i].Name) + lineOverheadBase
	switch {
	case hasTimeMarkerGap(lines, i):
		n += lineOverheadTimeGap
	case hasTimeMarkerClock(lines, i):
		n += lineOverheadTimeClock
	}
	return n
}

// hasTimeMarkerGap 判断第 i 条会不会被打上「带间隔」的时间标记。
//
// 口径与 renderLines 的 timeMarkerAt 一致：间隔落在引用窗口内才带间隔，
// 超过窗口只给时刻（那个数字对决策已无用，详见 timeMarkerAt 的注释）。
func hasTimeMarkerGap(lines []memory.Line, i int) bool {
	if lines[i].TS.IsZero() {
		return false
	}
	prev := prevTimedLine(lines, i)
	if prev.IsZero() {
		// 首行：renderLines 拿它跟 now 比，估算侧拿不到 now，不猜。
		// 下面 hasTimeMarkerClock 同样返回 false——两档都不算。
		return false
	}
	gap := lines[i].TS.Sub(prev)
	return gap >= timeMarkerThreshold && gap <= quoteWindow
}

// hasTimeMarkerClock 判断第 i 条会不会被打上「只带时刻」的时间标记。
//
// 首行返回 false（不猜）：renderLines 会给首行打标记（它跟 now 比），
// 但估算侧拿不到 now，于是无从判断。**宁可漏算首行**——
// 对每个窗口都白加一次的话，刷屏的群里每轮都多算 6 token，
// 而实际一个标记都不会出现。首行的漏算由 lineOverheadBase 的余量兜。
func hasTimeMarkerClock(lines []memory.Line, i int) bool {
	if lines[i].TS.IsZero() {
		return false
	}
	prev := prevTimedLine(lines, i)
	if prev.IsZero() {
		return false
	}
	return lines[i].TS.Sub(prev) >= timeMarkerThreshold
}

// prevTimedLine 往前找最近一条有时间的消息。
//
// 与 renderLines 里「零值 TS 不推进 prev」的口径一致：中间夹一条
// 零值记录不该把间隔链条打断。
func prevTimedLine(lines []memory.Line, i int) time.Time {
	for j := i - 1; j >= 0; j-- {
		if !lines[j].TS.IsZero() {
			return lines[j].TS
		}
	}
	return time.Time{}
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
