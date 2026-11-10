package admin

import (
	"strings"
	"testing"
)

// 管理端页面里，输入框的 id、renderPersona 填值的引用、collectPersona 收值的引用
// 是三处**互相独立**的字符串。任何一处对不上都不会报错，只是静默失效：
// 症状是「保存了没生效」或「打开页面框里是空的」，很难查。
//
// 这类 wiring 不适合逐个手点验证——加一项就是加三处字符串，
// 迟早会漏。所以在这里钉住。
func TestFallbackLinesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-fallback"`) {
		t.Error("页面缺少兜底话术输入框 pf-fallback")
	}
	// 渲染：把配置里的兜底话术填进框
	if !strings.Contains(html, `getElementById('pf-fallback').value = (p.fallback_lines || []).join('\n')`) {
		t.Error("renderPersona 没有把 fallback_lines 填进 pf-fallback（打开页面会是空框）")
	}
	// 收集：把框里的内容写回配置
	if !strings.Contains(html, `CFG.persona.fallback_lines = lines(document.getElementById('pf-fallback').value)`) {
		t.Error("collectPersona 没有把 pf-fallback 写回 CFG（保存后不生效）")
	}
}

// busy_lines 与 fallback_lines 一样是「填框 → 读回 → 存回」三处字符串，
// 少一处就静默失效。跟着上一个测试一起钉住。
func TestBusyLinesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-busy"`) {
		t.Error("页面缺少艾特兜底话术输入框 pf-busy")
	}
	if !strings.Contains(html, `getElementById('pf-busy').value = (p.busy_lines || []).join('\n')`) {
		t.Error("renderPersona 没有把 busy_lines 填进 pf-busy")
	}
	if !strings.Contains(html, `CFG.persona.busy_lines = lines(document.getElementById('pf-busy').value)`) {
		t.Error("collectPersona 没有把 pf-busy 写回 CFG（保存后不生效）")
	}
}

// reply_rules 是 2026-10-05 新增的字段，为了给「回复的指导」一个正向的落脚点
// （原先只能塞进「行为边界」那堆禁令里）。三处 wiring 与前两项同构。
func TestReplyRulesFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-reply"`) {
		t.Error("页面缺少回话分寸输入框 pf-reply")
	}
	if !strings.Contains(html, `getElementById('pf-reply').value = (p.reply_rules || []).join('\n')`) {
		t.Error("renderPersona 没有把 reply_rules 填进 pf-reply（打开页面会是空框）")
	}
	if !strings.Contains(html, `CFG.persona.reply_rules = lines(document.getElementById('pf-reply').value)`) {
		t.Error("collectPersona 没有把 pf-reply 写回 CFG（保存后不生效）")
	}
}

// voice 跟上面几项同构：输入框、填值、收值三处字符串少一处就静默失效。
// 这一项失效的后果更隐蔽——保存一次人设，固定段的口吻选择就被抹成空串，
// 机器人退回默认口吻，而页面上看不出任何报错。
func TestVoiceFormIsWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, `id="pf-voice"`) {
		t.Error("页面缺少口吻输入框 pf-voice")
	}
	if !strings.Contains(html, `getElementById('pf-voice').value = p.voice || ''`) {
		t.Error("renderPersona 没有把 voice 填进 pf-voice（打开页面会是空框）")
	}
	if !strings.Contains(html, `CFG.persona.voice = document.getElementById('pf-voice').value.trim()`) {
		t.Error("collectPersona 没有把 pf-voice 写回 CFG（保存后不生效）")
	}
}

// 分区重组（2026-10-05）：人设页由两张卡拆成四张。
// 卡片的 h2 标题是这次重组的全部可见产物，所以在这里钉住——
// 「怎么回话」独立成卡正是用户提的核心诉求，塌回「行为边界」就等于没改。
func TestPersonaPanelsSplitIntoFour(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	for _, want := range []string{"它是谁", "怎么回话", "不做什么", "应急话术"} {
		if !strings.Contains(html, ">"+want+" <span class=\"hint\"") {
			t.Errorf("人设页缺少卡片「%s」", want)
		}
	}
	// 旧的两个标题必须消失，否则重组等于没发生
	for _, gone := range []string{"人设本体", "行为边界"} {
		if strings.Contains(html, ">"+gone+" <span class=\"hint\"") {
			t.Errorf("旧卡片「%s」还在，四卡拆分没生效", gone)
		}
	}
	// pf-reply 必须落在「怎么回话」那张卡里，不能漂到别处。
	// 用位置比对：它出现在 pf-catch 之后、pf-roast 之前——
	// 顺序由上面那张表的排布决定，漂了这条就红。
	reply := strings.Index(html, `id="pf-reply"`)
	catch := strings.Index(html, `id="pf-catch"`)
	roast := strings.Index(html, `id="pf-roast"`)
	if reply < 0 || catch < 0 || roast < 0 {
		t.Fatalf("缺少输入框: pf-reply=%d pf-catch=%d pf-roast=%d", reply, catch, roast)
	}
	if !(catch < reply && reply < roast) {
		t.Errorf("pf-reply 应排在「怎么回话」卡里（pf-catch 之后、pf-roast 之前），实际位置 %d/%d/%d",
			catch, reply, roast)
	}
}

// 主题切换是「head 里尽早执行的内联脚本 + header 按钮 + 点击写回」三处字符串，
// 少一处就退化成「按钮在但点了不生效」或者「每次刷新闪一下」。
// 两个页面共用同一个 localStorage key，登录页也必须跟着切换，否则登录后换主题、
// 退出再登录会看到另一套配色。
func TestThemeToggleIsWired(t *testing.T) {
	for _, name := range []string{"assets/index.html", "assets/login.html"} {
		b, err := assetsFS.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		html := string(b)
		for _, want := range []string{
			`localStorage.getItem('admin-theme')`,
			`localStorage.setItem('admin-theme', next)`,
			`document.documentElement.dataset.theme = t`,
			`prefers-color-scheme: light`,
			`html[data-theme="light"]`,
			`color-scheme:light`,
			`id="theme-toggle"`,
			`class="i-sun"`,
			`class="i-moon"`,
		} {
			if !strings.Contains(html, want) {
				t.Errorf("%s 缺少主题切换片段 %q", name, want)
			}
		}
	}
}

// 深色是默认值，浅色只做变量覆盖。这里钉住几个关键语义变量，
// 防止以后把颜色直接写回选择器里——那样浅色主题会漏块、看起来一块深一块浅。
// 接管聊天前端接线（设计文档步骤 8 / T22）。
//
// 这些字符串是「页面功能是否真的接上」的唯一回归依据：按钮点了调哪个函数、
// 小窗轮询打哪个接口、刷新复活读哪个 sessionStorage key。
// 少一处不会报错，只会静默失效——症状是「按钮在但点了没反应」或「刷新后接管断了」。
func TestTakeoverUIWired(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	for _, want := range []string{
		`id="tk"`,                     // 接管小窗
		`data-mode="window"`,          // 三档默认选中 window
		`/api/takeover/mode`,          // 切档
		`/api/takeover/lease`,         // acquire / beat / release
		`/api/takeover/window`,        // 只读快照
		`/api/takeover/members`,       // @ 成员
		`/api/takeover/send`,          // 发送文字
		`/api/takeover/meme`,          // 发送表情
		`/api/takeover/end_all`,       // 全部恢复正常
		`tk_started`,                  // 刷新复活标记
		`tk_epoch`,                    // 旧页面识别
		`sendBeacon`,                  // pagehide 释放
		`addEventListener('pagehide'`, // 页面卸载
		`visibilitychange`,            // 隐藏降频
		`sessionStorage`,              // 租约持久化
		`applyTakeoverBadges(`,        // 徽标
		`data-tk-badge=`,              // 群卡片徽标挂载点
		`id="takeover-banner"`,        // 顶部常驻横幅
		`openTakeover(`,               // 群卡片按钮入口
		`closeTakeover(`,              // 点 × 释放
		`endAllTakeover(`,             // 全部恢复正常
		`id="tk-count"`,               // mm:ss 倒计时
		`id="tk-bar"`,                 // 进度条
		`id="tk-reacquire"`,           // 连接断开后手动重连
	} {
		if !strings.Contains(html, want) {
			t.Errorf("接管前端缺少片段 %q", want)
		}
	}
	// 群卡片必须真的挂上「接管窗口」按钮，而不是只在别处出现函数名
	if !strings.Contains(html, `onclick="openTakeover('`) {
		t.Error("群卡片没有「接管窗口」按钮（openTakeover 永远不会被调用）")
	}
	// 大号倒计时必须绑「这条消息的被动回复有效期」，不能绑 10 秒租约 TTL：
	// 租约每 2 秒续一次，显示十秒倒计时会让人误以为消息快过期。
	if !strings.Contains(html, "a.expires_at") {
		t.Error("接管倒计时没有绑定 selected.expires_at")
	}
}

// 昵称、群别名、消息内容都是用户可控文本。接管小窗必须用 createElement +
// textContent 渲染，绝不能把值拼进 innerHTML——一个恶意昵称就能注入脚本。
func TestTakeoverRendersUserTextSafely(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	// 正向：这三类是接管小窗里唯一的用户文本出口，必须走 textContent
	for _, want := range []string{
		`b.textContent = m.name || m.card || '群友'`,
		`body.textContent = l.content ?? l.text ?? ''`,
		`who.textContent = (isBot ? '机器人' : (l.name || '群友')) + '：'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("接管小窗缺少安全文本写入 %q（用户昵称/消息必须用 textContent）", want)
		}
	}
	// 反向：这些插值一旦出现在 HTML 模板里就是注入点
	for _, bad := range []string{`${m.name}`, `${m.card}`, `${l.content}`, `${l.text}`} {
		if strings.Contains(html, bad) {
			t.Errorf("用户可控文本被拼进 HTML 模板：%q", bad)
		}
	}
	// 群名/别名进入标题与横幅时也必须转义
	if !strings.Contains(html, `esc(items.join('、'))`) {
		t.Error("接管横幅没有对群名做 esc 转义")
	}
}

// 移动端与主题：小窗铺满、按钮换行、成员 chip 横滚、表情网格自适应；
// 颜色只用主题变量，明暗两套都能读。
func TestTakeoverMobileAndTheme(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)

	for _, want := range []string{
		`#tk{position:fixed;inset:0;background:var(--overlay)`,
		`#tk .box{background:var(--card)`,
		`#tk .seg{display:flex`,                                    // 三档等宽
		`#tk .tk-chips{display:flex;gap:6px;overflow-x:auto`,       // 成员 chip 横滚
		`grid-template-columns:repeat(auto-fill,minmax(56px,1fr))`, // 表情网格自适应
		`#tk .box{width:100%;max-width:none;height:100%`,           // 小屏铺满
		`#tk .seg{flex-wrap:wrap}`,                                 // 小屏按钮换行
		`@media (max-width:768px)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("接管小窗缺少移动端/主题片段 %q", want)
		}
	}
}

// 深色是默认值，浅色只做变量覆盖。这里钉住几个关键语义变量，
// 防止以后把颜色直接写回选择器里——那样浅色主题会漏块、看起来一块深一块浅。
func TestIndexThemeVariablesExist(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取页面失败: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`:root{`,
		`--surface-3:#222836`,
		`--surface-2:#14171e`,
		`--log-line:#1d222c`,
		`--line-strong:#3a4252`,
		`--cat-bg:#232a37`,
		`--chart-grid:`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("页面缺少深色主题变量 %q", want)
		}
	}
	// 硬编码底色必须从选择器里消失，只允许留在变量定义里
	for _, gone := range []string{
		`.tab.on{background:#222836`,
		`background:#1d222c`,
		`input,select,textarea{background:#0d1016`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("选择器里仍有硬编码颜色 %q，应改用变量", gone)
		}
	}
}
