package webhook

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OpCode 回调 op 类型
const (
	OpDispatch     = 0  // 事件推送
	OpHTTPCallback = 12 // HTTP 回调 ACK
	OpValidate     = 13 // 回调地址验证
)

// Payload 通用回调结构
type Payload struct {
	ID string          `json:"id"`
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int             `json:"s"`
	T  string          `json:"t"`
}

// ValidationReq op=13 回调地址验证请求
type ValidationReq struct {
	PlainToken string `json:"plain_token"`
	EventTs    string `json:"event_ts"`
}

// ValidationRsp op=13 响应
type ValidationRsp struct {
	PlainToken string `json:"plain_token"`
	Signature  string `json:"signature"`
}

// User 事件中的用户
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Bot          bool   `json:"bot"`
	UnionOpenID  string `json:"union_openid"`
	UserOpenID   string `json:"user_openid"`
	MemberOpenID string `json:"member_openid"`
	MemberRole   string `json:"member_role"` // member / admin / owner
}

// MessageScene 消息场景
type MessageScene struct {
	Source string   `json:"source"`
	Ext    []string `json:"ext"`
}

// Attachment 附件（图片/语音/视频/文件）
type Attachment struct {
	URL          string `json:"url"`
	FileName     string `json:"filename"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Size         int    `json:"size"`
	ContentType  string `json:"content_type"` // voice / image/jpeg / video/mp4 / file
	VoiceWavURL  string `json:"voice_wav_url"`
	ASRReferText string `json:"asr_refer_text"`
}

// MsgElement 消息元素（引用消息、聊天记录等）
type MsgElement struct {
	MsgIdx      string        `json:"msg_idx"`
	Author      *User         `json:"author"`
	MessageType int           `json:"message_type"`
	Content     string        `json:"content"`
	Attachments []*Attachment `json:"attachments"`
	MsgElements []*MsgElement `json:"msg_elements"`
}

// GroupMessage 群消息事件体（GROUP_AT_MESSAGE_CREATE / GROUP_MESSAGE_CREATE 通用）
type GroupMessage struct {
	ID           string        `json:"id"`
	Author       *User         `json:"author"`
	Content      string        `json:"content"`
	GroupOpenID  string        `json:"group_openid"`
	Timestamp    string        `json:"timestamp"`
	MessageType  int           `json:"message_type"`
	MessageScene *MessageScene `json:"message_scene"`
	Attachments  []*Attachment `json:"attachments"`
	Mentions     []*User       `json:"mentions"`
	MsgElements  []*MsgElement `json:"msg_elements"`
}

// C2CMessage 单聊消息事件体
type C2CMessage struct {
	ID          string        `json:"id"`
	Author      *User         `json:"author"`
	Content     string        `json:"content"`
	Timestamp   string        `json:"timestamp"`
	MessageType int           `json:"message_type"`
	Attachments []*Attachment `json:"attachments"`
}

// GroupRobotEvent 机器人被添加/移出群
//
// 注意 timestamp 用 FlexTimestamp 而不是 string：平台对这一族事件推的是 JSON number，
// 而 GroupMessage/C2CMessage 的同名字段推的确实是字符串——平台行为本身不一致。
// 早先这里声明成 string，导致四个机器人生命周期事件解析失败率 3/3，handler 一次都没跑过。
type GroupRobotEvent struct {
	Timestamp   FlexTimestamp `json:"timestamp"`
	GroupOpenID string        `json:"group_openid"`
	OpdID       string        `json:"opd_id"` // 操作者 openid
}

// FlexTimestamp 兼容平台 timestamp 字段的两种推送形式（JSON 字符串 / JSON number），
// 统一归一成 Unix 秒。字符串按 RFC3339 解析，解析不了再当纯数字秒处理。
type FlexTimestamp int64

// UnmarshalJSON 兼容解析：首字符是引号走字符串分支，否则走 number 分支
func (f *FlexTimestamp) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if s[0] == '"' {
		var raw string
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		return f.parseString(raw)
	}
	var num json.Number
	if err := json.Unmarshal(b, &num); err != nil {
		return fmt.Errorf("timestamp 既不是字符串也不是数字: %s", s)
	}
	n, err := num.Int64()
	if err != nil {
		// 平台偶尔会推浮点秒（如 1725442341.0），容忍一下
		fv, ferr := num.Float64()
		if ferr != nil {
			return fmt.Errorf("timestamp 数字无法解析: %s", s)
		}
		n = int64(fv)
	}
	*f = FlexTimestamp(n)
	return nil
}

func (f *FlexTimestamp) parseString(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*f = 0
		return nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		*f = FlexTimestamp(n)
		return nil
	}
	// RFC3339 / RFC3339Nano，带不带时区都试一遍
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if ts, err := time.Parse(layout, raw); err == nil {
			*f = FlexTimestamp(ts.Unix())
			return nil
		}
	}
	return fmt.Errorf("timestamp 字符串无法解析: %q", raw)
}

// Time 转成 time.Time，0 值返回零值时间
func (f FlexTimestamp) Time() time.Time {
	if f == 0 {
		return time.Time{}
	}
	return time.Unix(int64(f), 0)
}

// String 返回原始形态的可读时间，零值返回空串
func (f FlexTimestamp) String() string {
	if f == 0 {
		return ""
	}
	return f.Time().Format(time.RFC3339)
}

// EventType 事件名常量
//
// 只列真正接线了的事件。平台另外还推 FRIEND_ADD / FRIEND_DEL / C2C_MSG_RECEIVE /
// C2C_MSG_REJECT 四个事件，但仓库里从没有过消费方——与其留一组看着像支持、实则永远
// 落到「未识别事件」分支的常量，不如不列：真要接的时候加上 Handler 方法和 dispatch 分支即可。
const (
	EventGroupAtMessage = "GROUP_AT_MESSAGE_CREATE"
	EventGroupMessage   = "GROUP_MESSAGE_CREATE"
	EventC2CMessage     = "C2C_MESSAGE_CREATE"
	EventGroupAddRobot  = "GROUP_ADD_ROBOT"
	EventGroupDelRobot  = "GROUP_DEL_ROBOT"
	EventGroupMsgRecv   = "GROUP_MSG_RECEIVE" // 群主开启全量消息
	EventGroupMsgReject = "GROUP_MSG_REJECT"  // 群主关闭全量消息
)
