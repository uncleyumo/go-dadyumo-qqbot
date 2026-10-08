// Package qqapi 封装 botgo OpenAPI：token 自动刷新、群消息发送、被动/主动通道降级与限流。
package qqapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	botgo "github.com/tencent-connect/botgo"
	"github.com/tencent-connect/botgo/dto"
	"github.com/tencent-connect/botgo/openapi"
	"github.com/tencent-connect/botgo/token"
	"golang.org/x/oauth2"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

// Client QQ OpenAPI 客户端
type Client struct {
	store   *config.Store
	api     openapi.OpenAPI
	anchors *AnchorPool
	quotes  *QuoteIndex
	limiter *tokenBucket

	// ts 持有 token source 是为了发富媒体：botgo 的 openapi 接口只封装了
	// 发消息，没封「上传富媒体拿 file_info」，而发图必须先上传。
	ts      oauth2.TokenSource
	appID   string
	sandbox bool
	httpc   *http.Client
}

// New 创建客户端并启动 token 自动刷新。
// 注意：这里不使用 botgo 的 token.StartRefreshAccessToken —— 它在连续失败 10 次后会 panic，
// 且首次失败时不会启动后台刷新；对常驻服务而言这等同于「凭证写错或断网就进程崩溃」。
func New(ctx context.Context, store *config.Store) (*Client, error) {
	// 必须早于任何 botgo 对象创建：SDK 的日志是包级变量，装晚了这次启动的
	// 凭据就已经打进 journald 了。
	installBotgoLogFilter()

	cfg := store.Get()
	cred := &token.QQBotCredentials{AppID: cfg.QQ.AppID, AppSecret: cfg.QQ.AppSecret}
	ts := token.NewQQBotTokenSource(cred)
	// 首次获取失败不致命：后台自愈重试，服务照常起来
	if _, err := ts.Token(); err != nil {
		logx.Warn("首次获取 access token 失败，转入后台重试（服务继续启动）",
			"err", err.Error(), "appid", cfg.QQ.AppID)
	}
	go refreshLoop(ctx, ts)
	var api openapi.OpenAPI
	if cfg.QQ.Sandbox {
		api = botgo.NewSandboxOpenAPI(cfg.QQ.AppID, ts)
	} else {
		api = botgo.NewOpenAPI(cfg.QQ.AppID, ts)
	}
	timeout := time.Duration(cfg.QQ.SendTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	api = api.WithTimeout(timeout)
	logx.Info("QQ OpenAPI 就绪", "appid", cfg.QQ.AppID, "sandbox", cfg.QQ.Sandbox, "timeout", timeout.String())
	return &Client{
		store:   store,
		api:     api,
		anchors: NewAnchorPool(20),
		quotes:  NewQuoteIndex(0),
		limiter: newTokenBucket(20, time.Minute), // 保守：全局 20 条/分钟
		ts:      ts,
		appID:   cfg.QQ.AppID,
		sandbox: cfg.QQ.Sandbox,
		httpc:   &http.Client{Timeout: timeout + 10*time.Second},
	}, nil
}

// refreshLoop 后台刷新 access token：失败只记日志并按指数退避重试，绝不 panic、绝不退出进程。
// 正常情况在 token 过期前 15 分钟刷新一次；失败时退避 5s→10s→…→最多 5 分钟。
func refreshLoop(ctx context.Context, ts oauth2.TokenSource) {
	backoff := 5 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		tk, err := ts.Token()
		if err != nil {
			logx.Warn("刷新 access token 失败，将重试", "err", err.Error(), "next", backoff.String())
		} else {
			if backoff > 5*time.Second {
				logx.Info("access token 恢复正常")
			}
			backoff = 5 * time.Second
			wait := time.Until(tk.Expiry) - 15*time.Minute
			if wait < 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Anchors 暴露锚点池，供上层在收到消息时登记
func (c *Client) Anchors() *AnchorPool { return c.anchors }

// Quotes 暴露引用索引，供上层在收到消息时登记、在遇到引用时反查。
// 自己发出去的消息由本包在发送成功后自动登记，上层不用管。
func (c *Client) Quotes() *QuoteIndex { return c.quotes }

// SendGroup 向群发送文本，只走被动回复（挂 msg_id）。
func (c *Client) SendGroup(ctx context.Context, groupOpenID, content string) error {
	return c.SendGroupTo(ctx, groupOpenID, content, "")
}

// SendGroupTo 向群发送文本，并尽量把这条回复挂到 replyToOpenID 说的话下面。
//
// 曾经这里会在没有锚点时降级成「主动消息」（不挂 msg_id 凭空发一条）。
// 2025-04 起腾讯已下线 QQ 机器人的主动消息推送，硬发只会被 40034105 拒掉，
// 连带着冷场找话、后续句免 @ 这些玩法一起失效，所以主动通道整个拆掉了。
// 现在没锚点就直接报错让上层跳过，不再做无谓的尝试。
//
// replyToOpenID 为空时退化成旧行为：挂到群里最新那条消息下。
func (c *Client) SendGroupTo(ctx context.Context, groupOpenID, content, replyToOpenID string) error {
	return c.sendGroup(ctx, groupOpenID, content, replyToOpenID, false)
}

// SendGroupQuote 与 SendGroupTo 相同，但这条挂一个真正的引用气泡
// （message_reference）——引用的是 replyToOpenID 那条的锚点。
//
// 什么时候该引用由 brain 那边的模型逐块决定（Block.Q），不由这一层决定：
// 2026-10-05 之前是「平台给了 refIdx 就用」，而平台每条消息都给，
// 结果每条回复都套着引用卡片——真人聊天里没有这种机器人。
func (c *Client) SendGroupQuote(ctx context.Context, groupOpenID, content, replyToOpenID string) error {
	return c.sendGroup(ctx, groupOpenID, content, replyToOpenID, true)
}

func (c *Client) sendGroup(ctx context.Context, groupOpenID, content, replyToOpenID string, quote bool) error {
	if strings.TrimSpace(content) == "" || groupOpenID == "" {
		return errors.New("空的发送目标或内容")
	}
	cfg := c.store.Get()

	// 先看有没有锚点，再扣限流配额：发不出去的消息不该白吃一分钟的额度。
	// PickAndReserve 同时完成了占用与取 seq，所以必须早于任何可能失败的发送动作。
	if cfg.QQ.PreferPassive {
		msgID, refIdx, seq, ok, matched := c.anchors.PickAndReserve(groupOpenID, replyToOpenID)
		// 模型没开口要引用就把 refIdx 丢掉：它不填进请求，消息照发，只是不带气泡。
		if !quote {
			refIdx = ""
		}
		// 挂靠到了**别人**的消息上（指定的人已经不在存活锚点里）。
		// 这时绝不能带引用气泡：气泡里会出现一个陌生人，而内容明显是在回我
		// 指定的那个人——群里人一眼就看出挂错了。宁可不带气泡。
		if !matched {
			refIdx = ""
		}
		if ok {
			if !c.limiter.allow() {
				c.anchors.Release(groupOpenID, msgID)
				return errors.New("发送限流：本分钟配额已用尽")
			}
			sentRefIdx, err := c.post(ctx, groupOpenID, content, msgID, refIdx, seq)
			if err == nil {
				// 把刚发出去的这条记进引用索引：平台不会把自己的消息推回给自己，
				// 不记的话「有人引用了老爹说过的话」永远反查不到（2026-10-08 实测
				// 这一路能多覆盖当天引用的 14/81）。
				//
				// 正文去掉 @ 标签再记：模型看到 `<qqbot-at-user id="…"/>`
				// 认不出是谁，还可能学着写那串乱码（agent 侧对收到的消息
				// 也做同样的清洗，两边得一致）。
				c.quotes.Add(groupOpenID, sentRefIdx, QuoteSrc{
					OpenID: cfg.QQ.SelfOpenID,
					Name:   cfg.Persona.Name,
					Text:   strings.TrimSpace(atTagInText.ReplaceAllString(content, "")),
				})
				logx.Debug("群消息已发送（被动）", "group", groupOpenID, "seq", seq, "挂给", replyToOpenID,
					"引用", refIdx != "", "本条引用id", sentRefIdx != "")
				return nil
			}
			// 只还额度，不回滚 seq——见 anchor.go 里 seq 只增不减的说明
			c.anchors.Release(groupOpenID, msgID)
			logx.Warn("被动消息发送失败", "group", groupOpenID, "err", err.Error())
			return err
		}
	}
	// 没有可挂载的被动锚点：5 分钟内没人在这个群说过话（或锚点用满 5 次）。
	// 主动发不出去，只能这轮不说话。
	return errors.New("无可用被动锚点（主动消息已下线，无法凭空发送）")
}

// post 发一条被动回复。
//
// **带 @ 的那条走 markdown 通道（msg_type=2），不带 @ 的走纯文本（msg_type=0）。**
//
// 2026-10-05 生产实测：同一个 `<qqbot-at-user id="…" />` 标签，
// 放纯文本 content 里 HTTP 200 OK，但客户端**原样打印成字面量**
// （那串 hex 还被自动识别成链接），完全不渲染成 @；
// 放 markdown.content 里才真的艾特到人（社区 ala-mobile-tool 五轮实测矩阵同结论，
// 同症状同标签，唯一差别就是 msg_type）。
// 官方文档写着 msg_type=0 也支持——这句与实现对不上，别再照它写。
//
// 官方明确「传了 markdown 后 content 字段必须为空」，两者互斥，
// 所以 buildGroupMessage 里把 content 清空。Content 有 omitempty，不会残留。
//
// 只给带 @ 的消息换通道：markdown 消息在群里渲染成卡片样式，
// 让机器人所有发言都变成卡片是很大的观感变更，而绝大多数消息根本不 @ 人。
//
// markdown 通道无需申请（官方：群聊场景自定义 Markdown 已对所有机器人开放），
// 但富媒体/模板权限那套报错是存在的，真发不出去时日志里会看到
// 「被动消息发送失败」，不会静默丢消息。
//
// msgID 是被动回复必需的锚点；refIdx 非空时**额外**挂一个 message_reference，
// 让这条以真正的引用气泡展示。两者可以共存（官方请求示例就是并存的），
// 所以加引用不必牺牲被动回复资格。
//
// refIdx 为空时整个 MessageReference 字段为 nil，序列化后不出现——
// 平台不一定每条消息都给 refIdx，不该因为缺它就发不出去。
//
// 返回值是本条消息**自己**的引用 id（平台在响应的 ext_info.ref_idx 里给）。
// 它唯一的用途是进 QuoteIndex：别人以后引用这条时，回调里只带这个 id。
func (c *Client) post(ctx context.Context, groupOpenID, content, msgID, refIdx string, seq uint32) (string, error) {
	msg := buildGroupMessage(content, msgID, refIdx, seq)
	return c.postGroupMessage(ctx, groupOpenID, msg)
}

// postGroupMessage 自己发 POST /v2/groups/{id}/messages，只为拿到响应里的
// ext_info.ref_idx。
//
// 为什么不用 botgo 的 api.PostGroupMessage：它的 dto.Message 里**没有**
// ext_info 字段（dto.MessageScene 也只有 source/callback_data，没有 ext），
// 响应体整个被丢掉，而我们正需要这一个字段。botgo 没有留任何拿原始响应体的口子
// （options.Option 只能改 URL 和 hidetip），所以只能自己发这一条。
//
// 请求形态逐项对着 botgo 的 setupClient 抄，**少一样都可能被平台拒**：
//
//   - Authorization 用 QQBot 方案（botgo 是 SetAuthScheme(tk.TokenType)），
//     不是 Bearer；媒体上传那边写死 Bearer 会被 401 code=11241 顶回来。
//   - X-Union-Appid 填 appID（botgo 无条件带这个头）。
//   - body 就是 dto.MessageToCreate 的 JSON，和 botgo 序列化出来的一模一样。
//
// 出错时的返回与 botgo 一致（错误信息里带平台 code），调用方的
// 「失败就 Release 锚点」逻辑不用改。
func (c *Client) postGroupMessage(ctx context.Context, groupOpenID string, msg *dto.MessageToCreate) (string, error) {
	buf, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	tk, err := c.ts.Token()
	if err != nil {
		return "", fmt.Errorf("取 access token 失败: %w", err)
	}
	url := fmt.Sprintf("%s/v2/groups/%s/messages", c.apiBase(), groupOpenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(tk))
	req.Header.Set("X-Union-Appid", c.appID)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送群消息失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		ExtInfo struct {
			RefIdx string `json:"ref_idx"`
		} `json:"ext_info"`
		Message string `json:"message"`
		ErrCode int    `json:"err_code"`
	}
	// 解析失败不致命：2xx 时只是拿不到 ref_idx（退化成改动前的行为），
	// 非 2xx 时下面那条错误信息里的 body 摘要仍然有用。
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("发送群消息 HTTP %d code=%d: %s", resp.StatusCode, out.ErrCode,
			strings.TrimSpace(truncateStr(string(raw), 200)))
	}
	return out.ExtInfo.RefIdx, nil
}

// buildGroupMessage 拼一条被动回复。
//
// 抽成独立函数是为了能被测试直接验序列化结果——botgo 的 apiBase 写死在
// SDK 里，测试没法指向 httptest 服务器，而这里恰恰是最容易出错的一处：
// MessageReference 只要给了非 nil 指针，MessageID 没有 omitempty，
// 空 refIdx 也会被序列化成 {"message_id":""} 送出去，平台会拒。
func buildGroupMessage(content, msgID, refIdx string, seq uint32) *dto.MessageToCreate {
	msg := &dto.MessageToCreate{
		Content: content,
		MsgType: dto.TextMsg,
		MsgID:   msgID,
		MsgSeq:  seq,
	}
	if refIdx != "" {
		msg.MessageReference = &dto.MessageReference{MessageID: refIdx}
	}
	if atTagInText.MatchString(content) {
		msg.Content = ""
		msg.MsgType = dto.MarkdownMsg
		msg.Markdown = &dto.Markdown{Content: flattenNewlines(content)}
	}
	return msg
}

// atTagInText 匹配出口渲染出来的 @ 内嵌标签。
//
// brain 包渲染它、qqapi 包只是在这里认出「这条要不要换通道」，
// 所以这里匹配的是字面量而不是去 import brain——两个包互不依赖。
var atTagInText = regexp.MustCompile(`<qqbot-at-user\s+id="[^"]*"\s*/?>`)

// flattenNewlines 把换行换成空格。
//
// markdown 通道的内容里有换行会被平台拒（40034009 markdown参数有换行符），
// 而正常路径上本来就没有换行（brain 那边按句子切开了），所以这只是兜底：
// 真出现换行时宁可压成一行，也不至于让整条消息发不出去。
func flattenNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
}

// SendC2C 向用户单聊发送文本
func (c *Client) SendC2C(ctx context.Context, userOpenID, content string) error {
	if userOpenID == "" || content == "" {
		return errors.New("空的发送目标或内容")
	}
	_, err := c.api.PostC2CMessage(ctx, userOpenID, &dto.MessageToCreate{
		Content: content,
		MsgType: dto.TextMsg,
	})
	return err
}

// tokenBucket 简易令牌桶
type tokenBucket struct {
	mu      sync.Mutex
	cap     int
	tokens  int
	window  time.Duration
	resetAt time.Time
}

func newTokenBucket(cap int, window time.Duration) *tokenBucket {
	return &tokenBucket{cap: cap, tokens: cap, window: window, resetAt: time.Now().Add(window)}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.After(b.resetAt) {
		b.tokens = b.cap
		b.resetAt = now.Add(b.window)
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
