package agent

import (
	"testing"

	"dadyumo/internal/brain"
	"dadyumo/internal/qqapi"
)

// 引用反查的三种结局各钉一条，外加「不是引用时一个字都不许造」。
//
// 2026-10-08 实况（群 117掏桥洞）：
//
//	22:26:48  青鸟主簿 发了一张 gif 表情包，老爹收到了，
//	          那条回调里 msg_idx=REFIDX_h7ef7rBg…
//	22:27:03  奶酱「肉不肉麻啊……」引用它，
//	          老爹收到的回调里 ref_msg_idx=REFIDX_h7ef7rBg…
//
// 同一个字符串，但改动之前谁也没把这两头对上：引用回调里
// **没有 author、没有 attachments**（当天 81 条含引用的回调，
// 带 attachments 的 0 条，20 条带 msg_elements 的里带 author 的 0 条）。
// 于是那张图在老爹眼里成了奶酱自己发的。

const testGroup = "group-1"

// 反查命中：原作者、原文、图片一起回来，且图归**原作者**。
func TestResolveQuotedFromIndex(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)
	idx.Add(testGroup, "REFIDX_waifu", qqapi.QuoteSrc{
		OpenID: "openid-qingniao", Name: "青鸟主簿",
		Text:   "刚拍的",
		Images: []string{"https://example.com/waifu.gif"},
	})

	text, pics, name, openID := resolveQuoted(idx, testGroup, "REFIDX_waifu",
		"", nil, "", "")

	if text != "刚拍的" {
		t.Errorf("原文应来自索引，got %q", text)
	}
	if name != "青鸟主簿" || openID != "openid-qingniao" {
		t.Errorf("原作者应来自索引，got %q / %q", name, openID)
	}
	if len(pics) != 1 || pics[0].URL != "https://example.com/waifu.gif" {
		t.Fatalf("被引用的图应带出来，got %#v", pics)
	}
	if pics[0].Name != "青鸟主簿" || pics[0].OpenID != "openid-qingniao" {
		t.Errorf("被引用的图必须归原作者，否则又变成「引用者自己发的」，got %q / %q",
			pics[0].Name, pics[0].OpenID)
	}
}

// 索引命中的是一条纯图消息：正文给占位，图照样带出来。
func TestResolveQuotedIndexImageOnly(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)
	idx.Add(testGroup, "REFIDX_pic", qqapi.QuoteSrc{
		OpenID: "openid-q", Name: "青鸟主簿",
		Images: []string{"https://example.com/a.gif"},
	})

	text, pics, name, _ := resolveQuoted(idx, testGroup, "REFIDX_pic", "", nil, "", "")

	if text != quotedPicOnly {
		t.Errorf("纯图引用应给图片占位，got %q", text)
	}
	if len(pics) != 1 || pics[0].Name != "青鸟主簿" {
		t.Errorf("图仍要带出来并归原作者，got %#v", pics)
	}
	if name != "青鸟主簿" {
		t.Errorf("作者应带出，got %q", name)
	}
}

// 索引里没有，但平台给了一段没有作者的原文（msg_elements）：原文照用，作者留空。
func TestResolveQuotedPlatformTextOnly(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)

	text, pics, name, openID := resolveQuoted(idx, testGroup, "REFIDX_old",
		"前面七个都还像话", nil, "", "")

	if text != "前面七个都还像话" {
		t.Errorf("平台给的原文应保留，got %q", text)
	}
	if name != "" || openID != "" {
		t.Errorf("平台没给作者就不能编，got %q / %q", name, openID)
	}
	if len(pics) != 0 {
		t.Errorf("不该凭空造图，got %#v", pics)
	}
}

// 索引里没有、平台也什么都没给：必须报出「这是一条引用别人的消息」。
//
// 这条是本次改动的重点。以前这种情况整段被丢掉，模型只看见
// 「肉不肉麻啊……」一句光话，就把被引用的图算到引用者头上了。
func TestResolveQuotedUnknownStillReported(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)

	text, pics, name, openID := resolveQuoted(idx, testGroup, "REFIDX_gone", "", nil, "", "")

	if text == "" {
		t.Fatal("引用关系是知道的，就不能返回空——下游把空当成「没有引用」，整段一起丢")
	}
	if text != quotedUnknown {
		t.Errorf("应给「别人发的消息」占位，got %q", text)
	}
	if name != "" || openID != "" || len(pics) != 0 {
		t.Errorf("查不到就什么都不许编，got %q / %q / %#v", name, openID, pics)
	}
}

// 平台只给了图、没给正文（理论上 extractQuoted 会补占位，这里兜住漏网的）：
// 不能退化成「内容未知」——图明明就在手里。
func TestResolveQuotedPicsWithoutText(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)

	text, pics, _, _ := resolveQuoted(idx, testGroup, "REFIDX_x", "",
		[]brain.MediaRef{{OpenID: "o", Name: "某人", URL: "https://example.com/x.jpg"}}, "", "")

	if text != quotedPicOnly {
		t.Errorf("手里有图就该说图，got %q", text)
	}
	if len(pics) != 1 {
		t.Errorf("图不该被丢掉，got %#v", pics)
	}
}

// 不是引用（没有 ref_msg_idx）时，一个字都不许造。
//
// 这条防的是「占位写漏了条件」：普通消息一旦被塞进 quotedUnknown，
// 提示词里每条消息都会变成「某人引用了一条别人的消息」。
func TestResolveQuotedPlainMessageUntouched(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)

	text, pics, name, openID := resolveQuoted(idx, testGroup, "", "", nil, "", "")

	if text != "" || len(pics) != 0 || name != "" || openID != "" {
		t.Errorf("普通消息不该被造出引用，got text=%q pics=%#v %q/%q", text, pics, name, openID)
	}
}

// 别的群的同 id 消息不能串台。refIdx 是平台按群+机器人签的，
// 但索引是按群存的，跨群查到就是索引本身的 bug。
func TestResolveQuotedNotCrossGroup(t *testing.T) {
	idx := qqapi.NewQuoteIndex(10)
	idx.Add("group-2", "REFIDX_same", qqapi.QuoteSrc{Name: "别人", Text: "别的群的话"})

	text, _, name, _ := resolveQuoted(idx, testGroup, "REFIDX_same", "", nil, "", "")

	if name != "" || text != quotedUnknown {
		t.Errorf("跨群不该命中，got %q / %q", text, name)
	}
}
