package brain

import (
	"regexp"
	"strings"
	"time"
)

// ImpulseInput 计算「想不想说话」所需的全部上下文
type ImpulseInput struct {
	AtMe        bool          // 被 @ 了
	NameCalled  bool          // 有人直接叫了它的名字
	Question    bool          // 是个问句
	ReplyToBot  bool          // 明显是在接它上一句话
	FromMaster  bool          // 说话的人是主人
	Chatter     bool          // 群里确实有人在说话（闲聊基数）
	NewCount    int           // 这一批里攒了多少条新消息
	Consecutive int           // 它已经连着说了几轮
	SinceSpeak  time.Duration // 距离它上次发言多久
	Idle        time.Duration // 群里冷场多久
}

// ImpulseResult 冲动值与其构成，便于排查「为什么它又说话了 / 为什么它不说话」
type ImpulseResult struct {
	Score   float64
	Reasons []string
}

// 冲动权重。这些数字是「沉默程度」的调节旋钮：
// 调高 threshold 它就变闷葫芦，调低就变话痨。默认偏向沉默。
const (
	wAtMe       = 0.90 // 被点名几乎必须回，否则像掉线
	wNameCalled = 0.70
	wMaster     = 0.50 // 主人的话给面子
	wReplyToBot = 0.30
	wQuestion   = 0.25
	wChatter    = 0.25 // 群里有人在说话的基数分：纯闲聊也该有机会被咨询
	wManyNew    = 0.15 // 攒够一批热闹，值得插一句
	wIdleBoost  = 0.30 // 冷场时主动找话

	pConsecutive2 = -0.50 // 已经连着说过，收着点
	pConsecutive4 = -0.90
	pJustSpoke    = -0.80 // 刚说完就别抢话

	// pAtOthers 有人在跟别人说话。取值盖过 wChatter+wManyNew（0.25+0.15=0.40），
	// 这样「@ 别人 + 攒够一批」不会刚好压在阈值上挤进来。
	// 2026-10-01 实况：0.40 ≥ 0.40 阈值，机器人抢了一句。
	// 不是硬闸：抢话和恰当的接话只差一个量，一刀切会把合法插话也毙掉。
	//
	// 注意它**不在 ComputeImpulse 里生效**：那个函数算的是单条消息的分数，
	// 而批次取的是最高分，扣分会被后面的消息盖掉。必须在批次层面单独压，
	// 见 engine.go 里 st.impulse 的处理。
	pAtOthers = -0.50
)

// ComputeImpulse 计算冲动值，返回 0~1。
//
// 这里刻意不做「语义相关性」判断——那要额外一次模型调用，成本翻倍。
// 用规则把明显不该开口的情形挡掉，剩下的交给模型去决定要不要说。
func ComputeImpulse(in ImpulseInput) ImpulseResult {
	var score float64
	var reasons []string

	if in.AtMe {
		score += wAtMe
		reasons = append(reasons, "被@")
	}
	if in.NameCalled {
		score += wNameCalled
		reasons = append(reasons, "被叫名字")
	}
	if in.FromMaster {
		score += wMaster
		reasons = append(reasons, "主人说话")
	}
	if in.ReplyToBot {
		score += wReplyToBot
		reasons = append(reasons, "有人在接话")
	}
	if in.Question {
		score += wQuestion
		reasons = append(reasons, "有人提问")
	}
	if in.Chatter {
		score += wChatter
		reasons = append(reasons, "群里有闲聊")
	}
	if in.NewCount >= 5 {
		score += wManyNew
		reasons = append(reasons, "攒了一批新消息")
	}
	if in.Idle > 0 {
		score += wIdleBoost
		reasons = append(reasons, "群里冷场")
	}

	switch {
	case in.Consecutive >= 4:
		score += pConsecutive4
		reasons = append(reasons, "已经连着说太多轮")
	case in.Consecutive >= 2:
		score += pConsecutive2
		reasons = append(reasons, "刚说过话")
	}
	// 注意必须是 >0：调用方不填这个字段时零值表示「没说过话」，
	// 零值 < 25s 会让它误以为「刚发过言」，把被 @ 的场景也一并压死。
	if in.SinceSpeak > 0 && in.SinceSpeak < 25*time.Second {
		score += pJustSpoke
		reasons = append(reasons, "刚发过言")
	}

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return ImpulseResult{Score: score, Reasons: reasons}
}

// 判定一句话是否像在提问
var questionMarks = []string{"?", "？", "吗", "呢", "么", "谁", "为什么", "怎么", "咋", "是不是", "有没有", "能不能"}

// LooksLikeQuestion 粗略判断是否问句。
// 只要能挡掉「陈述句也被当成提问」就够了，精确与否不重要。
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
// 为什么需要它：图片刷屏有 imgSpamCount 兜着，可 QQ 表情走的是正文文本路径，
// 完全绕过了那个机制——连发 8 个表情照样把攒批条数顶上去、
// 推高冲动值、最后挤进门限发言。2026-10-01 实况就是这样在没被 @ 的情况下
// 回了两句。刷屏就该闭嘴，这是刷屏，不是在跟它说话。
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
