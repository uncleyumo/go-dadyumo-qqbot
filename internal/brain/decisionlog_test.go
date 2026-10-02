package brain

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
)

// 决策链路必须看得见。
//
// 2026-10 生产实况：群里有人说话，机器人一声不吭，用户完全不知道原因。
// 查下来发现——**记录一直都有**，fire() 的六道闸全部记在 logx.Debug 上，
// 而生产默认 Info 级（QQPAL_LOG_LEVEL 没设），Debug 在那里等于不存在。
//
// 这组测试盯的是「决策日志必须带 cat=decision 且在 Info 级以上」，
// 以及「闸门日志必须带齐能复现的参数」。参数不全的日志等于没有：
// 「冲动值不足」看不出差多少，「不在在线时段」更是没法复现（那是概率判定）。

// countDecisions 数最近日志里的 decision 条目数。
//
// 用增量而不是绝对值：logx 的环形缓冲是包级共享的，同包里别的用例
// 留下的决策条目会被数进来。「这一轮新增了几条」才是要断言的东西。
func countDecisions() int {
	n := 0
	for _, e := range logx.Recent(400) {
		if e.Cat == logx.CatDecision {
			n++
		}
	}
	return n
}

// findDecision 从最近日志里找带指定关键字的 decision 类条目。
func findDecision(t *testing.T, kw string) logx.Entry {
	t.Helper()
	for _, e := range logx.Recent(400) {
		if e.Cat == logx.CatDecision && strings.Contains(e.Msg, kw) {
			return e
		}
	}
	t.Fatalf("最近日志里找不到 decision 类且含 %q 的条目。决策链路不可见 = "+
		"「它为什么不回」永远查不出来", kw)
	return logx.Entry{}
}

// newGateEngine 造一个只用来跑决策闸门的 Engine。
//
// 复用 toolloop_test.go 里已有的 newTestEngine（同一个包、同一个 Engine 结构），
// 不另起一套——两套构造器迟早会走偏，而这里只需要 store + mem 两个字段。
func newGateEngine(t *testing.T, tune func(*config.Config)) *Engine {
	t.Helper()
	cfgFn := func(c *config.Config) {
		if tune != nil {
			tune(c)
		}
	}
	return &Engine{
		store: config.NewStoreFrom(cfgFn),
		mem:   memory.New(50),
		// states 必须初始化：stateOf() 直接往这个 map 里写，
		// 漏了会 panic（assignment to entry in nil map）。
		// toolloop_test.go 里的 newTestEngine 没给，因为那些用例不碰 stateOf。
		states: map[string]*groupState{},
	}
}

// fireGates 往一个群里灌一批消息状态，然后跑一轮 fire。
// impulse/reasons 直接写进 groupState，绕过消息解析——
// 这里要测的是闸门，不是冲动值怎么算出来的。
func fireGates(e *Engine, groupID string, impulse float64, reasons []string, n int) {
	s := e.stateOf(groupID)
	s.mu.Lock()
	s.newCount = n
	s.impulse = impulse
	s.reasons = reasons
	s.mu.Unlock()
	e.fire(groupID)
}

// TestDecisionLogsAreVisible 决策链路的每一条都必须分类为 decision 且级别可见。
//
// 这是本次改动的全部主张：用户点开日志就能知道它为什么不回。
func TestDecisionLogsAreVisible(t *testing.T) {
	e := newGateEngine(t, func(c *config.Config) {
		c.Brain.ImpulseThreshold = 0.9 // 抬高，让这批必然过不了
	})
	e.mem.Group("g1", "测试群")
	fireGates(e, "g1", 0.10, []string{"群里有闲聊"}, 1)

	entry := findDecision(t, "冲动值")
	if entry.Level == "DEBUG" {
		t.Errorf("决策日志是 DEBUG 级，生产默认 Info 看不见——"+
			"这正是「群里毛也不回却查不出原因」的成因，got: %s", entry.Level)
	}
}

// TestImpulseGateLogsFullParams 冲动值闸必须带齐能判断的参数。
func TestImpulseGateLogsFullParams(t *testing.T) {
	e := newGateEngine(t, func(c *config.Config) {
		c.Brain.ImpulseThreshold = 0.9
	})
	e.mem.Group("g1", "测试群")
	fireGates(e, "g1", 0.25, []string{"群里有闲聊", "攒了一批新消息"}, 3)

	entry := findDecision(t, "冲动值")
	for _, k := range []string{"冲动", "阈值", "差", "构成"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("冲动值闸的日志缺 %q 这项参数：只有结论没有数字，等于没法判断"+
				"「差多少」「为什么是这个分」。got: %v", k, entry.KV)
		}
	}
	if v, _ := entry.KV["差"].(string); v == "" {
		t.Error("「差」这一项是空的")
	}
}

// TestScheduleGateLogsRollAndRate 在线率闸必须带摇到的值和当时的阈值。
//
// 这一条最容易做漏：ScheduleAllow 过去只返回布尔，
// 「本次摇到 0.73，阈值 0.85」这个信息压根没往外传过。
// 结果就是日志里只有一句「当前时段不在线」——
// 同样的配置同样的时间下次可能就中了，永远复现不了。
// TestScheduleGateLogsRollAndRate 在线率判定必须把摇到的数和阈值带出来。
//
// **不经过 fire()**：那里是概率判定，再小的 base_rate 也有极小概率摇中，
// 一旦摇中就会穿透所有闸门走到 decide()——而这个测试 Engine 没配 router，
// 直接 nil 解引用。第一版就是这么崩的（而且天然 flaky）。
//
// 所以拆成两半：
//   - 这里直接测 ScheduleDecide 的输出（确定性的，覆盖拒绝分支）
//   - fire 里那行日志的字段，由下面 TestScheduleGateLogFields 单独验
func TestScheduleDecideReportsRollAndRate(t *testing.T) {
	// base_rate=0 → OnlineRate 返回 0 → roll<0 恒为假 → 必然拒绝，且**不 flaky**
	s := config.ScheduleConfig{Enabled: true, Mode: "always", BaseRate: 0.0001}
	s.BaseRate = 0.0001

	var sawReject bool
	for i := 0; i < 200; i++ {
		d := ScheduleDecide(s, time.Now(), false, false, time.Hour)
		if d.Allowed {
			continue
		}
		sawReject = true
		if d.Roll <= 0 || d.Roll >= 1 {
			t.Fatalf("摇到的值 %v 不在 [0,1)", d.Roll)
		}
		if d.Rate <= 0 {
			t.Fatalf("在线率 %v 应为极小正数", d.Rate)
		}
		if d.Roll < d.Rate {
			t.Fatalf("拒绝却摇到 %.3f < 阈值 %.3f，自相矛盾", d.Roll, d.Rate)
		}
		if d.Label == "" {
			t.Fatal("拒绝时必须带上命中的档位")
		}
		break
	}
	if !sawReject {
		t.Fatal("200 次都没摇中极小在线率，测试环境异常")
	}
}

// TestScheduleGateLogsInSource 在线率闸那行日志的字段名必须真的写在 fire 里。
//
// 为什么用源码检查而不用运行时日志：那条闸是概率判定，
// 在测试里没法 100% 复现「被它拦下」——即使 base_rate 极小也有极小概率摇中，
// 而一旦摇中就会穿透到 decide()，那个 Engine 没配 router，直接 panic。
// 第一版就是这么崩的，且天然 flaky。
//
// 源码检查的代价是脆（改了变量名要同步改这里），但它确定、且能钉住
// 「这三个字段必须出现在决策日志里」这件事本身。
func TestScheduleGateLogsInSource(t *testing.T) {
	b, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "本次摇骰子没上线")
	if i < 0 {
		t.Fatal("fire 里找不到「本次摇骰子没上线」这行日志——"+
			"在线率闸的判定过程必须留下痕迹")
	}
	chunk := src[i:min(len(src), i+400)]
	for _, k := range []string{"摇到", "在线率", "档位"} {
		if !strings.Contains(chunk, `"`+k+`"`) {
			t.Errorf("在线率闸的日志缺 %q 字段：只记「不在线」没用——"+
				"那是概率判定，不记摇到的数和当时的阈值，下次根本复现不了", k)
		}
	}
}

// TestScheduleDecideReportsAllFields 非拒绝路径的字段也要齐（档位/原因）。
func TestScheduleDecideReportsAllFields(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "daytime", BaseRate: 0.2}
	d := ScheduleDecide(s, time.Now(), false, false, time.Hour)
	if d.Why == "" {
		t.Error("ScheduleDecide 必须说明结果原因，否则日志里只有结论没法判断")
	}
	if d.Label == "" {
		t.Error("必须带上命中的档位说明（「白天活跃」/「平时（非高峰）」）")
	}
	// 允许与拒绝两条路径都必须自洽：Allowed ⟺ Roll<Rate
	if d.Allowed != (d.Roll < d.Rate) {
		t.Errorf("判定与摇骰不一致：Allowed=%v 但 Roll=%.3f Rate=%.2f",
			d.Allowed, d.Roll, d.Rate)
	}
}

// TestScheduleAllowWrapsDecide 老接口行为不变。
func TestScheduleAllowWrapsDecide(t *testing.T) {
	s := config.ScheduleConfig{Enabled: true, Mode: "always", BaseRate: 1.0}
	if !ScheduleAllow(s, time.Now(), false, false, time.Hour) {
		t.Error("在线率 1.0 时必须放行")
	}
	// 被 @ / 私聊 / 宽限期 一律放行。
	// AtGraceSec 必须给非零值：grace<=0 时宽限分支根本不进，
	// 那是「不配宽限」的正确行为，不是 bug。
	s2 := config.ScheduleConfig{Enabled: true, Mode: "always", BaseRate: 0.0001, AtGraceSec: 300}
	if !ScheduleAllow(s2, time.Now(), true, false, time.Hour) {
		t.Error("被 @ 必须放行")
	}
	if !ScheduleAllow(s2, time.Now(), false, true, time.Hour) {
		t.Error("私聊必须放行")
	}
	if !ScheduleAllow(s2, time.Now(), false, false, time.Second) {
		t.Error("点名宽限期内必须放行")
	}
}

// TestSilenceAndIntervalGatesAreVisible 静默期与最小间隔闸也要可见且带参数。
func TestSilenceAndIntervalGatesAreVisible(t *testing.T) {
	e := newGateEngine(t, func(c *config.Config) {
		c.Brain.ImpulseThreshold = 0.1
		c.Brain.MinSpeakIntervalSec = 600 // 刚发过言 → 必然被拦
		// 在线率设 100%：即使最小间隔这闸没拦住，也不会掉进后面的模型调用
		// （那需要 router，而这个 Engine 没配）。
		c.Schedule.Enabled = false
	})
	e.mem.Group("g1", "测试群").MarkBotSpoke("刚说过")
	fireGates(e, "g1", 0.9, []string{"被叫名字"}, 1)

	entry := findDecision(t, "距上次发言")
	for _, k := range []string{"距上次", "要求", "还差"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("最小间隔闸缺 %q：光说「太近」不知道要等多久、还差多少。got: %v",
				k, entry.KV)
		}
	}
}

// TestQuietRoundLogsExactlyOnce 一次模型调用必须在日志里留下**恰好一条**决策。
//
// 过去是两条：「决策已得出」记模型的原始 act，紧接着「决策：模型选择闭嘴」
// 再记一遍。两行 os 逐字相同、理由相同、模型相同、冲动相同——平铺在日志里
// 就是「一次思考出了两个结论」，用户反复来问是不是同一轮记了两遍。
//
// 所以现在合并成一条，用「结果」字段承担原来第二条的职责。
func TestQuietRoundLogsExactlyOnce(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{content: `<os>人家嘲别人去了</os>` +
			`<json>{"act":"quiet","mem":[]}</json>`},
	})
	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	before := countDecisions()
	e.decide(e.store.Get(), g, "有人@了别人", "openid-x", "Pytorch搬运工",
		false, false, []string{"主人说话", "群里有闲聊"}, 0.75,
		nil, "", "", "", "", nil, "", false)

	if n := countDecisions() - before; n != 1 {
		var msgs []string
		for _, x := range logx.Recent(400) {
			if x.Cat == logx.CatDecision {
				msgs = append(msgs, x.Msg+" "+fmt.Sprint(x.KV))
			}
		}
		t.Fatalf("一次调用应只落 1 条决策日志，实际新增 %d 条：\n%s",
			n, strings.Join(msgs, "\n"))
	}

	entry := findDecision(t, "决策已得出")
	if entry.KV["act"] != "quiet" {
		t.Errorf("act 应是模型给的 quiet，got %v", entry.KV["act"])
	}
	if entry.KV["结果"] != "闭嘴" {
		t.Errorf("结果应是「闭嘴」，got %v", entry.KV["结果"])
	}
	if os, _ := entry.KV["os"].(string); os != "人家嘲别人去了" {
		t.Errorf("os 没带过来: %v", entry.KV["os"])
	}
	if r, _ := entry.KV["理由"].(string); r != "主人说话+群里有闲聊" {
		t.Errorf("理由不对，看不出这 0.75 分是怎么攒出来的: %v", entry.KV["理由"])
	}
	for _, k := range []string{"冲动", "model", "结果", "耗时ms", "解析", "轮数"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("合并后的这条缺 %q：got %v", k, entry.KV)
		}
	}
}

// TestSayWithEmptyContentIsVisible 模型说要说但内容全被拦下时，
// act 和结果必须分开记——这是最需要被看见的一种「没发言」。
func TestSayWithEmptyContentIsVisible(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{content: `<json>{"act":"say","to":"","blocks":[],"text":"  ","mem":[]}</json>`},
	})
	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	e.decide(e.store.Get(), g, "有人@了我", "openid-x", "某人",
		true, false, []string{"被@"}, 1.0,
		nil, "", "", "", "", nil, "", true)

	entry := findDecision(t, "决策已得出")
	if entry.KV["act"] != "say" {
		t.Errorf("act 应保留模型的原始判断 say，got %v", entry.KV["act"])
	}
	if entry.KV["结果"] != "无内容" {
		t.Errorf("结果应是「无内容」（模型想说但内容被拦），got %v", entry.KV["结果"])
	}
	if texts, _ := (&recordingSender{}).count(); texts != 0 {
		t.Errorf("无内容时绝不能发送")
	}
}

// TestMutedGroupGateIsVisible 静默期闸也要带剩余时间。
func TestMutedGroupGateIsVisible(t *testing.T) {
	e := newGateEngine(t, nil)
	e.mem.Group("g1", "测试群").Mute(10 * time.Minute)
	fireGates(e, "g1", 1.0, []string{"被叫名字"}, 1)

	entry := findDecision(t, "静默期")
	for _, k := range []string{"静默至", "剩余"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("静默期闸缺 %q：得知道它什么时候回来。got: %v", k, entry.KV)
		}
	}
}