package llm

import (
	"math"
	"time"
)

// EWMA 半衰期：约 20 次调用后旧数据权重衰减到一半，保证排序能跟上接入点的实时状态变化
const ewmaAlpha = 0.05

// 打分归一化上限
const (
	ttftCeilMS    = 8000  // 首字延迟超过 8s 视为一样差
	latencyCeilMS = 30000 // 端到端超过 30s 视为一样差
)

// Health 单个调用目标（接入点 × 模型）的健康度
type Health struct {
	TTFTMS     float64 // 首字延迟 EWMA（毫秒）
	LatencyMS  float64 // 端到端延迟 EWMA（毫秒）
	SuccessEWA float64 // 成功率 EWMA，0~1，初始 0.5（中性）

	Total      int64 // 累计调用次数
	Fails      int64 // 累计失败次数
	ConsecFail int   // 连续失败次数

	// EverSucceeded 有没有至少成功过一次。
	// 必须和「延迟类指标是否为 0」分开：TTFTMS<=0 有两种来源——从没探索过，
	// 或者调了很多次全失败（延迟只在成功时更新）。当成一回事的话，
	// 一个成功率 0% 的坏目标会按「未探索的新模型」拿满分延迟分。
	EverSucceeded bool

	CooldownUntil time.Time // 冷却截止
	LastUsed      time.Time
	LastSuccess   time.Time
	LastError     string
	Dead          bool // 被判定为不可用（如模型 404），需人工或探测恢复
}

func newHealth() *Health {
	return &Health{SuccessEWA: 0.5}
}

// Cooling 是否处于冷却中
func (h *Health) Cooling(now time.Time) bool {
	return now.Before(h.CooldownUntil)
}

// CooldownRemaining 冷却剩余时间
func (h *Health) CooldownRemaining(now time.Time) time.Duration {
	d := h.CooldownUntil.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// observeSuccess 记录一次成功
func (h *Health) observeSuccess(ttftMS, latencyMS float64, now time.Time) {
	h.Total++
	h.ConsecFail = 0
	h.EverSucceeded = true
	h.observe(1, ttftMS, latencyMS)
	h.LastSuccess = now
	h.LastError = ""
}

// observeFailure 记录一次失败，并返回应冷却的时长
func (h *Health) observeFailure(errMsg string, now time.Time) time.Duration {
	h.Total++
	h.Fails++
	h.ConsecFail++
	h.observe(0, 0, 0)
	h.LastError = errMsg
	// 指数退避：5s → 10s → 20s … 上限 5 分钟
	backoff := time.Duration(5*(1<<min(h.ConsecFail-1, 6))) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	h.CooldownUntil = now.Add(backoff)
	return backoff
}

// observe 更新 EWMA。kind=1 成功，kind=0 失败。
// 延迟类指标只在成功时更新，避免把「快速失败」误判为「快速响应」。
func (h *Health) observe(kind float64, ttftMS, latencyMS float64) {
	h.SuccessEWA = ewma(h.SuccessEWA, kind)
	if kind == 1 {
		if h.TTFTMS == 0 {
			h.TTFTMS = ttftMS
			h.LatencyMS = latencyMS
		} else {
			h.TTFTMS = ewma(h.TTFTMS, ttftMS)
			h.LatencyMS = ewma(h.LatencyMS, latencyMS)
		}
	}
}

func ewma(old, sample float64) float64 {
	return old + ewmaAlpha*(sample-old)
}

// Scale 归一化区间。延迟类指标采用「候选集内相对归一化」而不是绝对上限：
// 绝对上限（如首字 8s）在实测数据面前毫无意义——线上入库的首字延迟其实是
// 含生成过程的总耗时（reasoning 模型动辄 7~40 秒），400ms 与 0ms 的差距
// 会被随机抖动盖过，排序就失去意义。
type Scale struct {
	TTFTMin, TTFTMax float64
	LatMin, LatMax   float64
	HasData          bool // 候选集中是否已有成功样本
}

// Score 健康度打分，越高越优先。权重来自配置。
// 未探索过的目标给额外加成，保证新接入的模型和接入点一定会被试到。
func (h *Health) Score(w Weight, now time.Time, explored bool, s Scale) float64 {
	ttftScore := 0.5
	switch {
	case h.EverSucceeded && s.TTFTMax > s.TTFTMin:
		// 成功过的目标一律走归一化，哪怕样本恰好是 0。
		// 用 TTFTMS>0 当门槛会让「快到测不出耗时」的目标被当成没数据，
		// 只能拿中性 0.5 分，反而输给唯一进得了归一化区间的慢目标。
		ttftScore = 1 - clamp01((h.TTFTMS-s.TTFTMin)/(s.TTFTMax-s.TTFTMin))
	case h.Total == 0 && s.HasData:
		// 从没试过的新目标：按「当前最快水平」乐观估值，
		// 否则它会因为拿中性分而永远输给「已知很慢但至少有数据」的目标，
		// 新接入的模型就再也没机会被探索。
		ttftScore = 1
	case !h.EverSucceeded:
		// 试过、一次都没成功：没有任何延迟样本可评，给 0 而不是中性 0.5。
		// 给中性分会让它靠这两项白拿 0.30，反超「已知很慢但确实能用」的目标
		// （后者在相对归一化里是区间最慢端、本就得 0），
		// 于是冷却一过又被顶到第一位，继续浪费调用。0 分才符合事实。
		ttftScore = 0
	}
	latencyScore := 0.5
	switch {
	case h.EverSucceeded && s.LatMax > s.LatMin:
		latencyScore = 1 - clamp01((h.LatencyMS-s.LatMin)/(s.LatMax-s.LatMin))
	case h.Total == 0 && s.HasData:
		latencyScore = 1
	case !h.EverSucceeded:
		latencyScore = 0
	}
	cooldownPenalty := math.Min(h.CooldownRemaining(now).Minutes(), 1.0)

	score := w.TTFT*ttftScore +
		w.Failure*h.SuccessEWA +
		w.Latency*latencyScore -
		w.Cooldown*cooldownPenalty

	if !explored {
		score += 0.15 // 冷启动探索加成
	}
	return score
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// computeScale 基于一组健康快照算出相对归一化区间。
// 只统计成功过的目标（everSucceeded），避免从未成功的样本把区间拉歪。
// 门槛不能用 TTFT>0：快到耗时测出来是 0 的目标会被踢出去，
// 于是最慢的那个目标成了区间里唯一的样本、归一化后拿满分。
//
// 注意：非流式目标的 TTFT 是用总耗时顶替的（见 client.go 的 result），
// 所以全量非流式时 TTFTMin/Max 与 LatMin/Max 是同一组数。
// Reload 在这种情况下会把 TTFT 权重并进 Latency，这里保留 TTFT 区间的计算，
// 是为了开流式后不用再改这里。
func computeScale(hs []Health) Scale {
	var s Scale
	init := false
	for _, h := range hs {
		if !h.EverSucceeded {
			continue
		}
		s.HasData = true
		if !init {
			s.TTFTMin, s.TTFTMax = h.TTFTMS, h.TTFTMS
			s.LatMin, s.LatMax = h.LatencyMS, h.LatencyMS
			init = true
			continue
		}
		if h.TTFTMS < s.TTFTMin {
			s.TTFTMin = h.TTFTMS
		}
		if h.TTFTMS > s.TTFTMax {
			s.TTFTMax = h.TTFTMS
		}
		if h.LatencyMS < s.LatMin {
			s.LatMin = h.LatencyMS
		}
		if h.LatencyMS > s.LatMax {
			s.LatMax = h.LatencyMS
		}
	}
	// 区间过于接近时人为拉开，让微小差异也能体现到排序上
	if s.TTFTMax-s.TTFTMin < 50 {
		s.TTFTMax = s.TTFTMin + 50
	}
	if s.LatMax-s.LatMin < 50 {
		s.LatMax = s.LatMin + 50
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Weight 排序权重
type Weight struct {
	TTFT     float64
	Failure  float64
	Latency  float64
	Cooldown float64
}
