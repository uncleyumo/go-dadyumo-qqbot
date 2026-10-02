package memory

import "testing"

// 账号昵称与群名片的关系，是本次排查里最容易被想当然的地方。
//
// 平台给同一个人两个不同字段的两个不同名字，而且会**交替出现**：
// 他自己发言时 author 给账号昵称，别人 @ 他时 mentions 给群名片。
// 生产数据四个样本零例外（见 Member.Card 的注释）。
//
// 混在一起处理会产生两个后果：主名被群名片占掉，以及别名表被这两个
// 名字刷满、真正的改名记录反而丢光。

func TestAccountNameIsPrimaryCardIsSeparate(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-qn", "群友乙")
	g.TouchMemberCard("openid-qn", "群名片丁")

	m, ok := g.MemberOf("openid-qn")
	if !ok {
		t.Fatal("成员应存在")
	}
	if m.Name != "群友乙" {
		t.Errorf("主名应是账号昵称（本人自称），got %q", m.Name)
	}
	if m.Card != "群名片丁" {
		t.Errorf("群名片应单独存住，got %q", m.Card)
	}
}

// 两个名字会随群里的互动交替到来，重复登记是常态
func TestAlternatingNamesDoNotFillAliases(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-qn", "群友乙")
	for i := 0; i < 10; i++ {
		g.TouchMemberCard("openid-qn", "群名片丁")
		g.TouchMember("openid-qn", "群友乙")
	}
	m, _ := g.MemberOf("openid-qn")
	if m.Name != "群友乙" {
		t.Errorf("主名不该被名片抢走，got %q", m.Name)
	}
	if len(m.Aliases) != 1 {
		t.Errorf("交替出现 20 次也只该留 1 条别名，实际 %d 条: %v",
			len(m.Aliases), m.Aliases)
	}
}

// 真的改过名的话，历史必须留得住——不能被账号名/名片交替刷掉
func TestRealRenameStillRecorded(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-z", "张三")
	g.TouchMemberCard("openid-z", "群名片乙")
	g.TouchMember("openid-z", "群友丙") // 真的改名了
	g.TouchMemberCard("openid-z", "群名片乙")

	m, _ := g.MemberOf("openid-z")
	if m.Name != "群友丙" {
		t.Errorf("主名应是最新账号昵称，got %q", m.Name)
	}
	found := map[string]bool{}
	for _, a := range m.Aliases {
		found[a] = true
	}
	if !found["张三"] {
		t.Errorf("旧账号昵称应保留，aliases=%v", m.Aliases)
	}
	if !found["群名片乙"] {
		t.Errorf("群名片应保留，aliases=%v", m.Aliases)
	}
}

func TestKnownNamesCoversAll(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-z", "张三")
	g.TouchMemberCard("openid-z", "群名片乙")
	g.TouchMember("openid-z", "群友丙")

	m, _ := g.MemberOf("openid-z")
	got := map[string]bool{}
	for _, n := range m.KnownNames() {
		got[n] = true
	}
	for _, want := range []string{"群友丙", "群名片乙", "张三"} {
		if !got[want] {
			t.Errorf("KnownNames 应含 %q，实际 %v", want, got)
		}
	}
}

// 撞名统计要算上群名片与旧称，而且同一个人自己名下重复不算撞
func TestDupNamesAcrossAllForms(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-a", "张三")
	g.TouchMember("openid-a", "李四")
	g.TouchMemberCard("openid-a", "张三") // a 自己：账号李四、名片张三 → 不算撞

	g.TouchMember("openid-b", "李四") // 与 a 的账号昵称撞
	dup := g.DupNames()
	if dup["李四"] != 2 {
		t.Errorf("账号昵称撞名应被统计，got %v", dup)
	}
	if dup["张三"] != 0 {
		t.Errorf("同一个人自己名下重复出现不算撞名，got %v", dup)
	}
}

// 名片撞车也要算：群名片是展示名，撞的概率不低
func TestDupNamesIncludesCard(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-a", "阿强")
	g.TouchMemberCard("openid-a", "隔壁老王")
	g.TouchMember("openid-b", "小明")
	g.TouchMemberCard("openid-b", "隔壁老王") // 两人的名片撞了

	dup := g.DupNames()
	if dup["隔壁老王"] != 2 {
		t.Errorf("群名片撞名应被统计，got %v", dup)
	}
}

// 空名字不该污染成员表
func TestEmptyNamesIgnored(t *testing.T) {
	g := NewGroup("g1", "测试群")
	g.TouchMember("openid-a", "  ")
	g.TouchMemberCard("openid-a", "")
	g.TouchMember("openid-a", "阿强")

	m, ok := g.MemberOf("openid-a")
	if !ok {
		t.Fatal("成员应存在")
	}
	if m.Name != "阿强" {
		t.Errorf("空白名不该成为主名，got %q", m.Name)
	}
	if m.Card != "" {
		t.Errorf("空白名片不该被记下，got %q", m.Card)
	}
}

// 超长昵称要截断，防着撑爆内存和提示词
func TestLongNameClipped(t *testing.T) {
	g := NewGroup("g1", "测试群")
	long := ""
	for i := 0; i < 100; i++ {
		long += "超长昵称"
	}
	g.TouchMember("openid-a", long)
	m, _ := g.MemberOf("openid-a")
	if n := len([]rune(m.Name)); n > maxAliasRune {
		t.Errorf("超长昵称应截断到 %d 字，实际 %d", maxAliasRune, n)
	}
}