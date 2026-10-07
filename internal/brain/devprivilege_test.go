package brain

import (
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

// 开发者特权的开关不变量。
//
// # 为什么这个特性重要
//
// 2026-10-03 之前的实况：作为开发者的 openid 享受 6~8 处豁免——
// 跳过在线率摇骰子、豁免预算、超预算后仍放行、用快节奏发、
// trigger 里说「他是你的开发者」、被标成最高优先级触发者、
// 提示词注入「给他面子」、聊天记录里打「（主人）」。
//
// 于是**你看到的「它今天话好多」可能只是它对你话多**。生产数据里
// 开发者 25 条 0 次拦截、其他群友拦截率 25%~100%——这个巨大差异里
// 有相当一部分不是内容差异，而是身份特权。拿一个有偏的样本去判断
// 「它是不是太健谈了」，永远查不出真实问题。
//
// 所以这个开关的价值不在于「更拟人」，而在于**让样本无偏**。
//
// # 认人和特权是两件事
//
// 关闭开关**不影响**绑定：openid 列表照常维护、认主口令照常工作。
// 关掉的只是「区别对待」。

// fireOnce 走一轮 OnMessage 后把攒批定时器停掉。
//
// 必须停：OnMessage 会起 time.AfterFunc（攒批窗口 10~18 秒），
// 测试早就结束了它才触发，打到已结束的测试上会 panic。
func fireOnce(t *testing.T, e *Engine, groupID string) *groupState {
	t.Helper()
	st := e.stateOf(groupID)
	t.Cleanup(func() {
		st.mu.Lock()
		if st.timer != nil {
			st.timer.Stop()
		}
		st.mu.Unlock()
	})
	return st
}

// devEngine 造一个绑定了开发者、但没开特权的 Engine。
func devEngine(t *testing.T, devOn bool) *Engine {
	t.Helper()
	return newGateEngine(t, func(c *config.Config) {
		c.Master.DevEnabled = devOn
		c.Master.OpenIDs = []string{"openid-dev"}
		c.Schedule.Enabled = false // 关掉在线率，免得 fire 掉进概率分支
	})
}

// TestDevPrivilegeOffWritesNoFromMaster 特权关闭时不许写 st.fromMaster。
//
// **这一格是整个特性的单点**：fire() 的在线率豁免、allowCall 的预算豁免、
// eager 快节奏、buildTrigger 的「是你的开发者」case、recordTrigger 的
// 触发者归属、decide 的 masterHint——六处特权全部只读 st.fromMaster。
// 它一旦为真，六处会同时复活。
//
// 所以只钉这一格就够了，不用分别去测六处的 if——那正是这次改造要消灭的
// 「散落在八个地方、加第七处时没人记得回来补」的问题。
func TestDevPrivilegeOffWritesNoFromMaster(t *testing.T) {
	e := devEngine(t, false)
	e.OnMessage(&Event{GroupID: "g1", OpenID: "openid-dev", Name: "开发者", Content: "在吗"})

	st := fireOnce(t, e, "g1")
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fromMaster {
		t.Error("特权关闭时开发者的消息不该产生 fromMaster——" +
			"这一格为真，在线率豁免/预算豁免/eager/trigger 开发者 case/" +
			"触发者归属/masterHint 六处特权会同时复活")
	}
	// 前置条件：确认这条消息确实进了攒批，否则上面那句断言可能是「什么都没发生」
	if st.newCount != 1 {
		t.Fatalf("前置条件不成立：这条消息应该进了攒批，newCount=%d", st.newCount)
	}
}

// TestDevPrivilegeOnWritesFromMaster 开关打开时特权必须生效。
//
// 必须有这条正向测试，否则「把 isDev 写成恒 false」这种错改也不会被发现
// ——单靠上面那条，恒 false 一样是绿的。
func TestDevPrivilegeOnWritesFromMaster(t *testing.T) {
	e := devEngine(t, true)
	e.OnMessage(&Event{GroupID: "g1", OpenID: "openid-dev", Name: "开发者", Content: "在吗"})

	st := fireOnce(t, e, "g1")
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.fromMaster {
		t.Error("特权开启时开发者的消息必须产生 fromMaster——" +
			"否则开关打开也没用，开发者享受不到任何特权")
	}
}

// TestDevPrivilegeOffKeepsBindWorking 特权关着，绑定必须照常工作。
//
// 这是「认人与特权分开」这条设计的核心保证。认主口令、IsMaster、
// 绑定列表管理都不看 DevEnabled——开关管的是「区别对待谁」，
// 不是「认得出谁」。
func TestDevPrivilegeOffKeepsBindWorking(t *testing.T) {
	e := newGateEngine(t, func(c *config.Config) {
		c.Master.DevEnabled = false
		c.Master.BindEnabled = true // 认主口令的另一个前置条件（engine.go:322）
		c.Master.BindToken = "tok"
		c.Master.OpenIDs = nil
		// BindMaster 走 Store.Update → Validate，而 Validate 会要求
		// qq.app_id 与 app_secret 都非空。Default() 里它们是空的，
		// 不补就会看到「绑定开发者失败: qq.app_id 不能为空」——
		// 那是测试环境问题，不是特权开关的问题（逐个补会踩三次，
		// 一次补齐更省事）。
		c.QQ.AppID = "1905690675"
		c.QQ.AppSecret = "test-secret"
	})
	e.OnMessage(&Event{GroupID: "g1", OpenID: "openid-new", Name: "新人", Content: "#认主 tok"})

	cfg := e.store.Get()
	if !cfg.IsMaster("openid-new") {
		t.Error("特权关闭时认主口令仍必须能绑定——绑定与特权是两件事，" +
			"关了特权就认不出人就本末倒置了")
	}
	// 认主那条消息本身不该产生特权
	st := fireOnce(t, e, "g1")
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fromMaster {
		t.Error("认主那条消息不该产生 fromMaster——它是口令，不是特权请求")
	}
}

// TestDevPrivilegeOffNoMasterHintInPrompt 特权关闭时提示词里不该有身份暗示。
//
// 覆盖 masterHint（「给他面子」）。它读的是 decide 传进来的 fromMaster，
// 而 fromMaster 已经是「身份 + 开关」，所以这里自动为真——
// 但「自动满足」意味着没人验证过，所以要有。
func TestDevPrivilegeOffNoMasterHintInPrompt(t *testing.T) {
	cfg := *config.Default()
	cfg.Master.DevEnabled = false
	cfg.Master.Nickname = "老张"
	g := memory.New(cfg.Brain.MaxHistory).Group("g", "测试群")

	// masterHint 传空（特权关闭时 fire 传的就是空）
	sys := systemPrompt(cfg, g, MoodSignal{}, "", "", "老张")
	for _, s := range []string{"给他面子", "开发者昵称", "【相关的人】", "关于你的开发者"} {
		if strings.Contains(sys, s) {
			t.Errorf("特权关闭时提示词不该出现 %q——"+
				"模型会照着它偏向那个人，而这个提示词没有任何开关能关掉它", s)
		}
	}
}

// TestDevPrivilegeOffNoMasterTagInHistory 特权关闭时聊天记录里不打开发者标记。
//
// 覆盖 renderLines 的「（开发者）」标签。
func TestDevPrivilegeOffNoMasterTagInHistory(t *testing.T) {
	cfg := *config.Default()
	cfg.Master.DevEnabled = false
	cfg.Master.OpenIDs = []string{"openid-zhang"}
	g := testGroup(t)
	g.TouchMember("openid-zhang", "张三")
	g.Append(memory.Line{Role: memory.RoleUser, Name: "张三",
		OpenID: "openid-zhang", Content: "在吗"}, 20)
	// 夹具的 TS 全是零值，now 取什么都不会产生时间标记
	now := time.Now()

	if out := renderLines(g, g.Recent(20), masterSet(g, cfg), now); strings.Contains(out, "（开发者）") {
		t.Errorf("特权关闭时历史里不该打开发者标记，got: %s", out)
	}
	// 也不该出现「带（开发者）的是你的开发者」这句悬空说明
	up := userPrompt(cfg, g, g.Recent(20),
		buildTrigger(false, false, false, false, false, false, false, 1, "张三", false), "", now)
	if strings.Contains(up, "开发者") {
		t.Errorf("特权关闭时 userPrompt 不该提开发者，got: %s", up)
	}
}

// TestDevPrivilegeOnMarksHistory 开着的反向对照——开关必须是双向的。
//
// 缺了这条，masterSet 写成「永远返回 nil」时上面那条照样绿。
func TestDevPrivilegeOnMarksHistory(t *testing.T) {
	cfg := *config.Default()
	cfg.Master.DevEnabled = true
	cfg.Master.OpenIDs = []string{"openid-zhang"}
	g := testGroup(t)
	g.TouchMember("openid-zhang", "张三")
	g.Append(memory.Line{Role: memory.RoleUser, Name: "张三",
		OpenID: "openid-zhang", Content: "在吗"}, 20)
	// 夹具的 TS 全是零值，now 取什么都不会产生时间标记
	now := time.Now()

	out := renderLines(g, g.Recent(20), masterSet(g, cfg), now)
	if !strings.Contains(out, "张三（开发者）") {
		t.Errorf("特权开启时历史里应打开发者标记，got: %s", out)
	}
	up := userPrompt(cfg, g, g.Recent(20),
		buildTrigger(false, false, false, false, false, false, false, 1, "张三", false), "", now)
	if !strings.Contains(up, "带（开发者）的是你的开发者") {
		t.Errorf("特权开启时格式说明里应提到开发者标记，got: %s", up)
	}
}

// TestMasterLegendMatchesActualTags 聊天记录的说明与实际标记必须严格一致。
//
// 2026-10-03 埋过这个坑：把「（主人）」改成「（开发者）」时改了 renderLines
// 的标记，忘了改 userPrompt 里「带（主人）的是你的开发者」那句说明——
// 提示词里就留下一条「带（开发者）的是你的开发者」的规则，而聊天记录里
// 没有任何一行带这个标签。模型会照着规则去找一个不存在的人，
// 或者更糟：把任意一行推断成开发者。
//
// 这条断言就是防它再犯，而且必须**两个开关状态都验**——只在开着时验的话，
// 「关着时还留着说明」这个方向漏掉。
func TestMasterLegendMatchesActualTags(t *testing.T) {
	for _, devOn := range []bool{false, true} {
		cfg := *config.Default()
		cfg.Master.DevEnabled = devOn
		cfg.Master.OpenIDs = []string{"openid-zhang"}
		g := testGroup(t)
		g.TouchMember("openid-zhang", "张三")
		g.Append(memory.Line{Role: memory.RoleUser, Name: "张三",
			OpenID: "openid-zhang", Content: "在吗"}, 20)
		now := time.Now()

		hasTag := strings.Contains(renderLines(g, g.Recent(20), masterSet(g, cfg), now), "（开发者）")
		up := userPrompt(cfg, g, g.Recent(20),
			buildTrigger(false, false, false, false, false, false, false, 1, "张三", false), "", now)
		hasLegend := strings.Contains(up, "带（开发者）的是你的开发者")

		if hasTag != hasLegend {
			t.Errorf("DevEnabled=%v：标记存在=%v 但说明存在=%v——两者必须严格一致。"+
				"说明与标记脱节，模型会拿一条对不上的规则去推断谁是谁",
				devOn, hasTag, hasLegend)
		}
	}
}

// TestPersonaLoyaltyIsGone 固定段里不许再有「关于你的开发者」。
//
// persona.loyalty 已于 2026-10-03 整段删除。它原本在**固定段无条件注入**，
// 等于开了一个绕过特权的泄漏口：即使把 dev_enabled 关掉，模型仍能从
// 「那是把你做出来的人，给他点面子」里知道谁是开发者，而程序侧已经按
// 普通人处理了。两边不一致，模型会照着提示词偏向那个人——于是
// 「关掉开关就能看到真实行为」这个承诺是假的。
//
// 开发者身份现在只从 master.dev_enabled 一个地方进来。
func TestPersonaLoyaltyIsGone(t *testing.T) {
	cfg := *config.Default()
	cfg.Master.DevEnabled = false
	g := memory.New(cfg.Brain.MaxHistory).Group("g", "测试群")
	sys := systemPrompt(cfg, g, MoodSignal{}, "", "", "")
	if strings.Contains(sys, "开发者") {
		t.Errorf("特权关闭时固定段不该出现「开发者」——"+
			"persona.loyalty 已删除，这里若还有就是泄漏口又开了。got: %s",
			truncate(sys, 200))
	}
}
