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
		// sender 也必须给：认主口令那种路径会调 replyOne → e.sender.SendGroupTo，
		// 缺了会 nil panic。以前没暴露是因为没有用例走到那里。
		sender: &recordingSender{},
	}
}

// fireGates 往一个群里灌一批消息状态，然后跑一轮 fire。
// 只写状态字段，绕过消息解析——这里要测的是闸门，不是 trigger 怎么拼的
// （trigger 的措辞由 trigger_test.go 管）。
//
// 注意这里不再写 impulse/reasons：冲动值机制已于 2026-10-03 废除，
// 那两个字段连同它们服务的两道闸（冲动值门限、刷屏硬闸）一起没了。
// 剩下的闸由这里逐个测。
func fireGates(e *Engine, groupID string, n int) {
	s := e.stateOf(groupID)
	s.mu.Lock()
	s.newCount = n
	s.mu.Unlock()
	e.fire(groupID)
}

// TestDecisionLogsAreVisible 决策链路的每一条都必须分类为 decision 且级别可见。
//
// 这是本次改动的全部主张：用户点开日志就能知道它为什么不回。
//
// 2026-10-03：闸门从六道减到四道，这里用「静默期」当样本——
// 它是剩下四道里最容易被手动触发的（群卡片点一下就有）。
func TestDecisionLogsAreVisible(t *testing.T) {
	e := newGateEngine(t, nil)
	g := e.mem.Group("g1", "测试群")
	g.Mute(30 * time.Minute)
	fireGates(e, "g1", 1)

	entry := findDecision(t, "静默")
	if entry.Level == "DEBUG" {
		t.Errorf("决策日志是 DEBUG 级，生产默认 Info 看不见——"+
			"这正是「群里毛也不回却查不出原因」的成因，got: %s", entry.Level)
	}
}

// TestMuteGateLogsFullParams 静默期闸必须带齐能判断的参数。
//
// 静默期是唯一纯手动触发的闸（「静默 30 分钟」按钮），所以日志得让人
// 一眼看出「还剩多久」——不然用户只会以为机器人坏了。
func TestMuteGateLogsFullParams(t *testing.T) {
	e := newGateEngine(t, nil)
	g := e.mem.Group("g1", "测试群")
	g.Mute(30 * time.Minute)
	fireGates(e, "g1", 1)

	entry := findDecision(t, "静默")
	for _, k := range []string{"静默至", "剩余"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("静默期闸的日志缺 %q：用户点了静默却不知道要等多久，got: %v", k, entry.KV)
		}
	}
	if v, _ := entry.KV["剩余"].(string); v == "" {
		t.Error("「剩余」这一项是空的")
	}
}

// TestScheduleDecideReportsRollAndRate 在线率判定必须把摇到的数和阈值带出来。
//
// 这一条最容易做漏：ScheduleAllow 过去只返回布尔，
// 「本次摇到 0.73，阈值 0.85」这个信息压根没往外传过。
// 结果就是日志里只有一句「当前时段不在线」——
// 同样的配置同样的时间下次可能就中了，永远复现不了。
//
// **2026-10-03 起可以完全确定性了**：ScheduleDecide 增加了 roll 入参，
// 不再自己调全局 math/rand。以前这里要循环 200 次等它摇中极小在线率，
// 现在直接传一个固定的 roll 就能精确覆盖拒绝分支，且天然不 flaky。
// 也不必再绕开 fire()——它当年绕开是因为概率不可控，现在可控了。
func TestScheduleDecideReportsRollAndRate(t *testing.T) {
	// base_rate=0.0001 → OnlineRate 返回 0.0001，传 roll=0.9 必然拒绝
	s := config.ScheduleConfig{Enabled: true, Mode: "always", BaseRate: 0.0001}

	d := ScheduleDecide(s, time.Now(), false, false, time.Hour, 0.9)
	if d.Allowed {
		t.Fatal("roll 0.9 远大于在线率 0.0001，必须拒绝")
	}
	if d.Roll != 0.9 {
		t.Fatalf("回传摇到的值 %v，期望原样返回 0.9——日志里「摇到」必须就是参与比较的那个数", d.Roll)
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
	d := ScheduleDecide(s, time.Now(), false, false, time.Hour, 0.1) // 0.1 < 0.2 → 放行
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
	// 用 always_strict（真的 1.0），不是 always。
	// 2026-10-03 拆档后 always 是 0.90，而这条测试一直写着 "always"
	// 却断言「在线率 1.0 时必须放行」——它其实是在赌那 10%。
	// 单跑必过（运气好），-count=20 稳定失败 2 次左右：
	// 于是在 -shuffle=on 的全量里随机炸，炸时看起来像是本轮改动碰坏了它。
	s := config.ScheduleConfig{Enabled: true, Mode: "always_strict", BaseRate: 1.0}
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
		false, false, nil, "", "", "", "", nil, "", false)

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
	for _, k := range []string{"model", "结果", "耗时ms", "解析", "轮数"} {
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
		true, false, nil, "", "", "", "", nil, "", true)

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
	fireGates(e, "g1", 1)

	entry := findDecision(t, "静默期")
	for _, k := range []string{"静默至", "剩余"} {
		if _, ok := entry.KV[k]; !ok {
			t.Errorf("静默期闸缺 %q：得知道它什么时候回来。got: %v", k, entry.KV)
		}
	}
}
// TestQuietFallbackWarnsAndKeepsOS 降级闭嘴那一路必须既留痕又保住 os。
//
// 2026-10-03 生产实况：模型只吐了 <os> 就被截断时，日志里是一条
// `act quiet + os 空 + 解析 fallback`。三个问题叠在一起：
//  1. os 被 parse.go 的降级出口丢了（「它为什么不说话」看不出来）
//  2. 告警条件是「fallback+say」，而这条出口的 act 恒为 quiet → 一条都不响
//  3. 管理端看到的 act=quiet 其实是解析器猜的，不是模型的判断
//
// 这条用例把三件事一起钉住：告警响了、os 进了决策日志、act 被标成猜的。
func TestQuietFallbackWarnsAndKeepsOS(t *testing.T) {
	// 只吐 os 就没了——精确命中 parse.go「剥完标签什么都不剩」那条出口
	srv, _ := startFakeUpstream(t, []fakeStep{{content: `<os>懒得理他</os>`}})
	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	e.decide(e.store.Get(), g, "有人在群里发了消息", "openid-x", "某人",
		false, false, nil, "", "", "", "", nil, "", false)

	// 1. 告警必须响。归到 CatDecision 是为了管理端默认视图能直接看到
	// （logx/log.go 里 CatDecision 的定义就是「为什么闭嘴的全部」）。
	warn := findDecision(t, "未解析到合法 JSON")
	for _, k := range []string{"raw", "os"} {
		if _, ok := warn.KV[k]; !ok {
			t.Errorf("降级告警缺 %q：只剩一句「解析失败」没法定位，"+
				"得能看到模型到底输出了什么、它自己想说什么。got: %v", k, warn.KV)
		}
	}

	// 2. os 进了决策日志，且真的是模型写的那句
	entry := findDecision(t, "决策已得出")
	if v, _ := entry.KV["os"].(string); v != "懒得理他" {
		t.Errorf("os 没进决策日志：got %q，期望 %q"+
			"（管理端默认只看决策链路，os 丢了就等于什么都没说）",
			entry.KV["os"], "懒得理他")
	}
	if v, _ := entry.KV["解析"].(string); v != "fallback" {
		t.Fatalf("解析应是 fallback，实际 %q——用例会走到别的出口，等于没测到", v)
	}

	// 3. act 是解析器猜的，必须标出来
	if v, _ := entry.KV["act来源"].(string); v != "解析器猜的" {
		t.Errorf("降级轮次的 act 来自解析器硬编码，日志必须标明，got %q"+
			"（不标的话 act=quiet 会被误读成「它自己决定闭嘴」）", entry.KV["act来源"])
	}
}

// TestSayFallbackAlsoMarkedAsGuess 降级发言那一路**同样**要标「解析器猜的」。
//
// 这条一开始写的是「fallback+say 不该标注」，跑完发现是错的：parse.go
// 三条降级出口全都没有模型给 act——quiet 那两条是硬编码 "quiet"，
// 而 say 这条的 `Act: "say"` 连同 `Tone: "roast"` 一起是硬编码的
//（parse.go:84）。所以「解析器猜的」对三条降级出口一视同仁，
//
// 这比「quiet 才标注」更诚实：降级轮次里模型的 tone、对象、记忆全都丢了，
// 它唯一表达出来的就是 os 那一句话。看到「解析器猜的」就知道
// 这一行的 act 和语气都不能当真。
func TestSayFallbackAlsoMarkedAsGuess(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{content: `<os>懒得理他</os>他今天话真多`},
	})
	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	e.decide(e.store.Get(), g, "有人在群里发了消息", "openid-x", "某人",
		false, false, nil, "", "", "", "", nil, "", false)

	entry := findDecision(t, "决策已得出")
	if v, _ := entry.KV["解析"].(string); v != "fallback" {
		t.Fatalf("解析应是 fallback，实际 %q——没走到降级路径，用例白测", v)
	}
	if v, _ := entry.KV["act来源"].(string); v != "解析器猜的" {
		t.Errorf("降级发言时 act 也是解析器填的（parse.go 里 Act 和 Tone 都硬编码），"+
			"必须同样标注。got %q", entry.KV["act来源"])
	}
	// os 仍然必须保住（另一条降级出口）
	if v, _ := entry.KV["os"].(string); v != "懒得理他" {
		t.Errorf("降级发言时 os 也要保住，got %q", entry.KV["os"])
	}
}

// TestNormalRoundHasNoActSourceMark 正常轮次不该出现「解析器猜的」。
//
// 防止上面的标注逻辑写成「总是加上」——那会让 99% 的正常日志多一个字段。
func TestNormalRoundHasNoActSourceMark(t *testing.T) {
	srv, _ := startFakeUpstream(t, []fakeStep{
		{content: `<os>懒得理他</os>` + `<json>{"act":"quiet","mem":[]}</json>`},
	})
	e := newEngineWithUpstream(t, srv.URL, &recordingSender{}, nil, nil)
	g := e.mem.Group("g1", "测试群")

	e.decide(e.store.Get(), g, "有人在群里发了消息", "openid-x", "某人",
		false, false, nil, "", "", "", "", nil, "", false)

	entry := findDecision(t, "决策已得出")
	if v, _ := entry.KV["解析"].(string); v != "ok" {
		t.Fatalf("解析应是 ok，实际 %q", v)
	}
	if _, ok := entry.KV["act来源"]; ok {
		t.Errorf("正常解析的轮次不该标「解析器猜的」，got: %v", entry.KV["act来源"])
	}
}
