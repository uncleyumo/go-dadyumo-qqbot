// Package webhook 实现 QQ 开放平台的 HTTP 回调接收端：Ed25519 验签、回调地址验证、落盘队列、事件分发。
package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

const (
	// maxBodyBytes 回调请求体上限。
	// ed25519 验签必须拿到完整 body，所以「先读后验」本身改不掉；缺的是读之前的这道闸门，
	// 否则一个超大 body 就能把内存吃光。实测正常回调只有几百字节，1 MiB 绰绰有余。
	maxBodyBytes = 1 << 20

	// queuePollInterval 队列轮询周期。回调量是「群消息」级别，200ms 延迟远小于人对回复的感知阈。
	queuePollInterval = 200 * time.Millisecond

	// queueMaxAttempts 单条事件的最大重试次数，超过后移进 failed 子目录等人来看。
	// 毒消息（解析不了、handler 一直报错）不能无限重试刷日志，也不能直接丢。
	queueMaxAttempts = 8
)

// Handler 事件回调接口，由上层 agent 实现
type Handler interface {
	// OnGroupMessage 群消息；atMe 表示消息来自 GROUP_AT_MESSAGE_CREATE
	OnGroupMessage(ev *GroupMessage, atMe bool)
	// OnC2CMessage 单聊消息
	OnC2CMessage(ev *C2CMessage)
	// OnGroupRobotEvent 机器人进出群 / 群主开启关闭全量消息
	OnGroupRobotEvent(eventType string, ev *GroupRobotEvent)
}

// Server 回调服务
//
// 正常路径（NewWithQueue + Start）：HTTP 线程只做「验签 → 落盘 → 200」，事件处理全在
// 后台 worker 里。这就是原注释「平台要求快速回包，业务异步处理」当年该兑现却没兑现的语义。
// 降级路径（没启用队列，或队列目录写不了）：在 HTTP 线程同步跑 dispatch，此时不再假装
// 异步，处理失败会如实回 5xx。
type Server struct {
	store   *config.Store
	handler Handler

	// queueDir 非空时启用本地落盘队列：HTTP 线程只做「验签 → 落盘 → 200」，
	// 落盘成功即代表事件不会丢，真正的业务处理交给后台 worker。
	// 空目录表示不落盘，直接在 HTTP 线程同步处理（仅供测试与不想要持久化的场景）。
	queueDir string
	// failedDir 队列里重试到上限的事件落在这里，等人手工查看
	failedDir string
}

// New 创建回调服务（不落盘，HTTP 线程同步处理）
func New(store *config.Store, h Handler) *Server {
	return &Server{store: store, handler: h}
}

// NewWithQueue 创建带落盘队列的回调服务。
// dir 下的每个文件是一条完整回调 body；MsgID 相同即视为同一条事件，靠文件名去重。
// 队列是纯文件实现，所以进程重启后未消费的事件自动还在。
func NewWithQueue(store *config.Store, h Handler, dir string) *Server {
	return &Server{
		store:     store,
		handler:   h,
		queueDir:  dir,
		failedDir: filepath.Join(dir, "failed"),
	}
}

// Start 建好队列目录并拉起后台消费 worker。
// ctx 一取消 worker 就退出；退出时队列文件留在盘上，下次 Start 继续消费。
func (s *Server) Start(ctx context.Context) error {
	if s.queueDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.queueDir, 0o755); err != nil {
		return fmt.Errorf("创建回调队列目录失败: %w", err)
	}
	if err := os.MkdirAll(s.failedDir, 0o755); err != nil {
		return fmt.Errorf("创建回调队列失败目录失败: %w", err)
	}
	// 上一次进程崩在写盘中途留下的半截临时文件，续写没意义，直接清掉免得越积越多
	if olds, err := filepath.Glob(filepath.Join(s.queueDir, "*.tmp")); err == nil {
		for _, f := range olds {
			_ = os.Remove(f)
		}
	}
	go s.runQueue(ctx)
	return nil
}

// ServeHTTP 处理回调
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 闸门必须在读之前套上：一旦 io.ReadAll 开始，就没有"读一半就停"的机会了
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := ReadBody(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			logx.Warn("回调请求体超限，已拒绝", "limit", mbe.Limit, "remote", r.RemoteAddr)
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		logx.Warn("读取回调请求体失败", "err", err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := s.store.Get()

	if cfg.QQ.RequireSignature && !VerifyRequest(cfg.QQ.AppSecret, r, body) {
		logx.Warn("回调签名校验失败，已拒绝", "remote", r.RemoteAddr, "ua", r.Header.Get("User-Agent"))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		// 连信封都解不出来就没什么可排队的（连 MsgID 都没有），直接判死；
		// 重投也不会变好，403/400 比 5xx 诚实——告诉平台"别试了"。
		logx.Warn("回调 payload 解析失败", "err", err.Error(), "remote", r.RemoteAddr)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	switch p.Op {
	case OpValidate:
		s.handleValidate(w, p)
	case OpDispatch:
		s.acceptDispatch(w, body, p)
	default:
		logx.Info("收到未知 op 的回调，已 ACK 但不处理", "op", p.Op)
		ackDispatch(w)
	}
}

// acceptDispatch 接住一条事件推送。
//
// 关键在于「成功」必须对平台说真话：
//   - 队列可用 → 落盘成功即代表不会丢，回 200 是诚实的；
//   - 队列不可用 → 降级同步处理，处理失败就回 5xx 让平台重投，
//     绝不能像以前那样解析失败也回 200，把「彻底丢弃」伪装成「处理成功」。
func (s *Server) acceptDispatch(w http.ResponseWriter, body []byte, p Payload) {
	if s.queueDir != "" {
		err := s.enqueue(body, p.ID)
		if err == nil {
			ackDispatch(w)
			return
		}
		// 落盘失败不能等于丢消息：降级同步处理，至少还有 handler 兜底
		logx.Error("回调落盘失败，降级为同步处理", "err", err.Error(), "id", p.ID, "t", p.T)
	}

	if err := s.dispatch(p); err != nil {
		logx.Error("同步处理回调失败，返回 5xx 请平台重投", "err", err.Error(), "id", p.ID, "t", p.T)
		http.Error(w, "dispatch failed", http.StatusInternalServerError)
		return
	}
	ackDispatch(w)
}

// ackDispatch 回平台约定的 ACK。回这个就等于说「这条我收了」，所以必须真的收了才回。
func ackDispatch(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"op":12}`))
}

func (s *Server) handleValidate(w http.ResponseWriter, p Payload) {
	cfg := s.store.Get()
	var req ValidationReq
	if err := json.Unmarshal(p.D, &req); err != nil {
		logx.Warn("回调验证请求解析失败", "err", err.Error())
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var buf strings.Builder
	buf.WriteString(req.EventTs)
	buf.WriteString(req.PlainToken)
	rsp := ValidationRsp{
		PlainToken: req.PlainToken,
		Signature:  Sign(cfg.QQ.AppSecret, []byte(buf.String())),
	}
	logx.Info("收到回调地址验证请求，已响应签名", "event_ts", req.EventTs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rsp)
}

// dispatch 把事件交给 handler。返回 error 表示这条事件没被处理掉，
// 调用方据此决定要不要回 5xx（同步路径）或保留重试（队列路径）。
func (s *Server) dispatch(p Payload) error {
	if s.handler == nil {
		logx.Warn("回调服务未接 handler，事件被丢弃", "t", p.T, "id", p.ID)
		return fmt.Errorf("handler 未配置")
	}
	start := time.Now()
	switch p.T {
	case EventGroupAtMessage, EventGroupMessage:
		var ev GroupMessage
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("群消息事件解析失败", "err", err.Error(), "id", p.ID, "t", p.T)
			return fmt.Errorf("群消息事件解析失败: %w", err)
		}
		// 原文只在 debug 下打。2026-10-05：引用气泡一直出不来，
		// 而「平台到底有没有在群里消息的 message_scene.ext 里给 REFIDX」
		// 只能从原文上看——反序列化后剩下的字段看不出「本来就没有」。
		raw := string(p.D)
		if len(raw) > 2000 {
			raw = raw[:2000] + "…"
		}
		logx.Debug("群消息原文", "t", p.T, "raw", raw)
		s.handler.OnGroupMessage(&ev, p.T == EventGroupAtMessage)
	case EventC2CMessage:
		var ev C2CMessage
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("单聊消息事件解析失败", "err", err.Error(), "id", p.ID, "t", p.T)
			return fmt.Errorf("单聊消息事件解析失败: %w", err)
		}
		s.handler.OnC2CMessage(&ev)

	// 机器人生命周期四事件逐个分支：早先四个共用一条 warn，下次定位还得翻源码才知道是哪一类炸的。
	// 这四条以前失败率 3/3（timestamp 被平台推成 JSON number），handler 是死代码。
	case EventGroupAddRobot:
		var ev GroupRobotEvent
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("加群事件解析失败", "err", err.Error(), "id", p.ID, "ts_raw", rawTimestamp(p.D))
			return fmt.Errorf("加群事件解析失败: %w", err)
		}
		logx.Info("收到机器人被加入群事件", "group", ev.GroupOpenID, "opd_id", ev.OpdID, "event_ts", ev.Timestamp.String())
		s.handler.OnGroupRobotEvent(EventGroupAddRobot, &ev)
	case EventGroupDelRobot:
		var ev GroupRobotEvent
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("退群事件解析失败", "err", err.Error(), "id", p.ID, "ts_raw", rawTimestamp(p.D))
			return fmt.Errorf("退群事件解析失败: %w", err)
		}
		logx.Info("收到机器人被移出群事件", "group", ev.GroupOpenID, "opd_id", ev.OpdID, "event_ts", ev.Timestamp.String())
		s.handler.OnGroupRobotEvent(EventGroupDelRobot, &ev)
	case EventGroupMsgRecv:
		var ev GroupRobotEvent
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("全量消息开启事件解析失败", "err", err.Error(), "id", p.ID, "ts_raw", rawTimestamp(p.D))
			return fmt.Errorf("全量消息开启事件解析失败: %w", err)
		}
		logx.Info("收到群主开启全量消息事件", "group", ev.GroupOpenID, "opd_id", ev.OpdID, "event_ts", ev.Timestamp.String())
		s.handler.OnGroupRobotEvent(EventGroupMsgRecv, &ev)
	case EventGroupMsgReject:
		var ev GroupRobotEvent
		if err := json.Unmarshal(p.D, &ev); err != nil {
			logx.Warn("全量消息关闭事件解析失败", "err", err.Error(), "id", p.ID, "ts_raw", rawTimestamp(p.D))
			return fmt.Errorf("全量消息关闭事件解析失败: %w", err)
		}
		logx.Info("收到群主关闭全量消息事件", "group", ev.GroupOpenID, "opd_id", ev.OpdID, "event_ts", ev.Timestamp.String())
		s.handler.OnGroupRobotEvent(EventGroupMsgReject, &ev)

	default:
		// 平台推我们不订阅的事件是常态（比如只订了 @ 消息却推来全量消息），
		// 重投多少次结果都一样，所以这不算「丢失」，Info 级留痕即可对账。
		// 早先是 Debug，生产环境 100% 静默：日志里出现过 10 次回调拿到 200 却找不到任何派发记录。
		logx.Info("收到未处理的事件，已忽略", "t", p.T, "id", p.ID)
		return nil
	}
	logx.Debug("事件分发完成", "t", p.T, "cost_ms", time.Since(start).Milliseconds())
	return nil
}

// rawTimestamp 解析失败时把原始 timestamp 片段挖出来打进日志，省得再翻源码
func rawTimestamp(d json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(d, &m); err != nil {
		return ""
	}
	return string(m["timestamp"])
}

// ---------- 本地落盘队列 ----------

// enqueue 把回调原文写进队列目录。同一个 MsgID 只入队一次（平台重投不会灌出第二条）。
// 先写 .tmp 再 rename，保证目录里出现的 .json 一定是写完整了的。
func (s *Server) enqueue(body []byte, msgID string) error {
	base := safeFileName(msgID)
	if base == "" {
		// 没有 MsgID 就没有去重依据，只能靠唯一文件名
		base = fmt.Sprintf("anon-%d-%s", time.Now().UnixNano(), randHex(4))
	}
	final := filepath.Join(s.queueDir, base+".json")
	if _, err := os.Stat(final); err == nil {
		logx.Debug("回调已入队，跳过重复事件", "id", msgID)
		return nil
	}
	tmp := filepath.Join(s.queueDir, fmt.Sprintf("%s.%d%s.tmp", base, time.Now().UnixNano(), randHex(4)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	logx.Debug("回调已落盘入队", "id", msgID, "file", filepath.Base(final))
	return nil
}

// runQueue 后台消费 worker：轮询队列目录，逐条 dispatch。
// 处理成功才删文件；处理失败留在盘上等下一轮，超过上限挪进 failed/ 目录等人来看。
func (s *Server) runQueue(ctx context.Context) {
	ticker := time.NewTicker(queuePollInterval)
	defer ticker.Stop()
	attempts := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			logx.Info("回调队列消费已停止，未消费事件保留在盘上", "dir", s.queueDir)
			return
		case <-ticker.C:
		}
		for _, f := range s.pendingFiles() {
			if ctx.Err() != nil {
				return
			}
			name := filepath.Base(f)
			if attempts[name] >= queueMaxAttempts {
				s.deadLetter(name, fmt.Errorf("重试 %d 次仍失败", attempts[name]))
				delete(attempts, name)
				continue
			}
			body, err := os.ReadFile(f)
			if err != nil {
				logx.Warn("读取队列文件失败，稍后重试", "file", name, "err", err.Error())
				continue
			}
			var p Payload
			if err := json.Unmarshal(body, &p); err != nil {
				// 落盘时就已验过签名，body 是完整的；解不出来只可能是磁盘损坏，重投无意义
				s.deadLetter(name, fmt.Errorf("队列文件损坏: %w", err))
				continue
			}
			if err := s.dispatch(p); err != nil {
				attempts[name]++
				logx.Error("队列事件处理失败，保留待重试", "err", err.Error(), "file", name,
					"t", p.T, "attempt", attempts[name])
				continue
			}
			if err := os.Remove(f); err != nil {
				// 删不掉只会导致下一轮再处理一次；事件本身已经处理过了，幂等由 handler 负责
				logx.Warn("队列文件删除失败", "file", name, "err", err.Error())
			}
			delete(attempts, name)
		}
	}
}

// pendingFiles 列出待消费的队列文件（跳过临时文件与 failed 子目录）
func (s *Server) pendingFiles() []string {
	entries, err := os.ReadDir(s.queueDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logx.Warn("读取回调队列目录失败", "err", err.Error())
		}
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(s.queueDir, e.Name()))
	}
	return out
}

// deadLetter 把反复处理不了的事件挪到 failed/ 目录，留证据而不是静默丢弃
func (s *Server) deadLetter(name string, cause error) {
	src := filepath.Join(s.queueDir, name)
	dst := filepath.Join(s.failedDir, name)
	if err := os.Rename(src, dst); err != nil {
		logx.Error("队列事件转入失败目录失败", "file", name, "err", err.Error())
		return
	}
	logx.Error("队列事件已放弃并转入 failed 目录", "file", name, "err", cause.Error(), "dir", s.failedDir)
}

// safeFileName 把 MsgID 规整成安全文件名；无法规整时返回空串
func safeFileName(msgID string) string {
	msgID = strings.TrimSpace(msgID)
	if msgID == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range msgID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 100 {
			break
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return ""
	}
	return out
}

// randHex 生成 n 字节随机 hex，仅用于让无 MsgID 的文件名天然唯一
func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}
