package statsdb

import (
	"database/sql"
	"strings"
	"time"

	"dadyumo/internal/logx"
)

// 写队列参数：200ms 或积满 64 条刷一次。
// 崩溃（kill -9）最多丢 200ms 的明细——统计数据这个精度完全够用。
const (
	flushInterval = 200 * time.Millisecond
	flushBatch    = 64
)

// writeOp 一条待写操作。kind 区分明细插入、目标状态 UPSERT、事件日志。
type writeOp struct {
	kind   int // 1=call, 2=target, 3=event
	call   Call
	target TargetState
	event  Event
}

const (
	opCall = iota + 1
	opTarget
	opEvent
)

// Event 一条事件日志。
//
// Group 只取一个扁平字段出来，是为了能按群索引过滤——
// kv 里虽然也有 group，但它在 JSON 里面，SQL 侧没法直接 WHERE。
type Event struct {
	TS    time.Time
	Cat   string
	Level string
	Msg   string
	Group string
	// KV 已经是序列化好的紧凑 JSON（nil 时存空串）。
	KV string
}

// RecordEvent 落一条事件日志。永不阻塞：队列满直接丢弃。
//
// 为什么日志可以丢而 calls 不能：统计丢了影响的是「哪个模型好用」的判断，
// 日志丢了一条影响的是「某次为什么没回」的可查性——
// 后者下次还有同类事件，前者丢了就永远丢了。丢日志的计数挂在同一个
// dropped 上，管理端 /api/stats/status 已经展示它，不用改。
func (d *DB) RecordEvent(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	// msg 截断：群消息正文可能很长（原文截到 200），日志不该无限长。
	if len([]rune(e.Msg)) > 400 {
		e.Msg = string([]rune(e.Msg)[:400]) + "…"
	}
	if len(e.KV) > 4000 {
		e.KV = string([]rune(e.KV)[:4000]) + "…"
	}
	d.enqueue(writeOp{kind: opEvent, event: e})
}

// RecordCall 记录一次调用明细。永不阻塞：队列满直接丢弃。
func (d *DB) RecordCall(c Call) {
	if c.TS.IsZero() {
		c.TS = time.Now()
	}
	if len([]rune(c.Err)) > 200 {
		c.Err = string([]rune(c.Err)[:200])
	}
	d.enqueue(writeOp{kind: opCall, call: c})
}

// RecordTarget 写入（UPSERT）一个目标的健康度快照。永不阻塞。
func (d *DB) RecordTarget(s TargetState) {
	if s.Key == "" || !strings.Contains(s.Key, "|") {
		return
	}
	d.enqueue(writeOp{kind: opTarget, target: s})
}

func (d *DB) enqueue(op writeOp) {
	// 关库之后一律丢弃，不进队列。
	//
	// 这一支不是「理论上不可能」：Close 自己在关完之后还要打一条日志，
	// logx 的 sink 会把它送回 RecordEvent。见 DB.closedMu。
	//
	// 读锁必须盖住整个 select：只判一次 closed 标志是不够的，
	// 「判完到发送之间」通道仍可能被关掉，那正是 -race 报的那个竞态。
	d.closedMu.RLock()
	defer d.closedMu.RUnlock()
	if d.closed.Load() {
		return
	}
	select {
	case d.ops <- op:
	default:
		// 队列满：丢统计不丢主流程。计数留给管理端 /api/stats/status 观测。
		if d.dropped.Add(1)%1000 == 1 {
			logx.Warn("统计库写队列已满，丢弃统计数据", "累计丢弃", d.dropped.Load())
		}
	}
}

// writerLoop 单写协程：攒批 -> 单事务落库。退出时排空残余。
func (d *DB) writerLoop() {
	defer close(d.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]writeOp, 0, flushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		d.writeBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case op, ok := <-d.ops:
			if !ok {
				// 通道关闭：把队列里剩余的全部排掉再退出
				for op := range d.ops {
					batch = append(batch, op)
					if len(batch) >= flushBatch {
						flush()
					}
				}
				flush()
				return
			}
			batch = append(batch, op)
			if len(batch) >= flushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
			//  retention 检查放这里：写协程闲着也是闲着，不用单开定时器。
			// 是否到期由 retentionDue 在 retMu 下判断——这个字段会被
			// Open 和测试从别的 goroutine 写。
			if d.retentionDue(time.Now()) {
				d.deleteExpired()
			}
		}
	}
}

func (d *DB) writeBatch(batch []writeOp) {
	tx, err := d.sql.Begin()
	if err != nil {
		logx.Warn("统计库开事务失败", "err", err.Error())
		return
	}
	for _, op := range batch {
		var err error
		switch op.kind {
		case opCall:
			err = d.insertCall(tx, op.call)
		case opTarget:
			err = d.upsertTarget(tx, op.target)
		case opEvent:
			err = d.insertEvent(tx, op.event)
		}
		if err != nil {
			// 单条失败不回滚整批：统计不是账务数据，丢一条好过丢一批
			logx.Warn("统计库写入失败（已跳过该条）", "err", err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		logx.Warn("统计库提交事务失败", "err", err.Error())
	}
}

func (d *DB) insertCall(tx *sql.Tx, c Call) error {
	day := c.TS.Format("2006-01-02")
	ok := 0
	if c.OK {
		ok = 1
	}
	_, err := tx.Exec(
		`INSERT INTO calls (ts, day, hour, endpoint_id, model, ok, latency_ms, ttft_ms, prompt_tokens, output_tokens, err)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.TS.Unix(), day, c.TS.Hour(), c.EndpointID, c.Model, ok,
		c.LatencyMS, c.TTFTMS, c.PromptTokens, c.OutputTokens, c.Err)
	return err
}

func (d *DB) insertEvent(tx *sql.Tx, e Event) error {
	_, err := tx.Exec(
		`INSERT INTO events (ts, day, cat, level, msg, grp, kv) VALUES (?,?,?,?,?,?,?)`,
		e.TS.Unix(), e.TS.Format("2006-01-02"), e.Cat, e.Level, e.Msg, e.Group, e.KV)
	return err
}

func (d *DB) upsertTarget(tx *sql.Tx, s TargetState) error {
	_, err := tx.Exec(
		`INSERT INTO target_state (key,total,fails,consec_fail,success_ewa,ttft_ema,latency_ema,last_success,last_used,last_error,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET
		   total=excluded.total, fails=excluded.fails, consec_fail=excluded.consec_fail,
		   success_ewa=excluded.success_ewa, ttft_ema=excluded.ttft_ema, latency_ema=excluded.latency_ema,
		   last_success=excluded.last_success, last_used=excluded.last_used,
		   last_error=excluded.last_error, updated_at=excluded.updated_at`,
		s.Key, s.Total, s.Fails, s.ConsecFail, s.SuccessEWA, s.TTFTMS, s.LatencyMS,
		unixOrZero(s.LastSuccess), unixOrZero(s.LastUsed), s.LastError, time.Now().Unix())
	return err
}

// LoadTargets 读取全部目标健康度快照（启动恢复用）
func (d *DB) LoadTargets() (map[string]TargetState, error) {
	rows, err := d.sql.Query(
		`SELECT key,total,fails,consec_fail,success_ewa,ttft_ema,latency_ema,last_success,last_used,last_error FROM target_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TargetState{}
	for rows.Next() {
		var s TargetState
		var lastSuccess, lastUsed int64
		if err := rows.Scan(&s.Key, &s.Total, &s.Fails, &s.ConsecFail, &s.SuccessEWA,
			&s.TTFTMS, &s.LatencyMS, &lastSuccess, &lastUsed, &s.LastError); err != nil {
			return nil, err
		}
		s.LastSuccess = timeFromUnix(lastSuccess)
		s.LastUsed = timeFromUnix(lastUsed)
		out[s.Key] = s
	}
	return out, rows.Err()
}

// DeleteTargetsNotIn 清理已不在当前配置里的孤立目标记录（改名/删除后调用）
func (d *DB) DeleteTargetsNotIn(keys []string) {
	keep := make(map[string]bool, len(keys))
	for _, k := range keys {
		keep[k] = true
	}
	existing, err := d.LoadTargets()
	if err != nil {
		logx.Warn("统计库读取目标列表失败，跳过孤立清理", "err", err.Error())
		return
	}
	for k := range existing {
		if keep[k] {
			continue
		}
		if _, err := d.sql.Exec(`DELETE FROM target_state WHERE key = ?`, k); err != nil {
			logx.Warn("统计库清理孤立目标失败", "key", k, "err", err.Error())
		} else {
			logx.Info("统计库已清理孤立目标记录", "key", k)
		}
	}
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeFromUnix(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}
