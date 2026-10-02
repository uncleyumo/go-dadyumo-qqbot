package brain

import (
	"testing"
	"time"

	"dadyumo/internal/config"
)

// 发言节奏的判据：**间隔不能写死**。
//
// 肉眼看「像不像人在打字」，只有条与条之间的间隔能暴露这一点。首字延迟
// 在本项目里意义有限——攒批窗口 8 秒加 LLM 调用（实测均延迟 4.4 秒）
// 决定了首字最快也在 12 秒后，几百毫秒淹没在里面。
//
// 所以这组测试全部围绕 segDelay：不测「等于某个数」，只测「同一个输入
// 摇多次是不是不同值」。写死间隔的实现在这里会立刻现形。

// newPacingCfg 用真人档参数构造配置，与生产一致。
func newPacingCfg() config.Config {
	cfg := *config.Default()
	cfg.Speak.MinDelayMS = 900
	cfg.Speak.MaxDelayMS = 2600
	cfg.Speak.PerCharMS = 150
	cfg.Speak.EagerScale = 0.5
	cfg.Speak.TailRamp = 0.18
	return cfg
}

// TestSegDelayIsNotConstant 同一个输入连摇 N 次必须给出 N 个不同的间隔。
//
// 这是整个步骤的核心断言。原来 segDelay 虽然有 rand，但 per_char 之外的部分
// 在 min==max 时恒定；更重要的是——「递减系数」如果写成确定值，
// 即使基础间隔在摇，整条曲线仍然是可预测的。
func TestSegDelayIsNotConstant(t *testing.T) {
	cfg := newPacingCfg()
	const prev = "你看看你这都什么玩意儿"

	seen := map[time.Duration]int{}
	for i := 0; i < 200; i++ {
		seen[segDelay(cfg, prev, false, 1, 3)]++
	}
	if len(seen) < 50 {
		t.Errorf("200 次采样只得到 %d 个不同间隔，间隔基本是写死的。"+
			"真人每次发消息的间隔都不一样，这是最容易被察觉的机器特征", len(seen))
	}
}

// TestSegDelayStaysInRange 间隔必须落在配置给出的区间内，且按字数递增。
func TestSegDelayStaysInRange(t *testing.T) {
	cfg := newPacingCfg()

	for _, prev := range []string{"6", "这是一条比较长的消息，用来验证打字时间确实按字数叠加", ""} {
		for i := 0; i < 50; i++ {
			d := segDelay(cfg, prev, false, 1, 1)
			lo := time.Duration(cfg.Speak.MinDelayMS) * time.Millisecond
			hi := time.Duration(cfg.Speak.MaxDelayMS+len([]rune(prev))*cfg.Speak.PerCharMS) * time.Millisecond
			if d < lo || d > hi {
				t.Fatalf("字数 %d、间隔 %v，超出区间 [%v, %v]", len([]rune(prev)), d, lo, hi)
			}
		}
	}
}

// TestPerCharAddsTypingTime 同样随机下，字多的那条间隔必须更长。
//
// 不比单次值（那会 flaky），而是各自摇 100 次比均值。
func TestPerCharAddsTypingTime(t *testing.T) {
	cfg := newPacingCfg()
	short := "6"
	long := "这是一条很长的消息你慢慢看我要花更多时间来打完这些字"

	avg := func(s string) time.Duration {
		var sum time.Duration
		const n = 200
		for i := 0; i < n; i++ {
			sum += segDelay(cfg, s, false, 1, 1)
		}
		return sum / n
	}
	as, al := avg(short), avg(long)
	// 期望差值 = 字数差 × per_char，允许 30% 误差
	want := time.Duration((len([]rune(long))-len([]rune(short)))*cfg.Speak.PerCharMS) * time.Millisecond
	got := al - as
	if got < want*7/10 || got > want*13/10 {
		t.Errorf("长消息比短消息多等 %v，期望约 %v（字数差 %d × %dms）。"+
			"「按字数打字」这个直觉必须成立",
			got, want, len([]rune(long))-len([]rune(short)), cfg.Speak.PerCharMS)
	}
}

// TestEagerShortensDelay 被点名时整体提前，这是「有人在等」的档位。
func TestEagerShortensDelay(t *testing.T) {
	cfg := newPacingCfg()
	const prev = "你看看你"

	avg := func(eager bool) time.Duration {
		var sum time.Duration
		const n = 200
		for i := 0; i < n; i++ {
			sum += segDelay(cfg, prev, eager, 1, 1)
		}
		return sum / n
	}
	a, e := avg(false), avg(true)
	if e >= a {
		t.Errorf("eager 档平均 %v 反而 >= 插话档 %v，缩放没生效", e, a)
	}
	// 缩放系数 0.5，允许 30% 误差
	if e > time.Duration(float64(a)*0.7) {
		t.Errorf("eager 档均值 %v，相对插话档 %v 缩得不够狠（eager_scale=%.2f，应该约在 %.0f%% 左右），"+
			"eager_scale 是不是没生效", e, a, cfg.Speak.EagerScale, float64(e)/float64(a)*100)
	}
}

// TestTailRampShortensLaterSegments 多条连发时，后面的间隔必须更短。
//
// 这是对「5 条一条一条慢慢爬」的对策：不砍条数上限（那是限制模型发挥），
// 而让尾部加速。真人发消息是越说越急的。
func TestTailRampShortensLaterSegments(t *testing.T) {
	cfg := newPacingCfg()
	const prev = "这是一条挺长的消息"

	avg := func(idx, total int) time.Duration {
		var sum time.Duration
		const n = 200
		for i := 0; i < n; i++ {
			sum += segDelay(cfg, prev, false, idx, total)
		}
		return sum / n
	}
	first, last := avg(1, 5), avg(4, 5)
	if last >= first {
		t.Errorf("第 4 条间隔 %v 不比第 1 条 %v 短，TailRamp 没生效——"+
			"5 条固定间隔就是「一条一条慢慢爬」，比秒回更像机器人", last, first)
	}
	// 单调性不能是死板的：随机幅度之内，第 2 条偶尔可以比第 1 条还慢，
	// 但整体趋势必须是递减。
	if last > first*8/10 {
		t.Errorf("第 4 条 %v 相对第 1 条 %v 缩得不够，TailRamp=%.2f 太小",
			last, first, cfg.Speak.TailRamp)
	}
}

// TestTailRampIsRandomNotDeterministic 递减系数本身必须带随机。
//
// 这一条是被生产问题逼出来的：递减如果写成确定的（idx=3 固定 ×0.46），
// 那么「基础间隔在摇」这件事救不了它——整条曲线仍然可预测，
// 而可预测正是机器人感的来源。
//
// **难点：不能直接测 segDelay 的输出。** 它里面的基础间隔本来就在摇，
// 无论系数是否确定，200 次都能摇出几十个不同的值——第一版就栽在这，
// 变异后测试照样全绿（假绿）。
//
// 正解是**把基础随机关掉**，只留递减系数这一个变量：
// 让 min==max 则 rand 部分贡献为 0，此时输出完全由系数决定，
// 系数确定 ⇒ 输出唯一；系数随机 ⇒ 输出有分布。
func TestTailRampIsRandomNotDeterministic(t *testing.T) {
	cfg := newPacingCfg()
	// 关掉基础间隔的随机：min==max 时 d=base + rand(0) ，不再有抖动
	cfg.Speak.MinDelayMS = 1000
	cfg.Speak.MaxDelayMS = 1000
	cfg.Speak.PerCharMS = 0 // 每字叠加也去掉，只留系数这一个变量
	const prev = "消息"

	seen := map[time.Duration]int{}
	for i := 0; i < 200; i++ {
		seen[segDelay(cfg, prev, false, 3, 5)]++
	}
	if len(seen) < 20 {
		t.Errorf("基础随机关掉后，idx=3 摇 200 次只有 %d 个不同值："+
			"递减系数是写死的。真人第 3 条等了多久每次都不一样，"+
			"而可预测正是机器人感的来源", len(seen))
	}
}

// TestRampFloorKeepsLastSegmentApart 最后一条与倒数第二条之间必须还有间隔。
//
// 系数递减到 0 会让两条同一毫秒发出去，平台把它们合并成一条，
// 那模型本来想分开发的东西（比如「一句文字 + 一张图」）就没了。
func TestRampFloorKeepsLastSegmentApart(t *testing.T) {
	cfg := newPacingCfg()
	// 极端配置：递减拉到上限 0.9
	cfg.Speak.TailRamp = 0.9
	const prev = "长消息"

	for i := 0; i < 100; i++ {
		d := segDelay(cfg, prev, false, 4, 5)
		if d <= 0 {
			t.Fatalf("间隔归零了：平台会把两条消息合并成一条，"+
				"模型本来想分开发的文字+表情包就没了")
		}
		if d < 100*time.Millisecond {
			t.Errorf("间隔只有 %v，已经小到看不出是两条消息了", d)
		}
	}
}

// TestRampDisabledIsUniform 单条发言不该有递减。
func TestRampDisabledIsUniform(t *testing.T) {
	cfg := newPacingCfg()
	cfg.Speak.TailRamp = 0
	a := segDelay(cfg, "一样长度的消息内容", false, 1, 5)
	b := segDelay(cfg, "一样长度的消息内容", false, 4, 5)
	if a != b {
		// 关掉递减时两者应该在同一区间内随机，不是逐位相同也不是必然不同
		// 这里只要求它们都落在合法区间（真正的随机性由 TestSegDelayIsNotConstant 守）
		lo := time.Duration(cfg.Speak.MinDelayMS) * time.Millisecond
		if a < lo || b < lo {
			t.Errorf("关掉 TailRamp 后间隔越界：%v %v", a, b)
		}
	}
}