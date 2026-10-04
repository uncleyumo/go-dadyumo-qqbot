package agent

import (
	"testing"

	"dadyumo/internal/webhook"
)

// 从 message_scene.ext 里挑出 REFIDX 那一项。
//
// 官方文档说 message_reference 的 message_id 取自 message_scene.ext，
// 格式 `REFIDX_xxx==`；webhook 那边把 ext 声明成 []string，
// 说明它不是 JSON 对象而是若干个串。具体哪一项官方没写死，
// 所以这里只认「以 REFIDX 开头」这一条规则。
func TestRefIdxOfPicksTheREFIDXEntry(t *testing.T) {
	cases := []struct {
		name string
		ext  []string
		want string
	}{
		{"只有一项", []string{"REFIDX_abc=="}, "REFIDX_abc=="},
		{"混在多项里", []string{"xxx", "REFIDX_def==", "yyy"}, "REFIDX_def=="},
		{"带空格", []string{"  REFIDX_ghi==  "}, "REFIDX_ghi=="},
		{"没有", []string{"aaa", "bbb"}, ""},
		{"空", nil, ""},
		// 光有前缀没有 base64 尾巴多半是别的字段，认它等于往
		// message_reference 里塞垃圾串，平台会拒。
		{"前缀有但缺 == 尾巴", []string{"REFIDX"}, ""},
		{"前缀有但尾巴不全", []string{"REFIDX_abc"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := refIdxOf(&webhook.MessageScene{Ext: c.ext})
			if got != c.want {
				t.Errorf("ext=%v 应得到 %q，实际 %q", c.ext, c.want, got)
			}
		})
	}
}

// scene 整个为 nil 不能 panic。
//
// 平台不保证每条消息都带 message_scene，而这条路径在每条群消息上都会跑——
// 一个 nil 指针就够让整个 webhook handler 挂掉。
func TestRefIdxOfNilScene(t *testing.T) {
	if got := refIdxOf(nil); got != "" {
		t.Errorf("scene 为 nil 应返回空串，实际 %q", got)
	}
}

// 别名不是 REFIDX 开头的一律不认：宁可没有引用，也不能把垃圾串送进
// message_reference——平台会拒，而本地看不出原因。
func TestRefIdxOfRejectsForeignPrefix(t *testing.T) {
	got := refIdxOf(&webhook.MessageScene{Ext: []string{"MSGID_abc==", "refidx_lower=="}})
	if got != "" {
		t.Errorf("只认大写 REFIDX 前缀，实际却认出了 %q", got)
	}
}
