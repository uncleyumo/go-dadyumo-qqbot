package brain

import (
	"os"
	"strings"
	"testing"
)

// 架构不变量：fire() 里不该再有「内容判断」类的闸。
//
// # 为什么用源码检查而不用运行时日志
//
// 那道闸一旦放行，消息就会一路走到 decide()，而这里的 Engine 没配 router，
// 直接 nil 解引用。想靠「构造一批必被拦的消息」来测，就必须先把
// 后面所有闸都关掉——而那正好是「把要测的东西拆掉」才能做到的事。
// decisionlog_test.go 当年为了测在线率闸也踩过同一个坑（那里更糟：
// 概率判定连构造都构造不出来）。所以只能查源码。
//
// 代价是脆：改了措辞要同步改这里。但它确定、能钉住
// 「这两道闸不许回来」这件事本身，而那正是 2026-10-03 要防的回归。
func TestOnlyScheduleAndSafetyGatesRemain(t *testing.T) {
	b, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	// 同样要剥掉注释：engine.go 的包注释里写了「冲动值」这三个字作为历史说明，
	// 那是有价值的，不该被当成闸门复辟。
	src := stripLineComments(string(b))

	// 已废除的三道闸：冲动值门限、刷屏硬闸、最小发言间隔。
	removed := map[string]string{
		"冲动值不足": "2026-10-03 废除。它用一堆权重算 0~1 分数再比阈值，" +
			"本质是替模型判断「值不值得回」，而真人没有这个内心过程。" +
			"实况：某群 25 条里拦下 18 条，冲动值恒 0.25、阈值 0.40，" +
			"差的那 0.15 是「他这条消息有没有带问号」。",
		"整批只有表情": "2026-10-03 废除。刷屏只陈述事实（这批发的是表情，" +
			"一个字都没有），该不该理交给模型——群里连发八个「666」，" +
			"真人看到也可能接一句「你复读机啊」。",
		"距上次发言太近": "2026-10-03 废除，理由是它**会丢消息**：" +
			"fire() 在函数开头已把攒批状态清空（st.newCount=0、st.timer=nil），" +
			"走到这道闸 return 后，defer 里那条 " +
			"`pending && timer==nil && newCount>0` 的补救条件不成立，" +
			"那批消息就彻底消失、从来没进过模型。" +
			"实况：17:36:41 进的群消息、17:36:58 被它拦掉。" +
			"而且它与攒批窗口（10~18 秒）语义重复，间隔设 15 秒，叠加后每两轮就可能扔一批。" +
			"防刷屏另有三道且都不丢消息：分段延迟、max_segments、平台 5 次上限。" +
			"⚠️ 特别提醒：这道闸被加回来时**连日志都看不出异常**——" +
			"因为那批消息根本没进模型，而被丢的现场看起来就像「机器人没理他」。",
	}
	for phrase, why := range removed {
		if strings.Contains(src, phrase) {
			t.Errorf("fire 里又出现了「%s」这道闸。%s", phrase, why)
		}
	}
}

// TestImpulseGoneFromStateAndConfig 冲动值机制的结构性残留必须清干净。
//
// 权重表就算不再被调用，只要还在，就是给后人的 invitation：
// 「看着挺合理，加上去？」——而它服务的两道闸已经不存在了。
//
// **只查代码，不查注释**：说明「这里曾经有什么、为什么删」的注释是有价值的，
// 而且正是它们在阻止后人把机制加回来。所以先把注释剥掉再找残留。
func TestImpulseGoneFromStateAndConfig(t *testing.T) {
	for _, f := range []string{"engine.go", "text.go", "../config/config.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		code := stripLineComments(string(b))
		for _, residue := range []string{"ImpulseThreshold", "impulse_threshold",
			"ComputeImpulse", "ImpulseInput", "ImpulseResult", "applyBatchPenalties",
			"wChatter", "wManyNew", "wAtMe", "pAtOthers", "st.impulse", "st.reasons"} {
			if strings.Contains(code, residue) {
				t.Errorf("%s 的代码里还有冲动值机制的残留 %q——"+
					"它服务的闸已经废除，留着会诱导后人对着一套死机制加逻辑"+
					"（注释里说明历史是可以的，那正是防它回来的东西）", f, residue)
			}
		}
	}
}

// stripLineComments 去掉 Go 的行注释，只留代码。粗略够用：字符串字面量里
// 出现 // 的情况本文件涉及的那些标识符里没有。
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestGatesLeftAreAllThrottle 剩下这几道闸都必须是「与技术性限流有关」的。
//
// 逐个点名，是为了防止「下次要不要加个新的内容判断闸」这种想法
// 悄悄溜进来：加之前先看看这几条，它们分别对应
// 「你手动关的」「人在不在电脑前」「别花超钱」「谁在说话」，
// 没有一条是「这话值不值得回」。
//
// 第三条曾经是「最小发言间隔」，2026-10-03 因为会丢消息而删掉——
// 见 TestOnlyScheduleAndSafetyGatesRemain 里它的删除理由。
//
// 第四条（2026-10-08 加的，另一台机器人刚说的话）与它只隔一层皮，所以特别说明：
// 那条问的是「最后说话的是谁」，答完之后消息仍在上下文里，丢的是这一轮的开口；
// 删掉的那条问的是「距上次发言多久」，会把真人刚问的话一起吞掉。
// 加新闸之前先读这两句，别把后者当前者加回来。
func TestGatesLeftAreAllThrottle(t *testing.T) {
	b, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, phrase := range []string{
		"跳过：该群处于静默期",       // 手动急停
		"跳过：本次摇骰子没上线",      // 在线率
		"跳过：今日预算已用尽",       // 成本硬闸
		"跳过：这轮是另一台机器人刚说的话", // 同群第二台机器人的降频闸
	} {
		if !strings.Contains(src, phrase) {
			t.Errorf("fire 里找不到「%s」这道闸——它不该消失，尤其不能是误删", phrase)
		}
	}
}
