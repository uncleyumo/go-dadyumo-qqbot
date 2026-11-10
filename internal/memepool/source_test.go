package memepool

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// AddFrom 记来源群；Add 保持旧行为（来源未知）。
func TestAddFromRecordsSourceGroup(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	now := time.Now()
	id, err := p.AddFrom(img(1), "image/jpeg", "来自群 A", "GROUP-A", now)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := p.Get(id)
	if !ok || m.SourceGroup != "GROUP-A" {
		t.Fatalf("来源群应记入 Meme，got %+v", m)
	}

	old, err := p.Add(img(2), "image/jpeg", "旧调用", now)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := p.Get(old); m.SourceGroup != "" {
		t.Fatalf("Add 是旧接口，来源应为空，got %q", m.SourceGroup)
	}
}

// 落盘/恢复要保留来源群；旧文件缺字段时读成空串，不阻断加载。
func TestSourceGroupPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.json")
	p := New(testCfg(), newMemStorage())
	now := time.Now()
	if _, err := p.AddFrom(img(1), "image/jpeg", "群 A 的图", "GROUP-A", now); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}

	q := New(testCfg(), newMemStorage())
	if err := q.Load(path); err != nil {
		t.Fatal(err)
	}
	ms := q.Browse(now)
	if len(ms) != 1 || ms[0].SourceGroup != "GROUP-A" {
		t.Fatalf("恢复后应保留来源群，got %+v", ms)
	}

	// 旧版本文件没有 source_group：能加载，来源为空。
	writeFile(t, path, `{"version":1,"memes":[{"id":1,"object_key":"k","descr":"旧图","added_at":1,"expires_at":9999999999}]}`)
	r := New(testCfg(), newMemStorage())
	if err := r.Load(path); err != nil {
		t.Fatalf("旧文件应能加载，got %v", err)
	}
	if ms := r.Browse(now); len(ms) != 1 || ms[0].SourceGroup != "" {
		t.Fatalf("旧数据来源应为空，got %+v", ms)
	}
}

// 策展过滤：只跳过被接管来源群的图；来源未知的旧数据照常参与。
func TestFilterPausedMemes(t *testing.T) {
	in := []Meme{
		{ID: 1, SourceGroup: "PAUSED"},
		{ID: 2, SourceGroup: "ACTIVE"},
		{ID: 3, SourceGroup: ""},
	}
	got := filterPausedMemes(in, func(g string) bool { return g == "PAUSED" })
	if len(got) != 2 {
		t.Fatalf("应保留 2 条，got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.SourceGroup == "PAUSED" {
			t.Fatalf("被接管的群应被过滤，got %+v", m)
		}
	}
}

// 全部候选都来自被接管的群时，runOnce 在调模型之前返回。
// router 传 nil：过滤失效会以 nil 解引用 panic 收场，而不是静默烧钱。
func TestCuratorSkipsAllPausedGroups(t *testing.T) {
	pool := New(testCfg(), newMemStorage())
	now := time.Now()
	if _, err := pool.AddFrom(img(1), "image/jpeg", "群 A", "GROUP-A", now); err != nil {
		t.Fatal(err)
	}
	c := NewCurator(pool, nil, fakeSrc{})
	c.SetPausedGroup(func(g string) bool { return g == "GROUP-A" })
	c.runOnce(context.Background())
}
