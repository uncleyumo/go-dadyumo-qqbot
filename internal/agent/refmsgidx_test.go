package agent

import (
	"testing"

	"dadyumo/internal/webhook"
)

// refMsgIdxOf 取「我引用了哪一条」，refIdxOf 取「我是哪一条」。
//
// 生产里这两项**经常同时出现**——一条引用别人的消息，ext 长这样
// （2026-10-08 原文，只截前缀）：
//
//	["ref_msg_idx=REFIDX_h7ef7rBg…", "msg_idx=REFIDX_AwWGWquU…", "auth_token=…"]
//
// 认错了后果不对称：refIdxOf 认错，发送层会拿「我引用了谁」去填
// message_reference，机器人就引用到自己头上；refMsgIdxOf 认错，
// 反查会查到「这条消息自己」而不是被引用的那条。
func TestRefMsgIdxOfIsNotRefIdxOf(t *testing.T) {
	ext := []string{
		"ref_msg_idx=REFIDX_quoted",
		"msg_idx=REFIDX_self",
		"auth_token=X-qanh",
	}
	scene := &webhook.MessageScene{Ext: ext}

	if got := refMsgIdxOf(scene); got != "REFIDX_quoted" {
		t.Errorf("应取 ref_msg_idx，实际 %q", got)
	}
	if got := refIdxOf(scene); got != "REFIDX_self" {
		t.Errorf("应取 msg_idx，实际 %q——取成 ref_msg_idx 会让机器人引用到自己头上", got)
	}
}

// 只有 msg_idx（没人引用、也没引用别人）时，refMsgIdxOf 必须返回空。
// 返回非空会让 resolveQuoted 以为这是条引用，给普通消息硬塞一段占位。
func TestRefMsgIdxOfEmptyWhenNoQuote(t *testing.T) {
	scene := &webhook.MessageScene{Ext: []string{"msg_idx=REFIDX_self", "auth_token=X-qanh"}}

	if got := refMsgIdxOf(scene); got != "" {
		t.Errorf("这条没引用别人，应返回空，实际 %q", got)
	}
}

// 只有 ref_msg_idx（机器人发的引用消息就是这种：连 msg_elements 都没有）。
func TestRefMsgIdxOfOnlyRef(t *testing.T) {
	scene := &webhook.MessageScene{Ext: []string{"ref_msg_idx=REFIDX_only", "auth_token=X-qanh"}}

	if got := refMsgIdxOf(scene); got != "REFIDX_only" {
		t.Errorf("应取 ref_msg_idx，实际 %q", got)
	}
	// 这一条没有 msg_idx，refIdxOf 只能返回空——它意味着「别人引用不了这条」，
	// 不是错误。
	if got := refIdxOf(scene); got != "" {
		t.Errorf("没有 msg_idx 时应返回空，实际 %q", got)
	}
}

// nil 不 panic：这条路径每条群消息都会跑。
func TestRefMsgIdxOfNilScene(t *testing.T) {
	if got := refMsgIdxOf(nil); got != "" {
		t.Errorf("scene 为 nil 应返回空串，实际 %q", got)
	}
}
