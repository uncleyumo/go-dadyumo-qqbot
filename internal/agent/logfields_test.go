package agent

import (
	"strings"
	"testing"

	"dadyumo/internal/webhook"
)

// 群消息日志的字段构造。控制台日志页直接渲染这些 kv，
// 拆错了字段就会在界面上错位或者串行，所以单独锁住。

func toMap(kv []any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// 清洗过的内容必须进 text，平台原文进「原文」——
// 控制台靠这两栏对照着判断是程序解析错了还是模型认错了
func TestGroupLogFieldsKeepsRawAndClean(t *testing.T) {
	raw := `<faceType=1,faceId="5",ext="eyJ0ZXh0Ijoi5rWB5rOqIn0="/>`
	clean, _ := cleanTags(raw, nil)
	if clean != "（流泪）" {
		t.Fatalf("前置条件不成立: %q", clean)
	}
	fields := buildGroupLogFields("测试群一号", "群友甲", false,
		clean, raw, false, "", func(string) string { return "" })
	m := toMap(fields)

	if m["text"] != "（流泪）" {
		t.Errorf("text 应是清洗结果，got %v", m["text"])
	}
	if m["原文"] != raw {
		t.Errorf("原文应是平台原文，got %v", m["原文"])
	}
}

// 没有任何平台标记时不要多出「原文」这一栏，否则日志会被无意义的重复刷屏
func TestGroupLogFieldsNoRawWhenUnchanged(t *testing.T) {
	same := "今天打不打"
	fields := buildGroupLogFields("测试群一号", "群友甲", false,
		same, same, false, "", func(string) string { return "" })
	if _, ok := toMap(fields)["原文"]; ok {
		t.Error("内容没变化时不该打「原文」")
	}
}

// @全体成员 与 @某人 要单独成栏，否则混在 text 里看不出发生了什么
func TestGroupLogFieldsAtAllAndTarget(t *testing.T) {
	fields := buildGroupLogFields("测试群一号", "群友甲", false,
		"@全体成员", "<@all>", true, "AAAA5555",
		func(oid string) string {
			if oid == "AAAA5555" {
				return "群友乙"
			}
			return ""
		})
	m := toMap(fields)

	if m["@全体"] != true {
		t.Errorf("@全体 应为 true，got %v", m["@全体"])
	}
	if m["@给"] != "群友乙" {
		t.Errorf("@给 应是昵称而不是 openid，got %v", m["@给"])
	}
}

// 字段必须严格成对：logx.pack 是按下标 0,1 / 2,3 读的，
// 落单会让后面所有字段整体错位
func TestGroupLogFieldsArePaired(t *testing.T) {
	fields := buildGroupLogFields("测试群一号", "群友甲", true,
		"@羽沫老爹 上号", "<@55558888>", true, "AAAA5555",
		func(string) string { return "群友乙" })
	if len(fields)%2 != 0 {
		t.Fatalf("字段数必须是偶数，实际 %d: %#v", len(fields), fields)
	}
	for i := 0; i < len(fields); i += 2 {
		if _, ok := fields[i].(string); !ok {
			t.Errorf("第 %d 个字段应是字符串键，实际 %T", i, fields[i])
		}
	}
	// 至少要能看到这几栏
	m := toMap(fields)
	for _, k := range []string{"group", "from", "at", "text", "原文", "@全体", "@给"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少字段 %q，实际 %#v", k, m)
		}
	}
}

// text 截断到 200 字：表情连发十几条时不截的话日志会爆
func TestGroupLogFieldsTruncates(t *testing.T) {
	long := strings.Repeat("啊", 500)
	fields := buildGroupLogFields("g", "f", false, long, long, false, "",
		func(string) string { return "" })
	if got := toMap(fields)["text"].(string); len([]rune(got)) > 201 {
		t.Errorf("未按 200 字截断: %d 字", len([]rune(got)))
	}
}

// 引用摘要：引用链读错是「回复挂错人」的主要来源，日志里必须能看出来
func TestQuotedPreview(t *testing.T) {
	els := []*webhook.MsgElement{{
		Author:  &webhook.User{MemberOpenID: "AAAA5555", Username: "群友乙"},
		Content: "帮看下这个",
	}}
	got := quotedPreview(els)
	if !strings.Contains(got, "群友乙") || !strings.Contains(got, "帮看下这个") {
		t.Errorf("引用摘要应含作者和内容，got %q", got)
	}
	// 没有引用时返回空，不占日志位置
	if quotedPreview(nil) != "" {
		t.Error("无引用时应返回空串")
	}
}
