package brain

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 在线时段档位表与管理端下拉必须一致。
//
// # 为什么这个测试存在
//
// 档位名有两份真源：Go 的 `schedulePresets`（决定实际行为）与
// `index.html` 里硬编码的 `<option value="...">`（决定用户能选到什么）。
// 两份全仓没有任何东西保证同步——HTML 是 `//go:embed assets` 编进二进制的，
// 连构建都不会报「多了个 option」。
//
// 不同步的后果**严重程度不对称**：
//
//   - **HTML 有、Go 没有**（严重）：用户选中保存 → windowsFor 查表未命中 →
//     **静默回落 daytime**（白天 0.85 / 深夜 0.50）。没有报错、没有日志差异，
//     现场表现只是「话变少了」。这是最坏的一类故障。
//   - **Go 有、HTML 没有**（轻微）：下拉里选不到，但手改 config.json 仍可用。
//
// 所以下面断言的是**双向相等**，两个方向各钉住一个不同的严重程度。
//
// # 为什么用源码 grep 而不是运行时接口
//
// 下拉是 HTML 里的静态 `<option>`，没有接口能拿到它；`/api/state` 也不下发
// 档位列表。读文件校对是本项目已有的手法（见 decisionlog_test.go 里的
// `os.ReadFile("engine.go")` 检查日志字段）。用运行时行为的测试需要把 Engine
// 整套架起来，成本高得多还只能覆盖单向。
func TestSchedulePresetMatchesAdminDropdown(t *testing.T) {
	t.Parallel()

	// Go 侧认识的全部档位 = 预设表的 key + 两个由 windowsFor switch 特判的档
	goModes := map[string]bool{"random_daily": true, "custom": true}
	for k := range schedulePresets {
		goModes[k] = true
	}

	// HTML 侧：抓 <select id="sch-mode"> 块里的所有 option value
	b, err := os.ReadFile("../admin/assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	start := strings.Index(html, `id="sch-mode"`)
	if start < 0 {
		t.Fatal("index.html 里找不到 <select id=\"sch-mode\">——"+
			"档位下拉改名或被删了，这条测试的断言对象已不存在")
	}
	rest := html[start:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		t.Fatal("sch-mode 的 <select> 没有闭合")
	}
	block := rest[:end]

	htmlModes := map[string]bool{}
	for _, m := range regexp.MustCompile(`<option value="([^"]+)"`).FindAllStringSubmatch(block, -1) {
		htmlModes[m[1]] = true
	}
	if len(htmlModes) == 0 {
		t.Fatal("sch-mode 的 <select> 里一个 option 都没有，HTML 是不是被改坏了")
	}

	var missingInGo, missingInHTML []string
	for m := range htmlModes {
		if !goModes[m] {
			missingInGo = append(missingInGo, m)
		}
	}
	for m := range goModes {
		// 空串不是 option 的值，跳过（它只存在于 Go 侧的默认值）
		if m == "" {
			continue
		}
		if !htmlModes[m] {
			missingInHTML = append(missingInHTML, m)
		}
	}
	sort.Strings(missingInGo)
	sort.Strings(missingInHTML)

	// 方向一：HTML 有、Go 无 → 静默回落，**这条最严重**
	if len(missingInGo) > 0 {
		t.Errorf("管理端下拉里有 Go 侧不存在的档位 %v：用户选中保存后会"+
			"被 windowsFor 静默回落 daytime（白天 0.85/深夜 0.50），"+
			"现场只表现为「话变少了」。要么在 schedulePresets 补上，要么把 option 删掉",
			missingInGo)
	}
	// 方向二：Go 有、HTML 无 → 下拉选不到，手改 config.json 仍可用
	if len(missingInHTML) > 0 {
		t.Errorf("Go 侧有 %v 但管理端下拉里没有：用户选不到它们。"+
			"要加档位请同时改 index.html 的 <option>，"+
			"且注意 HTML 是 go:embed 进二进制的，改完必须重新 build",
			missingInHTML)
	}
}

// TestAlwaysLabelDoesNotPromise100 档位的显示名不许暗示 100%。
//
// `always` 叫「全天在线」而实际 0.90，就是这个毛病。显示名是用户在管理端
// 唯一能看到的描述，它说的话必须与实际行为一致。
func TestAlwaysLabelDoesNotPromise100(t *testing.T) {
	t.Parallel()
	w := schedulePresets["always"]
	if len(w) != 1 {
		t.Fatalf("always 档应有且只有一个窗口，实际 %d 个", len(w))
	}
	if w[0].Rate >= 1.0 {
		t.Fatalf("always 档的 Rate 应小于 1.0（它不是 always_strict），实际 %v", w[0].Rate)
	}
	// Label 里出现「在线」而实际不是 100%，就是原来那个误导。
	// 新的 Label 应该是「全天在」——刻意不给「在线」两个字。
	if strings.Contains(w[0].Label, "在线") {
		t.Errorf("always 档的 Label %q 含「在线」但实际只有 %.0f%%——"+
			"这正是 2026-10-03 拆分前那个误导性名字的翻版",
			w[0].Label, w[0].Rate*100)
	}
	// 反向：always_strict 是 1.0，显示名就该把「100%」说清楚
	ws := schedulePresets["always_strict"]
	if len(ws) != 1 || ws[0].Rate != 1.0 {
		t.Fatalf("always_strict 应是单窗口、Rate 1.0，实际 %+v", ws)
	}
}
