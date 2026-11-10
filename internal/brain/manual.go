package brain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
	"dadyumo/internal/takeover"
)

// 管理端手动代发。
//
// 与 AI 发言走完全相同的出口（e.sender / e.imgSender），所以被动锚点、
// 引用气泡、@ 渲染、平台限流这些规则天然一致，不需要另开一条发送链路。
// 这里只补三样 AI 路径没有的东西：
//   - 每群单飞锁：同一个群一次只允许一条手动消息，TryLock 失败返回 busy；
//   - client_msg_id 幂等：网络超时后前端重试不会发两条；
//   - 接管档位闸门：off 拒绝、window 必须有有效租约、always 直接放行。

// manualDedupeTTL 幂等缓存时长。前端每次输入变化生成新 id，
// 网络失败重试复用同一个 id，10 分钟内重试返回缓存结果。
const manualDedupeTTL = 10 * time.Minute

// manualDedupeState 幂等记录的状态。
type manualDedupeState string

const (
	manualInFlight manualDedupeState = "in_flight"
	manualDone     manualDedupeState = "done"
)

// manualDedupeEntry 是一条幂等记录。failed 不落表：直接删掉，允许重试。
type manualDedupeEntry struct {
	At     time.Time
	State  manualDedupeState
	Result ManualSendResult
}

// 手动发送的错误。Error() 文本就是管理端的 reason 字符串。
var (
	// ErrManualNotReady engine/sender/图片通道未装配。
	ErrManualNotReady = errors.New("not_initialized")
	// ErrManualPaused 全局总开关关闭，手动发送也拒绝。
	ErrManualPaused = errors.New("paused")
	// ErrManualLeaseRequired window 档下请求没带 lease_id。
	ErrManualLeaseRequired = errors.New("lease_required")
	// ErrManualBusy 同群已有发送在途，或同一个 client_msg_id 正在发送。
	ErrManualBusy = errors.New("busy")
	// ErrManualBadClientMsgID client_msg_id 缺失。必填是幂等的前提。
	ErrManualBadClientMsgID = errors.New("bad_client_msg_id")
	// ErrManualTextEmpty 清洗 @ 标签后正文为空。
	ErrManualTextEmpty = errors.New("text_empty")
	// ErrManualBadAt at_openids 里有不在该群成员表里的 openid。
	ErrManualBadAt = errors.New("bad_at")
	// ErrManualBadMeme 表情包 id 不存在或池子未启用。
	ErrManualBadMeme = errors.New("bad_meme")
	// ErrManualTimeout 取图/发送被调用方 ctx 的超时或取消打断。
	// 与 bad_meme 分开：图可能已经取到甚至已经发出，管理端要提示「先到群里确认」。
	ErrManualTimeout = errors.New("timeout")
)

// ManualSendResult 是一次手动发送的结果。duplicate=true 时其余字段来自缓存。
type ManualSendResult struct {
	Duplicate      bool
	Sent           bool
	SentText       string
	LeaseExpiresAt int64 // unix 秒；always 档或没有租约时为 0
}

// ManualTextRequest 手动发文本的入参。
type ManualTextRequest struct {
	GroupID       string
	Text          string
	AtOpenIDs     []string
	Quote         bool
	ReplyToOpenID string
	LeaseID       string
	ClientMsgID   string
}

// ManualMemeRequest 手动发表情包的入参。
type ManualMemeRequest struct {
	GroupID       string
	MemeID        int64
	ReplyToOpenID string
	LeaseID       string
	ClientMsgID   string
	// Data / Mime 是管理端已经取好并校验过的图。非空时 Engine 不再重复下载；
	// 为空时 Engine 自己按 ctx 取，保持可以被其他调用方直接使用。
	Data []byte
	Mime string
}

// manualGate 检查全局开关、接管档位与租约。
//
// 顺序固定：全局 paused → mode_off → 墓碑 lease_released → 租约 lease_expired
// / lease_required。与设计文档 §4.4 的 can_send 判定顺序一致，
// 只是 group_disabled / passive_disabled / no_anchor 由 admin 在调用前判。
func (e *Engine) manualGate(groupID, leaseID string) (LeaseInfo, error) {
	if e == nil || e.store == nil {
		return LeaseInfo{}, ErrManualNotReady
	}
	if e.store.Get().Paused {
		return LeaseInfo{}, ErrManualPaused
	}
	switch e.TakeoverMode(groupID) {
	case takeover.ModeOff:
		return LeaseInfo{}, ErrTakeoverModeOff
	case takeover.ModeAlways:
		return LeaseInfo{}, nil
	}
	if leaseID == "" {
		return LeaseInfo{}, ErrManualLeaseRequired
	}
	info := e.LeaseInfo(groupID, leaseID)
	if info.Released {
		return info, ErrTakeoverLeaseReleased
	}
	if !info.Valid {
		return info, ErrTakeoverLeaseExpired
	}
	return info, nil
}

// groupSendLock 取该群的手动发送锁。sync.Map 里每群一个 *sync.Mutex。
func (e *Engine) groupSendLock(groupID string) *sync.Mutex {
	v, _ := e.sendMu.LoadOrStore(groupID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// beginManual 占一条幂等记录：
//   - 已有 done 且未过期 → 返回缓存结果，duplicate=true；
//   - 已有 in_flight → busy；
//   - 没有或已过期 → 登记 in_flight。
func (e *Engine) beginManual(key string, now time.Time) (ManualSendResult, bool, error) {
	e.manualMu.Lock()
	defer e.manualMu.Unlock()
	if e.manualDedupe == nil {
		e.manualDedupe = map[string]manualDedupeEntry{}
	}
	for k, ent := range e.manualDedupe {
		if now.Sub(ent.At) >= manualDedupeTTL {
			delete(e.manualDedupe, k)
		}
	}
	if ent, ok := e.manualDedupe[key]; ok {
		switch ent.State {
		case manualDone:
			ent.Result.Duplicate = true
			return ent.Result, true, nil
		case manualInFlight:
			return ManualSendResult{}, false, ErrManualBusy
		}
	}
	e.manualDedupe[key] = manualDedupeEntry{At: now, State: manualInFlight}
	return ManualSendResult{}, false, nil
}

// finishManual 记录成功结果；失败时删除记录，允许前端用同一个 id 重试。
func (e *Engine) finishManual(key string, result ManualSendResult, ok bool) {
	e.manualMu.Lock()
	defer e.manualMu.Unlock()
	if e.manualDedupe == nil {
		return
	}
	if !ok {
		delete(e.manualDedupe, key)
		return
	}
	e.manualDedupe[key] = manualDedupeEntry{At: e.now(), State: manualDone, Result: result}
}

// PeekManual 只读查询幂等缓存：同一 group|client_msg_id 已成功发送且未过期时
// 返回缓存结果（Duplicate=true），不占锁、不登记 in_flight。
//
// 管理端用它把重复请求挡在 can_send 预检之前：第一次发送已经成功但响应在
// 网络里丢了，重试时锚点额度/租约/模式可能早就变了；按设计 §4.3 这里仍应
// 返回 duplicate:true，而不是 409 让管理员以为没发出去。
func (e *Engine) PeekManual(groupID, clientMsgID string) (ManualSendResult, bool) {
	if e == nil || groupID == "" || clientMsgID == "" {
		return ManualSendResult{}, false
	}
	now := e.now()
	key := groupID + "|" + clientMsgID
	e.manualMu.Lock()
	defer e.manualMu.Unlock()
	ent, ok := e.manualDedupe[key]
	if !ok || ent.State != manualDone || now.Sub(ent.At) >= manualDedupeTTL {
		return ManualSendResult{}, false
	}
	res := ent.Result
	res.Duplicate = true
	return res, true
}

// SendManualText 由管理员代发一条文本。
//
// @ 的处理：先删掉正文里模型/人手抄进来的平台标签，再按 at_openids 拼在句首。
// 带 @ 时必须走 markdown 通道，而 markdown 内容不允许换行，所以拼完把换行压平。
func (e *Engine) SendManualText(ctx context.Context, req ManualTextRequest) (ManualSendResult, error) {
	var out ManualSendResult
	if e == nil || e.sender == nil || e.mem == nil {
		return out, ErrManualNotReady
	}
	if strings.TrimSpace(req.ClientMsgID) == "" {
		return out, ErrManualBadClientMsgID
	}
	if strings.TrimSpace(req.Text) == "" {
		return out, ErrManualTextEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}

	lock := e.groupSendLock(req.GroupID)
	if !lock.TryLock() {
		return out, ErrManualBusy
	}
	defer lock.Unlock()

	now := e.now()
	key := req.GroupID + "|" + req.ClientMsgID
	if cached, dup, err := e.beginManual(key, now); err != nil {
		return out, err
	} else if dup {
		return cached, nil
	}
	sentOK := false
	defer func() { e.finishManual(key, out, sentOK) }()

	info, err := e.manualGate(req.GroupID, req.LeaseID)
	if err != nil {
		return out, err
	}

	g := e.mem.Group(req.GroupID, "")
	content, err := composeManualText(g, req.Text, req.AtOpenIDs)
	if err != nil {
		return out, err
	}

	if req.Quote {
		err = e.sender.SendGroupQuote(ctx, req.GroupID, content, req.ReplyToOpenID)
	} else {
		err = e.sender.SendGroupTo(ctx, req.GroupID, content, req.ReplyToOpenID)
	}
	if err != nil {
		logx.Warn("手动发送失败", "group", req.GroupID, "err", err.Error())
		return out, err
	}

	// recordSent 在 Agent.sendGroup 内已经写过 RoleBot 记忆；这里补
	// 「我刚说过什么」的短状态（不增加 consecutive），否则提示词停在接管前的旧值。
	// 存的是 spokenForMemory 还原后的文本：平台 @ 标签换回〔@名字〕，
	// 与 AI 发言路径一致，避免模型把裸 openid 当成可抄的模板。
	g.MarkManualSpoke(spokenForMemory([]string{content}, g))
	out.Sent = true
	out.SentText = content
	out.LeaseExpiresAt = info.ExpiresAt
	sentOK = true
	logx.InfoCat(logx.CatSpeak, "管理员手动发言", "group", groupLabel(g),
		"内容", truncate(content, 80), "引用", req.Quote)
	return out, nil
}

// SendManualMeme 由管理员代发一张池子里的表情包。
//
// 表情包不能与文字/@ 同一条发，所以它是独立的一次发送；成功后写一条
// 「（发了个表情包）」记忆，让下一轮模型知道自己刚发过图。
func (e *Engine) SendManualMeme(ctx context.Context, req ManualMemeRequest) (ManualSendResult, error) {
	var out ManualSendResult
	if e == nil || e.mem == nil || e.memes == nil || e.imgSender == nil {
		return out, ErrManualNotReady
	}
	if strings.TrimSpace(req.ClientMsgID) == "" {
		return out, ErrManualBadClientMsgID
	}
	if ctx == nil {
		ctx = context.Background()
	}

	lock := e.groupSendLock(req.GroupID)
	if !lock.TryLock() {
		return out, ErrManualBusy
	}
	defer lock.Unlock()

	now := e.now()
	key := req.GroupID + "|" + req.ClientMsgID
	if cached, dup, err := e.beginManual(key, now); err != nil {
		return out, err
	} else if dup {
		return cached, nil
	}
	sentOK := false
	defer func() { e.finishManual(key, out, sentOK) }()

	info, err := e.manualGate(req.GroupID, req.LeaseID)
	if err != nil {
		return out, err
	}

	if _, ok := e.memes.Get(req.MemeID); !ok {
		return out, ErrManualBadMeme
	}
	data, mimeType := req.Data, req.Mime
	if len(data) == 0 || mimeType == "" {
		var err error
		data, mimeType, err = e.memes.DataCtx(ctx, req.MemeID)
		if err != nil {
			logx.Warn("手动发表情包取图失败", "group", req.GroupID, "id", req.MemeID, "err", err.Error())
			// 超时/取消不等于「图不存在」：图可能取到了、消息也可能已经发出，
			// 管理端要按 504 timeout 提示「先到群里确认」，不能报 bad_meme。
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return out, ErrManualTimeout
			}
			return out, ErrManualBadMeme
		}
	}
	if err := e.imgSender.SendImage(ctx, req.GroupID, data, mimeType, req.ReplyToOpenID); err != nil {
		logx.Warn("手动发表情包失败", "group", req.GroupID, "id", req.MemeID, "err", err.Error())
		return out, err
	}

	e.memes.MarkUsed(req.MemeID, now)
	g := e.mem.Group(req.GroupID, "")
	g.Append(memory.Line{TS: now, Role: memory.RoleBot, Content: manualMemeText}, 50)
	g.MarkManualSpoke(manualMemeText)
	out.Sent = true
	out.SentText = manualMemeText
	out.LeaseExpiresAt = info.ExpiresAt
	sentOK = true
	logx.InfoCat(logx.CatSpeak, "管理员手动发表情包", "group", groupLabel(g), "id", req.MemeID)
	return out, nil
}

// manualMemeText 手动发表情包后写进记忆的占位文本，与 AI 路径保持一致。
const manualMemeText = "（发了个表情包）"

// composeManualText 清洗正文里的 @ 标签并按 at_openids 重新拼装。
//
// 成员校验在这里再做一次：admin 已经判过，但 Engine 可能被别的调用方直接使用，
// 拼一个不存在的 openid 出去等于把串码渲染进群里。
func composeManualText(g *memory.Group, text string, atOpenIDs []string) (string, error) {
	if g == nil {
		return "", ErrManualNotReady
	}
	body := strings.TrimSpace(stripAtTags(text))

	tags := make([]string, 0, len(atOpenIDs))
	seen := map[string]struct{}{}
	for _, id := range atOpenIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, ok := g.MemberOf(id); !ok {
			return "", ErrManualBadAt
		}
		seen[id] = struct{}{}
		tags = append(tags, atTag(id))
	}
	if body == "" {
		return "", ErrManualTextEmpty
	}
	if len(tags) == 0 {
		return body, nil
	}
	out := strings.Join(tags, " ") + " " + body
	// 带 @ 走 markdown 通道，平台拒收带换行的 markdown（40034009）。
	// qqapi 出口还有一道兜底，这里先压平，避免把一条必然失败的请求发出去。
	return flattenManualNewlines(out), nil
}

// flattenManualNewlines 把换行压成空格。
func flattenManualNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
}
