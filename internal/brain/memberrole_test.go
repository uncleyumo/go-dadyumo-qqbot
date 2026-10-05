package brain

import (
	"strings"
	"testing"

	"dadyumo/internal/config"
)

// 群主/管理员必须出现在 prompt 里。
//
// 2026-10-05 生产实况：成员表里没有任何身份信息，于是「艾特一下群主」
// 对模型无解——它答「不知道谁是群主」，还因为 JSON 输出残缺整条被按闭嘴处理。
func TestPromptRendersMemberRoles(t *testing.T) {
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	e := newTestEngine(t, &recordingSender{}, nil, false)
	g := e.mem.Group("g1", "群A")
	g.TouchMember("OID-1", "群主大人")
	g.TouchMemberRole("OID-1", "owner")
	g.TouchMember("OID-2", "管理员甲")
	g.TouchMemberRole("OID-2", "admin")
	g.TouchMember("OID-3", "普通群众")
	g.TouchMemberRole("OID-3", "member")

	sys := systemPrompt(cfg.Get(), g, MoodSignal{}, "", "", "普通群众")

	if !strings.Contains(sys, "【群里的管理层】") {
		t.Fatal("prompt 里应有【群里的管理层】一节")
	}
	if !strings.Contains(sys, "- 群主大人：群主") {
		t.Errorf("群主没被渲染出来，实际:\n%s", sys)
	}
	if !strings.Contains(sys, "- 管理员甲：管理员") {
		t.Errorf("管理员没被渲染出来，实际:\n%s", sys)
	}
	// 普通成员占了绝大多数，全列出来只是烧 token
	if strings.Contains(sys, "- 普通群众：") {
		t.Errorf("不该把普通成员也列进管理层，实际:\n%s", sys)
	}
}

// 没人是管理层时整节都不出现——不能输出一个空的【群里的管理层】。
func TestPromptOmitsEmptyRoleSection(t *testing.T) {
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	e := newTestEngine(t, &recordingSender{}, nil, false)
	g := e.mem.Group("g1", "群A")
	g.TouchMember("OID-1", "普通群众")
	g.TouchMemberRole("OID-1", "member")

	sys := systemPrompt(cfg.Get(), g, MoodSignal{}, "", "", "普通群众")
	if strings.Contains(sys, "【群里的管理层】") {
		t.Errorf("没有管理层时不该有这一节，实际:\n%s", sys)
	}
}

// 提示词不能再说「你发出的每一条挂在对方消息下，群里已经看得出你在回谁」。
//
// 那句话是错的：被动回复的 msg_id 只是发送授权，不产生任何可见关联。
// 不发 message_reference 的消息在群里就是普通气泡（2026-10-05 客户端实测）。
// 留着它，模型会认定「反正已经挂在人家消息下了，不用引用」——
// 而引用气泡恰恰是唯一能让人看出在回谁的东西。
func TestPromptDoesNotClaimRepliesAreVisiblyAttached(t *testing.T) {
	cfg := config.NewStoreFrom(func(c *config.Config) {})
	e := newTestEngine(t, &recordingSender{}, nil, false)
	sys := systemPrompt(cfg.Get(), e.mem.Group("g1", "群A"), MoodSignal{}, "", "", "老张")

	for _, bad := range []string{"已经看得出你在回谁", "本来就挂在"} {
		if strings.Contains(sys, bad) {
			t.Errorf("提示词里不该再有 %q——那是错的，会劝退模型用引用", bad)
		}
	}
	// 反过来要明确说出真相：不给 q:true 就没有可见关联
	if !strings.Contains(sys, "看不出你在回谁") {
		t.Errorf("应明确说明默认看不出在回谁，实际:\n%s", sys)
	}
}
