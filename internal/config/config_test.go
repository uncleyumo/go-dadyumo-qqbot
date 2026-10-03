package config

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T, body string) *Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	return st
}

// 回归测试：管理端会给 API Key 打码后下发。若 Get 返回浅拷贝，
// 打码会通过共享 slice 污染真实配置，最终把 Key 写成掩码落盘。
func TestGetIsDeepCopy(t *testing.T) {
	st := newStore(t, `{
		"qq":{"app_id":"1","app_secret":"s"},
		"persona":{"red_lines":["不许伤人"],"catchphrases":["行吧"]},
		"llm":{"endpoints":[{"id":"e1","name":"E1","api_key":"sk-real-key","enabled":true,
			"models":[{"id":"m1","enabled":true}]}]}
	}`)

	snap := st.Get()
	snap.LLM.Endpoints[0].APIKey = "***mask"
	snap.LLM.Endpoints[0].Models[0].Enabled = false
	snap.Persona.RedLines[0] = "被改了"

	again := st.Get()
	if again.LLM.Endpoints[0].APIKey != "sk-real-key" {
		t.Fatalf("修改快照不应影响真实配置，实际 key=%q", again.LLM.Endpoints[0].APIKey)
	}
	if !again.LLM.Endpoints[0].Models[0].Enabled {
		t.Fatal("修改快照不应影响真实配置（模型开关被污染）")
	}
	if again.Persona.RedLines[0] != "不许伤人" {
		t.Fatalf("修改快照不应影响真实配置（红线被污染），实际=%q", again.Persona.RedLines[0])
	}
}

func TestSavePersistsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"qq":{"app_id":"1","app_secret":"s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *Config) (bool, error) {
		c.Brain.DailyBudget = 123
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	st2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Get().Brain.DailyBudget != 123 {
		t.Fatal("保存后重新加载应看到新值")
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatal("应生成备份文件")
	}
}

func TestValidateRejectsDuplicateEndpoint(t *testing.T) {
	_, err := Load(mustWrite(t, `{
		"qq":{"app_id":"1","app_secret":"s"},
		"llm":{"endpoints":[{"id":"dup","models":[]},{"id":"dup","models":[]}]}
	}`))
	if err == nil {
		t.Fatal("重复的接入点 id 应被拒绝")
	}
}

func TestValidateRequiresCredentials(t *testing.T) {
	_, err := Load(mustWrite(t, `{"qq":{}}`))
	if err == nil {
		t.Fatal("缺少 app_id/app_secret 应被拒绝")
	}
}

func mustWrite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUpdateNoChangeKeepsRev B6：fn 返回 false（无变化）时不得推进版本号、不得落盘
func TestUpdateNoChangeKeepsRev(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"qq":{"app_id":"1","app_secret":"s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	rev0 := st.Rev()
	if err := st.Update(func(c *Config) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if st.Rev() != rev0 {
		t.Fatalf("无变化的 Update 不应推进 rev: %d -> %d", rev0, st.Rev())
	}
	// 有变化才推进
	if err := st.Update(func(c *Config) (bool, error) { c.Brain.DailyBudget = 999; return true, nil }); err != nil {
		t.Fatal(err)
	}
	if st.Rev() != rev0+1 {
		t.Fatalf("有变化的 Update 应推进 rev 一次: %d -> %d", rev0, st.Rev())
	}
}

// TestUpdateFailedValidateDoesNotLeak B5：Validate 失败时，fn 对共享切片的修改不得泄漏进 Store
func TestUpdateFailedValidateDoesNotLeak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"qq":{"app_id":"1","app_secret":"s","require_signature":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// fn 先改坏一个字段（触发 Validate 失败），同时改一个合法字段
	err = st.Update(func(c *Config) (bool, error) {
		c.LLM.Endpoints = append(c.LLM.Endpoints, Endpoint{ID: "x", APIType: "bogus-type"})
		c.Brain.DailyBudget = 777
		return true, nil
	})
	if err == nil {
		t.Fatal("非法 api_type 应导致 Validate 失败")
	}
	got := st.Get()
	if got.Brain.DailyBudget == 777 {
		t.Fatal("Validate 失败后修改不应生效（DailyBudget 泄漏）")
	}
	if len(got.LLM.Endpoints) != 0 {
		t.Fatal("Validate 失败后共享切片修改不应泄漏进 Store")
	}
}

// okCfg 造一个能过 Validate 的配置。
// Validate 会要求 qq.app_id 与 app_secret 非空，Default() 里是空的——
// 不补就会看到「qq.app_id 不能为空」，那是环境问题不是被测行为。
func okCfg() *Config {
	c := Default()
	c.QQ.AppID = "1905690675"
	c.QQ.AppSecret = "test-secret"
	return c
}

// TestMaxFactsConfigable brain.max_facts 能配，且不配时有安全默认。
func TestMaxFactsConfigable(t *testing.T) {
	// 不配 → Validate 回填 24（与 memory.MaxFacts 初值一致）
	c := okCfg()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Brain.MaxFacts != 24 {
		t.Errorf("未配置时 MaxFacts 应为 24，实际 %d", c.Brain.MaxFacts)
	}
	// 配了就是配的值
	c2 := okCfg()
	c2.Brain.MaxFacts = 40
	if err := c2.Validate(); err != nil {
		t.Fatal(err)
	}
	if c2.Brain.MaxFacts != 40 {
		t.Errorf("MaxFacts 应为 40，实际 %d", c2.Brain.MaxFacts)
	}
	// 0 与负数都回落到默认（避免「配 0 变成不记事」）
	for _, bad := range []int{0, -5} {
		c3 := okCfg()
		c3.Brain.MaxFacts = bad
		if err := c3.Validate(); err != nil {
			t.Fatal(err)
		}
		if c3.Brain.MaxFacts != 24 {
			t.Errorf("MaxFacts=%d 应回落到 24，实际 %d", bad, c3.Brain.MaxFacts)
		}
	}
}
