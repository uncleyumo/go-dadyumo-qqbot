package statsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dadyumo/internal/logx"
)

// 事件日志查询。
//
// 单独成文件而不是塞进 query.go：查询面（usage_series / recent_calls / daily）
// 是聚合与图表用的，这边是「翻日志」，判据、排序、参数都不一样，
// 混在一起两边都不好读。

// EventFilter 事件日志的查询条件。
//
// 用结构体而不是一串位置参数：参数已经有五个（时间、分类、级别、关键字、条数），
// 再加就会开始写 `QueryEvents(since, "", "", "", 200, ...)` 这种
// 靠记忆对应顺序的调用。
type EventFilter struct {
	// Since 是起始时间（含）。零值表示不限。
	Since time.Time
	// Cats 是分类白名单，空表示不限。
	// 支持「未分类」：传 "runtime" 时匹配空 cat 的那些行。
	Cats []string
	// Levels 是级别白名单，空表示不限。
	Levels []string
	// Q 是关键字，对 msg 与 kv 一起做 LIKE 匹配。
	Q string
	// Limit 返回条数。<=0 或超过 MaxEventLimit 时按 MaxEventLimit 夹。
	Limit int
}

// MaxEventLimit 单次查询的硬上限。
//
// 必须有：管理端是 GET，任何人都能构造 ?limit=999999，
// 而 events 表按 ts 有索引但仍会扫出几十万行把唯一连接占死
// （SetMaxOpenConns(1)，见 db.go 里 idx_calls_ts 那条注释讲的是同一个坑）。
const MaxEventLimit = 1000

// EventRow 一条事件日志。KV 在读出时就反序列化成 map，
// 因为前端要按字段渲染而不是显示一坨 JSON 字符串。
type EventRow struct {
	TS    time.Time      `json:"ts"`
	Cat   string         `json:"cat"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Group string         `json:"group,omitempty"`
	KV    map[string]any `json:"kv,omitempty"`
}

// QueryEvents 按条件查事件日志，最新的在前。
func (d *DB) QueryEvents(f EventFilter) ([]EventRow, error) {
	limit := f.Limit
	if limit <= 0 || limit > MaxEventLimit {
		limit = 200
	}
	var where []string
	var args []any

	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.Unix())
	}
	if len(f.Cats) > 0 {
		ph := make([]string, 0, len(f.Cats))
		for _, c := range f.Cats {
			// runtime 是「未分类」的显示名，落库时是空串。
			// 这里做一次翻译，前端就不必知道这个内部表示。
			if c == "runtime" || c == "" {
				ph = append(ph, "?")
				args = append(args, "")
				continue
			}
			ph = append(ph, "?")
			args = append(args, c)
		}
		where = append(where, "cat IN ("+strings.Join(ph, ",")+")")
	}
	if len(f.Levels) > 0 {
		ph := make([]string, 0, len(f.Levels))
		for _, l := range f.Levels {
			ph = append(ph, "?")
			args = append(args, strings.ToUpper(l))
		}
		where = append(where, "level IN ("+strings.Join(ph, ",")+")")
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		// LIKE 的通配符必须转义，否则用户搜 "%" 会匹配全部、
		// 搜 "_" 会匹配任意单字符——那不是搜索，那是全表扫描。
		where = append(where, "(msg LIKE ? ESCAPE '\\' OR kv LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like)
	}
	sqlStr := `SELECT ts, cat, level, msg, grp, kv FROM events`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.sql.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]EventRow, 0, limit)
	for rows.Next() {
		var ts int64
		var kvRaw sql.NullString
		var r EventRow
		if err := rows.Scan(&ts, &r.Cat, &r.Level, &r.Msg, &r.Group, &kvRaw); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		if kvRaw.Valid && kvRaw.String != "" {
			// 反序列化失败不能让它炸掉整次查询——一条脏数据不该让整个日志页打不开
			if m, err := decodeKV(kvRaw.String); err == nil {
				r.KV = m
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountEvents 返回匹配条件的总条数（忽略 Limit）。
//
// 单独提供是因为日志页要显示「共 N 条」——
// 只按 LIMIT 出来的条数显示会让用户以为那就是全部。
func (d *DB) CountEvents(f EventFilter) (int, error) {
	f.Limit = 0
	var where []string
	var args []any
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.Unix())
	}
	if len(f.Cats) > 0 {
		ph := make([]string, 0, len(f.Cats))
		for _, c := range f.Cats {
			if c == "runtime" || c == "" {
				c = ""
			}
			ph = append(ph, "?")
			args = append(args, c)
		}
		where = append(where, "cat IN ("+strings.Join(ph, ",")+")")
	}
	if len(f.Levels) > 0 {
		ph := make([]string, 0, len(f.Levels))
		for _, l := range f.Levels {
			ph = append(ph, "?")
			args = append(args, strings.ToUpper(l))
		}
		where = append(where, "level IN ("+strings.Join(ph, ",")+")")
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		where = append(where, "(msg LIKE ? ESCAPE '\\' OR kv LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like)
	}
	sqlStr := `SELECT COUNT(*) FROM events`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := d.sql.QueryRow(sqlStr, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DecodeKV 解析事件日志的 kv 列。导出给写入侧复用，保证两边格式一致。
func DecodeKV(s string) (map[string]any, error) { return decodeKV(s) }

func decodeKV(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("kv 解析失败: %w", err)
	}
	return m, nil
}

// escapeLike 转义 LIKE 里的通配符。
// 默认转义符是反斜杠，所以这里补上它自己。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// EventCats 返回 events 表里实际出现过的分类及其条数。
//
// 管理端下拉用：与其在前端硬编码一份分类清单，不如报实际有的，
// 这样「库里有什么就能筛什么」，不会因为加了一个分类而前端选不到。
func (d *DB) EventCats() (map[string]int, error) {
	rows, err := d.sql.Query(
		`SELECT cat, COUNT(*) FROM events GROUP BY cat ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		if c == "" {
			c = "runtime"
		}
		out[c] += n
	}
	return out, rows.Err()
}

// LogSink 返回一个把 logx.Entry 转成 events 行的回调，
// 交给 logx.SetSink 装上。
//
// 单独成函数而不是让调用方自己拼：Entry→Event 的映射有若干约定
// （时间格式、cat 空值、kv 的 JSON 化、group 从 kv 里挑出来），
// 这些必须和读取侧（QueryEvents 里的 runtime 翻译）保持一致，
// 放在一处才不会走偏。
func (d *DB) LogSink() func(logx.Entry) {
	return func(e logx.Entry) {
		kvJSON := ""
		group := ""
		if len(e.KV) > 0 {
			if b, err := json.Marshal(e.KV); err == nil {
				kvJSON = string(b)
			}
			// group 提出来单独存：SQL 侧没法在 JSON 里 WHERE，
			// 而「只看某个群的日志」是最常见的筛选之一。
			if g, ok := e.KV["group"]; ok {
				if s, ok := g.(string); ok {
					group = s
				}
			}
		}
		d.RecordEvent(Event{
			TS:    parseLogTS(e.TS),
			Cat:   string(e.Cat),
			Level: e.Level,
			Msg:   e.Msg,
			Group: group,
			KV:    kvJSON,
		})
	}
}

// parseLogTS 解析 logx 的时间戳。
//
// logx 用的是 "2006-01-02 15:04:05.000"（见 outputCat），
// 本地时区。解析失败就退回零值——那会让这条记录被保留期
// 立刻清掉，但总比让整个 sink 崩掉、把后面所有日志都丢掉强。
func parseLogTS(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05.000", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}