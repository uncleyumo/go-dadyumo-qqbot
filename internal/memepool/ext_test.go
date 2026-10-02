package memepool

import (
	"strings"
	"testing"
	"time"
)

// 对象名的后缀必须跟真实格式一致。
//
// 原来一律 .jpg，注释说「MinIO 靠魔数判类型」——靠魔数的是 Content-Type，
// 而**文件后缀不是魔数**。对象名会原样进 URL，一堆 .jpg 里混着 GIF 时
// 浏览器和 QQ 都会照后缀猜类型，猜错的后果正是「本来会动的图发出去变静图」。
func TestObjectNameFollowsRealFormat(t *testing.T) {
	cases := []struct{ ct, want string }{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"", ".bin"},
		{"application/octet-stream", ".bin"},
	}
	for _, c := range cases {
		got := objectName(12345, c.ct)
		if !strings.HasSuffix(got, c.want) {
			t.Errorf("contentType=%q 应得到后缀 %s，实际 %q", c.ct, c.want, got)
		}
		if !strings.HasPrefix(got, "memes/") {
			t.Errorf("对象名应留在 memes/ 前缀下：%q", got)
		}
	}
}

// GIF 入池后对象名必须是 .gif
func TestAddStoresGIFWithGIFExtension(t *testing.T) {
	st := newMemStorage()
	p := New(testCfg(), st)
	gifBytes := append([]byte("GIF89a"), make([]byte, 2048)...)
	id, err := p.Add(gifBytes, "image/gif", "会动的猫", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, ok := p.Get(id)
	if !ok {
		t.Fatal("入库后应能取到")
	}
	if !strings.HasSuffix(m.ObjectKey, ".gif") {
		t.Errorf("GIF 的对象名应以 .gif 结尾，实际 %q", m.ObjectKey)
	}
	// 存进去的字节必须和入池时完全一致——动图不能在中途被改写
	got, _, err := st.Get(m.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(gifBytes) {
		t.Errorf("存储的字节与入池时不一致（可能中间被重编码了）")
	}
}

// 老数据（键是 .jpg）必须仍能加载并读出，不能因为后缀规则变了就丢图
func TestLoadAcceptsLegacyJPGKeys(t *testing.T) {
	dir := t.TempDir()
	st := newMemStorage()
	p := New(testCfg(), st)
	id, err := p.Add(img(1), "image/jpeg", "老数据", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := p.Get(id)
	if !strings.HasSuffix(m.ObjectKey, ".jpg") {
		t.Fatalf("前置条件不成立：%q", m.ObjectKey)
	}
	if err := p.Save(dir + "/memes.json"); err != nil {
		t.Fatal(err)
	}
	p2 := New(testCfg(), newMemStorage())
	if err := p2.Load(dir + "/memes.json"); err != nil {
		t.Fatalf("加载历史数据失败: %v", err)
	}
	if p2.Len() != 1 {
		t.Fatalf("应恢复 1 条，实际 %d", p2.Len())
	}
	m2, ok := p2.Get(id)
	if !ok {
		t.Fatal("按原 ID 应能取到")
	}
	if m2.ObjectKey != m.ObjectKey {
		t.Errorf("对象名应原样保留：%q != %q", m2.ObjectKey, m.ObjectKey)
	}
}
