package brain

import (
	"testing"
)

// 刷屏识别的验收标准。
//
// 事故实况（2026-10-01 14:01-14:02，测试群一号）：群友甲连发 8 个 QQ 表情，
// 全程 at:false，机器人却回了两句——
//
//	「6 / 表情包批发呢你」  14:01:48
//	「有事说事，别光发图」  14:02:44
//
// 成因不是「它判断错了该不该理」，而是**程序给模型编了一条假线索**：
// 过去 trigger 只说「某某刚在群里说了话」，加上「攒了一批新消息」把冲动值
// 顶过门限，模型据此以为有人在跟它聊天。
//
// 2026-10-03 的处理：冲动值机制整套废除，刷屏硬闸也删了。
// IsOnlyPlaceholders 保留下来，但只用来**陈述事实**——
// 让 buildTrigger 能如实说「这批发的是表情，一个字都没有」，
// 至于该不该理，模型自己判断。群里连发八个「666」，
// 一个真人看到也可能接一句「你复读机啊」，机械跳过才是错的。

// TestIsOnlyPlaceholders 刷屏识别的准确率：只认「真的一个字都没有」。
func TestIsOnlyPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"（你懂的）（出去玩）（自信学霸）（流泪）", true, "连发表情=刷屏"},
		{"（微笑）", true, "单个表情也算（配合 newCount 累积）"},
		{"（微笑）（大哭） 你好 （吐）（豹富）", false, "混了实义文字，不算刷屏"},
		{"你好", false, "纯文字"},
		{"@羽沫老爹 （微笑）", false, "带了 @，是明确在叫它"},
		{"[图片] [图片] [图片]", true, "纯图片刷屏同理"},
		{"[图片] 今天这张不错", false, "图片+评论，不算刷屏"},
		{"", false, "空内容不归这里管"},
		{"（", false, "残缺括号不算"},
	}
	for _, c := range cases {
		if got := IsOnlyPlaceholders(c.in); got != c.want {
			t.Errorf("IsOnlyPlaceholders(%q) = %v, want %v（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestFaceSpamFlagLiftedByRealContent 混进正经话就撤销刷屏标记。
//
// 这条现在比过去更重要：过去还有一道硬闸兜着（就算 faceSpam 判错了，
// 整批也只是被硬闸拦下），现在 faceSpam 唯一的用途是写进 trigger 告诉模型。
// 判错了就是直接给模型一句假事实——「这批发的是表情，一个字都没有」，
// 而实际混着一句正经话。
func TestFaceSpamFlagLiftedByRealContent(t *testing.T) {
	// 这是 OnMessage 里的 else 分支在做的事：混进正经话就撤销刷屏标记。
	faceSpam := true
	if !IsOnlyPlaceholders("最后说一句，今天真冷") {
		faceSpam = false
	}
	if faceSpam {
		t.Error("混进实义内容后刷屏标记应撤销，否则会给模型一句假事实（这批全是表情）")
	}
}
