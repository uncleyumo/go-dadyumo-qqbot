package memepool

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// memStorage 测试用的内存存储，顺便记录删除次数以便断言。
type memStorage struct {
	mu      sync.Mutex
	data    map[string][]byte
	deleted []string
}

func newMemStorage() *memStorage {
	return &memStorage{data: map[string][]byte{}}
}

func (m *memStorage) Put(k string, d []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = append([]byte(nil), d...)
	return nil
}

func (m *memStorage) Get(k string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[k]
	if !ok {
		return nil, "", ErrNotFound
	}
	return d, "image/jpeg", nil
}

func (m *memStorage) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, k)
	delete(m.data, k)
	return nil
}

func (m *memStorage) delCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deleted)
}

func img(seed int) []byte { return []byte(fmt.Sprintf("fake-jpeg-%d", seed)) }

func testCfg() Config {
	c := DefaultConfig()
	c.MoodAmp = 0 // 测试默认关掉波动，需要时单独开
	return c
}

func TestAddAndBrowse(t *testing.T) {
	st := newMemStorage()
	p := New(testCfg(), st)
	now := time.Now()

	id, err := p.Add(img(1), "image/jpeg", "无语的猫", now)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("应返回非零 ID")
	}
	got := p.Browse(now)
	if len(got) != 1 {
		t.Fatalf("应有 1 张，got %d", len(got))
	}
	if got[0].Descr != "无语的猫" {
		t.Errorf("描述不对: %q", got[0].Descr)
	}
	// 取回字节
	if raw, _, err := p.Data(id); err != nil || len(raw) == 0 {
		t.Errorf("应能取回图片: %v", err)
	}
}

// 同一张图重复入库不该占两个名额
func TestAddDeduplicatesByContent(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	now := time.Now()
	id1, _ := p.Add(img(7), "image/jpeg", "第一次", now)
	id2, _ := p.Add(img(7), "image/jpeg", "第二次", now)
	if id1 != id2 {
		t.Errorf("同样内容应得同一个 ID: %d vs %d", id1, id2)
	}
	if p.Len() != 1 {
		t.Errorf("重复入库不应增加名额，got %d", p.Len())
	}
}

// 硬要求：池子超过上限要自动淘汰
func TestPoolEnforcesMax(t *testing.T) {
	cfg := testCfg()
	cfg.MaxPool = 5
	p := New(cfg, newMemStorage())
	now := time.Now()

	for i := 0; i < 12; i++ {
		if _, err := p.Add(img(i), "image/jpeg", fmt.Sprintf("图%d", i), now); err != nil {
			t.Fatal(err)
		}
		// 每次推进 1 分钟，让心情波动有区分度
		now = now.Add(time.Minute)
	}
	if p.Len() > 5 {
		t.Errorf("池子应不超过 5，实际 %d", p.Len())
	}
	// 存储里也不该残留
	if st := p.storage.(*memStorage); st.delCount() == 0 {
		t.Error("淘汰时应删除存储对象，否则 MinIO 里会越堆越多")
	}
}

// 硬要求：没有任何图是永不过期的。
// 这条是用户明确提的——「永远允许有新图进来，不存在永远不被淘汰的旧图」。
func TestNoImmortalMeme(t *testing.T) {
	cfg := testCfg()
	cfg.MaxPool = 20
	cfg.MaxResidency = 30 * 24 * time.Hour
	p := New(cfg, newMemStorage())

	start := time.Now()
	id, _ := p.Add(img(1), "image/jpeg", "很老的图", start)

	// 让它被用 50 次、用得很勤——分数应该是全池最高的
	for i := 0; i < 50; i++ {
		p.MarkUsed(id, start.Add(time.Duration(i)*time.Hour))
	}

	// 走过 30 天
	after := start.Add(31 * 24 * time.Hour)
	p.Browse(after)

	if _, ok := p.Get(id); ok {
		t.Error("超过最长驻留期的图必须淘汰，哪怕它被用过很多次")
	}
	if p.Len() != 0 {
		t.Errorf("池子应被清空，实际还剩 %d", p.Len())
	}
}

// 到期时间应该随入库时间推后，不是固定值
func TestExpiryIsPerMeme(t *testing.T) {
	cfg := testCfg()
	p := New(cfg, newMemStorage())
	t0 := time.Now()
	idA, _ := p.Add(img(1), "image/jpeg", "A", t0)
	idB, _ := p.Add(img(2), "image/jpeg", "B", t0.Add(10*24*time.Hour))

	mA, _ := p.Get(idA)
	mB, _ := p.Get(idB)
	if !mB.ExpiresAt.After(mA.ExpiresAt) {
		t.Errorf("后入库的图到期应更晚: A=%v B=%v", mA.ExpiresAt, mB.ExpiresAt)
	}
}

// 心情：不同图要有不同相位，否则排序永远不变，随机等于没做
func TestMoodPhaseDiffersPerMeme(t *testing.T) {
	cfg := testCfg()
	cfg.MoodAmp = 0.9
	now := time.Now()

	// 一段时间内，不同 id 的心情系数应拉开差距
	same := 0
	total := 50
	var first float64
	for i := 1; i <= total; i++ {
		v := mood(int64(i), now, cfg)
		if i == 1 {
			first = v
			continue
		}
		if v == first {
			same++
		}
	}
	if same > total/10 {
		t.Errorf("不同 id 的心情应各不相同，实测 %d/%d 相同", same, total)
	}
}

// 心情应该真的会随时间变——真人阴晴不定，恒定偏好会显得像算法
func TestMoodChangesOverTime(t *testing.T) {
	cfg := testCfg()
	cfg.MoodAmp = 0.9
	base := time.Now()
	v0 := mood(42, base, cfg)
	changed := false
	for h := 1; h <= 72; h++ {
		if mood(42, base.Add(time.Duration(h)*time.Hour), cfg) != v0 {
			changed = true
			break
		}
	}
	if !changed {
		t.Error("心情系数应随时间变化")
	}
}

// 排序稳定：同样的输入必须给出同样的顺序，否则模型会觉得工具不可靠
func TestBrowseOrderIsDeterministic(t *testing.T) {
	cfg := testCfg()
	p := New(cfg, newMemStorage())
	now := time.Now()
	for i := 0; i < 8; i++ {
		p.Add(img(i), "image/jpeg", fmt.Sprintf("图%d", i), now)
	}
	first := p.Browse(now)
	for i := 0; i < 5; i++ {
		again := p.Browse(now)
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("第 %d 次调用顺序在第 %d 位变了: %d != %d", i, j, first[j].ID, again[j].ID)
			}
		}
	}
}

// 用得多的排前面（波动关掉时）
func TestBrowseRanksByUsageWhenMoodOff(t *testing.T) {
	cfg := testCfg()
	p := New(cfg, newMemStorage())
	now := time.Now()
	idCold, _ := p.Add(img(1), "image/jpeg", "没人用", now)
	idHot, _ := p.Add(img(2), "image/jpeg", "常用", now)
	for i := 0; i < 10; i++ {
		p.MarkUsed(idHot, now)
	}
	got := p.Browse(now)
	if len(got) != 2 {
		t.Fatalf("应有 2 张，got %d", len(got))
	}
	if got[0].ID != idHot {
		t.Errorf("用得多的应排前面，got %d 期望 %d", got[0].ID, idHot)
	}
	_ = idCold
}

func TestEmptyDescrRejected(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	if _, err := p.Add(img(1), "image/jpeg", "   ", time.Now()); err != ErrEmptyDescr {
		t.Errorf("空描述应被拒绝，got %v", err)
	}
}

func TestNoStorageError(t *testing.T) {
	p := New(testCfg(), nil)
	if _, err := p.Add(img(1), "image/jpeg", "x", time.Now()); err != ErrNoStorage {
		t.Errorf("无存储应报错，got %v", err)
	}
}

func TestGetUnknownID(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	if _, ok := p.Get(999); ok {
		t.Error("不存在的 ID 不该查到")
	}
	if _, _, err := p.Data(999); err != ErrNotFound {
		t.Errorf("取不存在的数据应报 ErrNotFound，got %v", err)
	}
}

func TestLongDescrClipped(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	long := ""
	for i := 0; i < 200; i++ {
		long += "很长的描述"
	}
	id, err := p.Add(img(1), "image/jpeg", long, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := p.Get(id)
	if len([]rune(m.Descr)) > 80 {
		t.Errorf("超长描述应截断到 80 字，实际 %d", len([]rune(m.Descr)))
	}
}

func TestZeroKeyAvoided(t *testing.T) {
	// 极端情况：哈希前 8 字节全 0 时也不能用 0 当 ID（0 被当成"没有"）
	for i := 0; i < 200; i++ {
		id, err := KeyFor(img(i))
		if err != nil {
			t.Fatal(err)
		}
		if id <= 0 {
			t.Fatalf("ID 必须为正，got %d", id)
		}
	}
}