package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/memory"
)

// 日志里出现裸 openid 是这次要消灭的问题：32 位十六进制对人毫无意义，
// 而「这条回复挂给了谁」恰恰是排查认错人时最想确认的一件事。

func TestGroupLabelPrefersName(t *testing.T) {
	g := &memory.Group{OpenID: "99998888777766665555444433332222", Name: "测试群一号"}
	if got := groupLabel(g); got != "测试群一号" {
		t.Errorf("应显示群名，got %q", got)
	}
	// 没配过名字的群退回 openid，至少还能和回调日志对上
	bare := &memory.Group{OpenID: "99998888777766665555444433332222"}
	if got := groupLabel(bare); got != bare.OpenID {
		t.Errorf("无名群应退回 openid，got %q", got)
	}
	if groupLabel(nil) != "" {
		t.Error("nil 群不应 panic")
	}
}

// 已发言这行日志里不得出现 openid。「回给」是该显示昵称的。
func TestSpeakLogHasNoOpenID(t *testing.T) {
	mem := memory.New(50)
	g := mem.Group("99998888777766665555444433332222", "测试群一号")
	g.TouchMember("11112222333344445555666677778888", "群友甲")

	// 复现 speak() 里组装「回给」的那段逻辑
	replyTo := "11112222333344445555666677778888"
	toName := replyTo
	if n := g.NameOfByOpenID(replyTo); n != "" {
		toName = n
	} else {
		toName = shortOpenID(replyTo)
	}
	if toName != "群友甲" {
		t.Errorf("回给应显示昵称，got %q", toName)
	}
	// 认不出来时至少给个短尾号，而不是 32 位全串
	unknown := "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
	fallback := shortOpenID(unknown)
	if len([]rune(fallback)) > 8 {
		t.Errorf("认不出的人应给短尾号，got %q", fallback)
	}
}

// 生产日志的实况：以前「group」「回给」两栏打的是 openid，看日志得自己查表是谁
func TestGroupLabelNeverLeaksInNormalFlow(t *testing.T) {
	mem := memory.New(50)
	g := mem.Group("99998888777766665555444433332222", "测试群一号")
	g.TouchMember("11112222333344445555666677778888", "群友甲")

	// 模拟「群消息」与「已发言」两行日志里所有会出现标识的地方
	parts := []string{
		groupLabel(g),
		g.NameOfByOpenID("11112222333344445555666677778888"),
	}
	joined := strings.Join(parts, " ")
	for _, bad := range []string{"99998888", "11112222"} {
		if strings.Contains(joined, bad) {
			t.Errorf("日志里出现了裸 openid %s: %s", bad, joined)
		}
	}
}
