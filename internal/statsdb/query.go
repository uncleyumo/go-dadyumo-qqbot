package statsdb

import (
	"strconv"
	"time"
)

// ModelStat 一个模型在一个时间桶里的聚合
type ModelStat struct {
	Calls        int64 `json:"calls"`
	Fails        int64 `json:"fails"`
	PromptTokens int64 `json:"prompt_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Bucket 一个时间桶（一分钟 / 一小时 / 一天）里各模型的聚合
type Bucket struct {
	Label  string               `json:"label"` // 展示用："14:05"（分钟）/"14"（小时）/"09-30"（日期）
	Minute int                  `json:"minute,omitempty"`
	Hour   int                  `json:"hour,omitempty"`
	Day    string               `json:"day,omitempty"`
	Total  int64                `json:"total"`
	Models map[string]ModelStat `json:"models"`
}

// HourlyToday 今日 0-23 点逐小时 × 模型聚合。24 个桶全量返回（无数据的桶 Total=0），
// 前端不用自己补缺口。
func (d *DB) HourlyToday() ([]Bucket, error) {
	day := time.Now().Format("2006-01-02")
	rows, err := d.sql.Query(
		`SELECT hour, model, COUNT(*), SUM(1-ok), SUM(prompt_tokens), SUM(output_tokens)
		 FROM calls WHERE day = ? GROUP BY hour, model`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make([]Bucket, 24)
	for h := 0; h < 24; h++ {
		buckets[h] = Bucket{Label: strconv.Itoa(h), Hour: h, Day: day, Models: map[string]ModelStat{}}
	}
	for rows.Next() {
		var hour int
		var model string
		var st ModelStat
		if err := rows.Scan(&hour, &model, &st.Calls, &st.Fails, &st.PromptTokens, &st.OutputTokens); err != nil {
			return nil, err
		}
		if hour < 0 || hour > 23 {
			continue
		}
		buckets[hour].Models[model] = st
		buckets[hour].Total += st.Calls
	}
	return buckets, rows.Err()
}

// MinutelyN 最近 n 分钟（含当前这一分钟）× 模型聚合，按时间升序，空分钟也返回。
//
// 为什么是「最近 n 分钟」而不是「今日逐分钟」：一天 1440 个桶画在 720px 里
// 每根柱子不足 1 像素，既看不清也没有意义；而机器人调用频率本就不高，
// 分钟粒度只有看「刚刚这段时间谁在跑」才有用。
//
// ts 存的是 unix 秒，直接 ts/60 就是分钟序号，不需要改表结构。
func (d *DB) MinutelyN(n int) ([]Bucket, error) {
	if n <= 0 {
		n = 60
	}
	cur := time.Now().Truncate(time.Minute)
	start := cur.Add(-time.Duration(n-1) * time.Minute)

	rows, err := d.sql.Query(
		`SELECT ts/60 AS m, model, COUNT(*), SUM(1-ok), SUM(prompt_tokens), SUM(output_tokens)
		 FROM calls WHERE ts >= ? GROUP BY m, model`, start.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make([]Bucket, n)
	idx := make(map[int64]int, n)
	for i := 0; i < n; i++ {
		m := start.Add(time.Duration(i) * time.Minute)
		idx[m.Unix()/60] = i
		buckets[i] = Bucket{
			Label:  m.Format("15:04"),
			Minute: m.Minute(),
			Models: map[string]ModelStat{},
		}
	}
	for rows.Next() {
		var min int64
		var model string
		var st ModelStat
		if err := rows.Scan(&min, &model, &st.Calls, &st.Fails, &st.PromptTokens, &st.OutputTokens); err != nil {
			return nil, err
		}
		i, ok := idx[min]
		if !ok {
			continue // 超出窗口（理论上不会，防时钟抖动）
		}
		buckets[i].Models[model] = st
		buckets[i].Total += st.Calls
	}
	return buckets, rows.Err()
}

// CallRow 一条调用明细（管理端「最近调用」用，精确到秒）
type CallRow struct {
	TS           time.Time `json:"ts"`
	EndpointID   string    `json:"endpoint_id"`
	Model        string    `json:"model"`
	OK           bool      `json:"ok"`
	LatencyMS    int64     `json:"latency_ms"`
	TTFTMS       int64     `json:"ttft_ms"`
	PromptTokens int       `json:"prompt_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Err          string    `json:"err"`
}

// RecentCalls 最近 limit 条调用明细，最新的在前。
// 柱状图看的是「分布」，这张表看的是「刚刚到底发生了什么」——
// 排查「为什么这次走了兜底模型」时，比任何聚合都有用。
func (d *DB) RecentCalls(limit int) ([]CallRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 30
	}
	rows, err := d.sql.Query(
		`SELECT ts, endpoint_id, model, ok, latency_ms, ttft_ms, prompt_tokens, output_tokens, err
		 FROM calls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CallRow, 0, limit)
	for rows.Next() {
		var ts int64
		var ok int
		var r CallRow
		if err := rows.Scan(&ts, &r.EndpointID, &r.Model, &ok, &r.LatencyMS, &r.TTFTMS,
			&r.PromptTokens, &r.OutputTokens, &r.Err); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		r.OK = ok != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// DailyN 近 n 天逐日 × 模型聚合（含今天），按日期升序，无数据的日子也返回空桶。
func (d *DB) DailyN(n int) ([]Bucket, error) {
	if n <= 0 {
		n = 7
	}
	now := time.Now()
	first := now.AddDate(0, 0, -(n - 1)).Format("2006-01-02")
	rows, err := d.sql.Query(
		`SELECT day, model, COUNT(*), SUM(1-ok), SUM(prompt_tokens), SUM(output_tokens)
		 FROM calls WHERE day >= ? GROUP BY day, model`, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	idx := map[string]int{}
	buckets := make([]Bucket, 0, n)
	for i := n - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		idx[day] = len(buckets)
		buckets = append(buckets, Bucket{
			Label:  now.AddDate(0, 0, -i).Format("01-02"),
			Day:    day,
			Models: map[string]ModelStat{},
		})
	}
	for rows.Next() {
		var day, model string
		var st ModelStat
		if err := rows.Scan(&day, &model, &st.Calls, &st.Fails, &st.PromptTokens, &st.OutputTokens); err != nil {
			return nil, err
		}
		i, ok := idx[day]
		if !ok {
			continue
		}
		buckets[i].Models[model] = st
		buckets[i].Total += st.Calls
	}
	return buckets, rows.Err()
}
