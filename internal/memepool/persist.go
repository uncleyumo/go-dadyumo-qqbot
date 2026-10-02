package memepool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// snapshot 是池子的落盘格式。
//
// 为什么要落盘：表情包是长期资产（图片在 MinIO 里），但评分、使用次数、
// 到期时间这些"运营状态"丢了就得重新攒——机器人会退回"只认新图"的状态，
// 而新入库的图评分是中性 0.5，等于整个池子被降级。
type snapshot struct {
	Version int             `json:"version"`
	Memes   []snapshotEntry `json:"memes"`
}

type snapshotEntry struct {
	ID        int64   `json:"id"`
	ObjectKey string  `json:"object_key"`
	Descr     string  `json:"descr"`
	AddedAt   int64   `json:"added_at"` // unix 秒
	Uses      int     `json:"uses"`
	LastUsed  int64   `json:"last_used"`
	Quality   float64 `json:"quality"`
	ExpiresAt int64   `json:"expires_at"`
}

const snapshotVersion = 1

// fileMu 串行化所有落盘。多个 goroutine 同时改池子时，
// 不加锁会互相覆盖对方的写入。
var fileMu sync.Mutex

// Load 从 JSON 恢复池子。
//
// 图片本体不在这儿（MinIO 管），这里只有元数据。恢复时对象在 MinIO 上
// 不存在的条目直接跳过——那种是"元数据还在、文件没了"的半截状态。
func (p *Pool) Load(path string) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次运行没有文件，正常
		}
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		// 解析失败就当没有，绝不让一份坏文件把整个池子卡死
		return &ParseError{Path: path, Err: err}
	}
	if snap.Version != snapshotVersion {
		return &VersionError{Got: snap.Version, Want: snapshotVersion}
	}

	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range snap.Memes {
		if e.ID == 0 || e.Descr == "" {
			continue
		}
		// 到期的直接不恢复：省得刚启动就先跑一轮淘汰
		if e.ExpiresAt > 0 && now.Unix() >= e.ExpiresAt {
			continue
		}
		p.memes[e.ID] = &Meme{
			ID:        e.ID,
			ObjectKey: e.ObjectKey,
			Descr:     e.Descr,
			AddedAt:   time.Unix(e.AddedAt, 0),
			Uses:      e.Uses,
			LastUsed:  time.Unix(e.LastUsed, 0),
			Quality:   e.Quality,
			ExpiresAt: time.Unix(e.ExpiresAt, 0),
		}
		p.byKey[e.ObjectKey] = e.ID
		if e.ID >= p.nextID {
			p.nextID = e.ID + 1
		}
	}
	p.evictLocked(now)
	return nil
}

// Save 把池子写到 JSON。先写临时文件再 rename——
// 写到一半掉电会留下半截文件，下次启动就读不出来了。
func (p *Pool) Save(path string) error {
	p.mu.Lock()
	out := make([]snapshotEntry, 0, len(p.memes))
	for _, m := range p.memes {
		out = append(out, snapshotEntry{
			ID:        m.ID,
			ObjectKey: m.ObjectKey,
			Descr:     m.Descr,
			AddedAt:   m.AddedAt.Unix(),
			Uses:      m.Uses,
			LastUsed:  m.LastUsed.Unix(),
			Quality:   m.Quality,
			ExpiresAt: m.ExpiresAt.Unix(),
		})
	}
	p.mu.Unlock()

	sortEntries(out)

	fileMu.Lock()
	defer fileMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(snapshot{Version: snapshotVersion, Memes: out}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// 必须 fsync：rename 只保证目录项落盘，不保证文件内容落盘。
	// 不加的话掉电后会得到一个长度正确但内容是空的文件。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// sortEntries 按 ID 排序，保证落盘文件稳定——
// 不稳定的话每次保存 diff 都很大，出问题时不好比对。
func sortEntries(s []snapshotEntry) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].ID < s[j-1].ID; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ApplyQuality 写回优选任务的评分。
func (p *Pool) ApplyQuality(scores map[int64]float64) {
	if len(scores) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, q := range scores {
		if m, ok := p.memes[id]; ok {
			m.Quality = q
		}
	}
}

// Floor 低于这个评分的条目该被淘汰
func (p *Pool) Floor() float64 { return 0.15 }

// PruneLowQuality 踢掉评分过低的条目，返回被踢的 ID。
//
// 与 evictLocked 的区别：那个按「容量」淘汰，这个按「质量」淘汰。
// 两者都要有——池子满了要腾地方，但腾出来的位置应该是垃圾而不是好图。
func (p *Pool) PruneLowQuality() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var dropped []int64
	for id, m := range p.memes {
		// 用量能救评分：一张图很烂但发过好几次，说明在这个群里它有效果，
		// 不该因为一次低分就被踢掉。
		if m.Quality < p.Floor() && m.Uses == 0 {
			dropped = append(dropped, id)
			p.removeLocked(id)
		}
	}
	return dropped
}