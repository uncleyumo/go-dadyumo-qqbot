package qqapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencent-connect/botgo/dto"
)

// 带 @ 的消息必须换 markdown 通道（msg_type=2）。
//
// 2026-10-05 生产实测：同一个 <qqbot-at-user> 标签放纯文本 content 里，
// HTTP 200 OK 但客户端原样打印成字面量；放 markdown.content 里才真的渲染
// 成 @。官方文档写「msg_type=0 也支持」，那句与实现对不上。
func TestAtMessageGoesThroughMarkdownChannel(t *testing.T) {
	body := `<qqbot-at-user id="OID-老王" /> 出来看戏`
	msg := buildGroupMessage(body, "MID", "", 1)

	if msg.MsgType != dto.MarkdownMsg {
		t.Fatalf("带 @ 的消息应走 markdown，实际 msg_type=%d", msg.MsgType)
	}
	if msg.Markdown == nil || msg.Markdown.Content != body {
		t.Fatalf("标签应原样放进 markdown.content，实际 %+v", msg.Markdown)
	}
	// 官方明确「传了 markdown 后 content 字段必须为空」，两者互斥
	if msg.Content != "" {
		t.Errorf("切了 markdown 通道后 content 必须为空，实际 %q", msg.Content)
	}
}

// content 的 omitempty：空了就该整个字段不出现在 JSON 里，
// 而不是发一个空字符串过去（平台的校验比看上去严）。
//
// 注意只能看**顶层**——markdown 对象自己也有一个 content 字段。
func TestMarkdownMessageSerializesWithoutContent(t *testing.T) {
	msg := buildGroupMessage(`<qqbot-at-user id="x" /> 嗨`, "MID", "", 1)
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["content"]; ok {
		t.Errorf("markdown 通道下顶层不该有 content 字段：%s", raw)
	}
	if _, ok := top["markdown"]; !ok {
		t.Fatalf("顶层应该有 markdown：%s", raw)
	}
}

// 不带 @ 的消息不受影响，仍然走纯文本。
//
// markdown 在群里渲染成卡片样式，让机器人所有发言都变成卡片是很大的观感变更，
// 而绝大多数消息根本不 @ 人——所以只给真带标签的那条换通道。
func TestPlainMessageStaysText(t *testing.T) {
	msg := buildGroupMessage("就一句话", "MID", "", 1)
	if msg.MsgType != dto.TextMsg {
		t.Fatalf("普通消息应走纯文本，实际 msg_type=%d", msg.MsgType)
	}
	if msg.Markdown != nil {
		t.Errorf("普通消息不该带 markdown：%+v", msg.Markdown)
	}
	if msg.Content != "就一句话" {
		t.Errorf("内容应留在 content，实际 %q", msg.Content)
	}
}

// @ 与引用可以共存：两者是不同的字段，不是二选一。
func TestAtAndQuoteCoexist(t *testing.T) {
	msg := buildGroupMessage(`<qqbot-at-user id="x" /> 说话`, "MID", "REFIDX_abc", 2)
	if msg.MessageReference == nil || msg.MessageReference.MessageID != "REFIDX_abc" {
		t.Fatalf("引用不该因为切了 markdown 通道丢掉：%+v", msg.MessageReference)
	}
	if msg.MsgType != dto.MarkdownMsg {
		t.Fatalf("带 @ 仍应是 markdown，实际 %d", msg.MsgType)
	}
}

// markdown 内容里的换行会被平台拒（40034009），压成空格。
//
// 正常路径上 brain 那边已经按句子切开、不该有换行；这是兜底，
// 真出现换行时宁可压成一行，也不要整条消息发不出去。
func TestMarkdownContentHasNoNewline(t *testing.T) {
	msg := buildGroupMessage("<qqbot-at-user id=\"x\" /> 第一行\n第二行", "MID", "", 1)
	if msg.Markdown == nil {
		t.Fatal("应是 markdown 消息")
	}
	if strings.ContainsAny(msg.Markdown.Content, "\r\n") {
		t.Errorf("markdown 内容里不该有换行，实际 %q", msg.Markdown.Content)
	}
}
