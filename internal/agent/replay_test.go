package agent

import (
	"strings"
	"testing"
)

// TestReplayRealGroupBurst 按生产日志的顺序重放 13:18 那一串消息，
// 检查清洗后的正文是不是「模型能读懂」的样子。
//
// 这是本次修复的验收标准：入站事件里平台塞了三样东西——
// @全体成员、@某个群友、一串 QQ 表情。修复前它们原样进模型上下文，
// 模型看到的是 <@all> 和 base64，只能回「搁这刷表情包呢」这类废话。
func TestReplayRealGroupBurst(t *testing.T) {
	// 生产日志原文（测试群一号，2026-10-01 13:16-13:18），逐条照抄
	events := []string{
		`<@AAAABBBBCCCCDDDDEEEEFFFF00001111>`,
		`<@all>`,
		`@羽沫老爹 你把今天该打游戏的人都艾特出来`,
		`求求你帮忙艾特出来`,
		faceEmpty,
		`😰`,
		faceFlow,
		faceGrin,
		faceStrong,
		faceScare,
		faceWoof,
	}

	// 群里认识的人：机器人自己 + 被 @ 的那位
	names := map[string]string{
		"55556666777788889999000011112222": "羽沫老爹",
		"AAAABBBBCCCCDDDDEEEEFFFF00001111": "群友乙",
	}
	nameOf := func(oid string) string { return names[oid] }

	var got []string
	sawAtAll := false
	for i, raw := range events {
		clean, atAll := cleanTags(raw, nameOf)
		sawAtAll = sawAtAll || atAll
		got = append(got, clean)
		t.Logf("%2d 原文=%-58s -> %s", i+1, raw, clean)
	}

	// 1. 平台标记一个都不许残留
	joined := strings.Join(got, " | ")
	for _, bad := range []string{"<@", "faceType", "eyJ0ZXh0", "@all"} {
		if strings.Contains(joined, bad) {
			t.Errorf("残留平台标记 %q: %s", bad, joined)
		}
	}
	// 2. 裸 openid 一个都不许进上下文（32 位十六进制，纯烧 token）
	if strings.Contains(joined, "AAAA5555") || strings.Contains(joined, "55558888") {
		t.Errorf("裸 openid 泄漏进上下文: %s", joined)
	}
	// 3. @全体成员 必须被认出来
	if !sawAtAll {
		t.Error("应识别出 @全体成员")
	}
	if got[1] != "〔@全体成员〕" {
		t.Errorf("第 2 条应是 〔@全体成员〕，实际 %q", got[1])
	}
	// 4. @群友 要换成真名，并包进 〔〕 说明这是一次 @ 动作
	if got[0] != "〔@群友乙〕" {
		t.Errorf("第 1 条应是 〔@群友乙〕，实际 %q", got[0])
	}
	// 5. 表情要读得出中文名
	for _, want := range []string{"（流泪）", "（呲牙）", "（坚强）", "（惊吓）", "（汪汪）"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺 %s: %s", want, joined)
		}
	}
	// 6. 普通文字一个字都不能动
	if got[2] != "@羽沫老爹 你把今天该打游戏的人都艾特出来" {
		t.Errorf("普通文本被改动了: %q", got[2])
	}
	if got[3] != "求求你帮忙艾特出来" {
		t.Errorf("普通文本被改动了: %q", got[3])
	}
	// 7. 真 emoji 原样保留
	if got[5] != "😰" {
		t.Errorf("真 emoji 应保留: %q", got[5])
	}
	// 8. 空 ext 不是内置表情，是群友发的表情包：既不能吐 base64 回去，
	//    也不能说成「表情」（见 TestNamelessExtIsMeme）
	if got[4] != "（表情包）" {
		t.Errorf("空 ext 应退化成（表情包），实际 %q", got[4])
	}
}

// 表情必须用圆括号，不能用方括号——
// 方括号在这个系统里是「这是媒体附件」的占位符（[图片]/[语音]/[视频]），
// 混用会让模型把一串表情当成「又发了堆图」。
// 2026-10-01 实况：用 [流泪] 时它回「表情包批发呢你」「有事说事，别光发图」。
func TestCleanedEmojiUsesRoundBrackets(t *testing.T) {
	clean, _ := cleanTags(faceFlow+faceGrin+faceStrong, nil)
	if clean != "（流泪）（呲牙）（坚强）" {
		t.Fatalf("got %q", clean)
	}
	if strings.ContainsAny(clean, "[]<>") {
		t.Errorf("表情里混进了方括号/尖括号，会和附件占位符混淆: %q", clean)
	}
}
