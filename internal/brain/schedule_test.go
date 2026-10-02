package brain

import (
	"testing"
	"time"

	"dadyumo/internal/config"
)

func at(h, m int) time.Time { return time.Date(2026, 9, 30, h, m, 0, 0, time.Local) }

func TestOnlineRateDeepSeekOffPeak(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "deepseek_offpeak", AtGraceSec: 300}
	// 半价时段内：00:30-08:30
	if r, _ := OnlineRate(s, at(3, 0)); r != 0.8 {
		t.Fatalf("半价时段应 0.8, got %v", r)
	}
	// 半价时段外
	if r, _ := OnlineRate(s, at(14, 0)); r != 0.2 {
		t.Fatalf("非半价应 0.2, got %v", r)
	}
	// 边界：08:29 在、08:30 不在
	if r, _ := OnlineRate(s, at(8, 29)); r != 0.8 {
		t.Fatalf("08:29 应在半价内, got %v", r)
	}
	if r, _ := OnlineRate(s, at(8, 30)); r != 0.2 {
		t.Fatalf("08:30 应出半价, got %v", r)
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
	// 统计：白天非 @ 且无宽限，放行比例应接近 20%
	allow := 0
	const n = 2000
	for i := 0; i < n; i++ {
		if ScheduleAllow(s, at(14, 0), false, false, time.Hour) {
			allow++
		}
	}
	p := float64(allow) / float64(n)
	if p < 0.13 || p > 0.27 {
		t.Fatalf("白天放行比例应约 0.2, got %.2f", p)
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
