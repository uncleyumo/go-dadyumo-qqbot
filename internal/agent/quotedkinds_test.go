package agent

import (
	"encoding/base64"
	"strings"
	"testing"

	"dadyumo/internal/webhook"
)

// 引用的三种形态各自要正确落到提示词里：纯文字、文字+图片、单个表情。
//
// 2026-10-02 生产实况（测试群二号群）：群友甲引用了
// 群名片丙 00:53 发的一个表情（就是那只猫），问
// 「我引用的这条是谁发的聊天记录？」，机器人答
// 「你引用的那条我这儿看不见，截图发出来」。
//
// 原因不在图，在**表情**：平台没给 attachments（它是内置表情不是上传的图），
// 元素正文里装的是一整串
//
//	<faceType=6,faceId="0",ext="eyJ0ZXh0Ijoi5p2O5YqoIn0="/>
//
// 而 extractQuoted 读 el.Content 时**没做清洗**（清洗只发生在本条消息的
// 正文上），于是这坨 base64 残渣原样进了提示词。模型不是看不见，
// 是收到了一坨看不懂的东西。

// faceTag 造一个平台表情标签。text 是表情名，按平台的编码方式放进 ext。
func faceTag(text string) string {
	ext := base64.StdEncoding.EncodeToString([]byte(`{"text":"` + text + `"}`))
	return `<faceType=6,faceId="0",ext="` + ext + `"/>`
}

// TestQuotedFaceTagIsCleaned 引用的表情必须解成可读文本，不能留 base64 残渣。
func TestQuotedFaceTagIsCleaned(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author:  &webhook.User{MemberOpenID: "openid-yang", Username: "群名片丙"},
			Content: faceTag("猫"),
		},
	}

	text, _, _, _ := extractQuoted(els)

	if strings.Contains(text, "faceType") || strings.Contains(text, "ext=") {
		t.Fatalf("引用里的平台标记必须清洗，原样透给模型它只能答「看不见，截图发出来」（2026-10-02 实况），got %q", text)
	}
	if !strings.Contains(text, "猫") {
		t.Errorf("表情名应解出来，got %q", text)
	}
	if strings.Contains(text, "eyJ") { // base64 片段
		t.Errorf("不该残留 base64，got %q", text)
	}
}

// TestQuotedFaceWithoutAuthor 不给 author 时也不能凭空说有作者。
func TestQuotedFaceWithoutAuthor(t *testing.T) {
	els := []*webhook.MsgElement{{Content: faceTag("猫")}} // Author 为 nil

	text, _, authorOpenID, authorName := extractQuoted(els)

	if text == "" {
		t.Fatal("被引用了表情就得报出来，不能当成没有引用")
	}
	if authorOpenID != "" || authorName != "" {
		t.Errorf("平台没给作者就不该编，got %q / %q", authorOpenID, authorName)
	}
}

// TestQuotedTextOnly 纯文字引用（用户问的第一种）：文本和作者都要到。
func TestQuotedTextOnly(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author:  &webhook.User{MemberOpenID: "openid-zhang", Username: "张三"},
			Content: "你说的那个我懂",
		},
	}
	ev := &webhook.GroupMessage{Content: "同意吗", MsgElements: els}

	imgs, quotedText, quotedPics, authorOpenID, authorName := parsePics(ev)

	if quotedText != "你说的那个我懂" {
		t.Errorf("引用文本应原样带出，got %q", quotedText)
	}
	if authorName != "张三" || authorOpenID != "openid-zhang" {
		t.Errorf("原作者应带出，got %q / %q", authorName, authorOpenID)
	}
	if len(imgs) != 0 || len(quotedPics) != 0 {
		t.Errorf("纯文字引用不该产生图，got %d / %d", len(imgs), len(quotedPics))
	}
}

// TestQuotedTextPlusImage 文字+图片引用（用户问的第二种）：两者都要，且分开归属。
func TestQuotedTextPlusImage(t *testing.T) {
	ev := &webhook.GroupMessage{
		Content: "这个怎么说",
		MsgElements: []*webhook.MsgElement{
			{
				Author:  &webhook.User{MemberOpenID: "openid-zhang", Username: "张三"},
				Content: "看下这张",
				Attachments: []*webhook.Attachment{
					{URL: "https://example.com/pic.jpg", ContentType: "image/jpeg"},
				},
			},
		},
	}

	imgs, quotedText, quotedPics, _, authorName := parsePics(ev)

	if quotedText != "看下这张" {
		t.Errorf("引用文本应带出，got %q", quotedText)
	}
	if len(imgs) != 0 {
		t.Errorf("引用里的图不能算这条消息自己发的，got %#v", imgs)
	}
	if len(quotedPics) != 1 || quotedPics[0].URL != "https://example.com/pic.jpg" {
		t.Fatalf("引用里的图应带出，got %#v", quotedPics)
	}
	if quotedPics[0].Name != "张三" {
		t.Errorf("引用里的图应归原作者，got %q", quotedPics[0].Name)
	}
	_ = authorName
}

// TestQuotedTextWithAtTag 引用里 @ 了机器人或别人，标签也要清洗成昵称。
func TestQuotedTextWithAtTag(t *testing.T) {
	els := []*webhook.MsgElement{
		{
			Author:  &webhook.User{MemberOpenID: "openid-zhang", Username: "张三"},
			Content: "<@ABCDEF0123456789ABCDEF0123456789> 帮忙看下",
		},
	}

	text, _, _, _ := extractQuoted(els)

	if strings.Contains(text, "ABCDEF0123456789") {
		t.Errorf("引用里的裸 openid 也要清掉，模型认不出是谁还会学着写乱码，got %q", text)
	}
	if !strings.Contains(text, "帮忙看下") {
		t.Errorf("正文应保留，got %q", text)
	}
}

// TestQuotedNestedChatRecord 甩一整段聊天记录：多个作者都要带上。
func TestQuotedNestedChatRecord(t *testing.T) {
	ev := &webhook.GroupMessage{
		Content: "看看这个",
		MsgElements: []*webhook.MsgElement{
			{
				Author: &webhook.User{MemberOpenID: "o-a", Username: "张三"},
				MsgElements: []*webhook.MsgElement{
					{Author: &webhook.User{MemberOpenID: "o-b", Username: "李四"}, Content: "我不同意"},
				},
			},
		},
	}

	_, quotedText, _, _, authorName := parsePics(ev)

	if !strings.Contains(quotedText, "我不同意") {
		t.Errorf("嵌套的正文应提取出来，got %q", quotedText)
	}
	if !strings.Contains(authorName, "张三") || !strings.Contains(authorName, "李四") {
		t.Errorf("多个作者都要报出来，got %q", authorName)
	}
}