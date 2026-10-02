package brain

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"dadyumo/internal/config"
)

// 在线时段调度：让机器人像真人一样有作息，同时把钱花在便宜的时候。
//
// 两个设计选择，都是为了「不机械」：
//  1. 用概率而不是硬开关。硬开关会出现「一到 8:30 整它就不说话了」这种诡异的机械感，
//     概率在线则表现为「白天偶尔冒泡、夜里话多一点」，更像真人作息。
//  2. 被 @ 永远放行，且 @ 之后的宽限期里保持实时——被点名了还装死是最伤体验的。
//
// 注意：内置档位里的 deepseek_offpeak 虽然照着「半价 80% / 其余 20%」做，
// 但并不适合当长期默认——半价时段是 00:30-08:30，那会儿群里根本没人，
// 把白天压到 20% 只会让机器人在大家真正在聊的时候装死。
// 所以默认档是 daytime：白天几乎全在线，凌晨（本就没人）才降到平时水平。

// baseOnlineRate 没匹配到任何窗口、且配置里没给 base_rate 时的兜底在线率
const baseOnlineRate = 0.20

// 内置档位。改这里就能改管理端下拉里的选项。
var schedulePresets = map[string][]config.ScheduleWindow{
	// DeepSeek 错峰半价：北京时间 00:30-08:30（省钱优先，白天会明显安静）
	"deepseek_offpeak": {
		{From: "00:30", To: "08:30", Rate: 0.80, Label: "DeepSeek 错峰半价时段"},
	},
	// 白天为主（默认）：群里有人说话的时段保持在线，凌晨降下来
	"daytime": {
		{From: "09:00", To: "23:00", Rate: 0.85, Label: "白天活跃"},
		{From: "23:00", To: "01:00", Rate: 0.50, Label: "睡前"},
	},
	// 夜猫子：晚上和半夜话多
	"night_owl": {
		{From: "18:00", To: "02:00", Rate: 0.80, Label: "夜里活跃"},
	},
	// 全天在线（只在预算上省钱，不省时段）
	"always": {
		{From: "00:00", To: "24:00", Rate: 0.90, Label: "全天"},
	},
}

// schedulePresetNames 管理端下拉用
var schedulePresetNames = []struct{ ID, Name string }{
	{"daytime", "白天为主（默认·白天几乎全在线）"},
	{"deepseek_offpeak", "DeepSeek 错峰半价（省钱，白天只有 20%）"},
	{"night_owl", "夜猫子"},
	{"always", "全天在线"},
	{"random_daily", "每天随机（今天挑一段高活跃）"},
	{"custom", "自定义（用下面的窗口表）"},
}

// parseClock 解析 HH:MM，支持 24:00 表示当天结束
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil || h < 0 || h > 24 {
		return 0, false
	}
	m, err := strconv.Atoi(s[3:])
	if err != nil || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inWindow 判断时刻是否落在窗口内，支持跨午夜（比如 18:00-02:00）。
// from==to 视为全天。
func inWindow(now time.Time, from, to string) bool {
	f, ok1 := parseClock(from)
	t, ok2 := parseClock(to)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if f == t {
		return true
	}
	if f < t {
		return cur >= f && cur < t
	}
	// 跨午夜
	return cur >= f || cur < t
}

// windowsFor 按模式取出生效的窗口表
func windowsFor(s config.ScheduleConfig, now time.Time) []config.ScheduleWindow {
	switch s.Mode {
	case "custom", "":
		if len(s.Windows) > 0 {
			return s.Windows
		}
		return schedulePresets["daytime"]
	case "random_daily":
		return randomDailyWindows(now)
	}
	if w, ok := schedulePresets[s.Mode]; ok {
		return w
	}
	return schedulePresets["daytime"]
}

// randomDailyWindows 每天随机挑一段高活跃时段。
//
// 按日期做种子，保证同一天内每次算出来都一样——否则机器人会表现得像在抽搐：
// 这一分钟在线、下一分钟不在线。跨天才会换一段。
// 长度 3~5 小时，起点避开凌晨最便宜的半价段，让「随机」真的带来多样性。
func randomDailyWindows(now time.Time) []config.ScheduleWindow {
	y, m, d := now.Date()
	seed := int64(y*10000 + int(m)*100 + d)
	r := rand.New(rand.NewSource(seed))
	start := 14 + r.Intn(9) // 14:00 ~ 22:00 之间起步
	dur := 3 + r.Intn(3)    // 3~5 小时
	end := start + dur
	from := fmtClock(start * 60)
	to := fmtClock(end * 60)
	return []config.ScheduleWindow{{From: from, To: to, Rate: 0.85, Label: "今天的活跃时段（随机）"}}
}

func fmtClock(mins int) string {
	mins %= 24 * 60
	return strconv.Itoa(mins/60/10) + strconv.Itoa(mins/60%10) + ":" + strconv.Itoa(mins%60/10) + strconv.Itoa(mins%60%10)
}

// OnlineRate 当前时刻应有的在线率，以及命中的窗口说明（给管理端展示用）
func OnlineRate(s config.ScheduleConfig, now time.Time) (float64, string) {
	if !s.Enabled {
		return 1, "未启用在线时段调度"
	}
	// 「平时」在线率由 base_rate 控制，没配就用兜底值
	base := s.BaseRate
	if base <= 0 {
		base = baseOnlineRate
	}
	if base > 1 {
		base = 1
	}
	for _, w := range windowsFor(s, now) {
		if inWindow(now, w.From, w.To) {
			rate := w.Rate
			if rate <= 0 {
				rate = base
			}
			if rate > 1 {
				rate = 1
			}
			label := w.Label
			if label == "" {
				label = w.From + "-" + w.To
			}
			return rate, label
		}
	}
	return base, "平时（非高峰）"
}

// ScheduleDecision 一次在线时段判定的完整结果。
//
// 为什么要把摇到的值带出来：**只返回布尔是没用的**。
// 「当前时段不在线」这条日志如果不带 rate 和 label，下次复现不了——
// 同样的配置、同样的时间，下次可能就中了，排查会卡在这里。
// 带上「摇到 0.73 < 阈值 0.85」才看得见当时到底发生了什么。
type ScheduleDecision struct {
	Allowed bool
	// Rate 是本次生效的在线率阈值（0~1）。
	Rate float64
	// Roll 是本次摇到的随机值（0~1）。仅在真正摇了的情况下有意义。
	Roll float64
	// Label 是命中的窗口说明，如「白天活跃」「平时（非高峰）」。
	Label string
	// Why 说明为什么是这个结果：enabled / atMe / c2c / grace / roll
	Why string
}

// ScheduleAllow 这一轮是否被允许说话。
//
// 放行顺序（越靠前越优先）：
//  1. 未启用 → 放行
//  2. 被 @ / 被点名 → 放行（@ 之后的宽限期内同样一律放行）
//  3. 单聊 → 放行（私聊没有「水群成本」这回事）
//  4. 其余按时段概率摇一次
func ScheduleAllow(s config.ScheduleConfig, now time.Time, atMe, isC2C bool, sinceAtHit time.Duration) bool {
	return ScheduleDecide(s, now, atMe, isC2C, sinceAtHit).Allowed
}

// ScheduleDecide 同 ScheduleAllow，但返回完整判定过程供日志使用。
func ScheduleDecide(s config.ScheduleConfig, now time.Time, atMe, isC2C bool, sinceAtHit time.Duration) ScheduleDecision {
	if !s.Enabled {
		return ScheduleDecision{Allowed: true, Rate: 1, Why: "未启用时段调度"}
	}
	if atMe {
		return ScheduleDecision{Allowed: true, Rate: 1, Why: "被点名"}
	}
	if isC2C {
		return ScheduleDecision{Allowed: true, Rate: 1, Why: "私聊"}
	}
	grace := time.Duration(s.AtGraceSec) * time.Second
	if grace > 0 && sinceAtHit >= 0 && sinceAtHit < grace {
		return ScheduleDecision{Allowed: true, Rate: 1,
			Why: fmt.Sprintf("点名宽限期内（剩 %v）", grace-sinceAtHit)}
	}
	rate, label := OnlineRate(s, now)
	roll := rand.Float64()
	return ScheduleDecision{
		Allowed: roll < rate,
		Rate:    rate,
		Roll:    roll,
		Label:   label,
		Why:     "按时段概率",
	}
}
