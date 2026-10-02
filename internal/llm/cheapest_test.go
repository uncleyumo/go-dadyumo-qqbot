package llm

import (
	"os"
	"path/filepath"
	"testing"

	"dadyumo/internal/config"
)

// CheapestModel 是优选任务的唯一模型来源（memepool.Curator 通过 ConfigSource 用它）。
//
// 生产事故：这里原本写死 `if v.Vision { continue }`，而生产配置 5 个启用模型
// vision 全是 true，于是永远返回空串 → judge 报「没有可用的打分模型」
// → 优选任务从上线到现在一次都没跑过。而这件事**不产生任何日志**，
// 只有「优选」两个字从来没出现过才看得出来。
func storeWithModels(t *testing.T, models ...[3]any) *config.Store {
	t.Helper()
	// models: [modelID, vision(bool), priority(int)]
	ms := make([]string, 0, len(models))
	for _, m := range models {
		id, vision, pri := m[0].(string), m[1].(bool), m[2].(int)
		visionStr := "false"
		if vision {
			visionStr = "true"
		}
		ms = append(ms, `{"id":"`+id+`","label":"`+id+`","enabled":true,`+
			`"max_ctx":8000,"max_out":512,"stream":false,"vision":`+visionStr+
			`,"priority":`+itoa(pri)+`}`)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"qq":{"app_id":"1","app_secret":"s"},"llm":{"max_attempts":4,"endpoints":[` +
		`{"id":"ep","name":"ep","base_url":"http://x","api_key":"k","enabled":true,` +
		`"timeout_ms":5000,"models":[` + joinStr(ms) + `]}]}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(p)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return st
}

// itoa 已在 chain_test.go 定义，此处直接复用
func unusedItoa(i int) string {
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

func joinStr(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// 全是 vision 模型时也必须选出一个来 —— 这正是生产配置的样子
func TestCheapestModelFallsBackToVisionOnly(t *testing.T) {
	st := storeWithModels(t,
		[3]any{"dots-3-free", true, 3},
		[3]any{"qwen3.5-flash", true, 1},
		[3]any{"deepseek-flash", true, 2},
	)
	r := NewRouter(st)
	id, model := r.CheapestModel()
	if id == "" || model == "" {
		t.Fatal("全是 vision 模型时必须退回一个可用目标，不能返回空串（否则优选任务永不执行）")
	}
	// priority 最低的那档
	if model != "qwen3.5-flash" {
		t.Errorf("应选 priority 最低的 qwen3.5-flash，实际 %q", model)
	}
}

// 有纯文本模型时优先用它（打分只吃文字，派 vision 模型纯属浪费）
func TestCheapestModelPrefersTextOnly(t *testing.T) {
	st := storeWithModels(t,
		[3]any{"qwen3.5-flash", true, 1},
		[3]any{"cheap-text", false, 5},
	)
	r := NewRouter(st)
	_, model := r.CheapestModel()
	if model != "cheap-text" {
		t.Errorf("有纯文本档时应选它（哪怕 priority 更高），实际 %q", model)
	}
}

// 禁用的模型不能被选中
func TestCheapestModelSkipsDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"qq":{"app_id":"1","app_secret":"s"},"llm":{"max_attempts":4,"endpoints":[` +
		`{"id":"ep","name":"ep","base_url":"http://x","api_key":"k","enabled":true,"timeout_ms":5000,` +
		`"models":[{"id":"off","label":"off","enabled":false,"max_ctx":8000,"max_out":512,` +
		`"stream":false,"vision":false,"priority":0}]}]}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(st)
	if id, model := r.CheapestModel(); id != "" || model != "" {
		t.Errorf("只有禁用模型时应返回空串让调用方跳过，实际 %q/%q", id, model)
	}
}

// 一个可用目标都没有时也返回空串 —— 这才是「跳过」的正当理由
func TestCheapestModelEmptyWhenNothingEnabled(t *testing.T) {
	st := storeWithModels(t)
	r := NewRouter(st)
	if id, model := r.CheapestModel(); id != "" || model != "" {
		t.Errorf("无可用目标时应返回空串，实际 %q/%q", id, model)
	}
}

// 同样配置必须每次都选同一个（map 遍历顺序随机）
func TestCheapestModelDeterministic(t *testing.T) {
	st := storeWithModels(t,
		[3]any{"a", true, 2},
		[3]any{"b", true, 2},
		[3]any{"c", true, 2},
	)
	r := NewRouter(st)
	first, _ := r.CheapestModel()
	for i := 0; i < 20; i++ {
		if got, _ := r.CheapestModel(); got != first {
			t.Fatalf("同档位时选择应稳定，实际 %q != %q", got, first)
		}
	}
}

var _ = config.APIResponses
