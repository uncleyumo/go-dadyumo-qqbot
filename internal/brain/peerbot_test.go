package brain

import "testing"

// 同群第二台机器人的降频闸：rate 的两个端点各是一句话，必须严格成立。
//
// 为什么单独测这个纯函数：闸门本身在 fire() 里，放行后会一路走到 decide()，
// 而测试用的 Engine 没有 router，构造不出「必被这道闸拦下」的现场
// （gates_test.go 的注释里记着同样的坑）。抽成纯函数就能把判定本身钉死，
// 剩下的「闸门有没有被调用」交给源码检查。
func TestPeerBotAllowed(t *testing.T) {
	cases := []struct {
		name string
		rate float64
		roll float64
		want bool
	}{
		{"默认值 1 = 和改动前完全一样", 1, 0, true},
		{"默认值 1 对任何摇到的数都放行", 1, 0.999999, true},
		{"0 = 对方再也叫不动它", 0, 0, false},
		{"0 对任何摇到的数都不放行", 0, 0.999999, false},
		{"0.25 摇中下限放行", 0.25, 0, true},
		{"0.25 摇到 0.249 放行", 0.25, 0.249, true},
		{"0.25 摇到 0.25 不放行（边界归不放行）", 0.25, 0.25, false},
		{"0.25 摇到 0.9 不放行", 0.25, 0.9, false},
		// 配置写错时的兜底方向：负数当「闭嘴」，大于 1 当「不限制」。
		// 两种都不该把机器人卡死或让它彻底失语。
		{"负数是配置写错，按不放行处理", -1, 0, false},
		{"大于 1 是配置写错，按放行处理", 2, 0.99, true},
	}
	for _, c := range cases {
		if got := peerBotAllowed(c.rate, c.roll); got != c.want {
			t.Errorf("%s：peerBotAllowed(rate=%v, roll=%v) = %v，想要 %v",
				c.name, c.rate, c.roll, got, c.want)
		}
	}
}

// 放行率必须真的能改变结果，而不是恰好都撞在同一侧。
//
// 上一条表测只覆盖了端点与边界，理论上一个「rate<0.5 返回 true、否则 false」的
// 错误实现也能全绿。这里用同一批摇到的数跑两遍不同 rate，检查结果确实分叉。
func TestPeerBotRateChangesOutcome(t *testing.T) {
	rolls := []float64{0.01, 0.1, 0.3, 0.5, 0.7, 0.99}
	var low, high int
	for _, r := range rolls {
		if peerBotAllowed(0.25, r) {
			low++
		}
		if peerBotAllowed(0.75, r) {
			high++
		}
	}
	if high <= low {
		t.Errorf("放行率从 0.25 提到 0.75，放行数没变多（%d → %d）——闸门没真正吃这个参数", low, high)
	}
	if low == 0 || high == len(rolls) {
		t.Errorf("两档都退化成恒真/恒假（low=%d high=%d，共 %d 个数），等于参数无效",
			low, high, len(rolls))
	}
}
