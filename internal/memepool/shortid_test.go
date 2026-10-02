package memepool

import (
	"path/filepath"
	"testing"
	"time"
)

// 池内 ID 必须是能徒手抄进 JSON 的短号。
//
// 内容哈希有 19 位，让模型逐位抄进 {"t":"img","id":…} 迟早抄错，
// 前端 JSON 也早就过不了 2^53 的精确整数上限。这里钉死它。
func TestIDsAreShortAndSequential(t *testing.T) {
	p := New(DefaultConfig(), newMemStorage())
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := p.Add([]byte{byte(i), 0xFF, 0xD8}, "image/jpeg", "图", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Errorf("第 %d 个应是 %d，got %d", i, i+1, id)
		}
		if id > 1_000_000 {
			t.Errorf("ID 必须短到能手抄，got %d", id)
		}
	}
}

// 同一张图重复入库要拿回同一个短号，且不占新名额
func TestDuplicateContentReusesID(t *testing.T) {
	st := newMemStorage()
	p := New(DefaultConfig(), st)
	data := []byte{1, 2, 3, 0xFF, 0xD8}

	a, err := p.Add(data, "image/jpeg", "图A", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Add(data, "image/jpeg", "图A", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("重复入库应拿回同一个 ID，got %d 与 %d", a, b)
	}
	if p.Len() != 1 {
		t.Errorf("不应占第二个名额，got %d", p.Len())
	}
	// 删掉之后重新入库要能再占回名额（否则 byKey 与 memes 会不同步）
	if err := p.Remove(a); err != nil {
		t.Fatal(err)
	}
	c, err := p.Add(data, "image/jpeg", "图A", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Len() != 1 {
		t.Errorf("重新入库应恢复成 1 张，got %d", p.Len())
	}
	if c == a {
		t.Errorf("重新入库应给新号（ID 不回收），got %d", c)
	}
	if len(st.data) != 1 {
		t.Errorf("MinIO 上应只剩一个对象，got %d", len(st.data))
	}
}

// 重启后 byKey 与 nextID 必须重建，否则会重复上传、还会撞号
func TestLoadRebuildsDedupIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memes.json")
	data := []byte{9, 8, 7, 0xFF, 0xD8}

	p1 := New(DefaultConfig(), newMemStorage())
	id1, err := p1.Add(data, "image/jpeg", "图", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.Save(path); err != nil {
		t.Fatal(err)
	}

	st2 := newMemStorage()
	p2 := New(DefaultConfig(), st2)
	if err := p2.Load(path); err != nil {
		t.Fatal(err)
	}
	// 同一张图再入库：应认得出是重复，不重新上传、不改 ID
	again, err := p2.Add(data, "image/jpeg", "图", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again != id1 {
		t.Errorf("重启后重复入库应拿回原 ID %d，got %d", id1, again)
	}
	if len(st2.data) != 0 {
		t.Errorf("重复入库不该再上传，storage 里却有 %d 个对象", len(st2.data))
	}
	// 新图必须拿到更大的号，不能撞上恢复出来的号
	fresh, err := p2.Add([]byte{1, 1, 1, 0xFF, 0xD8}, "image/jpeg", "新图", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fresh == id1 {
		t.Errorf("新图撞上了已有 ID %d", id1)
	}
	if p2.Len() != 2 {
		t.Errorf("应有 2 张，got %d", p2.Len())
	}
}
