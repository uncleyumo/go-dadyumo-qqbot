package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

// EnvInfo 「世界状态」：特殊日子 + 天气。
//
// 设计原则（用户定的）：时间是活的，每次发言都取当下；
// 特殊日子和天气一天一变，换日后的第一次发言时懒刷新一次就够——
// 没必要为此跑定时器，也没必要每次都查天气 API。
type EnvInfo struct {
	day     string // 已刷新到的日期（YYYY-MM-DD）
	special string // 今天是什么日子，空串表示普通一天
	weather string // 天气一句话，空串表示没查到/未启用
}

// builtinHolidays 内置的公历节日。农历节日（春节中秋端午）每年日期都不同，
// 没有农历转换库就不硬算，用户可以在 brain.special_days 里手动补。
var builtinHolidays = map[string]string{
	"01-01": "元旦",
	"02-14": "情人节",
	"03-08": "妇女节",
	"04-01": "愚人节",
	"05-01": "劳动节",
	"05-04": "青年节",
	"06-01": "儿童节",
	"08-08": "父亲节",
	"09-10": "教师节",
	"10-01": "国庆节",
	"12-24": "平安夜",
	"12-25": "圣诞节",
}

// specialDayName 今天是什么日子。内置节日 + 用户自定义纪念日 + 星期规律。
func specialDayName(now time.Time, extra map[string]string) string {
	md := now.Format("01-02")
	if name, ok := extra[md]; ok && name != "" {
		return name
	}
	if name, ok := builtinHolidays[md]; ok {
		return name
	}
	switch now.Weekday() {
	case time.Thursday:
		return "疯狂星期四"
	case time.Saturday, time.Sunday:
		return "周末"
	}
	return ""
}

// weatherDesc Open-Meteo 的 WMO weather code 转人话。
// 只挑常见的，生僻编码（比如极光）在内陆城市一辈子遇不上一次。
func weatherDesc(code int) string {
	switch {
	case code == 0:
		return "晴"
	case code <= 3:
		return "多云"
	case code == 45 || code == 48:
		return "雾"
	case code >= 51 && code <= 57:
		return "毛毛雨"
	case code >= 61 && code <= 67:
		return "雨"
	case code >= 71 && code <= 77:
		return "雪"
	case code >= 80 && code <= 82:
		return "阵雨"
	case code >= 95 && code <= 99:
		return "雷雨"
	default:
		return "天知道什么天气"
	}
}

// fetchWeather 查一次实时天气（Open-Meteo，免费免 key）。
// 失败返回空串——天气只是调味料，查不到不影响说话。
func fetchWeather(ctx context.Context, lat, lon float64) string {
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&current=temperature_2m,weather_code&timezone=Asia%%2FShanghai",
		lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 8 * time.Second}
	rsp, err := client.Do(req)
	if err != nil {
		logx.Debug("天气查询失败", "err", err.Error())
		return ""
	}
	defer func() { _ = rsp.Body.Close() }()
	if rsp.StatusCode != http.StatusOK {
		logx.Debug("天气查询失败", "http", rsp.StatusCode)
		return ""
	}
	var body struct {
		Current struct {
			Temperature2M float64 `json:"temperature_2m"`
			WeatherCode   int     `json:"weather_code"`
		} `json:"current"`
	}
	if err := json.NewDecoder(rsp.Body).Decode(&body); err != nil {
		return ""
	}
	return fmt.Sprintf("%.0f°C %s", body.Current.Temperature2M, weatherDesc(body.Current.WeatherCode))
}

// refreshEnv 换日后懒刷新一次特殊日子与天气。
// 时间不在这里管——时间必须每次都取当下。
func (e *Engine) refreshEnv(now time.Time, cfg config.Config) {
	day := now.Format("2006-01-02")
	e.envMu.Lock()
	defer e.envMu.Unlock()
	if e.env.day == day {
		return
	}
	e.env.day = day
	e.env.special = specialDayName(now, cfg.Brain.SpecialDays)
	e.env.weather = ""
	if cfg.Brain.WeatherPlace != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e.env.weather = fetchWeather(ctx, cfg.Brain.WeatherLat, cfg.Brain.WeatherLon)
	}
	logx.Info("环境信息已刷新", "day", day, "special", e.env.special, "weather", e.env.weather)
}

// envSnapshot 取环境快照（副本），避免锁外读
func (e *Engine) envSnapshot() EnvInfo {
	e.envMu.Lock()
	defer e.envMu.Unlock()
	return e.env
}

// weekdayCN 星期几的中文叫法
func weekdayCN(now time.Time) string {
	return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(now.Weekday())]
}

// composeEnvLine 组装进提示词的环境行。时间是每次现取的（精确到秒），
// 日子和天气用快照。整块放在系统提示词的最尾部，不破坏 prompt 缓存前缀。
func composeEnvLine(now time.Time, env EnvInfo, place string) string {
	line := fmt.Sprintf("现在时间：%d年%d月%d日 %s（%s）",
		now.Year(), int(now.Month()), now.Day(), now.Format("15:04:05"), weekdayCN(now))
	if env.special != "" {
		line += "，今天" + env.special
	}
	if place != "" {
		line += "\n你在" + place + "（你就住这儿，本地人）"
	}
	if env.weather != "" {
		line += "\n天气：" + env.weather + "（今早看的，现在可能变了）"
	}
	return line
}
