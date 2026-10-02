// Package memepool 管理表情包池：存哪些图、什么时候淘汰、此刻先给模型看哪些。
//
// 设计要点（都是踩过的坑）：
//
//   - **池子小**。每次浏览会把整个池子返回给模型，直接烧 token。20 条是
//     「模型一次能扫完并选准」的上限，不是「能存多少」。
//   - **心情不进上下文**。权重、评分、时间波动全在程序侧算完，只把**排好的
//     列表**给模型，靠一句「排越前的越愿意用」表达偏好。把这些数字塞进提示词
//     只会增加模型的认知负担，还挤占它该花在聊天记录上的注意力。
//   - **没有永不过期的图**。每张图都有 expires_at，到点无条件踢。发过一百次
//     的图也得走，否则池子会凝固成一堆老面孔，新图永远挤不进来。
package memepool

import (
	"crypto/sha1"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"dadyumo/internal/logx"
)

// Meme 池中的一条。
type Meme struct {
	ID        int64
	ObjectKey string // MinIO 对象名
	Descr     string // 模型给的描述，模型选图全靠它
	AddedAt   time.Time
	Uses      int
	LastUsed  time.Time
	Quality   float64 // 优选任务给的评分 0~1
	ExpiresAt time.Time
}

// Config 池子的行为参数。
type Config struct {
	MaxPool      int           // 池子上限，超了按好感度踢最低的
	MaxResidency time.Duration // 单图最长驻留，到期无条件淘汰
	// MoodAmp 心情波动幅度（0~1）。0 = 恒定偏好，1 = 波动 ±100%。
	// 真人本来就是阴晴不定的，恒定的偏好列表会显得像算法。
	MoodAmp float64
	// MoodPeriod 每张图心情起伏的周期基准。实际周期按 id 错开，
	// 免得所有图同步起伏——那等于没有起伏。
	MoodPeriod time.Duration
}

// DefaultConfig 返回一组实测合理的默认值。
func DefaultConfig() Config {
	return Config{
		MaxPool:      20,
		MaxResidency: 30 * 24 * time.Hour,
		MoodAmp:      0.6,
		MoodPeriod:   26 * time.Hour,
	}
}

// Affection 此刻对某张图的「好感度」，越大越靠前。
//
// 只在 Browse 时计算、不落库：它本来就带时间波动，落库反而会让人误以为
// 这是个稳定分数。
func Affection(m Meme, now time.Time, cfg Config) float64 {
	base := 0.4*float64(m.Uses) + 0.6*m.Quality
	if base <= 0 {
		base = 0.05 // 没用过也没评分的，给个保底，不至于永远沉底
	}
	return base * mood(m.ID, now, cfg)
}

// mood 给每张图一个随时间起伏、且各图相位不同的系数。
//
// 相位用 id 错开是重点：如果所有图同步起伏，好感度排序就整体平移了，
// 实际上没改变任何顺序——那还不如不做随机。
func mood(id int64, now time.Time, cfg Config) float64 {
	amp := cfg.MoodAmp
	if amp <= 0 {
		return 1
	}
	period := cfg.MoodPeriod
	if period <= 0 {
		period = 26 * time.Hour
	}
	phase := float64(hashInt(id)%6283) / 1000.0 // 0~6.283 弧度
	angle := float64(now.UnixNano())/float64(period)*2*math.Pi + phase
	return 1 + amp*math.Sin(angle)
}

func hashInt(id int64) int {
	sum := sha1.Sum([]byte{
		byte(id), byte(id >> 8), byte(id >> 16), byte(id >> 24),
	})
	return int(sum[0])<<8 | int(sum[1])
}

// Pool 是池子本体。所有方法并发安全。
type Pool struct {
	mu  sync.Mutex
	cfg Config

	memes map[int64]*Meme
	// byKey 对象名 → 池内 ID。内容去重要靠它：
	// ID 是自增短号，不再等于内容哈希，没这张表就没法判重。
	byKey map[string]int64
	// nextID 下一个自增 ID。淘汰后不回收，避免出现「同一张图前后两个号」。
	nextID int64

	// storage 图片存取。接口而非具体实现，是为了让 memepool 不依赖
	// MinIO SDK，测试能塞个内存实现。
	storage Storage
	// judge 优选任务用的打分器。nil 表示不做优选，只靠时间淘汰。
	judge Judge

	stop chan struct{}
	once sync.Once
}

// Storage 图片的存取后端。
type Storage interface {
	Put(key string, data []byte, contentType string) error
	Get(key string) ([]byte, string, error)
	Delete(key string) error
}

// Judge 优选任务：给一批描述打分，返回每条的值（0~1）。
// 低于 poolFloor 的会被淘汰。
type Judge interface {
	JudgeBatch(descs []string) ([]float64, error)
}

// New 创建一个池。storage 为 nil 时池子只存元数据，不能真正发送。
func New(cfg Config, storage Storage) *Pool {
	return &Pool{
		cfg:     cfg,
		memes:   map[int64]*Meme{},
		byKey:   map[string]int64{},
		nextID:  1,
		storage: storage,
		stop:    make(chan struct{}),
	}
}

// Add 收编一张图：压缩后存进 storage，元数据入池。
//
// contentType 由调用方给出（imgproc.Fetch 会返回）。返回池内短 ID，
// 模型在交付轮用它指认要发哪张。
//
// ID 是自增短号（1、2、3…），**不是**内容哈希。哈希有 19 位，
// 让模型逐位抄进 {"t":"img","id":…} 迟早抄错，前端 JSON 也过不了 2^53。
// 内容哈希只用来做对象名与判重。
func (p *Pool) Add(data []byte, contentType, descr string, now time.Time) (int64, error) {
	if p.storage == nil {
		return 0, ErrNoStorage
	}
	descr = cleanDescr(descr)
	if descr == "" {
		return 0, ErrEmptyDescr
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 内容去重：同一张图反复入库没意义，白占名额。
	key, err := objectKey(data)
	if err != nil {
		return 0, err
	}
	objName := objectName(key, contentType)
	if id, dup := p.byKey[objName]; dup {
		return id, nil // 已存在，把已有 ID 返回去，调用方不会重复建
	}
	if err := p.storage.Put(objName, data, contentType); err != nil {
		return 0, err
	}
	id := p.nextID
	p.nextID++
	m := &Meme{
		ID:        id,
		ObjectKey: objName,
		Descr:     descr,
		AddedAt:   now,
		Quality:   0.5, // 刚入库的中性分，等优选任务来打分
		ExpiresAt: now.Add(p.cfg.MaxResidency),
	}
	p.memes[id] = m
	p.byKey[objName] = id
	p.evictLocked(now)
	return id, nil
}

// Browse 返回此刻最值得给模型看的那批图，已按好感度降序。
//
// 排序就是全部的「心情表达」——模型看到的只有顺序，看不到任何分数。
func (p *Pool) Browse(now time.Time) []Meme {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictLocked(now)

	out := make([]Meme, 0, len(p.memes))
	for _, m := range p.memes {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := Affection(out[i], now, p.cfg), Affection(out[j], now, p.cfg)
		if ai == aj {
			// 分数相同按 id 定序，保证「同样的输入给出同样的输出」，
			// 否则每次刷新顺序都在抖，模型会觉得这个工具不稳定
			return out[i].ID < out[j].ID
		}
		return ai > aj
	})
	return out
}

// Get 按 ID 取一条。
func (p *Pool) Get(id int64) (Meme, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.memes[id]
	if !ok {
		return Meme{}, false
	}
	return *m, true
}

// Data 取图片字节，发送时用。
func (p *Pool) Data(id int64) ([]byte, string, error) {
	p.mu.Lock()
	m, ok := p.memes[id]
	p.mu.Unlock()
	if !ok {
		return nil, "", ErrNotFound
	}
	if p.storage == nil {
		return nil, "", ErrNoStorage
	}
	return p.storage.Get(m.ObjectKey)
}

// MarkUsed 记一次「被发出去」。次数影响后续排序。
func (p *Pool) MarkUsed(id int64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.memes[id]; ok {
		m.Uses++
		m.LastUsed = now
	}
}

// Len 当前池子大小。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.memes)
}

// Remove 手动删掉一张（管理端用）。
//
// 元数据与 MinIO 对象一起删（removeLocked 里做，不要在这里再来一次）。
// 返回 ErrNotFound 表示它已经被淘汰了——管理端点两次点击之间
// 可能正好过了淘汰窗口，那不是错误。
func (p *Pool) Remove(id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.memes[id]; !ok {
		return ErrNotFound
	}
	p.removeLocked(id)
	return nil
}

// Ranked 是给管理端看的一行：池内排序结果 + 权重明细。
//
// 「排越前的越愿意用」这个规则是给模型看的，模型只拿到顺序。
// 但人（也就是你）需要看到**为什么**是这个顺序——用了多少次、
// 优选评了多少、还剩几天到期。看不到这些就没法判断池子的健康度。
type Ranked struct {
	ID        int64     `json:"id"`
	ObjectKey string    `json:"object_key"`
	Descr     string    `json:"descr"`
	Uses      int       `json:"uses"`
	Quality   float64   `json:"quality"`
	Affection float64   `json:"affection"` // 此刻好感度（含心情波动）
	Rank      int       `json:"rank"`      // 1 是模型会看到的第一条
	AddedAt   time.Time `json:"added_at"`
	LastUsed  time.Time `json:"last_used"`
	ExpiresIn int64     `json:"expires_in"` // 距到期还有几天，负数=已过期
}

// Storage 返回存储后端，可能为 nil（只存元数据的裸池）。
func (p *Pool) Storage() Storage { return p.storage }

// MaxPool 池子上限，管理端展示用。
func (p *Pool) MaxPool() int { return p.cfg.MaxPool }

// Ranked 按当前好感度排序返回全部条目，供管理端展示。
func (p *Pool) Ranked(now time.Time) []Ranked {
	memes := p.Browse(now)
	out := make([]Ranked, 0, len(memes))
	for i, m := range memes {
		out = append(out, Ranked{
			ID:        m.ID,
			ObjectKey: m.ObjectKey,
			Descr:     m.Descr,
			Uses:      m.Uses,
			Quality:   m.Quality,
			Affection: Affection(m, now, p.cfg),
			Rank:      i + 1,
			AddedAt:   m.AddedAt,
			LastUsed:  m.LastUsed,
			ExpiresIn: int64(m.ExpiresAt.Sub(now).Hours() / 24),
		})
	}
	return out
}

// evictLocked 淘汰到上限以下。调用方必须持有 mu。
//
// 顺序：先踢「到期」的（无条件），再踢「好感度最低」的（补位）。
// 到期的必须无条件踢——这是「没有永不过期的图」这条硬要求的落点，
// 一张发过一百次的图到期也得走，否则池子会凝固。
func (p *Pool) evictLocked(now time.Time) {
	for id, m := range p.memes {
		if !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt) {
			p.removeLocked(id)
		}
	}
	if p.cfg.MaxPool <= 0 {
		return
	}
	for len(p.memes) > p.cfg.MaxPool {
		var worstID int64
		worst := math.Inf(1)
		for id, m := range p.memes {
			a := Affection(*m, now, p.cfg)
			if a < worst {
				worst, worstID = a, id
			}
		}
		p.removeLocked(worstID)
	}
}

func (p *Pool) removeLocked(id int64) {
	m, ok := p.memes[id]
	if !ok {
		return
	}
	delete(p.memes, id)
	delete(p.byKey, m.ObjectKey)
	if p.storage != nil {
		// 同步删，不用 goroutine。
		//
		// 异步的诱惑是「别让网络 IO 挡住淘汰」，但那会造成两种坏结果：
		// 进程退出时 goroutine 直接丢掉，MinIO 里留下永久孤儿对象；
		// 测试也没法断言「到底删没删」。淘汰是低频路径（最多 20 个对象），
		// 一次几十毫秒的网络 IO 换确定性，值得。
		//
		// 删失败只记不返：元数据已经没了，残留对象只是空间浪费，
		// 不该让一次淘汰失败把整轮决策带崩。
		if err := p.storage.Delete(m.ObjectKey); err != nil {
			logx.Warn("表情包对象删除失败（可能有孤儿对象）",
				"key", m.ObjectKey, "err", err.Error())
		}
	}
}

// All 导出全部条目，优选任务与测试用。
func (p *Pool) All() []Meme {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Meme, 0, len(p.memes))
	for _, m := range p.memes {
		out = append(out, *m)
	}
	return out
}

// objectKey 用内容哈希当 ID 与对象名。
//
// 同一张图无论入库多少次都得到同一个 key，这既是天然去重，
// 也让「模型记住的某个短 ID」在图片不变时一直有效。
func objectKey(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, ErrEmptyData
	}
	sum := sha1.Sum(data)
	// 取前 8 字节当 int64。碰撞概率对几十张图的池子可以忽略，
	// 真撞了也只是两张图共用一个 ID，不会损坏数据。
	var id int64
	for i := 0; i < 8; i++ {
		id = id<<8 | int64(sum[i])
	}
	if id < 0 {
		id = -id
	}
	if id == 0 {
		id = 1
	}
	return id, nil
}

// KeyFor 暴露内容哈希，供测试与外部校验用。
func KeyFor(data []byte) (int64, error) { return objectKey(data) }

// objectName 把内容哈希变成 MinIO 对象名。
//
// 后缀按**真实格式**给，不能一律 .jpg：对象名会原样进 URL，
// 而一堆 .jpg 里混着 GIF，浏览器和 QQ 都会照后缀猜类型——
// 猜错的后果是「本来会动的图发出去变成静图」。
// 没有魔数能认的格式（webp 之类标准库解不了的）退回 .bin。
func objectName(id int64, contentType string) string {
	ext := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
		"image/webp": ".webp",
	}[contentType]
	if ext == "" {
		ext = ".bin"
	}
	return fmt.Sprintf("memes/%d%s", id, ext)
}

func cleanDescr(s string) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return trimSpace(s)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
