package qqapi

import (
	"encoding/base64"
	"encoding/json"

	"golang.org/x/oauth2"
	"strings"
	"testing"
)

// 富媒体两步走的报文形状。
//
// 这两条是 2026-10-01 生产事故换来的，两处都「看着完全没问题」：
//   1. Authorization 写死 Bearer  → 401 code=11241 参数格式错误
//   2. file_data 加 "base64://" 前缀 → 400 code=40011000 请求数据异常
//   3. file_info 走 botgo 的 []byte → 被 json 再 base64 一次，发出去的是
//      base64(base64(info))，平台认不出
// 三处都是拿真机真凭据打一遍才发现的，所以这里把字节钉死。

// authHeader 必须用 token 自带的方案名（QQBot），不能是 Bearer
func TestAuthHeaderUsesTokenType(t *testing.T) {
	if got := authHeader(&oauth2.Token{TokenType: "QQBot", AccessToken: "abc"}); got != "QQBot abc" {
		t.Errorf("got %q", got)
	}
	// scheme 缺失时也不能退回 Bearer —— 那个值平台根本不认
	if got := authHeader(&oauth2.Token{AccessToken: "abc"}); !strings.HasPrefix(got, "QQBot ") {
		t.Errorf("缺 TokenType 时应退回 QQBot，got %q", got)
	}
	if got := authHeader(&oauth2.Token{TokenType: "QQBot", AccessToken: "abc"}); strings.Contains(got, "Bearer") {
		t.Errorf("不该出现 Bearer：%q", got)
	}
}

// 上传报文的 file_data 必须是裸 base64
func TestUploadFileDataIsPlainBase64(t *testing.T) {
	raw := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	buf, _ := json.Marshal(uploadPayload(raw))
	body := string(buf)

	want := base64.StdEncoding.EncodeToString(raw)
	if !strings.Contains(body, `"file_data":"`+want+`"`) {
		t.Errorf("file_data 应是裸 base64 %q：%s", want, body)
	}
	for _, bad := range []string{"base64://", "data:image"} {
		if strings.Contains(body, bad) {
			t.Errorf("file_data 不能带 %s 前缀（真机实测 400）：%s", bad, body)
		}
	}
	// srv_send_msg 必须为 false：true 会占用主动消息额度，而那条通道已下线
	if !strings.Contains(body, `"srv_send_msg":false`) {
		t.Errorf("srv_send_msg 应为 false：%s", body)
	}
}

// 发消息时 file_info 必须原样透传，不能被 json 再 base64 一次
func TestRichMediaFileInfoNotDoubleEncoded(t *testing.T) {
	const info = "thRvYgj11pt3c+DNvWPJChZF/CRWFaJcJ3jI2deBactaC5kayCzw5uIS"
	buf, _ := json.Marshal(mediaMsg{
		MsgType: msgTypeRichMedia,
		Media:   mediaInner{FileInfo: info},
		MsgID:   "MID", MsgSeq: 2,
	})
	body := string(buf)

	if !strings.Contains(body, `"file_info":"`+info+`"`) {
		t.Errorf("file_info 必须原样透传，实际：%s", body)
	}
	// 这正是原先用 dto.MediaInfo{FileInfo: []byte(info)} 的结果
	doubled := base64.StdEncoding.EncodeToString([]byte(info))
	if strings.Contains(body, doubled) {
		t.Error("file_info 被重复 base64 了")
	}
	// 挂被动回复的两个字段也必须在，缺了就发成主动消息（通道已下线）
	for _, want := range []string{`"msg_type":7`, `"msg_id":"MID"`, `"msg_seq":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s：%s", want, body)
		}
	}
}
