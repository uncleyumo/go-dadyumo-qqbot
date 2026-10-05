package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 身份必须存下来。
//
// 2026-10-05 生产实况：平台每个事件都带 author.member_role，
// 但这个字段一路丢在 webhook 结构体里没人读，成员表里没有任何身份信息。
// 于是「艾特一下群主」对模型无解——它答「不知道谁是群主」，
// 还因为 JSON 输出残缺整条被按闭嘴处理。
func TestTouchMemberRoleStores(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.TouchMemberRole("OID-1", "owner")
	g.TouchMemberRole("OID-2", "admin")
	g.TouchMemberRole("OID-3", "member")

	for openID, want := range map[string]string{
		"OID-1": "owner", "OID-2": "admin", "OID-3": "member",
	} {
		m, ok := g.MemberOf(openID)
		if !ok {
			t.Fatalf("%s 应在成员表里", openID)
		}
		if m.Role != want {
			t.Errorf("%s 身份应为 %q，实际 %q", openID, want, m.Role)
		}
	}
}

// 认不出来的值不写。
//
// 关键在「宁可没有」而不是「默认 member」：平台在别人 @ 他时给的 mentions 里
// MemberRole 未必填，空字符串只说明「这次事件没带」，不代表他是普通成员。
// 存成 member 会让 prompt 里凭空多出一个「不是管理层的凭据」，而它是假的。
func TestTouchMemberRoleIgnoresUnknown(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.TouchMember("OID-1", "老王")
	g.TouchMemberRole("OID-1", "")
	g.TouchMemberRole("OID-1", "ROOT")
	g.TouchMemberRole("OID-1", "owner")

	m, _ := g.MemberOf("OID-1")
	if m.Role != "owner" {
		t.Fatalf("只有认得的值才该写，实际 %q", m.Role)
	}
}

// 角色会变：管理员被撤、群主转让。
// 所以是覆盖而不是累积，也不能因为新值是 member 就把旧的抹成空。
func TestTouchMemberRoleOverwrites(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.TouchMemberRole("OID-1", "admin")
	g.TouchMemberRole("OID-1", "member")

	m, _ := g.MemberOf("OID-1")
	if m.Role != "member" {
		t.Fatalf("身份应被最新值覆盖，实际 %q", m.Role)
	}
}

// 没登记过的人直接给身份，也要能建出成员条目——
// author 事件的顺序不保证先 TouchMember 后 TouchMemberRole。
func TestTouchMemberRoleCreatesMissing(t *testing.T) {
	g := NewGroup("g1", "群A")
	g.TouchMemberRole("OID-new", "owner")

	m, ok := g.MemberOf("OID-new")
	if !ok || m.Role != "owner" {
		t.Fatalf("应建出条目并记上身份，实际 ok=%v %+v", ok, m)
	}
}

// Role 要能落盘再读回来。
//
// 成员表是持久化的：重启后 prompt 还要靠它渲染【群里的管理层】，
// 不序列化就等于每次重启都丢掉所有身份信息。
func TestRoleSurvivesPersist(t *testing.T) {
	s := New(10)
	g := s.Group("G1", "群一")
	g.TouchMember("OID-1", "群主大人")
	g.TouchMemberRole("OID-1", "owner")

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"role": "owner"`) {
		t.Fatalf("memory.json 里没有 role，原文:\n%s", raw)
	}

	s2 := New(10)
	if err := s2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	m, ok := s2.Group("G1", "").MemberOf("OID-1")
	if !ok {
		t.Fatal("还原后应有这个人")
	}
	if m.Role != "owner" || m.Name != "群主大人" {
		t.Errorf("身份或名字没还原过来：%+v", m)
	}
}
