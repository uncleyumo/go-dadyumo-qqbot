package brain

import (
	"regexp"
	"strings"
)

// 本文件是「文本形状判断」的住处，2026-10-03 由 impulse.go 拆分而来。
//
// 原 impulse.go 是整个「冲动值」机制：11 个权重常量、ComputeImpulse、
// 以及围绕它的扣分与门限比较。那套整套废弃了，只留下两个
// 「文本长什么样」的判断——它们与冲动值无关，是纯粹的事实识别。

// 判定一句话是否像在提问
var questionMarks = []string{"?", "？", "吗", "呢", "么", "谁", "为什么", "怎么", "咋", "是不是", "有没有", "能不能"}

// LooksLikeQuestion 粗略判断是否问句。
// 只要能挡掉「陈述句也被当成提问」就够了，精确与否不重要。
//
// 它的输出只用在 buildTrigger 里告诉模型「{谁}在问问题」——
// 属于**可从文本验证的客观描述**，不是对「这话是不是问机器人」的猜测。
// 这一点很重要：2026-10-03 起 trigger 铁律是只陈述事实、不下结论
// （见 buildTrigger 的注释），而「在问问题」恰好是可以陈述的事实。
//
// 反过来说，问句**不等于**「这话在问机器人」。触发词表很宽，
// 「你懂吗」「谁说的」都会命中，模型仍要自己判断这话是问谁。
func LooksLikeQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	for _, m := range questionMarks {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// 单个占位符：媒体附件 [图片] 与 QQ 表情（流泪）
//
// 用于识别「一句话里没有任何真正的文字」。一次只匹配一个——
// 表情是连着发的（（流泪）（呲牙）（坚强）），要靠外层循环一个个剥。
var onePlaceholder = regexp.MustCompile(
	`\[[^\[\]]{1,8}\]|（[^（）]{1,12}）`)

// 占位符之间允许的分隔符与语气词
var placeholderFiller = " \t，,、。.！!？?~～…"

// IsOnlyPlaceholders 判断一条消息是不是「只有表情/附件、没有一个字的话」。
//
// 注意它的用途：只用来**陈述事实**，不用来拦下这一批。
// 曾经它配套着「扣 0.40 冲动值 + 一道硬闸」，把连发 8 个表情的批次整个毙掉，
// 2026-10-01 那次抢话就是这么来的（见 buildTrigger 的注释）。
// 现在它只负责让 buildTrigger 能如实说「这批发的是表情，一个字都没有」，
// 至于该不该理，模型自己判断——群里连发八个「666」，
// 一个真人看到也可能接一句「你复读机啊」，机械跳过才是错的。
func IsOnlyPlaceholders(text string) bool {
	rest := strings.TrimSpace(text)
	if rest == "" {
		return false
	}
	// 至少要有一个占位符，否则纯文字会被误判成刷屏
	found := false
	for {
		loc := onePlaceholder.FindStringIndex(rest)
		if loc == nil {
			break
		}
		found = true
		rest = rest[:loc[0]] + rest[loc[1]:]
	}
	if !found {
		return false
	}
	return strings.Trim(rest, placeholderFiller) == ""
}
