package brain

import (
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
)

func at(h, m int) time.Time { return time.Date(2026, 9, 30, h, m, 0, 0, time.Local) }

// at() 的日期是周三——这个测试整套断言都建立在「工作日」上，
// 所以先把它钉死：哪天有人改了 at() 的日子而没看这里，下面全会红得莫名其妙。
func TestAtIsAWeekday(t *testing.T) {
	t.Parallel()
	if wd := at(12, 0).Weekday(); wd != time.Wednesday {
		t.Fatalf("at() 应为周三（工作日），实际 %v。本文件的峰谷断言全依赖这一点", wd)
	}
}

// TestOnlineRateDeepSeekOffPeak 峰谷档按 DeepSeek 官方口径走。
//
// 官方（api-docs.deepseek.com/quick_start/pricing 脚注 2）：
// 峰 = 01:00-04:00 与 06:00-10:00 UTC，周一至周五，不含法定节假日；
// 其余（含周末全天）半价。折成北京时间即工作日 09:00-12:00、14:00-18:00。
//
// 这条测试替换掉的是旧断言 00:30-08:30 —— 那是 2025-02「错峰优惠活动」的时段，
// 活动早结束、定价规则也换成了峰谷定价，旧窗口在代码里躺到了 2026-10。
func TestOnlineRateDeepSeekOffPeak(t *testing.T) {
	t.Parallel()
	s := config.ScheduleConfig{Enabled: true, Mode: "deepseek_offpeak", AtGraceSec: 300}

	// 谷时段：整夜、午休、傍晚之后
	for _, hm := range [][2]int{{0, 0}, {3, 0}, {8, 59}, {12, 0}, {13, 59}, {18, 0}, {23, 59}} {
		if r, _ := OnlineRate(s, at(hm[0], hm[1])); r != 0.8 {
			t.Errorf("周三 %02d:%02d 属谷时段，应 0.80，实际 %v", hm[0], hm[1], r)
		}
	}
	// 峰时段：压到 base_rate（未配置时 0.20），且标签要说清是峰
	for _, hm := range [][2]int{{9, 0}, {11, 59}, {14, 0}, {17, 59}} {
		r, label := OnlineRate(s, at(hm[0], hm[1]))
		if r != 0.2 {
			t.Errorf("周三 %02d:%02d 属峰时段，应压到 0.20，实际 %v", hm[0], hm[1], r)
		}
		if !strings.Contains(label, "峰") {
			t.Errorf("周三 %02d:%02d 的标签 %q 没说是峰时段——日志里就靠它分辨当时贵不贵",
				hm[0], hm[1], label)
		}
	}
}

// TestDeepSeekPresetWeekendIsAllOffPeak 周末全天都是谷时段。
//
// 这是「峰窗口必须带 Days」的回归测试，也是旧写法最容易让人再踩一次的坑：
// 不带工作日限制的话，周六周日下午——群里最热闹的时候——会被当成峰压到 20%，
// 而 DeepSeek 那天根本不涨价。周末下午的机器人沉默，没人会联想到是这个原因。
func TestDeepSeekPresetWeekendIsAllOffPeak(t *testing.T) {
	t.Parallel()
	s := config.ScheduleConfig{Enabled: true, Mode: "deepseek_offpeak"}
	// 2026-10-03 是周六，2026-10-04 是周日
	for _, day := range []int{3, 4} {
		for _, h := range []int{0, 9, 10, 14, 17, 23} {
			now := time.Date(2026, 10, day, h, 0, 0, 0, time.Local)
			if r, label := OnlineRate(s, now); r != 0.8 {
				t.Errorf("%v %02d:00 周末应全天谷时段 0.80，实际 %v（%q）",
					now.Weekday(), h, r, label)
			}
		}
	}
}

func TestDaysMatch(t *testing.T) {
	t.Parallel()
	// 2026-10-07 是周三，2026-10-10 是周六，2026-10-11 是周日
	wed := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)
	sat := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	sun := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)

	cases := []struct {
		days string
		now  time.Time
		want bool
	}{
		{"", sat, true},        // 空 = 每天，旧配置语义不变
		{"1-5", wed, true},     // 工作日
		{"1-5", sat, false},    // 周末不算工作日
		{"1-5", sun, false},    // 周日是 7，不是 0——写成 0-4 的偏移就栽在这
		{"6,7", sat, true},
		{"6,7", sun, true},
		{"7", sun, true},
		{"7", wed, false},
		{"3", wed, true},
		{" 1-5 ", wed, true},   // 容忍空格
		{"mon-fri", wed, false}, // 解析不出来 → 该窗口永不命中，而不是「每天生效」
		{"0-5", wed, false},     // 0 越界
		{"5-1", wed, false},     // 反区间不绕回
		{"8", wed, false},
	}
	for _, c := range cases {
		if got := daysMatch(c.now, c.days); got != c.want {
			t.Errorf("daysMatch(%v, %q) = %v，应为 %v", c.now.Weekday(), c.days, got, c.want)
		}
	}
}

func TestOnlineRateCrossMidnight(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "night_owl", AtGraceSec: 300}
	if r, _ := OnlineRate(s, at(23, 0)); r != 0.8 {
		t.Fatalf("23:00 应在夜猫子段内, got %v", r)
	}
	if r, _ := OnlineRate(s, at(1, 0)); r != 0.8 {
		t.Fatalf("01:00 应跨午夜仍在段内, got %v", r)
	}
	if r, _ := OnlineRate(s, at(12, 0)); r != 0.2 {
		t.Fatalf("12:00 不应在段内, got %v", r)
	}
}

func TestOnlineRateDisabledIsAlwaysOn(t *testing.T) {
	s := config.ScheduleConfig{Enabled: false, Mode: "deepseek_offpeak"}
	if r, _ := OnlineRate(s, at(14, 0)); r != 1 {
		t.Fatalf("未启用应全天放行, got %v", r)
	}
}

func TestRandomDailyStableWithinDay(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "random_daily"}
	a, _ := OnlineRate(s, at(10, 0))
	b, _ := OnlineRate(s, at(20, 0))
	// 同一天不同时刻：窗口是否命中可能不同，但窗口本身必须稳定
	w1 := windowsFor(s, at(10, 0))
	w2 := windowsFor(s, at(20, 0))
	if len(w1) != 1 || w1[0].From != w2[0].From || w1[0].To != w2[0].To {
		t.Fatalf("同一天随机窗口应稳定: %#v vs %#v", w1, w2)
	}
	// 换一天应该换一段（不一定，但连续 30 天至少出现两种不同窗口）
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		d := at(10, 0).AddDate(0, 0, i)
		w := windowsFor(s, d)
		seen[w[0].From] = true
	}
	if len(seen) < 2 {
		t.Fatal("随机档应在不同天给出不同窗口")
	}
	_ = a
	_ = b
}

func TestScheduleAllowAtAndGrace(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "deepseek_offpeak", AtGraceSec: 300}
	// 白天（在线率 0.2），被 @ 永远放行
	if !ScheduleAllow(s, at(14, 0), true, false, time.Hour) {
		t.Fatal("@ 必须放行")
	}
	// 单聊放行
	if !ScheduleAllow(s, at(14, 0), false, true, time.Hour) {
		t.Fatal("单聊必须放行")
	}
	// @ 后 1 分钟内：宽限期内放行
	if !ScheduleAllow(s, at(14, 0), false, false, time.Minute) {
		t.Fatal("@ 后宽限期内应放行")
	}
	// 概率分支：用固定 roll 断言边界，不再靠 2000 次采样统计。
	// ScheduleDecide 收了 roll 入参（2026-10-03）之后这就完全确定了——
	// 以前只能统计「放行比例落在 0.13~0.27」这种宽区间，配置一改就假绿。
	if d := ScheduleDecide(s, at(14, 0), false, false, time.Hour, 0.19); !d.Allowed {
		t.Fatalf("roll 0.19 < 白天在线率 0.2，应放行（实际摇到 %.2f，档位 %q）", d.Roll, d.Label)
	}
	if d := ScheduleDecide(s, at(14, 0), false, false, time.Hour, 0.21); d.Allowed {
		t.Fatalf("roll 0.21 > 白天在线率 0.2，应拒绝（实际摇到 %.2f）", d.Roll)
	}
	// 边界相等时：roll < rate 才放行，恰好相等不算
	if d := ScheduleDecide(s, at(14, 0), false, false, time.Hour, 0.2); d.Allowed {
		t.Error("roll 恰好等于在线率时应拒绝——比较是严格小于，写成 <= 会让高在线率档失真")
	}
	// 半价时段（在线率 0.8）用同一个 roll 应该放行，证明 rate 真的随时段变
	if d := ScheduleDecide(s, at(3, 0), false, false, time.Hour, 0.5); !d.Allowed {
		t.Errorf("凌晨在线率 0.8，摇到 0.5 应放行（实际 %v）", d.Allowed)
	}
}

func TestBaseRateIsConfigurable(t *testing.T) {
	// 平时在线率必须能调：嫌它太安静就往上拧
	s := config.ScheduleConfig{Enabled: true, Mode: "deepseek_offpeak", BaseRate: 0.6}
	if r, _ := OnlineRate(s, at(14, 0)); r != 0.6 {
		t.Fatalf("平时在线率应为 0.6, got %v", r)
	}
	// 命中窗口时不受 base_rate 影响
	if r, _ := OnlineRate(s, at(3, 0)); r != 0.8 {
		t.Fatalf("半价时段应仍为 0.8, got %v", r)
	}
}

func TestDaytimePresetKeepsChatHoursAlive(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "daytime", BaseRate: 0.2}
	// 白天（大家真正在聊的时候）必须几乎全在线
	for _, h := range []int{9, 12, 15, 20, 22} {
		if r, _ := OnlineRate(s, at(h, 0)); r < 0.8 {
			t.Fatalf("%d:00 白天在线率过低: %v", h, r)
		}
	}
	// 凌晨没人说话，降到平时水平省钱
	if r, _ := OnlineRate(s, at(3, 0)); r > 0.3 {
		t.Fatalf("凌晨应降下来, got %v", r)
	}
}

func TestScheduleAllowCustomWindows(t *testing.T) {
	s := config.ScheduleConfig{
		Enabled: true, Mode: "custom", AtGraceSec: 300,
		Windows: []config.ScheduleWindow{{From: "12:00", To: "13:00", Rate: 1, Label: "午休"}},
	}
	if r, label := OnlineRate(s, at(12, 30)); r != 1 || label != "午休" {
		t.Fatalf("自定义窗口未生效: %v %q", r, label)
	}
	if !ScheduleAllow(s, at(12, 30), false, false, time.Hour) {
		t.Fatal("100% 在线率必须放行")
	}
}

// TestAlwaysPresetIs90NotFullDay always 档是 0.90，不是 1.0。
//
// 这一条是 2026-10-03 拆档的原因：always 一直叫「全天在线」，暗示 100%，
// 实际是 0.90，名字在骗人。而它此前**零测试覆盖**——8 个旧测试用的是
// deepseek_offpeak / night_owl / daytime / random_daily / custom，
// 唯一在生产上用的档位反而没人管，所以这个矛盾活了很久才被发现。
//
// 显式断言 r != 1.0 是刻意的：只断 == 0.90 的话，
// 有人把它改成 1.0 时这个测试照样红（0.9 != 1.0），但意图不如直接写明清晰。
func TestAlwaysPresetIs90NotFullDay(t *testing.T) {
	t.Parallel()
	s := config.ScheduleConfig{Enabled: true, Mode: "always", BaseRate: 0.2}
	for _, h := range []int{0, 3, 12, 22} {
		r, label := OnlineRate(s, at(h, 0))
		if r != 0.90 {
			t.Errorf("%02d:00 always 档在线率应为 0.90，实际 %v", h, r)
		}
		if r == 1.0 {
			t.Errorf("%02d:00 always 档绝不能是 1.0——它得靠 always_strict 才提供 100%%", h)
		}
		if label != "全天在" {
			t.Errorf("%02d:00 档位名应为「全天在」（旧名「全天」同样误导），实际 %q", h, label)
		}
	}
}

// TestAlwaysStrictPresetIsAlwaysOn always_strict 档真的 100% 放行。
//
// 防假绿的关键是**断言点选 roll 的上界**而不是中间值：
// rand.Float64() 返回 [0,1)，所以 0.9999999 是合法取值、必须过 1.0 的阈值。
// 若这一档的 Rate 被误改成 0.9、或比较被写成 `roll < rate - eps`、
// 或 id 拼错导致回落 daytime（白天 0.85 / 深夜 0.2），下面每一条断言都会红。
func TestAlwaysStrictPresetIsAlwaysOn(t *testing.T) {
	t.Parallel()
	s := config.ScheduleConfig{Enabled: true, Mode: "always_strict", BaseRate: 0.2}

	// 先断 Rate 本身：四个时间点都必须精确是 1.0（含 23:59 边界，
	// 证明 To:"24:00" 没有覆盖空洞——inWindow 是 cur < 1440）
	for _, hm := range [][2]int{{0, 0}, {3, 0}, {12, 0}, {23, 59}} {
		r, _ := OnlineRate(s, at(hm[0], hm[1]))
		if r != 1.0 {
			t.Fatalf("%02d:%02d always_strict 在线率应为 1.0，实际 %v"+
				"（被夹取了，或 id 拼错回落了别的档）", hm[0], hm[1], r)
		}
	}

	// 再断放行判定：上下界都要过
	for _, roll := range []float64{0, 0.5, 0.9999999} {
		d := ScheduleDecide(s, at(12, 0), false, false, time.Hour, roll)
		if !d.Allowed {
			t.Errorf("always_strict 摇到 %.7f 必须放行（在线率 %.2f）——"+
				"这一档的意义就是 100%% 必应", roll, d.Rate)
		}
		if d.Rate != 1 {
			t.Errorf("always_strict 回传的在线率应为 1，实际 %v", d.Rate)
		}
	}
}

// TestUnknownModeFallsBackToDaytime 未知档位静默回落 daytime。
//
// **这是本次最重要的防回归资产。** 它把「改名/手误 → 静默降级」从不可见陷阱
// 变成被测试钉住的明知故犯契约：将来谁想改 schedulePresets 的 key（比如把
// always 改名成 all_day），必须先来改这条测试，从而被迫意识到那是一次
// **没有报错、没有日志差异**的行为变更——现场表现只是「话变少了」。
//
// 另外 KnownScheduleMode 必须对拼错的 id 返回 false，否则启动时的告警形同虚设。
func TestUnknownModeFallsBackToDaytime(t *testing.T) {
	t.Parallel()
	for _, typo := range []string{"always_strictt", "always_typo", "全时段", "alwayz"} {
		s := config.ScheduleConfig{Enabled: true, Mode: typo, BaseRate: 0.2}
		// 白天应落回 daytime 的 0.85
		if r, _ := OnlineRate(s, at(12, 0)); r != 0.85 {
			t.Errorf("未知档位 %q 应回落到 daytime（白天 0.85），实际 %v", typo, r)
		}
		// 深夜应落回 daytime 的 0.50
		if r, _ := OnlineRate(s, at(23, 30)); r != 0.50 {
			t.Errorf("未知档位 %q 应回落到 daytime（深夜 0.50），实际 %v", typo, r)
		}
		// 启动告警依赖这个函数判断
		if KnownScheduleMode(typo) {
			t.Errorf("KnownScheduleMode(%q) 应为 false，否则启动告警永远不响", typo)
		}
	}

	// 反向：所有合法档位都该被认出来（含不在预设表里的两个特判档）
	for _, ok := range []string{"daytime", "deepseek_offpeak", "night_owl",
		"always", "always_strict", "random_daily", "custom", ""} {
		if !KnownScheduleMode(ok) {
			t.Errorf("KnownScheduleMode(%q) 应为 true，它是合法档位（空串回落 daytime）", ok)
		}
	}
}

// TestOnlineRateClampsWindowRate 自定义窗口的异常取值怎么处理。
//
// 最反直觉的一条：**rate 填 0 不是「关掉这个时段」，而是回落 base_rate**。
// 用户填 0 以为静音，实际仍有 20% 在线率——这曾经没有任何文档或测试提到。
func TestOnlineRateClampsWindowRate(t *testing.T) {
	t.Parallel()
	mk := func(rate float64) config.ScheduleConfig {
		return config.ScheduleConfig{
			Enabled: true, Mode: "custom", BaseRate: 0.35,
			Windows: []config.ScheduleWindow{{From: "00:00", To: "24:00", Rate: rate, Label: "全天"}},
		}
	}
	// 大于 1 被夹到 1，不会出现 >100% 的在线率
	if r, _ := OnlineRate(mk(1.5), at(12, 0)); r != 1 {
		t.Errorf("窗口 rate 1.5 应夹到 1.0，实际 %v", r)
	}
	// 1.0 原样保留——always_strict 档靠的就是这条
	if r, _ := OnlineRate(mk(1.0), at(12, 0)); r != 1 {
		t.Errorf("窗口 rate 1.0 应原样保留，实际 %v", r)
	}
	// 0 与负数都不是「关闭」，而是回落 base_rate
	for _, rate := range []float64{0, -0.5} {
		if r, _ := OnlineRate(mk(rate), at(12, 0)); r != 0.35 {
			t.Errorf("窗口 rate %v 不是「关闭」而是回落 base_rate(0.35)，实际 %v",
				rate, r)
		}
	}
}
