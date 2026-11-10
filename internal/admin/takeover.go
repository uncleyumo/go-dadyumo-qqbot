package admin

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dadyumo/internal/brain"
	"dadyumo/internal/config"
	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
	"dadyumo/internal/takeover"
)

// 人工接管的管理端接口。
//
// 路由、入参、错误码按设计文档 §4.2-§4.6 落位。这里只做三件事：
//   - 统一校验（方法 / body 上限 / JSON Content-Type / Origin / openid / 成员）；
//   - 把 Engine 的接管方法映射成 HTTP 状态与 reason；
//   - 拼 window DTO（锚点、租约、can_send、最近消息）。
//
// 模式、租约、epoch 的唯一真源都在 Engine / takeover.Store，admin 不持第二份状态。

// takeoverManualTimeout 手动发送（文本与表情包）的总预算。
// 管理端 http.Server 的 WriteTimeout 是 30 秒，留 5 秒写响应。
const takeoverManualTimeout = 25 * time.Second

// takeoverMaxTextRunes 单条手动文本的字数上限。
// 设计文档只列了 text_too_long，没有给具体数值；这里取 2000，
// 远大于正常人手动发言，又能挡住误粘贴整篇文章。
const takeoverMaxTextRunes = 2000

// takeoverMaxMemeBytes 表情包单图上限。10 MiB 来自设计文档的平台限制记录
// （标注为「推测」，待真机验证）。
const takeoverMaxMemeBytes = 10 << 20

// openidPattern 群 / 成员 openid 的格式。c2c: 前缀在正则之前单独拒绝。
var openidPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{6,64}$`)

// leaseIDPattern 窗口生成的 lease_id 与 client_msg_id 的格式。
// 前端用 crypto.randomUUID 生成，8 位下限给测试与旧页面留余量。
var leaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,64}$`)

// platformCodePattern 从 qqapi 的 "HTTP %d code=%d: ..." 错误文本里取平台码。
var platformCodePattern = regexp.MustCompile(`code=(\d+)`)

// takeoverReasonMsg 是 reason 对应的中文提示。前端可以直接展示 msg。
var takeoverReasonMsg = map[string]string{
	"bad_request":            "请求格式错误",
	"bad_origin":             "请求来源不合法",
	"unauthorized":           "未登录",
	"bad_openid":             "openid 格式不正确",
	"bad_mode":               "mode 必须是 off / window / always",
	"bad_action":             "action 必须是 acquire / beat / release",
	"bad_visibility":         "visibility 必须是 visible / hidden",
	"bad_lease":              "lease_id 格式不正确",
	"bad_client_msg_id":      "client_msg_id 格式不正确",
	"bad_at":                 "要 @ 的人不在该群成员表里",
	"bad_reply_to":           "要回复的人不在该群成员表里",
	"text_empty":             "内容不能为空",
	"text_too_long":          "内容过长",
	"bad_meme":               "表情包不存在或池子未启用",
	"meme_format":            "表情包格式不支持",
	"meme_too_large":         "表情包超过 10 MiB",
	"unknown_group":          "未知群：记忆与锚点池里都没有这个群",
	"c2c_not_supported":      "不支持单聊（c2c）",
	"mode_off":               "该群已关闭人工接管",
	"lease_required":         "窗口打开时接管需要 lease_id",
	"lease_released":         "本窗口已显式结束接管",
	"lease_expired":          "窗口连接已断开",
	"stale_epoch":            "接管状态已被全部恢复正常，请重新打开窗口",
	"busy":                   "同群已有发送在途",
	"paused":                 "全局总开关已关闭",
	"group_disabled":         "该群在配置里被禁用",
	"passive_disabled":       "平台未启用被动回复，无法手动发送",
	"no_anchor":              "群里没有可用锚点，先等一条新消息",
	"expired":                "锚点已全部过期，先等一条新消息",
	"quota_exhausted":        "锚点额度已用尽，先等一条新消息",
	"rate_limited":           "发送限流：本分钟配额已用尽",
	"markdown_newline":       "平台拒绝带换行的 markdown",
	"platform_error":         "平台返回错误",
	"timeout":                "发送超时，先到群里确认是否已发出",
	"not_initialized":        "服务未完成接管功能装配",
	"save_failed":            "接管模式写盘失败",
	"body_too_large":         "请求体超过 1MB",
	"unsupported_media_type": "写接口要求 Content-Type: application/json",
}

// SetTakeoverAnchors 注入锚点只读来源。nil 表示按「无锚点」返回，不 panic。
func (s *Server) SetTakeoverAnchors(src takeover.AnchorSource) { s.anchors = src }

// writeReason 统一错误响应：{"ok":false,"reason":...,"msg":...}。
func writeReason(w http.ResponseWriter, code int, reason string) {
	msg := takeoverReasonMsg[reason]
	if msg == "" {
		msg = reason
	}
	writeJSON(w, code, map[string]any{"ok": false, "reason": reason, "msg": msg})
}

// takeoverGuard 统一校验方法、Content-Type、Origin/Sec-Fetch-Site 与 body 上限。
// 返回 false 表示已经写过响应，调用方直接 return。
func takeoverGuard(w http.ResponseWriter, r *http.Request, method string, writeBody bool) bool {
	if r.Method != method {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return false
	}
	if writeBody {
		ct := r.Header.Get("Content-Type")
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			writeReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
			return false
		}
	}
	// sendBeacon 与普通 fetch 都会带 Origin；跨站请求一律拒绝。
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		writeReason(w, http.StatusForbidden, "bad_origin")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeReason(w, http.StatusForbidden, "bad_origin")
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return true
}

// decodeTakeoverBody 解析 JSON body，把超限与格式错误分开报。
func decodeTakeoverBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeReason(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return false
		}
		writeReason(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

// knownGroup 在记忆里找群但**不创建**。Store.Group 会造幽灵群，不能用来探测。
func (s *Server) knownGroup(openid string) *memory.Group {
	if s.mem == nil {
		return nil
	}
	for _, g := range s.mem.All() {
		if g.OpenID == openid {
			return g
		}
	}
	return nil
}

// groupKnown 判断群是否已知：记忆里有，或锚点池里有记录。
func (s *Server) groupKnown(openid string) bool {
	return s.knownGroup(openid) != nil || s.engine.HasAnchors(openid)
}

// resolveTakeoverGroup 校验 openid 并返回群对象；只靠锚点知道的群返回 nil 也不报错。
func (s *Server) resolveTakeoverGroup(w http.ResponseWriter, openid string) (*memory.Group, bool) {
	if strings.HasPrefix(openid, "c2c:") {
		writeReason(w, http.StatusConflict, "c2c_not_supported")
		return nil, false
	}
	if !openidPattern.MatchString(openid) {
		writeReason(w, http.StatusBadRequest, "bad_openid")
		return nil, false
	}
	g := s.knownGroup(openid)
	if g == nil && !s.engine.HasAnchors(openid) {
		writeReason(w, http.StatusNotFound, "unknown_group")
		return nil, false
	}
	return g, true
}

// memberExists 用成员表存在性判断，不用 NameOfByOpenID != ""（只有群名片的成员会被误拒）。
func memberExists(g *memory.Group, openid string) bool {
	if g == nil || openid == "" {
		return false
	}
	_, ok := g.MemberOf(openid)
	return ok
}

// lookupGroupConfig 按 openid 找群配置。仓库没有 Config.GroupByOpenID，也不新增。
func lookupGroupConfig(cfg config.Config, openid string) (config.GroupConfig, bool) {
	for _, gc := range cfg.Groups {
		if gc.OpenID == openid {
			return gc, true
		}
	}
	return config.GroupConfig{}, false
}

// groupAllowed 配置里没有该群时视为允许（与 agent.groupEnabled 同构）。
func groupAllowed(cfg config.Config, openid string) bool {
	gc, ok := lookupGroupConfig(cfg, openid)
	return !ok || gc.Enabled
}

// takeoverModeState 是 mode 写接口的响应。
type takeoverModeState struct {
	OK           bool   `json:"ok"`
	OpenID       string `json:"openid"`
	Mode         string `json:"mode"`
	ModeSince    int64  `json:"mode_since"`
	AIPaused     bool   `json:"ai_paused"`
	PausedReason string `json:"paused_reason"`
	LeaseCount   int    `json:"lease_count"`
	TakeoverRev  int64  `json:"takeover_rev"`
	ServerNow    int64  `json:"server_now"`
}

// takeoverModePayload 由 Engine 现算模式状态，engine 为 nil 时全部零值安全。
func (s *Server) takeoverModePayload(openid string) takeoverModeState {
	paused, reason, _, leases := s.engine.AIPauseInfo(openid)
	since := int64(0)
	if t := s.engine.TakeoverModeSince(openid); !t.IsZero() {
		since = t.Unix()
	}
	return takeoverModeState{
		OK:           true,
		OpenID:       openid,
		Mode:         string(s.engine.TakeoverMode(openid)),
		ModeSince:    since,
		AIPaused:     paused,
		PausedReason: reason,
		LeaseCount:   leases,
		TakeoverRev:  s.engine.TakeoverRev(),
		ServerNow:    time.Now().Unix(),
	}
}

// handleTakeoverMode 切换单个群的接管档位。
func (s *Server) handleTakeoverMode(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		OpenID string `json:"openid"`
		Mode   string `json:"mode"`
	}
	if !decodeTakeoverBody(w, r, &body) {
		return
	}
	mode := takeover.Mode(body.Mode)
	if mode != takeover.ModeOff && mode != takeover.ModeWindow && mode != takeover.ModeAlways {
		writeReason(w, http.StatusBadRequest, "bad_mode")
		return
	}
	if _, ok := s.resolveTakeoverGroup(w, body.OpenID); !ok {
		return
	}
	// group_disabled 不挡 mode：禁用群也要能切到 off 清理。
	if err := s.engine.SetTakeoverMode(body.OpenID, mode); err != nil {
		takeoverEngineError(w, err)
		return
	}
	logx.Info("管理端切换接管模式", "group", body.OpenID, "mode", body.Mode)
	writeJSON(w, http.StatusOK, s.takeoverModePayload(body.OpenID))
}

// handleTakeoverLease 处理租约的 acquire / beat / release。
func (s *Server) handleTakeoverLease(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		OpenID     string `json:"openid"`
		LeaseID    string `json:"lease_id"`
		Action     string `json:"action"`
		Visibility string `json:"visibility"`
		Final      bool   `json:"final"`
		Closing    bool   `json:"closing"`
		Epoch      int64  `json:"epoch"`
	}
	if !decodeTakeoverBody(w, r, &body) {
		return
	}
	if _, ok := s.resolveTakeoverGroup(w, body.OpenID); !ok {
		return
	}
	if body.Action != "acquire" && body.Action != "beat" && body.Action != "release" {
		writeReason(w, http.StatusBadRequest, "bad_action")
		return
	}
	if !leaseIDPattern.MatchString(body.LeaseID) {
		writeReason(w, http.StatusBadRequest, "bad_lease")
		return
	}
	visibility := body.Visibility
	if visibility == "" {
		visibility = "visible"
	}
	if visibility != "visible" && visibility != "hidden" {
		writeReason(w, http.StatusBadRequest, "bad_visibility")
		return
	}
	hidden := visibility == "hidden"

	released := false
	var err error
	switch body.Action {
	case "acquire":
		err = s.engine.AcquireLease(body.OpenID, body.LeaseID, hidden, body.Epoch)
	case "beat":
		err = s.engine.BeatLease(body.OpenID, body.LeaseID, hidden, body.Epoch)
	case "release":
		released, err = s.engine.ReleaseLease(body.OpenID, body.LeaseID, body.Closing, body.Final, body.Epoch)
	}
	if err != nil {
		takeoverEngineError(w, err)
		return
	}
	payload := s.takeoverWindowPayload(s.store.Get(), body.OpenID, body.LeaseID, "")
	payload["released"] = released
	payload["action"] = body.Action
	writeJSON(w, http.StatusOK, payload)
}

// handleTakeoverWindow 只读返回窗口 DTO；严格无副作用。
func (s *Server) handleTakeoverWindow(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodGet, false) {
		return
	}
	q := r.URL.Query()
	openid := q.Get("openid")
	g, ok := s.resolveTakeoverGroup(w, openid)
	if !ok {
		return
	}
	replyTo := q.Get("reply_to_openid")
	if replyTo != "" && !memberExists(g, replyTo) {
		writeReason(w, http.StatusBadRequest, "bad_reply_to")
		return
	}
	writeJSON(w, http.StatusOK,
		s.takeoverWindowPayload(s.store.Get(), openid, q.Get("lease_id"), replyTo))
}

// takeoverMemberDTO 是成员选择器需要的字段，不直接下发 memory.Member。
type takeoverMemberDTO struct {
	OpenID   string `json:"openid"`
	Name     string `json:"name"`
	Card     string `json:"card,omitempty"`
	Role     string `json:"role,omitempty"`
	LastSeen int64  `json:"last_seen"`
	MsgCount int    `json:"msg_count"`
	IsMaster bool   `json:"is_master"`
}

// handleTakeoverMembers 返回该群成员表（可按 q 过滤），供 @ / 引用选择器用。
func (s *Server) handleTakeoverMembers(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodGet, false) {
		return
	}
	q := r.URL.Query()
	openid := q.Get("openid")
	g, ok := s.resolveTakeoverGroup(w, openid)
	if !ok {
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(q.Get("q")))
	items := make([]takeoverMemberDTO, 0)
	if g != nil {
		for _, m := range g.Members() {
			if keyword != "" &&
				!strings.Contains(strings.ToLower(m.Name), keyword) &&
				!strings.Contains(strings.ToLower(m.Card), keyword) &&
				!strings.Contains(strings.ToLower(m.OpenID), keyword) {
				continue
			}
			items = append(items, takeoverMemberDTO{
				OpenID:   m.OpenID,
				Name:     m.Name,
				Card:     m.Card,
				Role:     m.Role,
				LastSeen: m.LastSeen.Unix(),
				MsgCount: m.MsgCount,
				IsMaster: m.IsMaster,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "members": items, "total": len(items)})
}

// writeManualDuplicate 返回幂等缓存命中的响应。字段形状与真正的发送成功一致，
// 前端只看 duplicate 决定提示，其他字段保持不变。
func (s *Server) writeManualDuplicate(w http.ResponseWriter, cfg config.Config,
	openid, leaseID, replyTo string, cached brain.ManualSendResult) {
	payload := s.takeoverWindowPayload(cfg, openid, leaseID, replyTo)
	payload["duplicate"] = true
	payload["sent_text"] = cached.SentText
	payload["lease_expires_at"] = cached.LeaseExpiresAt
	payload["msg_id"] = ""
	payload["seq"] = 0
	writeJSON(w, http.StatusOK, payload)
}

// handleTakeoverSend 由管理员代发一条文本。
func (s *Server) handleTakeoverSend(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		OpenID      string   `json:"openid"`
		Text        string   `json:"text"`
		AtOpenIDs   []string `json:"at_openids"`
		Quote       bool     `json:"quote"`
		ReplyTo     string   `json:"reply_to_openid"`
		LeaseID     string   `json:"lease_id"`
		ClientMsgID string   `json:"client_msg_id"`
	}
	if !decodeTakeoverBody(w, r, &body) {
		return
	}
	cfg := s.store.Get()
	g, ok := s.resolveTakeoverGroup(w, body.OpenID)
	if !ok {
		return
	}
	if !groupAllowed(cfg, body.OpenID) {
		writeReason(w, http.StatusConflict, "group_disabled")
		return
	}
	if !leaseIDPattern.MatchString(body.ClientMsgID) {
		writeReason(w, http.StatusBadRequest, "bad_client_msg_id")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeReason(w, http.StatusBadRequest, "text_empty")
		return
	}
	if len([]rune(body.Text)) > takeoverMaxTextRunes {
		writeReason(w, http.StatusBadRequest, "text_too_long")
		return
	}
	if body.LeaseID != "" && !leaseIDPattern.MatchString(body.LeaseID) {
		writeReason(w, http.StatusBadRequest, "bad_lease")
		return
	}
	for _, id := range body.AtOpenIDs {
		if !openidPattern.MatchString(id) || !memberExists(g, id) {
			writeReason(w, http.StatusBadRequest, "bad_at")
			return
		}
	}
	if body.ReplyTo != "" && !memberExists(g, body.ReplyTo) {
		writeReason(w, http.StatusBadRequest, "bad_reply_to")
		return
	}
	// 幂等命中要在 can_send 预检之前：第一次发送成功但响应丢了，重试时
	// 锚点额度/租约/模式可能已经变了，按设计 §4.3 仍应返回 duplicate:true。
	if cached, ok := s.engine.PeekManual(body.OpenID, body.ClientMsgID); ok {
		s.writeManualDuplicate(w, cfg, body.OpenID, body.LeaseID, body.ReplyTo, cached)
		return
	}
	// 发送前按 §4.4 的固定顺序预检：global_paused → mode → 群禁用 → 被动 →
	// 租约 → 锚点。Engine.manualGate 会再判一次，但锚点三类原因只有 admin 能分。
	if reason := s.takeoverBlockedReason(cfg, body.OpenID, body.LeaseID); reason != "" {
		writeReason(w, http.StatusConflict, reason)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), takeoverManualTimeout)
	defer cancel()
	res, err := s.engine.SendManualText(ctx, brain.ManualTextRequest{
		GroupID:       body.OpenID,
		Text:          body.Text,
		AtOpenIDs:     body.AtOpenIDs,
		Quote:         body.Quote,
		ReplyToOpenID: body.ReplyTo,
		LeaseID:       body.LeaseID,
		ClientMsgID:   body.ClientMsgID,
	})
	if err != nil {
		takeoverSendError(w, err)
		return
	}
	payload := s.takeoverWindowPayload(cfg, body.OpenID, body.LeaseID, body.ReplyTo)
	payload["duplicate"] = res.Duplicate
	payload["sent_text"] = res.SentText
	payload["lease_expires_at"] = res.LeaseExpiresAt
	// ManualSendResult 不携带平台的 msg_id/seq（qqapi 出口没有回传），
	// 字段保留是为了 DTO 与设计文档一致，值恒为空；额度以 anchors 快照为准。
	payload["msg_id"] = ""
	payload["seq"] = 0
	writeJSON(w, http.StatusOK, payload)
}

// handleTakeoverMeme 由管理员代发一张池里的表情包。
func (s *Server) handleTakeoverMeme(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		OpenID      string `json:"openid"`
		MemeID      int64  `json:"meme_id"`
		ReplyTo     string `json:"reply_to_openid"`
		LeaseID     string `json:"lease_id"`
		ClientMsgID string `json:"client_msg_id"`
	}
	if !decodeTakeoverBody(w, r, &body) {
		return
	}
	cfg := s.store.Get()
	g, ok := s.resolveTakeoverGroup(w, body.OpenID)
	if !ok {
		return
	}
	if s.memes == nil {
		writeReason(w, http.StatusInternalServerError, "not_initialized")
		return
	}
	if !groupAllowed(cfg, body.OpenID) {
		writeReason(w, http.StatusConflict, "group_disabled")
		return
	}
	if body.MemeID == 0 {
		writeReason(w, http.StatusBadRequest, "bad_meme")
		return
	}
	if !leaseIDPattern.MatchString(body.ClientMsgID) {
		writeReason(w, http.StatusBadRequest, "bad_client_msg_id")
		return
	}
	if body.LeaseID != "" && !leaseIDPattern.MatchString(body.LeaseID) {
		writeReason(w, http.StatusBadRequest, "bad_lease")
		return
	}
	if body.ReplyTo != "" && !memberExists(g, body.ReplyTo) {
		writeReason(w, http.StatusBadRequest, "bad_reply_to")
		return
	}
	// 幂等命中优先于取图与 can_send 预检：同一条表情包重试不该因为
	// 池子已被淘汰、额度用尽或租约过期而报错（设计 §4.3）。
	if cached, ok := s.engine.PeekManual(body.OpenID, body.ClientMsgID); ok {
		s.writeManualDuplicate(w, cfg, body.OpenID, body.LeaseID, body.ReplyTo, cached)
		return
	}
	if _, ok := s.memes.Get(body.MemeID); !ok {
		writeReason(w, http.StatusBadRequest, "bad_meme")
		return
	}
	if reason := s.takeoverBlockedReason(cfg, body.OpenID, body.LeaseID); reason != "" {
		writeReason(w, http.StatusConflict, reason)
		return
	}

	// 取图只用一次、且受同一个 25 秒预算约束：取到的字节直接交给 Engine 发送，
	// 不再让 Engine 下载第二遍。超时被正确映射为 504，而不是「表情包不存在」。
	ctx, cancel := context.WithTimeout(r.Context(), takeoverManualTimeout)
	defer cancel()
	data, mimeType, err := s.memes.DataCtx(ctx, body.MemeID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeReason(w, http.StatusGatewayTimeout, "timeout")
			return
		}
		writeReason(w, http.StatusBadRequest, "bad_meme")
		return
	}
	if !strings.HasPrefix(mimeType, "image/") {
		writeReason(w, http.StatusBadRequest, "meme_format")
		return
	}
	if len(data) > takeoverMaxMemeBytes {
		writeReason(w, http.StatusBadRequest, "meme_too_large")
		return
	}

	res, err := s.engine.SendManualMeme(ctx, brain.ManualMemeRequest{
		GroupID:       body.OpenID,
		MemeID:        body.MemeID,
		ReplyToOpenID: body.ReplyTo,
		LeaseID:       body.LeaseID,
		ClientMsgID:   body.ClientMsgID,
		Data:          data,
		Mime:          mimeType,
	})
	if err != nil {
		takeoverSendError(w, err)
		return
	}
	payload := s.takeoverWindowPayload(cfg, body.OpenID, body.LeaseID, body.ReplyTo)
	payload["duplicate"] = res.Duplicate
	payload["sent_text"] = res.SentText
	payload["lease_expires_at"] = res.LeaseExpiresAt
	payload["msg_id"] = ""
	payload["seq"] = 0
	writeJSON(w, http.StatusOK, payload)
}

// handleTakeoverEndAll 把所有已知群设为 off，清空租约并递增 epoch。
func (s *Server) handleTakeoverEndAll(w http.ResponseWriter, r *http.Request) {
	if !takeoverGuard(w, r, http.MethodPost, true) {
		return
	}
	var body struct{}
	if !decodeTakeoverBody(w, r, &body) {
		return
	}
	res, err := s.engine.EndAll()
	if err != nil {
		takeoverEngineError(w, err)
		return
	}
	logx.Info("管理端全部恢复正常", "群数", res.ModesReset, "释放租约", res.LeasesReleased)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"modes_reset":     res.ModesReset,
		"leases_released": res.LeasesReleased,
		"epoch":           res.Epoch,
		"takeover_rev":    res.Rev,
	})
}

// takeoverBlockedReason 按设计文档 §4.4 的固定顺序返回第一个拒绝原因，空串表示可发。
func (s *Server) takeoverBlockedReason(cfg config.Config, openid, leaseID string) string {
	if cfg.Paused {
		return "paused"
	}
	mode := s.engine.TakeoverMode(openid)
	if mode == takeover.ModeOff {
		return "mode_off"
	}
	if !groupAllowed(cfg, openid) {
		return "group_disabled"
	}
	if !cfg.QQ.PreferPassive {
		return "passive_disabled"
	}
	if mode == takeover.ModeWindow {
		info := s.engine.LeaseInfo(openid, leaseID)
		if info.Released {
			return "lease_released"
		}
		if leaseID != "" && !info.Valid {
			return "lease_expired"
		}
		if leaseID == "" {
			return "lease_required"
		}
	}
	if s.anchors == nil {
		return "no_anchor"
	}
	all, alive := s.anchors.ListAnchors(openid)
	if len(all) == 0 {
		return "no_anchor"
	}
	if alive == 0 {
		for _, a := range all {
			if a.Used >= a.MaxUse {
				return "quota_exhausted"
			}
		}
		return "expired"
	}
	return ""
}

// takeoverLineDTO 是最近消息的只读副本（memory.Line 没有 json tag，不能直接下发）。
type takeoverLineDTO struct {
	TS      int64  `json:"ts"`
	Role    string `json:"role"`
	Name    string `json:"name"`
	OpenID  string `json:"openid"`
	Content string `json:"content"`
	MsgID   string `json:"msg_id,omitempty"`
}

// takeoverSelectedDTO 是发送前预检选中的锚点。
type takeoverSelectedDTO struct {
	MsgID         string `json:"msg_id"`
	SenderOpenID  string `json:"sender_openid"`
	SenderName    string `json:"sender_name"`
	ExpiresAt     int64  `json:"expires_at"`
	RemainingSec  int64  `json:"remaining_sec"`
	Used          int    `json:"used"`
	MaxUse        int    `json:"max_use"`
	RemainingUses int    `json:"remaining_uses"`
	Fallback      bool   `json:"fallback"`
}

// takeoverAnchorView 生成 anchors / selected / total_quota 三件套。
func (s *Server) takeoverAnchorView(openid, replyTo string) ([]takeover.AnchorSnapshot, *takeoverSelectedDTO, int) {
	anchors := make([]takeover.AnchorSnapshot, 0)
	if s.anchors == nil {
		return anchors, nil, 0
	}
	all, _ := s.anchors.ListAnchors(openid)
	anchors = append(anchors, all...)
	total := 0
	for _, a := range all {
		if !a.Alive {
			continue
		}
		if n := a.MaxUse - a.Used; n > 0 {
			total += n
		}
	}
	var selected *takeoverSelectedDTO
	if a, matched, ok := s.anchors.PickAnchor(openid, replyTo); ok {
		now := time.Now().Unix()
		remainUses := a.MaxUse - a.Used
		if remainUses < 0 {
			remainUses = 0
		}
		remainSec := a.ExpiresAt - now
		if remainSec < 0 {
			remainSec = 0
		}
		selected = &takeoverSelectedDTO{
			MsgID:         a.MsgID,
			SenderOpenID:  a.SenderOpenID,
			SenderName:    a.SenderName,
			ExpiresAt:     a.ExpiresAt,
			RemainingSec:  remainSec,
			Used:          a.Used,
			MaxUse:        a.MaxUse,
			RemainingUses: remainUses,
			Fallback:      replyTo != "" && !matched,
		}
	}
	return anchors, selected, total
}

// takeoverWindowPayload 拼 GET /window 与租约接口共用的完整 DTO。
func (s *Server) takeoverWindowPayload(cfg config.Config, openid, leaseID, replyTo string) map[string]any {
	paused, reason, since, leases := s.engine.AIPauseInfo(openid)
	info := s.engine.LeaseInfo(openid, leaseID)
	sinceMode := int64(0)
	if t := s.engine.TakeoverModeSince(openid); !t.IsZero() {
		sinceMode = t.Unix()
	}

	g := s.knownGroup(openid)
	lines := make([]takeoverLineDTO, 0)
	if g != nil {
		for _, l := range g.Recent(20) {
			lines = append(lines, takeoverLineDTO{
				TS:      l.TS.Unix(),
				Role:    l.Role,
				Name:    l.Name,
				OpenID:  l.OpenID,
				Content: l.Content,
				MsgID:   l.MsgID,
			})
		}
	}

	anchors, selected, total := s.takeoverAnchorView(openid, replyTo)
	canReason := s.takeoverBlockedReason(cfg, openid, leaseID)
	return map[string]any{
		"ok":               true,
		"mode":             string(s.engine.TakeoverMode(openid)),
		"mode_since":       sinceMode,
		"ai_paused":        paused,
		"paused_reason":    reason,
		"global_paused":    cfg.Paused,
		"ai_paused_since":  since,
		"lease_valid":      info.Valid,
		"lease_expires_at": info.ExpiresAt,
		"lease_count":      leases,
		"epoch":            s.engine.TakeoverEpoch(),
		"takeover_rev":     s.engine.TakeoverRev(),
		"can_send":         canReason == "",
		"can_send_reason":  canReason,
		"total_quota":      total,
		"selected":         selected,
		"anchors":          anchors,
		"last_lines":       lines,
		"server_now":       time.Now().Unix(),
	}
}

// takeoverEngineError 把 Engine 接管方法的哨兵错误映射成 HTTP 状态。
func takeoverEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, brain.ErrTakeoverNotReady), errors.Is(err, brain.ErrManualNotReady):
		writeReason(w, http.StatusInternalServerError, "not_initialized")
	case errors.Is(err, brain.ErrTakeoverModeOff):
		writeReason(w, http.StatusConflict, "mode_off")
	case errors.Is(err, brain.ErrTakeoverStaleEpoch):
		writeReason(w, http.StatusConflict, "stale_epoch")
	case errors.Is(err, brain.ErrTakeoverLeaseExpired):
		writeReason(w, http.StatusConflict, "lease_expired")
	case errors.Is(err, brain.ErrTakeoverLeaseReleased):
		writeReason(w, http.StatusConflict, "lease_released")
	case errors.Is(err, brain.ErrManualLeaseRequired):
		writeReason(w, http.StatusConflict, "lease_required")
	case errors.Is(err, brain.ErrManualBusy):
		writeReason(w, http.StatusConflict, "busy")
	case errors.Is(err, brain.ErrManualPaused):
		writeReason(w, http.StatusConflict, "paused")
	case errors.Is(err, brain.ErrBadLease):
		writeReason(w, http.StatusBadRequest, "bad_lease")
	case errors.Is(err, brain.ErrManualBadClientMsgID):
		writeReason(w, http.StatusBadRequest, "bad_client_msg_id")
	case errors.Is(err, brain.ErrManualTextEmpty):
		writeReason(w, http.StatusBadRequest, "text_empty")
	case errors.Is(err, brain.ErrManualBadAt):
		writeReason(w, http.StatusBadRequest, "bad_at")
	case errors.Is(err, brain.ErrManualBadMeme):
		writeReason(w, http.StatusBadRequest, "bad_meme")
	case errors.Is(err, brain.ErrManualTimeout):
		writeReason(w, http.StatusGatewayTimeout, "timeout")
	default:
		// 写盘失败（Store.SetMode/BumpEpoch）与未知错误都走这里。
		writeReason(w, http.StatusInternalServerError, "save_failed")
	}
}

// takeoverSendError 映射发送链路的平台错误。
func takeoverSendError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeReason(w, http.StatusGatewayTimeout, "timeout")
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "发送限流") {
		writeReason(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if strings.Contains(msg, "无可用被动锚点") {
		writeReason(w, http.StatusConflict, "no_anchor")
		return
	}
	code := platformCode(msg)
	if code == 40034009 || strings.Contains(msg, "40034009") {
		writeReason(w, http.StatusBadRequest, "markdown_newline")
		return
	}
	// 已知的 Engine / 接管错误优先，别被当成平台错误吞成 502。
	switch {
	case errors.Is(err, brain.ErrTakeoverNotReady),
		errors.Is(err, brain.ErrManualNotReady),
		errors.Is(err, brain.ErrTakeoverModeOff),
		errors.Is(err, brain.ErrTakeoverStaleEpoch),
		errors.Is(err, brain.ErrTakeoverLeaseExpired),
		errors.Is(err, brain.ErrTakeoverLeaseReleased),
		errors.Is(err, brain.ErrManualLeaseRequired),
		errors.Is(err, brain.ErrManualBusy),
		errors.Is(err, brain.ErrManualPaused),
		errors.Is(err, brain.ErrBadLease),
		errors.Is(err, brain.ErrManualBadClientMsgID),
		errors.Is(err, brain.ErrManualTextEmpty),
		errors.Is(err, brain.ErrManualBadAt),
		errors.Is(err, brain.ErrManualBadMeme),
		errors.Is(err, brain.ErrManualTimeout):
		takeoverEngineError(w, err)
		return
	}
	// 其余平台错误透传 code 与前 200 字，前端提示「可能已发出」由超时文案负责。
	resp := map[string]any{
		"ok":     false,
		"reason": "platform_error",
		"msg":    truncateRunes(msg, 200),
	}
	if code != 0 {
		resp["platform_code"] = code
	}
	writeJSON(w, http.StatusBadGateway, resp)
}

// platformCode 从 "HTTP 400 code=40034105: ..." 里取平台错误码；取不到返回 0。
func platformCode(msg string) int64 {
	m := platformCodePattern.FindStringSubmatch(msg)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// truncateRunes 按字符截断，避免把多字节字符切成乱码。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
