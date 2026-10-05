package agent

import (
	"testing"

	"dadyumo/internal/webhook"
)

// 从 message_scene.ext 里挑出 REFIDX 那一项。
//
// 官方文档说 message_reference 的 message_id 取自 message_scene.ext；
// webhook 那边把 ext 声明成 []string，说明它不是 JSON 对象而是若干个串。
// 具体哪一项官方没写死，所以只认「以 REFIDX 开头」这一条规则。
//
// **不看结尾的等号**：原先要求 `HasSuffix(s, "==")`，理由是「光有前缀没有
// base64 尾巴的多半是别的字段」。2026-10-05 生产实测把这个理由证伪了——
// 平台自己在发送响应里回的 ref_idx 长这样，一个等号都没有：
//
//	REFIDX_Qei0iMbCOW3ppae9xcVSc0rm9yqc4qnAcCg4vzTeynTp9SdlGHyVAg008Hbl9
//	Jy+yA+LCZQ1Ain0c4/6ZvJqkgN1hB5UrAUCtlype9vWVI1rJuREWnOzxs0uipjLv5P2
//
// base64 补位有 `==` / `=` / 无 三种，按 `==` 收口等于把大部分 REFIDX
// 判成「不是引用 id」，于是 message_reference 一次都没填出去过。
// 前缀已经足够特异，改成只认前缀 + 不许含空白。
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
		// 生产实测的形态：ext 是 key=value 的列表，引用 id 挂在 msg_idx= 下面，
		// 而且没有 base64 补位。
		{"实测形态 msg_idx=", []string{
			"auth_token=X-qanh",
			"msg_idx=REFIDX_up/YiEUb8GdEXXvI6brWRvTBuCHlaxd5HElEqR5jze2wkEJVddGFxZiQzibnAXSCQB/CHEwNaKZiLI6LuedCqUBrSVSaLO0Oq3bS8uDYbglrrnPeQkuSGokwo51hdSHY",
		}, "REFIDX_up/YiEUb8GdEXXvI6brWRvTBuCHlaxd5HElEqR5jze2wkEJVddGFxZiQzibnAXSCQB/CHEwNaKZiLI6LuedCqUBrSVSaLO0Oq3bS8uDYbglrrnPeQkuSGokwo51hdSHY"},
		{"msg_idx 后面不是 REFIDX", []string{"msg_idx=whatever"}, ""},
		{"别的键不认", []string{"auth_token=REFIDX_abc"}, ""},
		{"值里带空格的不认", []string{"msg_idx=REFIDX abc"}, ""},
		{"光秃秃的前缀不认", []string{"msg_idx=REFIDX"}, ""},
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
