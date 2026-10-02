package agent

import (
	"strings"
	"testing"

	"dadyumo/internal/webhook"
)

func TestExtractQuotedText(t *testing.T) {
	els := []*webhook.MsgElement{
		{Content: "他说这话这么吊", MsgElements: []*webhook.MsgElement{
			{Content: "更里面一层"},
		}},
	}
	text, imgs, _, _ := extractQuoted(els)
	if text == "" {
		t.Fatal("应提取到引用文本")
	}
	if len(imgs) != 0 {
		t.Fatalf("不应有图, got %#v", imgs)
	}
	t.Logf("引用文本: %s", text)
}

func TestExtractQuotedImages(t *testing.T) {
	els := []*webhook.MsgElement{
		{Attachments: []*webhook.Attachment{
			{URL: "https://example.com/a.png", FileName: "a.png", ContentType: "image/png"},
		}},
		{Content: "看看这个"},
	}
	text, imgs, _, _ := extractQuoted(els)
	if text != "看看这个" {
		t.Fatalf("文本提取不对: %q", text)
	}
	if len(imgs) != 1 || imgs[0] != "https://example.com/a.png" {
		t.Fatalf("引用里的图应被提取: %#v", imgs)
	}
}

func TestExtractQuotedEmpty(t *testing.T) {
	if text, imgs, _, _ := extractQuoted(nil); text != "" || imgs != nil {
		t.Fatalf("空输入应返回空, got %q %#v", text, imgs)
	}
}

// TestExtractQuotedIdentifiesAuthor 引用必须同时给出「谁引的」和「被引的原作者是谁」。
//
// 只拿到文本的话，模型会把被引用的内容当成「刚才有人在群里说的」，
// 于是回错了人——群友甩一段聊天记录出来让你看的时候最容易发生。
func TestExtractQuotedIdentifiesAuthor(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author:  &webhook.User{MemberOpenID: "openid-zhang", Username: "张三"},
			Content: "你说的那个我懂",
		},
	}

	text, _, authorOpenID, authorName := extractQuoted(els)
	if text != "你说的那个我懂" {
		t.Fatalf("引用文本应提取出来，got %q", text)
	}
	if authorOpenID != "openid-zhang" {
		t.Errorf("被引用者的 openid 应提取出来，got %q", authorOpenID)
	}
	if authorName != "张三" {
		t.Errorf("被引用者的昵称应提取出来，got %q", authorName)
	}
}

// TestExtractQuotedNestedAuthors 引用里套聊天记录时，多个作者都要报出来。
func TestExtractQuotedNestedAuthors(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author: &webhook.User{MemberOpenID: "o-a", Username: "张三"},
			MsgElements: []*webhook.MsgElement{
				{Author: &webhook.User{MemberOpenID: "o-b", Username: "李四"}, Content: "我不同意"},
			},
		},
	}

	_, _, _, authorName := extractQuoted(els)
	if !strings.Contains(authorName, "张三") || !strings.Contains(authorName, "李四") {
		t.Errorf("多个作者都应报出来，got %q", authorName)
	}
}

// TestExtractQuotedNoAuthor 引用里没有作者信息时不能瞎编，返回空即可。
func TestExtractQuotedNoAuthor(t *testing.T) {
	els := []*webhook.MsgElement{{Content: "没有作者信息"}}
	text, _, authorOpenID, authorName := extractQuoted(els)
	if text != "没有作者信息" {
		t.Errorf("文本仍应提取，got %q", text)
	}
	if authorOpenID != "" || authorName != "" {
		t.Errorf("没有作者就不该编造，got %q / %q", authorOpenID, authorName)
	}
}

// TestExtractQuotedImageOnlyStillReports 引用一条**纯图片**消息时不能返回空文本。
//
// 2026-10-02 生产实况（测试群三号群）：群友甲引用「绝对 无法 被绿之人」
// 半小时前发的一张二次元图，问「这谁？」。被引用的那条一个字的正文都没有，
// extractQuoted 于是返回空串，而调用方拿「文本为空」当「这条消息没有引用」，
// 原作者是谁、引用这个动作，整段一起丢掉了。
//
// 模型最后只看到一张来路不明的图，答了「你自己发的你问我？」。
func TestExtractQuotedImageOnlyStillReports(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author:  &webhook.User{MemberOpenID: "openid-abs", Username: "绝对无法被绿之人"},
			Content: "", // 图片消息没有正文
			Attachments: []*webhook.Attachment{
				{URL: "https://example.com/waifu.jpg", ContentType: "image/jpeg"},
			},
		},
	}

	text, imgs, authorOpenID, authorName := extractQuoted(els)

	if text == "" {
		t.Fatal("引用了图片就必须给出非空占位：返回空会让调用方以为这条消息没有引用，" +
			"原作者和引用关系全丢（2026-10-02 实况就是这么答错人的）")
	}
	if len(imgs) != 1 {
		t.Fatalf("图应照常提取，got %#v", imgs)
	}
	if authorOpenID != "openid-abs" || authorName != "绝对无法被绿之人" {
		t.Errorf("原作者应提取出来，got %q / %q", authorOpenID, authorName)
	}
}

// 引用里既没文字也没图、但有作者时，也不能报成「没有引用」。
func TestExtractQuotedBareAuthorStillReports(t *testing.T) {
	els := []*webhook.MsgElement{
		{Author: &webhook.User{MemberOpenID: "openid-a", Username: "张三"}},
	}
	if text, _, _, _ := extractQuoted(els); text == "" {
		t.Error("只有作者信息时也该返回非空占位")
	}
}

// 真的什么都没有时（没有引用）才允许返回空——这是调用方判断「无引用」的唯一依据。
func TestExtractQuotedTrulyEmptyStaysEmpty(t *testing.T) {
	if text, _, oid, _ := extractQuoted(nil); text != "" || oid != "" {
		t.Errorf("空输入必须仍返回空，got %q / %q", text, oid)
	}
	els := []*webhook.MsgElement{{Author: &webhook.User{}}}
	if text, _, _, _ := extractQuoted(els); text != "" {
		t.Errorf("作者信息全空时不该凭空造占位，got %q", text)
	}
}

// TestParseMentionsDetectsBot 被 @ 机器人必须走 mentions 数组判定。
//
// 官方文档只承诺「content 已去除 @机器人 的前缀」，对 @他人 在正文里残留成什么
// 格式没有任何承诺，所以正文解析只能兜底，mentions 才是判据。
func TestParseMentionsDetectsBot(t *testing.T) {
	ms := []*webhook.User{
		{MemberOpenID: "openid-bot", Username: "羽沫老爹"},
		{MemberOpenID: "openid-li", Username: "李四"},
	}
	me, others := parseMentions(ms, "openid-bot")
	if !me {
		t.Fatal("mentions 里含机器人自己时应判定为被 @")
	}
	if len(others) != 1 || others["openid-li"] != "李四" {
		t.Errorf("其余被 @ 的人应单独返回，got %#v", others)
	}
	if _, dup := others["openid-bot"]; dup {
		t.Error("机器人自己不该出现在 others 里")
	}
}

// TestParseMentionsUserOpenIDFallback 平台有时把标识填在 user_openid 而不是 member_openid。
func TestParseMentionsUserOpenIDFallback(t *testing.T) {
	ms := []*webhook.User{{UserOpenID: "openid-bot", Username: "羽沫老爹"}}
	if me, _ := parseMentions(ms, "openid-bot"); !me {
		t.Error("user_openid 也应能识别出机器人自己")
	}
}

// TestParseMentionsEmpty 没被任何人 @ 时，两个返回值都应为空/false。
func TestParseMentionsEmpty(t *testing.T) {
	me, others := parseMentions(nil, "openid-bot")
	if me {
		t.Error("没有 mentions 时不应判定为被 @")
	}
	if len(others) != 0 {
		t.Errorf("没有 mentions 时 others 应为空，got %#v", others)
	}
	me, others = parseMentions([]*webhook.User{nil, {}}, "openid-bot")
	if me || len(others) != 0 {
		t.Errorf("nil 元素应被跳过，got me=%v others=%#v", me, others)
	}
}

// TestParseMentionsNoSelfOpenIDConfigured 没配 self_openid 时不能把谁都当成机器人。
func TestParseMentionsNoSelfOpenIDConfigured(t *testing.T) {
	ms := []*webhook.User{{MemberOpenID: "openid-a", Username: "张三"}}
	if me, _ := parseMentions(ms, ""); me {
		t.Error("self_openid 为空时不应判定被 @")
	}
}
