package qqapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"strings"

	"github.com/tencent-connect/botgo/token"

	"dadyumo/internal/logx"
)

// 富媒体（表情包）发送。
//
// 平台侧是**两步**，官方文档「富媒体消息概述」写得很清楚：
//
//  1. POST /v2/groups/{group_openid}/files  上传图片，拿到 file_info
//  2. POST /v2/groups/{group_openid}/messages  带 msg_type=7 + media.file_info 发出
//
// 关键：**第二步走的是普通发消息端点，带 msg_id / msg_seq**。
// 所以图片和文字一样能挂被动回复（回复给某人 5 次的额度也是共用的），
// 而 botgo 自带的 dto.RichMediaMessage 是直发 /files 的另一条路——
// 那个不带 MsgID，挂不上被动回复，等于主动消息（2025-04 已下线，发不出去）。
// 因此这里不用它，而是拿 file_info 之后走 MessageToCreate。
//
// file_info 是**临时**的（文档明说「多媒体文件不能复用」），
// 所以每次发送都要重新上传，MinIO 那边存的是图片本体。

// uploadPayload 组上传报文。单独抽出来是为了能直接断言它，
// 不必起 HTTP 服务就能把 file_data 的形状钉死。
func uploadPayload(data []byte) map[string]any {
	return map[string]any{
		"file_type":    1, // 1 = 图片
		"file_data":    base64Encode(data),
		"srv_send_msg": false,
	}
}

// 上传返回。file_info 上游给的是字符串，包在 JSON 里。
type mediaUploadResp struct {
	FileInfo string `json:"file_info"`
	ErrMsg   string `json:"message"`
	ErrCode  int    `json:"code"`
	// 某些实现把 file_info 放在嵌套对象里
	FileData struct {
		FileInfo string `json:"file_info"`
	} `json:"file_data"`
}

func (c *Client) apiBase() string {
	if c.sandbox {
		return "https://sandbox.api.sgroup.qq.com"
	}
	return "https://api.sgroup.qq.com"
}

// uploadImage 把图片字节交给 QQ 换取 file_info。
//
// srv_send_msg 传 false：这一步只上传、不发送。
// 直接设 true 会占用主动消息额度，而主动消息通道已经下线。
func (c *Client) uploadImage(ctx context.Context, groupOpenID string, data []byte, mime string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("空图片")
	}
	if mime == "" {
		mime = "image/jpeg"
	}
	tk, err := c.ts.Token()
	if err != nil {
		return "", fmt.Errorf("取 access token 失败: %w", err)
	}

	// 平台接受 URL 或 base64。走 base64 是因为图片本来就在我们手里，
	// 让平台再拉一次 MinIO 既是多余的往返，也多一个失败点。
	//
	// file_data 必须是**裸 base64**，不能带 "base64://" 前缀。
	// 2026-10-01 实测（真机真凭据，同一张图只换前缀）：
	//   "base64://xxx" → 400 请求数据异常 code=40011000
	//   "data:image/jpeg;base64,xxx" → 同样 400
	//   "xxx" → 200，返回 file_info 与 ttl=86400
	// 官方示例里那个前缀写法是最常见的误传。
	payload := uploadPayload(data)
	buf, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/v2/groups/%s/files", c.apiBase(), groupOpenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(tk))
	req.Header.Set("X-Union-Appid", c.appID)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传富媒体失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out mediaUploadResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("上传响应解析失败 HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(truncateStr(string(raw), 200)))
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("上传富媒体 HTTP %d code=%d: %s",
			resp.StatusCode, out.ErrCode, out.ErrMsg)
	}
	info := out.FileInfo
	if info == "" {
		info = out.FileData.FileInfo
	}
	if info == "" {
		return "", fmt.Errorf("上传富媒体没返回 file_info: %s",
			strings.TrimSpace(truncateStr(string(raw), 200)))
	}
	return info, nil
}

// SendImage 发一张图，挂在 replyToOpenID 的消息下面。
//
// data 是图片**字节**而不是 URL：图片本体存在 MinIO，要发给 QQ 必须先下载
// 再上传成 file_info。传 URL 的话又变成让 QQ 去拉 MinIO——多一个失败点，
// 而且 MinIO 若是内网地址 QQ 根本拉不到。
//
// 流程与 SendGroupTo 完全一致：先取锚点（被动回复挂载点）、再扣限流配额，
// 最后发送。**图片和文字共用同一份「同一条消息最多回 5 次」的额度**，
// 所以混发时必须算总账——那是 brain 层 sendBudget 的事。
func (c *Client) SendImage(ctx context.Context, groupOpenID string, data []byte, mime, replyToOpenID string) error {
	if strings.TrimSpace(groupOpenID) == "" || len(data) == 0 {
		return errors.New("空的发送目标或图片")
	}
	cfg := c.store.Get()

	// 与 SendGroupTo 同序：锚点必须早于任何可能失败的动作取，
	// 否则并发下会拿到同一个 seq，平台按 (msg_id, msg_seq) 判重会拒掉。
	//
	// refIdx 丢弃不用：这条是手搓报文（见 postRichMedia 的注释，botgo 的
	// RichMediaMessage 不带 MsgID，挂不上被动回复），而表情包 @ 人没有意义，
	// 不值得为它再往这份手写 JSON 里加一个字段。
	if cfg.QQ.PreferPassive {
		msgID, _, seq, ok, _ := c.anchors.PickAndReserve(groupOpenID, replyToOpenID)
		if ok {
			if !c.limiter.allow() {
				c.anchors.Release(groupOpenID, msgID)
				return errors.New("发送限流：本分钟配额已用尽")
			}
			err := c.postImage(ctx, groupOpenID, data, mime, msgID, seq)
			if err == nil {
				logx.Debug("群图片已发送（被动）", "group", groupOpenID, "seq", seq, "挂给", replyToOpenID)
				return nil
			}
			// 和文字一样：只还额度，不回滚 seq
			c.anchors.Release(groupOpenID, msgID)
			logx.Warn("被动图片发送失败", "group", groupOpenID, "err", err.Error())
			return err
		}
	}
	return errors.New("无可用被动锚点（主动消息已下线，无法凭空发送）")
}

func (c *Client) postImage(ctx context.Context, groupOpenID string, data []byte, mime, msgID string, seq uint32) error {
	// 第一步：上传换 file_info
	info, err := c.uploadImage(ctx, groupOpenID, data, mime)
	if err != nil {
		return err
	}
	// 第二步：带 msg_type=7 + media.file_info 发消息
	return c.postRichMedia(ctx, groupOpenID, info, msgID, seq)
}

// mediaMsg 是发图片的报文。
//
// 为什么不用 botgo 的 dto.MessageToCreate：它的 MediaInfo.FileInfo 是 []byte，
// 而 encoding/json 把 []byte 序列化成 base64 字符串——于是平台返回的那串
// file_info 会被**再 base64 一次**发出去，平台认不出来。
// （已实测：[]byte(info) 出来的是 base64(base64(info))。）
// 富媒体这一步自己拼报文，文本消息仍走 botgo。
type mediaMsg struct {
	MsgType int        `json:"msg_type"`
	Media   mediaInner `json:"media"`
	MsgID   string     `json:"msg_id,omitempty"`
	MsgSeq  uint32     `json:"msg_seq,omitempty"`
}

type mediaInner struct {
	FileInfo string `json:"file_info"`
}

// msgTypeRichMedia 富媒体（表情包）
const msgTypeRichMedia = 7

func (c *Client) postRichMedia(ctx context.Context, groupOpenID, fileInfo, msgID string, seq uint32) error {
	tk, err := c.ts.Token()
	if err != nil {
		return fmt.Errorf("取 access token 失败: %w", err)
	}
	buf, _ := json.Marshal(mediaMsg{
		MsgType: msgTypeRichMedia,
		Media:   mediaInner{FileInfo: fileInfo},
		MsgID:   msgID,
		MsgSeq:  seq,
	})

	url := fmt.Sprintf("%s/v2/groups/%s/messages", c.apiBase(), groupOpenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(tk))
	req.Header.Set("X-Union-Appid", c.appID)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("发图片失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("发图片 HTTP %d: %s", resp.StatusCode,
			strings.TrimSpace(truncateStr(string(raw), 200)))
	}
	return nil
}

// authHeader 拼 Authorization。
//
// 方案名取 token 自带的 TokenType（QQBot），不能写死 Bearer——
// botgo 自己的 openapi 客户端也是 SetAuthScheme(tk.TokenType)。
// 写死 Bearer 的话富媒体上传直接 401 code=11241「请求头Authorization参数格式错误」。
func authHeader(tk *oauth2.Token) string {
	scheme := tk.TokenType
	if scheme == "" {
		scheme = token.TypeQQBot
	}
	return scheme + " " + tk.AccessToken
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
