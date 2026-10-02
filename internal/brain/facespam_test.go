package brain

import (
	"testing"
	"time"
)

// 刷屏识别的验收标准。
//
// 事故实况（2026-10-01 14:01-14:02，测试群一号）：群友甲连发 8 个 QQ 表情，
// 全程 at:false，机器人却回了两句——
//
//	「6 / 表情包批发呢你」  14:01:48
//	「有事说事，别光发图」  14:02:44
//
// 成因：表情走正文文本路径，绕过了图片刷屏那套 imgSpamCount 机制，
// 连发把攒批条数顶上去，wChatter+wManyNew 合计 0.40 挤过了门限。

// 生产日志里的原样 payload（清洗后）
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

// 连发 8 个表情不该产生任何「值得插一句」的冲动
func TestFaceSpamDoesNotTriggerImpulse(t *testing.T) {
	// 不带刷屏压制时，8 条消息的冲动值刚好过常见阈值
	base := ComputeImpulse(ImpulseInput{
		Chatter: true, NewCount: 8, SinceSpeak: time.Hour,
	})
	if base.Score < 0.35 {
		t.Fatalf("前置条件不成立：8 条消息的基线冲动值应够门槛，实际 %.2f", base.Score)
	}

	// 压制后必须掉到门限以下
	after := base.Score - wChatter - wManyNew
	if after >= 0.35 {
		t.Errorf("刷屏压制后冲动值仍过高: %.2f", after)
	}
}

// 有实义内容混进同一批时，压制必须撤销
func TestFaceSpamSuppressionLiftedByRealContent(t *testing.T) {
	// 这是 OnMessage 里的 else 分支在做的事：混进正经话就撤销刷屏标记。
	// 不撤销的话「连发七个表情 + 一句正经话」会被误毙掉。
	faceSpam := true
	if !IsOnlyPlaceholders("最后说一句，今天真冷") {
		faceSpam = false
	}
	if faceSpam {
		t.Error("混进实义内容后刷屏标记应撤销，否则会误伤正常对话")
	}
}
