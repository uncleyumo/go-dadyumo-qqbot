package brain

import (
	"testing"

	"dadyumo/internal/config"
)

// hitCount 读假上游收到的请求数。hits 受 mu 保护，直接读会与
// 并发的 ServeHTTP 抢数据，-race 下必炸。
func (f *fakeUpstream) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// pauseCfg 关停用例的公共配置。
//
// 把在线时段和预算都关掉是刻意的：这两道闸会按概率/额度随机拒绝，
// 留着它们，「没调模型」就可能是因为摇骰子没摇中，而不是因为总开关——
// 断言会变成假绿，而且换个时间跑就红。
func pauseCfg(paused bool) func(*config.Config) {
	return func(c *config.Config) {
		c.Paused = paused
		c.Schedule.Enabled = false
		c.Brain.DailyBudget = 0
	}
}

const sayStep = `<json>{"act":"say","to":"","blocks":[{"t":"text","c":"在的"}],"mem":[]}</json>`

// TestPausedDoesNotCallModel 总开关关掉后，自动链路一个模型调用都不能发生。
//
// 这是本次功能的全部主张，也是唯一的硬约束。断言分三层，任何一层漏了都算失败：
//   - 入口闸：消息连记忆都不进（用户选的语义是「完全当它不在场」）
//   - 攒批闸：直接 fire（模拟关停前就排好的定时器到点）
//   - 结果：模型零调用、群零发言
func TestPausedDoesNotCallModel(t *testing.T) {
	t.Parallel()
	srv, up := startFakeUpstream(t, []fakeStep{{content: sayStep}})
	sender := &recordingSender{}
	e := newEngineWithUpstream(t, srv.URL, sender, nil, pauseCfg(true))
	e.states = map[string]*groupState{} // stateOf 直接往这个 map 里写，不给会 panic

	e.OnMessage(&Event{GroupID: "g1", GroupName: "测试群", OpenID: "u1", Name: "群友", Content: "在吗"})
	if n := len(e.mem.Group("g1", "").Recent(50)); n != 0 {
		t.Errorf("关停期间消息不该进记忆（它「不在场」），实际留了 %d 条", n)
	}

	// 关停前已经排好的攒批定时器到点——入口闸拦不住这个，靠 fire 里那道。
	fireGates(e, "g1", 1)

	if n := up.hitCount(); n != 0 {
		t.Errorf("关停期间发生了 %d 次模型调用——「一个 token 都不花」已破", n)
	}
	if texts, imgs := sender.count(); texts != 0 || imgs != 0 {
		t.Errorf("关停期间它开口了: texts=%d imgs=%d", texts, imgs)
	}
	// 日志必须落在 decision 分类且 Info 级以上，否则「它为什么不说话」查不出来
	if entry := findDecision(t, "总开关"); entry.Level == "DEBUG" {
		t.Errorf("关停日志是 DEBUG 级，生产默认 Info 看不见，got: %s", entry.Level)
	}
}

// TestNotPausedStillCalls 反向断言：没关停时照常调模型。
//
// 没有这一条，上面那个测试可能只是因为「闸门把整条链路掐死了」而通过——
// 一个永远不说话的实现也能让「零调用」成立。
func TestNotPausedStillCalls(t *testing.T) {
	t.Parallel()
	srv, up := startFakeUpstream(t, []fakeStep{{content: sayStep}})
	sender := &recordingSender{}
	e := newEngineWithUpstream(t, srv.URL, sender, nil, pauseCfg(false))
	e.states = map[string]*groupState{}

	fireGates(e, "g1", 1)

	if up.hitCount() == 0 {
		t.Fatal("没关停却不调模型——闸门把正常链路也掐死了")
	}
	if texts, _ := sender.count(); texts == 0 {
		t.Error("没关停时应该照常发言")
	}
}

// 后台优选任务（memepool.Curator）那一路的总开关由
// memepool/curator_pause_test.go 负责——它不看群消息、自己到点就调模型，
// 是本包这几道闸拦不住的第四个漏点，所以闸门与测试都在那个包里。
