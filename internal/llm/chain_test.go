package llm

import (
	"testing"
	"time"
)

// epWithPriority 造一个带头指定 priority 的接入点 JSON
func epWithPriority(id, baseURL string, models ...[2]any) string {
	ms := make([]string, 0, len(models))
	for _, m := range models {
		ms = append(ms, `{"id":"`+m[0].(string)+`","label":"`+m[0].(string)+
			`","priority":`+itoa(m[1].(int))+`,"enabled":true,"max_ctx":8000,"max_out":512,"stream":false}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","base_url":"` + baseURL + `","api_key":"k","enabled":true,"timeout_ms":5000,"models":[` + join(ms) + `]}`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// TestPreviewChainFollowsPriorityHardTier 预览顺序必须先按 priority 硬分层，
// 而不是按健康分——这是「预览=真实顺序」的核心约定。
func TestPreviewChainFollowsPriorityHardTier(t *testing.T) {
	// B 的 priority 更高，但它排在配置里的后面且从未被调用过（分数不占优）
	st := testStore(t, `"endpoints":[`+
		epWithPriority("low", "https://low.example/v1", [2]any{"slow", 0})+`,`+
		epWithPriority("high", "https://high.example/v1", [2]any{"fast", 9})+`]`)
	r := NewRouter(st)

	c := r.PreviewChain()
	if len(c.Chain) != 2 {
		t.Fatalf("应列出 2 个目标, got %d", len(c.Chain))
	}
	if c.Chain[0].Model != "fast" {
		t.Errorf("priority=9 的应排第一, got %q", c.Chain[0].Model)
	}
	if c.Chain[0].Rank != 1 || c.Chain[1].Rank != 2 {
		t.Errorf("rank 应从 1 递增: %+v", c.Chain)
	}
	if c.MaxAttempts != 4 {
		t.Errorf("max_attempts 应透出配置值 4, got %d", c.MaxAttempts)
	}
	if c.ExploreRate <= 0 {
		t.Errorf("探路概率应透出, got %v", c.ExploreRate)
	}
}

// TestPreviewChainMarksExploreWhenTierHasMultiple 同档多个候选时，
// 必须如实标注「不一定是它」——探路是随机分支，面板不能假装确定。
func TestPreviewChainMarksExploreWhenTierHasMultiple(t *testing.T) {
	st := testStore(t, `"endpoints":[`+
		epWithPriority("a", "https://a.example/v1", [2]any{"m1", 5})+`,`+
		epWithPriority("b", "https://b.example/v1", [2]any{"m2", 5})+`]`)
	r := NewRouter(st)

	c := r.PreviewChain()
	if len(c.Chain) != 2 {
		t.Fatalf("应列出 2 个目标, got %d", len(c.Chain))
	}
	if !c.Chain[0].ExploreHit {
		t.Error("同档多候选时第一名应标 ExploreHit（存在随机探路）")
	}
	for _, e := range c.Chain {
		if !e.Explore {
			t.Errorf("同档候选都应标 Explore: %+v", e)
		}
	}
}

// TestPreviewChainSingleCandidateNoExplore 只有唯一候选时不该出现探路提示
func TestPreviewChainSingleCandidateNoExplore(t *testing.T) {
	st := testStore(t, `"endpoints":[`+epWithPriority("a", "https://a.example/v1", [2]any{"only", 1})+`]`)
	r := NewRouter(st)
	c := r.PreviewChain()
	if len(c.Chain) != 1 {
		t.Fatalf("应只有 1 个目标, got %d", len(c.Chain))
	}
	if c.Chain[0].ExploreHit || c.Chain[0].Explore {
		t.Error("唯一候选不该标探路")
	}
}

// TestPreviewChainSkipsCoolingUntilExhausted 冷却中的目标在还有可用目标时，
// 不应出现在链路里（它只在候选耗尽后才兜底）。
func TestPreviewChainSkipsCoolingUntilExhausted(t *testing.T) {
	st := testStore(t, `"endpoints":[`+
		epWithPriority("a", "https://a.example/v1", [2]any{"alive", 5})+`,`+
		epWithPriority("b", "https://b.example/v1", [2]any{"cooling", 5})+`]`)
	r := NewRouter(st)

	// 手动把 cooling 那个打进冷却
	r.mu.RLock()
	tgt := r.targets["b|cooling"]
	r.mu.RUnlock()
	if tgt == nil {
		t.Fatal("找不到 b|cooling")
	}
	tgt.mu.Lock()
	tgt.h.CooldownUntil = time.Now().Add(3 * time.Minute)
	tgt.h.LastError = "制造冷却"
	tgt.mu.Unlock()

	c := r.PreviewChain()
	if len(c.Chain) == 0 {
		t.Fatal("链路不该为空")
	}
	if c.Chain[0].Model != "alive" || c.Chain[0].Cooling {
		t.Errorf("冷却中的目标不该排第一: %+v", c.Chain[0])
	}
	// 因为只有一个可用目标且 maxTry=4，冷却的会在候选耗尽后被兜底列出
	found := false
	for _, e := range c.Chain {
		if e.Model == "cooling" {
			found = true
			if !e.Cooling {
				t.Error("兜底项应标 Cooling")
			}
		}
	}
	if !found {
		t.Error("可用目标耗尽后，冷却中的目标应作为兜底出现在链路里")
	}
}

// TestPreviewChainSkipsDisabled 禁用的目标永远不该出现在链路里
func TestPreviewChainSkipsDisabled(t *testing.T) {
	st := testStore(t, `"endpoints":[`+
		`{"id":"a","name":"a","base_url":"https://a.example/v1","api_key":"k","enabled":false,"timeout_ms":5000,"models":[{"id":"off","enabled":true}]},`+
		epWithPriority("b", "https://b.example/v1", [2]any{"on", 1})+`]`)
	r := NewRouter(st)

	c := r.PreviewChain()
	for _, e := range c.Chain {
		if e.Model == "off" {
			t.Error("接入点被禁用时，其模型不该出现在链路里")
		}
	}
	if len(c.Chain) != 1 || c.Chain[0].Model != "on" {
		t.Errorf("应只剩 on: %+v", c.Chain)
	}
}

// TestPreviewChainDoesNotMutateState 预览是只读的：
// 反复调用不能改变健康度、也不能复活被判死的目标。
func TestPreviewChainDoesNotMutateState(t *testing.T) {
	st := testStore(t, `"endpoints":[`+epWithPriority("a", "https://a.example/v1", [2]any{"m", 1})+`]`)
	r := NewRouter(st)

	r.mu.RLock()
	tgt := r.targets["a|m"]
	r.mu.RUnlock()
	tgt.mu.Lock()
	tgt.h.Dead = true
	tgt.mu.Unlock()

	before := tgt.Health()
	for i := 0; i < 5; i++ {
		r.PreviewChain()
	}
	after := tgt.Health()
	if after.Dead != before.Dead || after.Total != before.Total {
		t.Errorf("预览改变了目标状态: before=%+v after=%+v", before, after)
	}
}

// TestOrderedMatchesPick 预览取到的第一名，必须与 pick 在无抖动时的选择一致。
// 这是「预览不骗人」的底线：两者共用 ordered()，不该出现分歧。
func TestOrderedMatchesPick(t *testing.T) {
	st := testStore(t, `"endpoints":[`+
		epWithPriority("a", "https://a.example/v1", [2]any{"x", 3}, [2]any{"y", 1})+`]`)
	r := NewRouter(st)

	snaps, _ := r.ordered(map[string]bool{}, false, false)
	if len(snaps) != 2 {
		t.Fatalf("应有两个候选, got %d", len(snaps))
	}
	c := r.PreviewChain()
	if snaps[0].t.Model != c.Chain[0].Model {
		t.Errorf("ordered 第一名 %q 与 PreviewChain 第一名 %q 不一致",
			snaps[0].t.Model, c.Chain[0].Model)
	}
	// priority 3 的应压过 priority 1
	if snaps[0].t.Model != "x" {
		t.Errorf("priority=3 应排第一, got %q", snaps[0].t.Model)
	}
}
