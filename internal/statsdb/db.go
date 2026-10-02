// Package statsdb 把 LLM 调用明细与目标健康度持久化到 SQLite。
//
// 设计约束：
//   - 纯 Go 驱动（modernc.org/sqlite），CGO_ENABLED=0 交叉编译 linux/amd64；
//   - 写入热路径（Router.Chat）永不阻塞：所有写都进内存队列，
//     由单写 goroutine 批量落库；队列满直接丢弃并计数，统计丢了不影响主流程；
//   - 任何数据库错误只记日志，绝不 panic、绝不向调用方返回错误。
package statsdb

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"dadyumo/internal/logx"

	_ "modernc.org/sqlite"
)

// Call 一次调用的明细。Token 为 0 表示上游未上报（不要拿估算值填，口径见 schema 注释）。
type Call struct {
	TS           time.Time
	EndpointID   string
	Model        string
	OK           bool
	LatencyMS    int64
	TTFTMS       int64
	PromptTokens int
	OutputTokens int
	Err          string // 落库前截断到 200 字符
}

// TargetState 目标健康度的可持久子集。
// CooldownUntil / Dead 故意不落盘：重启后给目标一次重新证明自己的机会，
// 避免一次 404 或一次网络抖动留下的状态把目标永久压住。
type TargetState struct {
	Key         string // endpoint_id|model
	Total       int64
	Fails       int64
	ConsecFail  int
	SuccessEWA  float64
	TTFTMS      float64
	LatencyMS   float64
	LastSuccess time.Time
	LastUsed    time.Time
	LastError   string
}

const schema = `
CREATE TABLE IF NOT EXISTS calls (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            INTEGER NOT NULL,          -- unix 秒
  day           TEXT    NOT NULL,          -- 本地时区 YYYY-MM-DD，聚合加速
  hour          INTEGER NOT NULL,          -- 0-23 本地时区
  endpoint_id   TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  ok            INTEGER NOT NULL,
  latency_ms    INTEGER NOT NULL DEFAULT 0,
  ttft_ms       INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0, -- 0 = 上游未上报
  output_tokens INTEGER NOT NULL DEFAULT 0,
  err           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_calls_day_hour ON calls(day, hour);
CREATE INDEX IF NOT EXISTS idx_calls_model_day ON calls(model, day);
-- ts 单列索引：deleteExpired 与 MinutelyN 都按 ts 范围过滤，而上面两个索引
-- 都以 day 打头，帮不上忙。没有它时这两条查询只能全表扫描 calls——
-- 90 天明细几十万行，SetMaxOpenConns(1) 下管理端每刷一次图表就独占
-- 唯一连接扫一遍，写协程全程排队。
CREATE INDEX IF NOT EXISTS idx_calls_ts ON calls(ts);

CREATE TABLE IF NOT EXISTS target_state (
  key          TEXT PRIMARY KEY,           -- endpoint_id|model
  total        INTEGER NOT NULL DEFAULT 0,
  fails        INTEGER NOT NULL DEFAULT 0,
  consec_fail  INTEGER NOT NULL DEFAULT 0,
  success_ewa  REAL    NOT NULL DEFAULT 0.5,
  ttft_ema     REAL    NOT NULL DEFAULT 0,
  latency_ema  REAL    NOT NULL DEFAULT 0,
  last_success INTEGER NOT NULL DEFAULT 0,
  last_used    INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT    NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL DEFAULT 0
);

-- 表情包池。图片本体在 MinIO，这里只存元数据。
--
-- 为什么单独一张表而不是塞进 memory.json：那张文件是给「模型要读的文字记忆」
-- 用的，格式契约是 {"k":"v"}，塞二进制元数据会破坏它。
--
-- expires_at 是硬淘汰时间。到点无条件踢出，**不管分数多高**——
-- 「永远好用」意味着新表情永远有机会进来，没有永不过期的旧图占位。
CREATE TABLE IF NOT EXISTS memes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  object_key  TEXT    NOT NULL UNIQUE,  -- MinIO 对象名
  descr       TEXT    NOT NULL,          -- 模型给的描述，检索与选择全靠它
  added_at    INTEGER NOT NULL,         -- unix 秒
  uses        INTEGER NOT NULL DEFAULT 0,
  last_used   INTEGER NOT NULL DEFAULT 0,
  quality     REAL    NOT NULL DEFAULT 0.5, -- 优选任务给的评分 0~1
  expires_at  INTEGER NOT NULL          -- unix 秒，到期必淘汰
);
CREATE INDEX IF NOT EXISTS idx_memes_expires ON memes(expires_at);
CREATE INDEX IF NOT EXISTS idx_memes_added   ON memes(added_at);

-- 事件日志。跟 calls 表的差别：calls 只记「模型调用」，这个记「发生了什么」。
--
-- 为什么必须落盘：内存环形缓冲只有 500 条，重启即失忆。
-- 而「它上周为什么不理我」这种问题恰恰要翻几天前的记录——
-- 管理端现在只能看到最近 200 条，等于问「为什么」时已经没得问了。
--
-- kv 存紧凑 JSON 而不是展开成列：kv 的键是发散的（现在就有 40+ 种，
-- 且各包各自为政），展开成列意味着每加一个字段就要改表结构 + 改查询。
-- 存 JSON、按需在 Go 侧过滤是更小更稳的做法。
CREATE TABLE IF NOT EXISTS events (
  id    INTEGER PRIMARY KEY AUTOINCREMENT,
  ts    INTEGER NOT NULL,             -- unix 秒
  day   TEXT    NOT NULL,             -- 与 calls 表同口径，聚合用
  cat   TEXT    NOT NULL DEFAULT '',  -- decision/speak/chat/... 空=未分类
  level TEXT    NOT NULL,             -- DEBUG/INFO/WARN/ERROR
  msg   TEXT    NOT NULL,
  grp   TEXT    NOT NULL DEFAULT '',  -- 群名/别名。列名叫 grp 而不是 group
                                    -- 因为 GROUP 是 SQL 保留字，不加引号会直接建表失败
  kv    TEXT    NOT NULL DEFAULT ''   -- kv 序列化后的紧凑 JSON
);
CREATE INDEX IF NOT EXISTS idx_events_ts   ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_day  ON events(day);
CREATE INDEX IF NOT EXISTS idx_events_cat  ON events(cat, ts);
`

// retention 明细保留时长。 calls 表是逐条明细，机器人调用频率不高，
// 90 天的量对 SQLite 来说毫无压力；再久的数据对「最近哪个模型好用」没有参考价值。
const retention = 90 * 24 * time.Hour

// eventRetention 日志保留时长。
//
// 比 calls 短得多，因为量级完全不同：生产实测 7 天 1040 条群消息、
// 423 条发言，加上决策链路后每天几千条。30 天足够翻查「它上周为什么不理我」，
// 再久的数据既没人查，也会把 SQLite 撑大到影响 opens。
const eventRetention = 30 * 24 * time.Hour

// DB 统计库句柄
type DB struct {
	sql  *sql.DB
	path string

	ops     chan writeOp
	done    chan struct{}
	dropped atomic.Int64 // 队列满被丢弃的写操作数

	// closedMu 把「往 ops 发送」和「close(ops)」互斥起来。
	//
	// 为什么必须有它：关库本身要打一条日志（「统计库已关闭」），
	// 而 logx 的落盘 sink 会把每条日志回灌到 RecordEvent——
	// 于是「关库日志」正好落在「通道已关」之后，enqueue 往关闭的
	// 通道发送，直接 panic: send on closed channel。
	// 2026-10-02 生产实况：每次 systemctl restart 都崩在这，
	// systemd 记 status=2/INVALIDARGUMENT，退出码非 0。
	//
	// 为什么是锁而不是一个 atomic 标志：标志只能缩小窗口，消不掉。
	// 「读到 false → 被调度出去 → Close 关通道 → 醒来发送」这个
	// 交错照样成立，go test -race 会直接报 chansend/closechan 竞态
	// （2026-10-02 实测）。必须让发送与关闭在时间上真正不重叠。
	//
	// 用 RWMutex 的读侧而不是写侧：写侧只有 Close 一处，读侧是所有
	// enqueue。读锁不互斥，吞吐不受影响；enqueue 的 select 带
	// default、绝不阻塞，所以持读锁的时长是有界的。
	closedMu sync.RWMutex
	closed   atomic.Bool // 只读不写的判定标志；幂等与「该丢就丢」用

	// retMu 保护下面两个 retention 字段。
	// deleteExpired 既会被写协程调用（writerLoop 的定时分支），也会被
	// Open 和测试从别的 goroutine 直接调；lastRetention 早先是裸字段，
	// go test -race 直接报 data race（db.go 的写 vs writer.go 的读）。
	retMu          sync.Mutex
	lastRetention  time.Time // 上次清理时刻
	retentionFails int       // 连续失败次数，用于指数退避
}

// Open 打开（不存在则创建）统计库并启动写协程。
func Open(path string) (*DB, error) {
	// SQLite 不会自动建父目录：不先 MkdirAll，sql.Open 不报错（它只是
	// 记下 DSN 懒连接），但紧接着的 Exec(schema) 会直接失败。而仓里
	// 唯一的 MkdirAll 在 memory.SaveTo 里，要等第一次 flush（5 分钟后）
	// 才发生——远晚于这里。首次部署会把统计功能永久降级，且只在日志里
	// 留下一行看不出所以然的 SQL 错误。
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	// 单连接：写全部走写协程串行执行，天然避免 SQLITE_BUSY。
	// 注意代价：虽然开了 WAL（读不阻塞写），但只有一条连接，
	// 任何查询和任何写入仍然互相串行——管理端一次全表扫描期间
	// 写队列会持续堆积（队列满即丢统计）。所以 calls.ts 上必须有索引，
	// 查询不能退化成全表扫描。
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schema); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	d := &DB{
		sql:  sqlDB,
		path: path,
		ops:  make(chan writeOp, 1024),
		done: make(chan struct{}),
	}
	d.deleteExpired()
	go d.writerLoop()
	logx.Info("统计库已打开", "path", path)
	return d, nil
}

// Close 排空队列、写完残余数据后关库。进程退出时必须调用。
//
// closed 必须**先**置位再 close(ops)：反过来的话，中间那一瞬还可能有
// 写操作涌进来（关库日志自己就是一条，见 DB.closed 的说明）。
// 用 CompareAndSwap 而不是 Store：它同时把「重复 Close」变成 no-op，
// 而 close 一个已关闭的通道会 panic——测试里的 t.Cleanup 和业务代码
// 都可能各关一次。
func (d *DB) Close() error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil // 已经关过了
	}
	d.closedMu.Lock()
	close(d.ops)
	d.closedMu.Unlock()
	<-d.done
	return d.sql.Close()
}

// Dropped 返回因队列满被丢弃的写操作总数（可观测降级程度）
func (d *DB) Dropped() int64 { return d.dropped.Load() }

// SizeBytes 数据库文件大小（含 WAL 估算），用于管理端展示
func (d *DB) SizeBytes() int64 {
	var total int64
	for _, p := range []string{d.path, d.path + "-wal"} {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		}
	}
	return total
}

// retentionInterval 正常情况下的清理间隔；retentionBackoffMax 是连续失败时的退避上限。
const (
	retentionInterval    = 24 * time.Hour
	retentionBackoffMax  = 30 * time.Minute
	retentionBackoffBase = 1 * time.Minute
)

// retentionDue 判断是否该清理。必须在 retMu 下读 lastRetention。
func (d *DB) retentionDue(now time.Time) bool {
	d.retMu.Lock()
	defer d.retMu.Unlock()
	return now.After(d.lastRetention)
}

// lastRetentionSnap / retentionFailsSnap / setRetentionFails / forceRetention
// 是给测试用的状态访问器：走 retMu 读写，避免测试自己写出裸访问的 data race。
func (d *DB) lastRetentionSnap() time.Time {
	d.retMu.Lock()
	defer d.retMu.Unlock()
	return d.lastRetention
}

func (d *DB) retentionFailsSnap() int {
	d.retMu.Lock()
	defer d.retMu.Unlock()
	return d.retentionFails
}

func (d *DB) setRetentionFails(n int) {
	d.retMu.Lock()
	d.retentionFails = n
	d.retMu.Unlock()
}

func (d *DB) forceRetention(t time.Time) {
	d.retMu.Lock()
	d.lastRetention = t
	d.retMu.Unlock()
}

// retentionBackoff 连续失败 fails 次后的下次重试间隔：1m→2m→4m…封顶 30m。
func retentionBackoff(fails int) time.Duration {
	if fails < 1 {
		fails = 1
	}
	shift := fails - 1
	if shift > 16 {
		shift = 16 // 防溢出；实际早被封顶截断
	}
	d := retentionBackoffBase << uint(shift)
	if d > retentionBackoffMax {
		d = retentionBackoffMax
	}
	return d
}

// deleteExpired 清理超过保留期的明细；错误只记日志，但绝不静默地把清理周期卡住。
func (d *DB) deleteExpired() {
	start := time.Now()

	d.retMu.Lock()
	fails := d.retentionFails
	d.retMu.Unlock()

	ok := false
	// 无论成败都要把 lastRetention 推到未来。早先这里是「出错直接 return、
	// 成功才写 lastRetention」，一旦 DELETE 持续失败它就永远停在零值，
	// time.Since(零值) 恒 > 24h，于是写协程每 200ms 重跑一次全表 DELETE，
	// 顺带每秒 5 条 Warn 把日志刷爆——真正的故障信息反而被淹掉。
	// 失败时改用指数退避（1m→2m→4m…封顶 30m）而不是 24h：
	// 一个持续报错的 DELETE 不该把清理彻底停摆一天。
	defer func() {
		d.retMu.Lock()
		if ok {
			d.retentionFails = 0
			d.lastRetention = start.Add(retentionInterval)
		} else {
			d.retentionFails = fails + 1
			d.lastRetention = start.Add(retentionBackoff(d.retentionFails))
		}
		d.retMu.Unlock()
	}()

	cutoff := start.Add(-retention).Unix()
	res, err := d.sql.Exec(`DELETE FROM calls WHERE ts < ?`, cutoff)
	if err != nil {
		logx.Warn("统计库清理过期明细失败", "err", err.Error(), "连续失败", fails+1)
		return
	}
	// 日志单独清理：保留期不同、失败也不该让另一张表的清理停摆
	evCutoff := start.Add(-eventRetention).Unix()
	evRes, evErr := d.sql.Exec(`DELETE FROM events WHERE ts < ?`, evCutoff)
	ok = true
	total, _ := res.RowsAffected()
	if evErr != nil {
		logx.Warn("统计库清理过期日志失败", "err", evErr.Error())
	} else {
		n, _ := evRes.RowsAffected()
		total += n
	}
	if total > 0 {
		logx.Info("统计库已清理过期数据", "条数", total)
		if _, err := d.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			logx.Warn("统计库 WAL checkpoint 失败", "err", err.Error())
		}
	}
}
