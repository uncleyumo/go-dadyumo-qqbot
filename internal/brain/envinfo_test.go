package brain

import (
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

func TestSpecialDayName(t *testing.T) {
	// 疯狂星期四：2026-10-01 是周四？先构造确定的周四 2026-10-08
	thu := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	if got := specialDayName(thu, nil); got != "疯狂星期四" {
		t.Fatalf("周四应为疯狂星期四, got %q", got)
	}
	national := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	if got := specialDayName(national, nil); got != "国庆节" {
		t.Fatalf("10-01 应为国庆节, got %q", got)
	}
	// 自定义纪念日覆盖内置
	if got := specialDayName(national, map[string]string{"10-01": "放假第一天"}); got != "放假第一天" {
		t.Fatalf("自定义纪念日应覆盖内置, got %q", got)
	}
	// 普通周一
	mon := time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local)
	if got := specialDayName(mon, nil); got != "" {
		t.Fatalf("普通周一应为空, got %q", got)
	}
}

func TestComposeEnvLine(t *testing.T) {
	now := time.Date(2026, 9, 30, 2, 6, 43, 0, time.Local) // 周三
	env := EnvInfo{special: "疯狂星期四", weather: "23°C 多云"}
	line := composeEnvLine(now, env, "示例市")
	for _, want := range []string{"2026年9月30日", "02:06:43", "周三", "疯狂星期四", "示例市", "23°C 多云"} {
		if !strings.Contains(line, want) {
			t.Fatalf("环境行缺少 %q: %s", want, line)
		}
	}
	// 什么都不配时也不该崩
	if line := composeEnvLine(now, EnvInfo{}, ""); strings.Contains(line, "你在") {
		t.Fatalf("未配地名不应出现地点: %s", line)
	}
}

func TestWeatherDesc(t *testing.T) {
	cases := map[int]string{0: "晴", 2: "多云", 61: "雨", 95: "雷雨", 71: "雪", 999: "天知道什么天气"}
	for code, want := range cases {
		if got := weatherDesc(code); got != want {
			t.Fatalf("code %d: want %q got %q", code, want, got)
		}
	}
}

func TestSystemPromptHasEnv(t *testing.T) {
	cfg := *config.Default()
	cfg.Brain.WeatherPlace = "示例市"
	g := memory.NewGroup("x", "测试群")
	s := systemPrompt(cfg, g, MoodSignal{}, "", "现在时间：2026年9月30日 02:06:43（周三）", "")
	if !strings.Contains(s, "02:06:43") {
		t.Fatal("系统提示词应包含环境行")
	}
	// 环境行必须在最尾部（动态段），前面的固定段不能含时间
	idx := strings.Index(s, "现在时间：")
	if idx < len(s)/2 {
		t.Fatalf("环境行应位于动态段（后 1/2），实际位置 %d/%d", idx, len(s))
	}
}
