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
// 注意：内置档位里的 deepseek_offpeak 虽然照着「谷时段 80% / 峰时段 20%」做，
// 但并不适合当长期默认——峰时段是工作日 09:00-12:00 与 14:00-18:00，
// 正好是群里真正在聊的时候，把这几段压到 20% 只会让机器人在大家说话时装死。
// 所以默认档是 daytime：白天几乎全在线，凌晨（本就没人）才降到平时水平。
//
// 3. 「全天」有两个档，别混：
//     always        = 0.90，日常档，**仍有 10% 的概率不接话**
//     always_strict = 1.00，调试/特殊场景，任何时候都必应
//     历史教训：always 曾经叫「全天在线」，暗示 100%，实际是 0.90——名字在骗人，
//     而且它零测试覆盖，所以这个矛盾活了很久才被发现。

// baseOnlineRate 没匹配到任何窗口、且配置里没给 base_rate 时的兜底在线率
const baseOnlineRate = 0.20

// 内置档位。改这里就能改管理端下拉里的选项。
var schedulePresets = map[string][]config.ScheduleWindow{
	// DeepSeek 峰谷定价（官方口径，2026-08-17 起）：高峰 = 01:00-04:00 与
	// 06:00-10:00 UTC，**只算工作日**（周一至周五，法定节假日除外）；其余时间
	// 一律半价，含周末全天。折成北京时间即峰 = 09:00-12:00、14:00-18:00。
	//
	// 这里曾写成 00:30-08:30 —— 那是 2025-02 的「错峰优惠活动」时段，活动早结束、
	// 定价规则也换了一轮，但这条窗口在代码里躺到了 2026-10。现场表现很隐蔽：
	// 选了这个档的人会发现机器人在中午和晚上（真正在聊的时候）沉默，午夜反倒话多。
	//
	// 峰窗口必须带 Days：不限工作日的话，**周末下午**会被当成峰压到 20%，
	// 而 DeepSeek 那天根本不涨价——这是最容易让人再踩一次的坑。
	// 法定节假日无法在本地判断（没有日历），工作日节假日会按峰处理，是已知偏差。
	//
	// 顺序即优先级：OnlineRate 取第一个命中的窗口，所以两条窄的峰窗口必须排在
	// 兜底的全天谷窗口之前。调换顺序 = 改行为，别动。
	//
	// 峰时段 Rate 填 0 是**故意的**：0 表示「回落 base_rate」，即「贵的时候压到
	// 平时水平」（默认 0.20），这样 base_rate 这个旋钮对这个档依然有效。
	"deepseek_offpeak": {
		{From: "09:00", To: "12:00", Days: "1-5", Rate: 0, Label: "DeepSeek 峰时段（全价）"},
		{From: "14:00", To: "18:00", Days: "1-5", Rate: 0, Label: "DeepSeek 峰时段（全价）"},
		{From: "00:00", To: "24:00", Rate: 0.80, Label: "DeepSeek 谷时段（半价）"},
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
	// 全天必应（调试 / 特殊场景用）：Rate 1.0。
	// rand.Float64() 返回 [0,1)，所以 roll < 1.0 恒真——这是真·全天在线。
	// 别拿它当长期默认：它不受任何时段约束，等于关掉 CONTRIBUTING 里
	// 「它必须能闭嘴」那条硬约束，账单会随群活跃度线性涨。
	"always_strict": {
		{From: "00:00", To: "24:00", Rate: 1.0, Label: "全天必应"},
	},
	// 全天在（日常档）：全天 0.90，仍然有 10% 的概率闭嘴。
	//
	// 2026-10-03 拆档：这一档原来叫「全天在线」，暗示 100%，实际 0.90，
	// 名字在骗人。拆成 always_strict(1.0) + always(0.9)。
	//
	// 保留 always 这个 id 不改名，是为了现网 config.json：写的是 "always"，
	// 改名会让它变成未知值然后被 windowsFor 静默回落 daytime（0.85/0.50）——
	// 一次没有报错、没有日志差异的行为变更，比 90% 变 100% 难查得多。
	"always": {
		{From: "00:00", To: "24:00", Rate: 0.90, Label: "全天在"},
	},
}

// 这里曾有一个 schedulePresetNames（管理端下拉用的名称表），2026-10-03 删掉：
// 它零读取点，是块死字段——管理端下拉实际是 index.html 里硬编码的 <option>。
// 留着它会制造「我改了 Go 这张表所以下拉同步了」的错觉，而真实同步点根本不在这里。
//
// 下拉与本表的同步由 preset_test.go 的双向相等测试守住（读 index.html 校对）。

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
// from==to 视为全天。窗口带 Days 时还要当天星期命中。
func inWindow(now time.Time, w config.ScheduleWindow) bool {
	if !daysMatch(now, w.Days) {
		return false
	}
	f, ok1 := parseClock(w.From)
	t, ok2 := parseClock(w.To)
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

// daysMatch 判断 now 的星期是否落在 days 描述里，days 为空表示每天都算。
//
// 语法：逗号分隔的 N 或 N-M，N 取 1~7，**1=周一 … 7=周日**（ISO 口径）。
// 刻意不直接吃 Go 的 time.Weekday：它把周日算成 0，于是 "1-5" 会变成
// 「周日到周四」——一位偏移，且偏移后覆盖面看着还挺合理，最难发现，所以这里显式换算。
//
// 解析不出来的写法一律返回 false（该窗口永不命中），与 parseClock 对非法
// from/to 的处理保持一致：宁可这个窗口不生效，也不要让手误的 "mon-fri"
// 被当成「每天生效」——那在管理端是看不出来的。
func daysMatch(now time.Time, days string) bool {
	days = strings.TrimSpace(days)
	if days == "" {
		return true
	}
	wd := int(now.Weekday())
	if wd == 0 {
		wd = 7 // 周日
	}
	for _, part := range strings.Split(days, ",") {
		if lo, hi, ok := parseWeekdayRange(strings.TrimSpace(part)); ok && wd >= lo && wd <= hi {
			return true
		}
	}
	return false
}

// parseWeekdayRange 解析 "3" 或 "1-5" 为闭区间 [lo,hi]。非法返回 ok=false。
func parseWeekdayRange(s string) (lo, hi int, ok bool) {
	loStr, hiStr := s, s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		loStr, hiStr = s[:i], s[i+1:]
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(loStr))
	b, err2 := strconv.Atoi(strings.TrimSpace(hiStr))
	if err1 != nil || err2 != nil || a < 1 || a > 7 || b < 1 || b > 7 || a > b {
		return 0, 0, false
	}
	return a, b, true
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
	// 未知 mode 静默回落 daytime：进程照常启动，只是换了档位。
	// 这本身是刻意的（配置写错不该让机器人起不来），但**改动 schedulePresets 的
	// key 时必须想到历史 config.json 里已经写死的那些 id**——改了就是静默降级，
	// 现场表现是「话变少了」，没人会想到是档位名失效了。
	// TestUnknownModeFallsBackToDaytime 把这个行为钉成契约，改名前先去看它。
	return schedulePresets["daytime"]
}

// KnownScheduleMode 报告一个档位 id 是否是本程序认识的。
//
// 存在的理由：windowsFor 对未知 mode 是**静默回落 daytime** 的（配置写错不该让
// 机器人起不来，这是刻意行为）。但静默的代价是：改名或手误在现场表现成
// 「话变少了」，没人会想到是档位名失效了。所以启动时必须显式校验一次。
//
// 注意 random_daily 与 custom 不在 schedulePresets 里（由 windowsFor 的 switch 特判），
// 但它们同样是合法档位，所以这里要一并算作已知。
func KnownScheduleMode(mode string) bool {
	// 空串是合法的：Validate 之前 config.json 里没有 mode 字段就是空的，
	// windowsFor 也把 "" 归到 custom 分支（windows 为空则回落 daytime）。
	// 所以这里必须返回 true，否则全新配置启动时会误报「档位名不认识」。
	if mode == "" {
		return true
	}
	if mode == "random_daily" || mode == "custom" {
		return true
	}
	_, ok := schedulePresets[mode]
	return ok
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
		if inWindow(now, w) {
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
//
// 这是 2026-10-03 起**唯一**的频率闸（冲动值闸门整套被废除）：它答的是
// 「这会儿人在不在电脑前」，而这正是真人的真实状态——人不会每条消息都回，
// 因为人有时候就没在看屏幕。要随机数只是为了省调用，不是为了决定「值不值得回」。
func ScheduleAllow(s config.ScheduleConfig, now time.Time, atMe, isC2C bool, sinceAtHit time.Duration) bool {
	return ScheduleDecide(s, now, atMe, isC2C, sinceAtHit, rand.Float64()).Allowed
}

// ScheduleDecide 同 ScheduleAllow，但返回完整判定过程供日志使用。
//
// roll 是 0~1 的随机数，**由调用方传入**。这个设计不是为了测试方便：
// 它是这套机制唯一的可观测窗口——日志里记了摇到的值，排查「为什么它今天
// 特别话多」时只有这两个数字可用。随机源藏在函数内部时，日志记的 roll
// 与实际比较的值无法对齐，排查就变成了猜。
//
// 副作用：同输入可复现了，测试不必再靠 2000 次采样统计放行率落在某个宽区间。
// 过去就因为不可复现，测试被迫分成「直接调函数」+「os.ReadFile 去 grep
// 源码字符串」两半（见 decisionlog_test.go 里的说明）。
func ScheduleDecide(s config.ScheduleConfig, now time.Time, atMe, isC2C bool,
	sinceAtHit time.Duration, roll float64) ScheduleDecision {
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
	return ScheduleDecision{
		Allowed: roll < rate,
		Rate:    rate,
		Roll:    roll,
		Label:   label,
		Why:     "按时段概率",
	}
}
