package brain

import (
	"strings"
	"testing"
)

// 引用一条**纯图片**消息时，整段引用信息（尤其原作者是谁）必须留下来。
//
// 2026-10-02 生产实况：群友甲引用别人 23:53 发的图问「这谁？」，
// 机器人答「你自己发的你问我？」。日志里那条群消息**没有「引用」字段**，
// 因为 extractQuoted 返回空文本，而这里的门槛正是 `if ev.Quoted != ""`。
// 原作者是谁，机器人从头到尾不知道。
//
// 这几个测试直接调真实的登记逻辑，不复刻。

// 纯图片引用（正文为空但有原作者）不能因为 Quoted 为空就丢掉整段。
func TestQuotedAuthorSurvivesEmptyQuotedText(t *testing.T) {
	st := &groupState{}
	registerQuoted(st, "", "绝对无法被绿之人", "openid-abs")

	if st.quotedAuthor != "绝对无法被绿之人" {
		t.Fatalf("纯图片引用时原作者必须留下（这是「这谁？」的答案），got %q", st.quotedAuthor)
	}
	if st.quotedAuthorOpen != "openid-abs" {
		t.Errorf("原作者 openid 必须留下，got %q", st.quotedAuthorOpen)
	}
}

// 有引用文本时行为不变。
func TestQuotedAuthorRecordedWithText(t *testing.T) {
	st := &groupState{}
	registerQuoted(st, "你说的那个我懂", "张三", "openid-zhang")

	if st.quoted != "你说的那个我懂" {
		t.Errorf("引用文本应留下，got %q", st.quoted)
	}
	if st.quotedAuthor != "张三" {
		t.Errorf("原作者应留下，got %q", st.quotedAuthor)
	}
}

// 真的没有引用时才什么都不记。
func TestNoQuotedRecordsNothing(t *testing.T) {
	st := &groupState{}
	registerQuoted(st, "", "", "")

	if st.quoted != "" || st.quotedAuthor != "" || st.quotedAuthorOpen != "" {
		t.Errorf("没有引用就不该留下任何残留，got %q / %q / %q",
			st.quoted, st.quotedAuthor, st.quotedAuthorOpen)
	}
}

// 提示词里那句「谁引用了谁的什么」必须出现原作者。
// 模型就是靠这句话知道该说「这是绝对发的」而不是「你自己发的」。
func TestQuoteNoteNamesQuotedAuthor(t *testing.T) {
	st := &groupState{}
	registerQuoted(st, "[图片]", "绝对无法被绿之人", "openid-abs")

	note := quoteNote("群友甲", st.quotedAuthor, st.quoted)

	if note == "" {
		t.Fatal("有引用就必须给出说明")
	}
	if !strings.Contains(note, "绝对无法被绿之人") {
		t.Errorf("说明里必须点名被引用者，否则模型会以为图是引用者自己发的，got: %s", note)
	}
	if !strings.Contains(note, "群友甲") {
		t.Errorf("说明里必须点名引用者，got: %s", note)
	}
}

// 没有引用时不该生成说明。
func TestQuoteNoteEmptyWhenNoQuote(t *testing.T) {
	if note := quoteNote("群友甲", "", ""); note != "" {
		t.Errorf("没有引用就不该有说明，got %q", note)
	}
}

// TestQuoteNoteNeverTeachesSelfExposure 认不出原作者时，说明里**不许出现自曝措辞**。
//
// 2026-10-02 实况：平台没给 author，旧说明写的是「平台没告诉我这条是谁发的」——
// 那等于在教模型自曝，它照着答了「你引用的那条我这儿看不见，截图发出来」。
// 群里每个人都能看见那条引用，模型看不见是它自己的事，不该转述。
func TestQuoteNoteNeverTeachesSelfExposure(t *testing.T) {
	note := quoteNote("群友甲", "", "（表情包）")

	// 内容要如实给出去
	if !strings.Contains(note, "表情包") {
		t.Errorf("引用内容应如实给模型，got: %s", note)
	}
	// 但不能出现任何「我方能力受限」的说法
	for _, bad := range []string{"没告诉我", "平台", "看不到", "不知道", "截图", "无法"} {
		if strings.Contains(note, bad) {
			t.Errorf("说明里不该出现 %q——那是在教模型自曝，got: %s", bad, note)
		}
	}
}

// 有原作者时措辞里必须有他，且同样不能有自曝措辞。
func TestQuoteNoteWithAuthorNoSelfExposure(t *testing.T) {
	note := quoteNote("群友甲", "绝对无法被绿之人", "（一张图）")

	if !strings.Contains(note, "绝对无法被绿之人") {
		t.Errorf("必须点名原作者，got: %s", note)
	}
	for _, bad := range []string{"平台", "看不到", "截图", "没告诉我"} {
		if strings.Contains(note, bad) {
			t.Errorf("说明里不该出现 %q，got: %s", bad, note)
		}
	}
}