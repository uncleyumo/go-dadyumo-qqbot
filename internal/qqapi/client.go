// Package qqapi 封装 botgo OpenAPI：token 自动刷新、群消息发送、被动/主动通道降级与限流。
package qqapi

import (
	"context"
	"errors"
	"net/http"
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
	if strings.TrimSpace(content) == "" || groupOpenID == "" {
		return errors.New("空的发送目标或内容")
	}
	cfg := c.store.Get()

	// 先看有没有锚点，再扣限流配额：发不出去的消息不该白吃一分钟的额度。
	// PickAndReserve 同时完成了占用与取 seq，所以必须早于任何可能失败的发送动作。
	if cfg.QQ.PreferPassive {
		msgID, seq, ok := c.anchors.PickAndReserve(groupOpenID, replyToOpenID)
		if ok {
			if !c.limiter.allow() {
				c.anchors.Release(groupOpenID, msgID)
				return errors.New("发送限流：本分钟配额已用尽")
			}
			err := c.post(ctx, groupOpenID, content, msgID, seq)
			if err == nil {
				logx.Debug("群消息已发送（被动）", "group", groupOpenID, "seq", seq, "挂给", replyToOpenID)
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

func (c *Client) post(ctx context.Context, groupOpenID, content, msgID string, seq uint32) error {
	msg := &dto.MessageToCreate{
		Content: content,
		MsgType: dto.TextMsg,
		MsgID:   msgID,
		MsgSeq:  seq,
	}
	_, err := c.api.PostGroupMessage(ctx, groupOpenID, msg)
	return err
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
