package brain

import (
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

// testDefault 给这组用例一份默认配置。与 testGroup 分开是因为
// testGroup 只管内存、不管配置，而时间标记的断言要看提示词全文。
func testDefault() config.Config {
	return *config.Default()
}

// 时间标记的判据是「与上一条的间隔」，所以这组夹具必须把两条的时间
// 拉开到阈值之上，否则测的是「没打标记」那个平凡分支。
var markerBase = time.Date(2026, 10, 6, 14, 0, 0, 0, time.Local)

func lineAt(role, name, content string, ts time.Time) memory.Line {
	return memory.Line{Role: role, Name: name, OpenID: "openid-" + name, Content: content, TS: ts}
}

// TestTimeMarkerOnlyOnRealGap 刷屏时不该有时间标记，隔久了必须有。
//
// 这是整件事的核心：标记的价值全在「这里真的静过一阵」这个信号上。
// 一旦每行都挂标记，它就退化成噪音，而那几十个 token 是白花的。
func TestTimeMarkerOnlyOnRealGap(t *testing.T) {
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")
	g.TouchMember("openid-乙", "乙")

	// 前四条挤在 40 秒内，最后一条隔了 3 分钟。
	// **必须用 3 分钟而不是半小时**：超过 quoteWindow（5 分钟）后
	// 标记只出时刻、不带「隔了」，这条用例的核心主张（密集段 vs 真间隔）
	// 就没法通过「隔了」这个字样来判定了。
	lines := []memory.Line{
		lineAt(memory.RoleUser, "甲", "在吗", markerBase),
		lineAt(memory.RoleUser, "乙", "在", markerBase.Add(15*time.Second)),
		lineAt(memory.RoleUser, "甲", "?", markerBase.Add(30*time.Second)),
		lineAt(memory.RoleUser, "乙", "说", markerBase.Add(40*time.Second)),
		lineAt(memory.RoleUser, "甲", "我这边下雪了", markerBase.Add(3*time.Minute)),
	}
	now := markerBase.Add(3*time.Minute + 30*time.Second)

	out := renderLines(g, lines, nil, now)

	// 首行按设计拿 now 作基准（离现在 3.5 分钟 → 窗口内，带间隔），
	// 加上那条真正隔开 3 分钟的，一共两个带间隔的标记。
	if n := strings.Count(out, "隔了"); n != 2 {
		t.Fatalf("应有 2 个带间隔的标记（首行 + 真实间隔），实际 %d 个:\n%s", n, out)
	}
	// 间隔是 3 分钟减去上一条那 40 秒 = 2 分 20 秒，humanGap 取整得 2。
	// 别写成 3——那样这条用例会因为一个自己没算清的数而一直红着。
	if !strings.Contains(out, "[14:03 隔了2分钟] · 甲：我这边下雪了") {
		t.Errorf("标记应与那一行紧挨着，格式为「[时刻 隔了多久] · 名字：内容」，实际:\n%s", out)
	}
	// 密集段里不该出现任何标记。
	// 必须**按行**判定：中文子串会互相命中（"在吗" 里就有 "在"，
	// 按 strings.Index 找会把首行误判成密集行）。
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[") {
			switch {
			case strings.Contains(line, "在吗"), strings.Contains(line, "我这边下雪了"):
				// 这两行本来就该有标记（首行 + 真实间隔）
			case strings.Contains(line, "· 乙：在"), strings.Contains(line, "· 甲：?"),
				strings.Contains(line, "· 乙：说"):
				t.Errorf("密集段不该有时间标记，却出现在这一行: %s", line)
			}
		}
	}
	// 密集段恰好三行且都没标记
	nMarked := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[") {
			nMarked++
		}
	}
	if nMarked != 2 {
		t.Errorf("整段应有 2 个标记（首行 + 真实间隔那条），实际 %d 个:\n%s", nMarked, out)
	}
}

// TestTimeMarkerThresholdBoundary 阈值两侧必须真的分得开。
//
// 差一秒就换一种行为，这种地方最容易在「手滑改个常数」时悄悄失效，
// 所以两侧都钉死。
func TestTimeMarkerThresholdBoundary(t *testing.T) {
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")

	just := timeMarkerThreshold - time.Second
	exact := timeMarkerThreshold

	if m := timeMarkerAt(markerBase, markerBase.Add(just), markerBase.Add(time.Hour)); m != "" {
		t.Errorf("间隔 %v 低于阈值 %v，不该打标记，实际 %q", just, timeMarkerThreshold, m)
	}
	if m := timeMarkerAt(markerBase, markerBase.Add(exact), markerBase.Add(time.Hour)); m == "" {
		t.Errorf("间隔正好等于阈值 %v 就该打标记", timeMarkerThreshold)
	}
}

// TestTimeMarkerZeroTSNeverRendered 零值时间不能被渲染出标记。
//
// 测试夹具与老 memory.json 都可能带零值 TS。渲染成一个模型看不懂的时间，
// 比不渲染更糟——它会当成真实时间去推理。
//
// 断言方式刻意**不查时间字面量**（"0001"/"00:00"）：Go 的零值时间是
// 0001-01-01，Format("15:04") 出来的是 "00:00"，靠猜字面量很容易漏。
// 直接要求「零值那一行前面没有标记」，它对任何渲染方式都成立。
// 变异测试确认过：把 timeMarkerAt 里的零值判断删掉，本用例会失败。
func TestTimeMarkerZeroTSNeverRendered(t *testing.T) {
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")
	lines := []memory.Line{
		{Role: memory.RoleUser, Name: "甲", OpenID: "openid-甲", Content: "零时间戳"},
		lineAt(memory.RoleUser, "甲", "有时间的", markerBase),
	}
	out := renderLines(g, lines, nil, markerBase.Add(time.Hour))

	// 逐行判定：含「零时间戳」的那一行前面不许出现标记
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "零时间戳") && strings.Contains(line, "[") {
			t.Errorf("零值 TS 的行不该带时间标记: %s", line)
		}
	}
	// 紧邻的那条有真实时间、距上一条很久 → 它该有标记（证明上面不是被别的条件挡住了）
	if !strings.Contains(out, "[") {
		t.Errorf("有真实时间的最后一条应带标记:\n%s", out)
	}
	// 零值行的正文必须保留
	if !strings.Contains(out, "零时间戳") {
		t.Errorf("正文本身必须保留:\n%s", out)
	}
}

// TestZeroTSDoesNotBreakGapChain 零值行不能把间隔链条打断。
//
// renderLines 用 prev 记录「上一条有时间的消息」。如果每行都无条件
// 更新 prev，一条零值记录会把链条清零，导致它之后每一条都被误判成
// 「隔了很久」——30 条窗口里可能平白冒出十几个标记。
//
// 只看「第三条」那一行：首行本来就该带标记（它离 now 有一小时），
// 那是设计行为，不是这条用例要防的东西。
func TestZeroTSDoesNotBreakGapChain(t *testing.T) {
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")

	// 甲(14:00) → 零值行 → 乙(14:00:20)：乙与甲只隔 20 秒，不该有标记
	lines := []memory.Line{
		lineAt(memory.RoleUser, "甲", "第一条", markerBase),
		{Role: memory.RoleUser, Name: "甲", OpenID: "openid-甲", Content: "无时间的中间行"},
		lineAt(memory.RoleUser, "甲", "第三条", markerBase.Add(20*time.Second)),
	}
	out := renderLines(g, lines, nil, markerBase.Add(time.Hour))

	// 首行有标记是预期的（它离 now 有一小时）
	if !strings.Contains(out, "[14:00] · 甲：第一条") {
		t.Errorf("首行离 now 一小时，应带时刻标记:\n%s", out)
	}
	// 关键：第三条前面不能有标记——它与第一条只隔 20 秒，
	// 中间那条零值行不该把链条打断
	if !strings.Contains(out, "\n· 甲：第三条") {
		t.Errorf("第三条不该被打上标记（与第一条只隔 20 秒）:\n%s", out)
	}
	// 零值行本身不能带标记（它没有任何可渲染的时间）
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "无时间的中间行") && strings.Contains(line, "[") {
			t.Errorf("零值行不该带标记: %s", line)
		}
	}
	if !strings.Contains(out, "无时间的中间行") {
		t.Errorf("零值行本身必须照常渲染:\n%s", out)
	}
}

// TestPrevOnlyAdvancesOnTimedLines prev 只能被有时间的行推进。
//
// 这是上一条的机理断言。如果 renderLines 无条件 `prev = l.TS`，
// 零值行会把链条清零，第三条变成「跟零值比」→ 间隔几千年 → 误判成隔了很久。
//
// 只断言最终输出的话，上游任何一次重构都可能把「零值行也更新 prev」
// 这类改动悄悄带进来。所以这里直接构造两种基准，
// 证明它们会得出**相反**的结论，用例才能真的区分这两种实现。
func TestPrevOnlyAdvancesOnTimedLines(t *testing.T) {
	t1 := markerBase
	t3 := markerBase.Add(20 * time.Second)

	// prev 正确保留 t1：间隔 20 秒 → 低于阈值 → 不打标记
	if gap := t3.Sub(t1); gap >= timeMarkerThreshold {
		t.Fatalf("夹具不对：t3 与 t1 只隔 20 秒，实际 %v", gap)
	}
	if m := timeMarkerAt(t1, t3, markerBase.Add(time.Hour)); m != "" {
		t.Errorf("prev 为 t1 时不该打标记，实际 %q", m)
	}

	// prev 被零值行覆盖成零值：间隔几千年 → 远超阈值 → 会打标记
	if gap := t3.Sub(time.Time{}); gap < timeMarkerThreshold {
		t.Fatalf("夹具不对：零值基准也算不出长间隔，实际 %v", gap)
	}
	if m := timeMarkerAt(time.Time{}, t3, markerBase.Add(time.Hour)); m == "" {
		t.Error("零值基准下确实会打标记——这正是零值行更新 prev 时会出现的误判")
	}
}

// TestTimeMarkerNegativeGapIsNotSilent 时钟漂移算出的负间隔不能变成标记。
//
// 负间隔不是「间隔很短」，是数据有问题。静默当成 0 处理会让这一行
// 看起来跟刚刚发生的一样，模型据此判断「这是实时对话」——而实际上
// 这条可能来自昨天。
func TestTimeMarkerNegativeGapIsNotSilent(t *testing.T) {
	if m := timeMarkerAt(markerBase, markerBase.Add(-time.Hour), markerBase); m != "" {
		t.Errorf("负间隔不该打出「隔了-60分钟」这种标记，实际 %q", m)
	}
}

// TestUserPromptExplainsTimeMarker 格式说明必须解释标记的含义。
//
// 这条是防「改了渲染忘了改说明」——2026-10-03 埋过一模一样的坑
// （改了「（主人）」的标记忘了改对应的说明句）。
//
// 更要紧的是：只说「有时会出现方括号」而不说它代表什么，模型会
// 当成无关的装饰，而这条约束（「隔久了就用引用」）就完全落空了。
func TestUserPromptExplainsTimeMarker(t *testing.T) {
	cfg := testDefault()
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")
	g.Append(lineAt(memory.RoleUser, "甲", "在吗", markerBase), 20)
	// now 必须离那条消息足够远，否则间隔不到 90 秒、根本不产生标记，
	// 那这条用例就测不到任何东西
	now := markerBase.Add(40 * time.Minute)

	up := userPrompt(cfg, g, g.Recent(20),
		buildTrigger(false, false, false, false, false, false, false, 1, "甲", false), "", now)

	// 这条消息确实该带标记——先确认前提成立
	if !strings.Contains(up, "[14:00]") {
		t.Fatalf("前置条件：这条离 now 40 分钟，该带时刻标记:\n%s", up)
	}
	for _, want := range []string{"一对方括号", "隔了多久", "引用气泡"} {
		if !strings.Contains(up, want) {
			t.Errorf("格式说明里应解释时间标记（缺 %q）:\n%s", want, up)
		}
	}
}

// TestQuoteRuleCoversStaleMessageAndTTL 引用规则必须覆盖两件事。
//
//  1. 隔久了更该引用——这是引用最主要的真实用途，原来那句「针对某一句」
//     太笼统，模型在攒批窗口里回一条 20 分钟前的话时意识不到该引用。
//  2. 5 分钟是 passiveTTL 的硬限，超窗取不到 refIdx、q:true 静默失效。
//     不写上限的话模型会照着规则硬填，消息发出去却没有气泡且不自知。
func TestQuoteRuleCoversStaleMessageAndTTL(t *testing.T) {
	cfg := testDefault()
	g := testGroup(t)
	sys := systemPrompt(cfg, g, MoodSignal{}, "", "", "")

	if !strings.Contains(sys, "5 分钟") {
		t.Errorf("引用规则必须写明 5 分钟这个硬上限:\n%s", sys)
	}
	// 规则里引用的标记写法必须与 userPrompt 的格式说明对得上。
	// 两边各写一套的话，模型会拿着一个描述去找另一种形状的行。
	if !strings.Contains(sys, "[时刻 隔了多久]") {
		t.Errorf("引用规则引用的标记写法应与 userPrompt 的说明一致:\n%s", sys)
	}
	// 超窗只说「别硬塞 q」，**不能说**「别拿旧话题接」——
	// 旧话题能不能聊跟引用能不能用是两件事（2026-10-06 用户明确指出）。
	// 把它们绑在一起等于凭一条技术限制去规定聊什么不聊什么。
	for _, banned := range []string{"顺着当下的话题接", "别拿它当话题接", "别去点名他"} {
		if strings.Contains(sys, banned) {
			t.Errorf("超窗分支不该规定「能不能聊旧话题」，那是内容禁令不是引用边界（出现了 %q）:\n%s",
				banned, sys)
		}
	}
}

// TestStaleTopicIsNotBanned 提示词不许把「旧话题」一刀切禁掉。
//
// 2026-10-06 用户纠正过一次：聊天时提起很久以前的事是正常的，
// 该管的是「反复问、没话找话、像机器人」这种行为问题，
// 而且它在 server config 的 red_lines 里已经有一条兜着
// （「不要一直追问一件事或者反复提及一件事，显得没话找话说」）。
//
// 曾经的写法是在引用规则里写「超窗就顺着当下的话题接 / 别点名他」——
// 那是拿一条技术限制（passiveTTL 5 分钟）去规定内容边界，
// 顺带把「引用」和「追旧话题」这两件不相干的事混为一谈。
// 这条用例防它再犯。
func TestStaleTopicIsNotBanned(t *testing.T) {
	cfg := testDefault()
	g := testGroup(t)
	sys := systemPrompt(cfg, g, MoodSignal{}, "", "", "")

	// 整个固定段都不该出现这类「别聊旧的」表述
	for _, banned := range []string{
		"别拿旧话题", "别提旧", "别追着", "顺着当下的话题",
		"别当现在的话题接", "已经是很久以前的事",
	} {
		if strings.Contains(sys, banned) {
			t.Errorf("固定段不该一刀切禁掉聊旧话题（出现了 %q）:\n%s", banned, sys)
		}
	}
	// 引用规则只说能力边界，不说内容边界
	if !strings.Contains(sys, "5 分钟") {
		t.Errorf("应保留「5 分钟内引得起来」这个能力边界的表述:\n%s", sys)
	}
}

// TestQuoteRuleExplainsWhichMessageGetsQuoted 提示词必须说清「引用引的是哪一条」。
//
// 2026-10-06 生产实况：用户在群里连着要求「引用刚才那条老消息」，
// 而机器人每次都把他**刚发的那句**原样引回来。根因在机制不在提示词——
// 锚点池按人选，模型没有字段能指定「引他第几条」：
// `PickAndReserve(group, replyToOpenID)` 倒序找这个人最近的一条存活锚点。
//
// 但模型不知道这件事，于是它以为自己在引想引的那句，反复要求、反复失败。
// **必须把「引用=他最新那条」如实写进提示词**，它才知道自己做不到，
// 可以改走 at 块或正文点名字。
//
// 这条用例是本次修改存在的理由：以后谁把这段删了，就是把「让模型白费劲」
// 那个 bug 请回来了。
func TestQuoteRuleExplainsWhichMessageGetsQuoted(t *testing.T) {
	cfg := testDefault()
	g := testGroup(t)
	sys := systemPrompt(cfg, g, MoodSignal{}, "", "", "")

	// 必须说清「引用圈的是那个人最新发的那条」
	if !strings.Contains(sys, "最新发的那条") {
		t.Errorf("提示词必须说清引用圈的是对方最新那条（否则模型会反复尝试指定老消息）:\n%s", sys)
	}
	// 必须说清「没法指定引第几条」
	if !strings.Contains(sys, "没法指定引他的第几条") {
		t.Errorf("提示词必须说清无法指定引哪一条:\n%s", sys)
	}
	// 说清做不到之后必须给替代路线，只说做不到模型会卡在那儿
	if !strings.Contains(sys, "at 块") {
		t.Errorf("说清引用做不到之后应给替代路线（at 块或正文点名）:\n%s", sys)
	}

	// 反向：不该承诺「能引用任意一条老消息」——那是骗模型
	for _, over := range []string{"引用任何一条", "可以引用指定的消息", "想引哪条都行"} {
		if strings.Contains(sys, over) {
			t.Errorf("提示词不该承诺做不到的事（出现了 %q）:\n%s", over, sys)
		}
	}
}

// TestMarkerInsideQuoteWindowCarriesGap 引用窗口内才带「隔了多久」。
//
// 窗口内模型要靠这个数决定「现在能不能引用」，所以必须给。
func TestMarkerInsideQuoteWindowCarriesGap(t *testing.T) {
	// 3 分钟 → 窗口内
	m := timeMarkerAt(markerBase, markerBase.Add(3*time.Minute), markerBase.Add(4*time.Minute))
	if m != "[14:03 隔了3分钟] " {
		t.Errorf("窗口内应带间隔，实际 %q", m)
	}
	// 正好卡在 5 分钟边界上仍然带（引得到）
	m = timeMarkerAt(markerBase, markerBase.Add(quoteWindow), markerBase.Add(6*time.Minute))
	if !strings.Contains(m, "隔了5分钟") {
		t.Errorf("正好 5 分钟仍引得得到，应带间隔，实际 %q", m)
	}
}

// TestMarkerOutsideQuoteWindowOnlyClock 超窗只给时刻，不给「隔了多久」。
//
// 三个理由（见 timeMarkerAt 的注释）：那个数字对决策已无用；
// 让模型自己判断「606 分钟 > 5 分钟吗」不可靠；时刻配「现在时间」够用了。
func TestMarkerOutsideQuoteWindowOnlyClock(t *testing.T) {
	// 14:00 那条与上一条隔了 20 分钟（>5 分钟窗口）。
	// now 推到当天 24 点之后 → 跨天，所以时刻要带日期。
	cur := markerBase                         // 10-06 14:00
	prev := markerBase.Add(-20 * time.Minute) // 13:40
	now := markerBase.Add(10 * time.Hour)     // 10-07 00:00
	m := timeMarkerAt(prev, cur, now)
	if strings.Contains(m, "隔了") {
		t.Errorf("超窗不该再给「隔了多久」，模型算不准这个减法，实际 %q", m)
	}
	if m != "[10-06 14:00] " {
		t.Errorf("超窗应只给时刻，跨天要带日期，实际 %q", m)
	}

	// 同一场景但 now 仍在当天 → 不带日期
	if m2 := timeMarkerAt(prev, cur, markerBase.Add(time.Hour)); m2 != "[14:00] " {
		t.Errorf("同一天内的超窗标记应只给时刻不带日期，实际 %q", m2)
	}
}

// TestCrossDayMarkerCarriesDate 跨天必须带日期。
//
// 08:55 看到「22:49」分不清是今天还是昨天——当成今天就是还没发生的将来，
// 整条时间轴就错了。这是纯时刻方案唯一的漏洞，必须补。
func TestCrossDayMarkerCarriesDate(t *testing.T) {
	// 次日 08:55 看到前一天 22:49 的消息（隔了 30 分钟 → 超窗）
	now := time.Date(2026, 10, 6, 8, 55, 0, 0, time.Local)
	y := time.Date(2026, 10, 5, 22, 49, 0, 0, time.Local)
	m := timeMarkerAt(y.Add(-30*time.Minute), y, now)
	if !strings.Contains(m, "10-05") {
		t.Errorf("跨天的标记必须带月日，否则模型分不清今天还是昨天，实际 %q", m)
	}
	// 超窗所以不带「隔了多久」
	if strings.Contains(m, "隔了") {
		t.Errorf("跨夜 30 分钟已超窗，不该带间隔，实际 %q", m)
	}

	// 同一天内则省掉日期（省 token）
	cur := time.Date(2026, 10, 6, 7, 0, 0, 0, time.Local)
	m2 := timeMarkerAt(cur.Add(-20*time.Minute), cur, now)
	if strings.Contains(m2, "10-") {
		t.Errorf("同一天内不该带日期，实际 %q", m2)
	}
	if m2 != "[07:00] " {
		t.Errorf("同一天内应只给时刻，实际 %q", m2)
	}
}

// TestCrossDayMarkerInsideWindow 窗口内的跨天标记也要带日期。
//
// 窗口内本来带「隔了多久」，模型靠那个数就能判断该不该引用，
// 日期看着多余——但它引的到底是**哪一天**那条消息，
// 日期写错等于让模型去引一条不存在的话。
func TestCrossDayMarkerInsideWindow(t *testing.T) {
	// 00:00 看到前一天 23:58 的消息（与 23:55 那条隔了 3 分钟 → 窗口内）
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local)
	cur := time.Date(2026, 10, 5, 23, 58, 0, 0, time.Local)
	prev := time.Date(2026, 10, 5, 23, 55, 0, 0, time.Local)
	m := timeMarkerAt(prev, cur, now)
	if !strings.Contains(m, "10-05") {
		t.Errorf("跨天的窗口内标记也必须带日期，实际 %q", m)
	}
	if !strings.Contains(m, "隔了3分钟") {
		t.Errorf("窗口内仍应带间隔，实际 %q", m)
	}
}

// TestFirstLineUsesNowAsBaseline 首行的间隔基准是「现在」，不是上一条。
//
// 首行没有上一条。renderLines 拿它跟 now 比——那正好回答了
// 「这些消息有多旧」。如果首行改成跟零值比或者干脆不标，
// 模型就完全看不到整个窗口离现在有多远。
func TestFirstLineUsesNowAsBaseline(t *testing.T) {
	g := testGroup(t)
	g.TouchMember("openid-甲", "甲")
	lines := []memory.Line{lineAt(memory.RoleUser, "甲", "很久以前说的", markerBase)}

	// 距今 2 小时 → 该标
	if m := timeMarkerAt(time.Time{}, markerBase, markerBase.Add(2*time.Hour)); m == "" {
		t.Error("首行距今 2 小时应打标记")
	}
	// 距今 10 秒 → 不该标
	if m := timeMarkerAt(time.Time{}, markerBase, markerBase.Add(10*time.Second)); m != "" {
		t.Errorf("首行距今 10 秒不该打标记，实际 %q", m)
	}
	// 渲染层必须真的画出来，而不只是函数返回非空
	out := renderLines(g, lines, nil, markerBase.Add(2*time.Hour))
	if !strings.Contains(out, "[") {
		t.Errorf("首行距今很久时应真的渲染出标记:\n%s", out)
	}
}

// TestTrimHistoryCountsTimeMarker 裁剪必须把标记算进开销。
//
// 不算的后果不是「被上游拒」（context_length_exceeded），
// 而是这轮白花钱直到被截断，且现象随群活跃度剧烈波动、极难归因。
//
// **这里必须用独立算术当期望值，不能拿 lineCost 自己算预算**——
// 那样 lineCost 被改小后预算也跟着变小，用例照样绿。
// 变异测试确认过：把 lineCost 里的 lineOverheadTimeClock 删掉，本用例会失败。
//
// 注意 TrimHistory 从最新往回累加，带标记的那条（最新那条）
// 无论预算多小都会被保留，所以观测点是**保留条数**而非「它被裁掉」。
func TestTrimHistoryCountsTimeMarker(t *testing.T) {
	// 20 条挤在 10 秒内（无标记）+ 1 条隔了 1 小时（有标记，且是最新那条）
	var lines []memory.Line
	for i := 0; i < 20; i++ {
		lines = append(lines, lineAt(memory.RoleUser, "甲", "短消息", markerBase.Add(time.Duration(i)*time.Second)))
	}
	lines = append(lines, lineAt(memory.RoleUser, "甲", "隔了很久的那条", markerBase.Add(time.Hour)))

	if !hasTimeMarkerClock(lines, len(lines)-1) {
		t.Fatal("最后一条按定义该有标记")
	}

	// 独立算术：正文 + 名字 + 基础开销，带标记的那条再加标记开销。
	plain := EstimateTokens("短消息") + EstimateTokens("甲") + lineOverheadBase
	marked := EstimateTokens("隔了很久的那条") + EstimateTokens("甲") + lineOverheadBase + lineOverheadTimeClock
	want := 20*plain + marked

	// 预算正好等于含标记总成本 → 全部留下
	if n := len(TrimHistory(lines, want)); n != len(lines) {
		t.Errorf("预算 %d 应能全留 %d 条，实际 %d 条", want, len(lines), n)
	}
	// 预算减 1 → 装不下，最后一条之后那条进不来 → 留 20 条
	if n := len(TrimHistory(lines, want-1)); n != 20 {
		t.Errorf("预算 %d（总成本减1）应留 20 条，实际 %d 条", want-1, n)
	}
	// 关键断言：如果 lineCost 漏算了标记开销，真实成本就是 want-10，
	// 上面那个 want-1 的预算反而够用 → 会留下 21 条，本用例失败。
}

// TestTrimHistoryNoPhantomMarkerOnFirstLine 密集窗口不该为首行白算标记开销。
//
// 这是首行估算分支的回归用例。原来它一律按有标记算，
// 于是「三条消息挤在 5 秒内」的窗口也会多算 10 token——
// 刷屏的群里每轮都白花，而渲染层一个标记都不会画出来。
// 后果不是被拒，是估算与渲染口径漂移、预算悄悄失准。
func TestTrimHistoryNoPhantomMarkerOnFirstLine(t *testing.T) {
	// 三条全挤在 5 秒内
	lines := []memory.Line{
		lineAt(memory.RoleUser, "甲", "一", markerBase),
		lineAt(memory.RoleUser, "甲", "二", markerBase.Add(2*time.Second)),
		lineAt(memory.RoleUser, "甲", "三", markerBase.Add(5*time.Second)),
	}
	for i := range lines {
		if hasTimeMarkerClock(lines, i) || hasTimeMarkerGap(lines, i) {
			t.Errorf("第 %d 行不该被算上标记开销（密集窗口）", i)
		}
	}
	total := 0
	for i := range lines {
		total += lineCost(lines, i)
	}
	// 预算等于「基础开销 + 正文」之和，说明没有多算任何标记
	plain := 0
	for _, l := range lines {
		plain += EstimateTokens(l.Content) + EstimateTokens(l.Name) + lineOverheadBase
	}
	if total != plain {
		t.Errorf("密集窗口的总估算(%d)不应高于纯基础开销(%d)", total, plain)
	}
}

// TestLineCostCountsMarkerExactly lineCost 的结果必须等于独立算术。
//
// 上一条只断言「带标记的 > 不带标记的」，那太弱：把 lineOverheadTimeClock
// 改成 1 也能过。这里钉死**精确数值**，用独立算术而不是拿 lineCost 自己算。
func TestLineCostCountsMarkerExactly(t *testing.T) {
	// 两条**正文等长**：差值断言只在正文完全相同时才成立，
	// 否则差值里混着正文长度差，测的就不是标记开销了。
	const content = "同样长度的内容"
	lines := []memory.Line{
		lineAt(memory.RoleUser, "甲", content, markerBase),
		lineAt(memory.RoleUser, "甲", content, markerBase.Add(time.Hour)),
	}
	plainWant := EstimateTokens(content) + EstimateTokens("甲") + lineOverheadBase
	markedWant := plainWant + lineOverheadTimeClock

	if got := lineCost(lines, 0); got != plainWant {
		t.Errorf("无标记那条成本应为 %d，实际 %d", plainWant, got)
	}
	if got := lineCost(lines, 1); got != markedWant {
		t.Errorf("有标记那条成本应为 %d（含 %d 的标记开销），实际 %d",
			markedWant, lineOverheadTimeClock, got)
	}
	if diff := lineCost(lines, 1) - lineCost(lines, 0); diff != lineOverheadTimeClock {
		t.Errorf("两条的差值应恰好是标记开销 %d，实际 %d", lineOverheadTimeClock, diff)
	}
}

// TestRenderOneLineHasNoTimeMarker 摘要路径不该带时间。
//
// renderOneLine 只喂给摘要调用，而摘要是唯一被长期保留的压缩物。
// 在那里加时间是纯浪费——摘要要的是「谁说了什么、聊到哪了」，
// 时间标记对它没用，还会挤掉本来就只给 300 token 的输出空间。
// 也因为它不带标记，摘要在跨天之后仍然读得懂（不受具体时刻误导）。
func TestRenderOneLineHasNoTimeMarker(t *testing.T) {
	l := lineAt(memory.RoleUser, "甲", "在吗", markerBase)
	if out := renderOneLine(l); strings.Contains(out, "[") {
		t.Errorf("摘要用的 renderOneLine 不该带时间标记，实际 %q", out)
	}
}
