package brain

import (
	"strings"
	"testing"
)

func TestSplitSegmentsByNewline(t *testing.T) {
	got := SplitSegments("我\n真的\n不\n知\n道\n！", 40, 5)
	want := []string{"我", "真的", "不", "知", "道"}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 条，实际 %d 条: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条期望 %q，实际 %q", i, want[i], got[i])
		}
	}
	// 最后一条「！」被条数上限截断，符合「最多 5 条」的约定
}

func TestSplitSegmentsTrimsAndDropsEmpty(t *testing.T) {
	got := SplitSegments("6\n\n  \n牛逼", 40, 5)
	if len(got) != 2 || got[0] != "6" || got[1] != "牛逼" {
		t.Fatalf("空行应被丢弃且两端去空格，实际 %#v", got)
	}
}

func TestSplitSegmentsLiteralBackslashN(t *testing.T) {
	// 模型偶尔不按 JSON 转义，直接写字面量 \n
	got := SplitSegments(`牛逼\nhhh`, 40, 5)
	if len(got) != 2 || got[0] != "牛逼" || got[1] != "hhh" {
		t.Fatalf("字面量 \\n 也应被当作分隔，实际 %#v", got)
	}
}

func TestSplitSegmentsSplitsLongByPunctuation(t *testing.T) {
	long := "这事我跟你讲啊真的挺离谱的，他居然能干出这种事，我是没想到的。"
	got := SplitSegments(long, 12, 5)
	if len(got) < 2 {
		t.Fatalf("长句应被拆成多条，实际只有 %d 条: %#v", len(got), got)
	}
	for _, s := range got {
		if runeLen(s) > 24 {
			t.Fatalf("拆出来的单条仍然过长(%d 字): %q", runeLen(s), s)
		}
	}
	if strings.Join(got, "") != long {
		t.Fatalf("拆分不应丢字，拼回来应为原文，实际 %q", strings.Join(got, ""))
	}
}

func TestSplitSegmentsHardSplitsWhenNoPunct(t *testing.T) {
	long := strings.Repeat("哈", 50)
	got := SplitSegments(long, 20, 5)
	if len(got) != 3 {
		t.Fatalf("无标点长串应被硬切成 3 条，实际 %d 条: %#v", len(got), got)
	}
	for i, s := range got {
		if runeLen(s) > 20 {
			t.Fatalf("第 %d 条超过上限: %d 字", i, runeLen(s))
		}
	}
}

func TestSplitSegmentsRespectsMaxSegments(t *testing.T) {
	got := SplitSegments("1\n2\n3\n4\n5\n6\n7\n8", 40, 5)
	if len(got) != 5 {
		t.Fatalf("条数上限应生效，实际 %d 条: %#v", len(got), got)
	}
	if got[0] != "1" || got[4] != "5" {
		t.Fatalf("应保留最前面的几条，实际 %#v", got)
	}
}

func TestSplitSegmentsEmpty(t *testing.T) {
	if got := SplitSegments("   \n\n", 40, 5); len(got) != 0 {
		t.Fatalf("空白输入应返回空，实际 %#v", got)
	}
}

func TestDedupeMentionsKeepsOnlyFirst(t *testing.T) {
	segs := []string{
		"@张三 这事儿吧",
		"@张三 关我屁事",
		"@张三 他发他的",
		"@张三 我看我的",
	}
	got := DedupeMentions(segs)
	if got[0] != "@张三 这事儿吧" {
		t.Fatalf("第一条的 @ 应保留: %q", got[0])
	}
	for i, s := range got[1:] {
		if strings.HasPrefix(s, "@") {
			t.Fatalf("第 %d 条不应再带 @: %q", i+1, s)
		}
	}
}

func TestDedupeMentionsStripsAllWhenFirstClean(t *testing.T) {
	got := DedupeMentions([]string{"在呢", "@李四 干啥", "@李四 别喊"})
	if strings.HasPrefix(got[0], "@") {
		t.Fatalf("第一条本来没有 @，不应新增: %q", got[0])
	}
	if strings.HasPrefix(got[1], "@") || strings.HasPrefix(got[2], "@") {
		t.Fatalf("后面的 @ 都应剥掉: %#v", got)
	}
}

func TestDedupeMentionsNoMention(t *testing.T) {
	in := []string{"6", "牛逼"}
	got := DedupeMentions(in)
	if got[0] != "6" || got[1] != "牛逼" {
		t.Fatalf("无 @ 时不应改动: %#v", got)
	}
}

// TestStripAtAllKillsFakeMention 机器人没有 @ 任何人的能力，
// 正文里的 @all / @全体成员 一定是模型在演，必须删掉。
//
// 事故原文（2026-10-01 13:14 与 13:18，测试群一号）：
//
//	「剩下那帮人你@all自己喊」
//	「你自己@all不就完了」
//	「搁这刷表情包呢 / 自己@all / 我又不是喊人的」
func TestStripAtAllKillsFakeMention(t *testing.T) {
	cases := []struct{ in, want string }{
		// 只删 @all 这个 token，周边的字一个不动
		{"你自己@all不就完了", "你自己不就完了"},
		{"剩下那帮人你@all自己喊", "剩下那帮人你自己喊"},
		{"自己@all", "自己"},
		{"@全体成员 来个人", "来个人"},
		{"@所有人 起立", "起立"},
		{"@all", ""},
		// 大小写都要覆盖，模型爱写 @ALL / @All
		{"你@ALL自己喊", "你自己喊"},
		{"@All 有人吗", "有人吗"},
		// 没有 @ 就不该动
		{"今天打不打", "今天打不打"},
		{"6", "6"},
	}
	for _, c := range cases {
		if got := stripAtAll(c.in); got != c.want {
			t.Errorf("stripAtAll(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// @全体成员 出现在句中时 mentionPrefix 那套管不到，必须由 stripAtAll 兜住
func TestDedupeMentionsStripsAtAllMidSentence(t *testing.T) {
	got := DedupeMentions([]string{"剩下那帮人你@all自己喊", "我又不是喊人的"})
	if strings.Contains(got[0], "@all") {
		t.Fatalf("句中的 @all 没被清掉: %q", got[0])
	}
	if got[1] != "我又不是喊人的" {
		t.Fatalf("正常内容不该被改: %q", got[1])
	}
}

// 整条只剩 @全体成员 时会清成空串，空串绝不能发到 QQ（会渲染成空白消息）
func TestDropEmptyRemovesBlankSegments(t *testing.T) {
	// 走真实调用链：先剥 @ 再滤空
	got := dropEmpty(DedupeMentions([]string{"自己@all", "我又不是喊人的", "  ", "@all"}))
	// "自己@all" 剥掉 @all 后剩「自己」——是个完整的词，不该丢
	if len(got) != 2 || got[0] != "自己" || got[1] != "我又不是喊人的" {
		t.Fatalf("空段应被丢掉、完整短句应保留: %#v", got)
	}
	if len(dropEmpty([]string{"", "  "})) != 0 {
		t.Fatal("全空时应返回空切片")
	}
}

// 走一遍真实事故那条消息的完整出口清理
func TestDedupeMentionsRealAccidentLine(t *testing.T) {
	// 模型原文：搁这刷表情包呢 / 自己@all / 我又不是喊人的
	segs := DedupeMentions([]string{"搁这刷表情包呢", "@all", "我又不是喊人的"})
	segs = dropEmpty(segs)
	if len(segs) != 2 {
		t.Fatalf("应剩 2 条，got %#v", segs)
	}
	for _, s := range segs {
		if strings.Contains(s, "@") {
			t.Errorf("残留 @: %q", s)
		}
	}
}
