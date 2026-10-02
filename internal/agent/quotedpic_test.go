package agent

import (
	"testing"

	"dadyumo/internal/webhook"
)

// 引用里的图必须和这条消息自己带的图分开，且归到**被引用的原作者**名下。
//
// 2026-10-02 生产实况（测试群三号在网络上就是die 群）：
//
//	23:53  绝对无法被绿之人：<一张二次元图>
//	00:27  群友甲：（引用上面那条）「这谁？」
//	00:28  羽沫老爹：你自己发的你问我？   ← 认错人了
//
// 当时 agent 把 quotedImgs 直接 append 进 ev.Images，而下游只按
// 「这条消息的发送人」给 Images 记账，于是提示词里成了
// 「群友甲 发了 1 张图」——他只是引用了别人，那图根本不是他发的。
//
// 这些测试走真实的 parsePics（即 OnGroupMessage 实际调用的那个入口），
// 不复刻逻辑、不直接调工具函数。之前那版直接调 extractQuoted+splitPics，
// 属于假绿：把调用点的合并改回去，全部照过——已经踩过一次。

// quoteImageEvent 构造 2026-10-02 那条消息：群友甲引用别人 23:53 发的图。
// 他自己只发了句「这谁？」，没带图。
func quoteImageEvent() *webhook.GroupMessage {
	return &webhook.GroupMessage{
		Content: "这谁？",
		MsgElements: []*webhook.MsgElement{
			{
				Author:  &webhook.User{MemberOpenID: "openid-abs", Username: "绝对无法被绿之人"},
				Content: "", // 图片消息没有正文
				Attachments: []*webhook.Attachment{
					{URL: "https://example.com/waifu.jpg", ContentType: "image/jpeg"},
				},
			},
		},
	}
}

// TestQuotedImageNotCountedAsOwners 把生产实况原样走一遍。
func TestQuotedImageNotCountedAsOwners(t *testing.T) {
	imgs, quotedText, quotedPics, authorOpenID, authorName := parsePics(quoteImageEvent())

	if len(imgs) != 0 {
		t.Errorf("这张图不是群友甲发的，不能进「自己带的图」，got %#v", imgs)
	}
	if len(quotedPics) != 1 {
		t.Fatalf("引用里的图应单独带出，got %#v", quotedPics)
	}
	if quotedPics[0].OpenID != "openid-abs" || quotedPics[0].Name != "绝对无法被绿之人" {
		t.Errorf("引用的图应归原作者，got %q / %q", quotedPics[0].OpenID, quotedPics[0].Name)
	}
	if quotedPics[0].URL != "https://example.com/waifu.jpg" {
		t.Errorf("引用的图地址应带出来，got %q", quotedPics[0].URL)
	}
	// 另一半：纯图片引用也必须报出「有引用」，否则原作者信息整段丢失
	if quotedText == "" {
		t.Error("纯图片引用也必须返回非空文本占位，否则调用方以为没有引用")
	}
	if authorOpenID != "openid-abs" || authorName != "绝对无法被绿之人" {
		t.Errorf("原作者应提取出来，got %q / %q", authorOpenID, authorName)
	}
}

// TestOwnAndQuotedPicsSeparated 自己发的和引用别人的，各自归属正确。
func TestOwnAndQuotedPicsSeparated(t *testing.T) {
	ev := quoteImageEvent()
	// 群友甲自己还发了张截图，同时引用了别人那张
	ev.Attachments = []*webhook.Attachment{
		{URL: "https://example.com/mine.jpg", ContentType: "image/jpeg"},
	}

	imgs, _, quotedPics, _, _ := parsePics(ev)

	if len(imgs) != 1 || imgs[0] != "https://example.com/mine.jpg" {
		t.Errorf("自己带的图应原样保留，got %#v", imgs)
	}
	if len(quotedPics) != 1 || quotedPics[0].URL != "https://example.com/waifu.jpg" {
		t.Fatalf("引用的图应带出来且与自己的分开，got %#v", quotedPics)
	}
	if quotedPics[0].Name != "绝对无法被绿之人" {
		t.Errorf("引用的图应记原作者，got %q", quotedPics[0].Name)
	}
}

// TestQuotedPicUnknownAuthor 原作者认不出来时留空，不能编也不能赖到引用者头上。
func TestQuotedPicUnknownAuthor(t *testing.T) {
	ev := quoteImageEvent()
	ev.MsgElements[0].Author = nil // 平台没给作者信息

	imgs, _, quotedPics, authorOpenID, _ := parsePics(ev)

	if len(imgs) != 0 {
		t.Errorf("没有作者信息时更不能赖到发送人头上，got %#v", imgs)
	}
	if len(quotedPics) != 1 {
		t.Fatalf("图仍要带出来给模型看，got %#v", quotedPics)
	}
	if quotedPics[0].OpenID != "" || quotedPics[0].Name != "" {
		t.Errorf("原作者认不出就留空，下游显示成「有人」；不能编也不能赖到引用者头上，got %q / %q",
			quotedPics[0].OpenID, quotedPics[0].Name)
	}
	if authorOpenID != "" {
		t.Errorf("没有作者信息时不该编造 openid，got %q", authorOpenID)
	}
}

// TestPlainMessageNoQuoted 纯文字消息不该凭空造出引用。
func TestPlainMessageNoQuoted(t *testing.T) {
	ev := &webhook.GroupMessage{Content: "今天群里怎么没人说话"}

	imgs, quotedText, quotedPics, _, _ := parsePics(ev)

	if len(imgs) != 0 {
		t.Errorf("没带图就不该有图，got %#v", imgs)
	}
	if quotedText != "" || len(quotedPics) != 0 {
		t.Errorf("没有引用就不该造出引用，got text=%q pics=%#v", quotedText, quotedPics)
	}
}

// TestOwnImageNoQuoted 引用了别人文字、自己又带图的普通消息。
func TestOwnImageNoQuoted(t *testing.T) {
	ev := &webhook.GroupMessage{
		Content: "看这个",
		Attachments: []*webhook.Attachment{
			{URL: "https://example.com/a.png", ContentType: "image/png"},
		},
	}

	imgs, quotedText, quotedPics, _, _ := parsePics(ev)

	if len(imgs) != 1 {
		t.Errorf("自己带的图应保留，got %#v", imgs)
	}
	if quotedText != "" || len(quotedPics) != 0 {
		t.Errorf("没有引用就不该造出引用，got text=%q pics=%#v", quotedText, quotedPics)
	}
}