package qqapi

import "testing"

// 反查命中。
func TestQuoteIndexAddLookup(t *testing.T) {
	q := NewQuoteIndex(10)
	q.Add("g1", "REFIDX_a", QuoteSrc{OpenID: "o-a", Name: "张三", Text: "你说的那个我懂"})

	src, ok := q.Lookup("g1", "REFIDX_a")
	if !ok {
		t.Fatal("刚登记的就该查得到")
	}
	if src.Name != "张三" || src.Text != "你说的那个我懂" || src.OpenID != "o-a" {
		t.Errorf("内容应原样取回，got %#v", src)
	}
}

// 查不到是常态（那条本机没见过），不能当成错误。
func TestQuoteIndexLookupMiss(t *testing.T) {
	q := NewQuoteIndex(10)
	if _, ok := q.Lookup("g1", "REFIDX_never"); ok {
		t.Error("没登记过的 id 不该命中")
	}
	if _, ok := q.Lookup("g-never", "REFIDX_a"); ok {
		t.Error("没见过的群不该命中")
	}
}

// refIdx 为空直接忽略：平台不一定每条消息都给 msg_idx，那不是错误，
// 更不能用空串当 key 把别的消息挤掉。
func TestQuoteIndexIgnoresEmptyRefIdx(t *testing.T) {
	q := NewQuoteIndex(10)
	q.Add("g1", "", QuoteSrc{Name: "无 id 的消息"})
	q.Add("", "REFIDX_a", QuoteSrc{Name: "没有群"})

	if _, ok := q.Lookup("g1", ""); ok {
		t.Error("空 refIdx 不该被登记")
	}
	if _, ok := q.Lookup("", "REFIDX_a"); ok {
		t.Error("空群号不该被登记")
	}
}

// 同一条消息重复登记（平台可能分多次推正文与附件）以最后一次为准。
func TestQuoteIndexSameRefIdxOverwrites(t *testing.T) {
	q := NewQuoteIndex(10)
	q.Add("g1", "REFIDX_a", QuoteSrc{Name: "张三", Text: "只有正文"})
	q.Add("g1", "REFIDX_a", QuoteSrc{Name: "张三", Text: "只有正文", Images: []string{"u1"}})

	src, ok := q.Lookup("g1", "REFIDX_a")
	if !ok {
		t.Fatal("应查得到")
	}
	if len(src.Images) != 1 {
		t.Errorf("后一次登记应补上附件，got %#v", src)
	}
	// 不能变成两条
	if n := len(q.m["g1"]); n != 1 {
		t.Errorf("同一条消息不该占两个位置，got %d", n)
	}
}

// 超出上限淘汰最旧的，且**新的必须还在**——淘汰方向写反的话，
// 索引里留下的全是老消息，引用刚说过的话反而查不到。
func TestQuoteIndexEvictsOldest(t *testing.T) {
	q := NewQuoteIndex(3)
	for _, id := range []string{"a", "b", "c", "d"} {
		q.Add("g1", "REFIDX_"+id, QuoteSrc{Text: id})
	}

	if _, ok := q.Lookup("g1", "REFIDX_a"); ok {
		t.Error("最旧的应被淘汰")
	}
	for _, id := range []string{"b", "c", "d"} {
		if _, ok := q.Lookup("g1", "REFIDX_"+id); !ok {
			t.Errorf("REFIDX_%s 应还在", id)
		}
	}
}

// 两个群互不干扰。
func TestQuoteIndexPerGroup(t *testing.T) {
	q := NewQuoteIndex(10)
	q.Add("g1", "REFIDX_a", QuoteSrc{Name: "一群的人"})
	q.Add("g2", "REFIDX_a", QuoteSrc{Name: "二群的人"})

	s1, _ := q.Lookup("g1", "REFIDX_a")
	s2, _ := q.Lookup("g2", "REFIDX_a")
	if s1.Name != "一群的人" || s2.Name != "二群的人" {
		t.Errorf("两个群的同 id 消息串台了：%q / %q", s1.Name, s2.Name)
	}
}

// keep <= 0 取默认值，不能变成「什么都存不下」。
func TestQuoteIndexDefaultKeep(t *testing.T) {
	q := NewQuoteIndex(0)
	if q.keep != defaultQuoteKeep {
		t.Fatalf("keep 应取默认值，got %d", q.keep)
	}
	q.Add("g1", "REFIDX_a", QuoteSrc{Name: "张三"})
	if _, ok := q.Lookup("g1", "REFIDX_a"); !ok {
		t.Error("默认配置下刚登记的必须查得到")
	}
}
