package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
	"dadyumo/internal/statsdb"
)

// Target 一个可调用目标 = 接入点 × 模型
//
// EndpointID 与 Model 在 Target 创建后不再被改写（Reload 只更新其余字段），
// 所以 Key() 可以直接裸读；其余字段都必须走 snap()。
type Target struct {
	EndpointID   string
	EndpointName string
	BaseURL      string
	APIKey       string
	APIType      string // chat_completions / responses，决定 URL 路径与报文格式
	Model        string
	Label        string
	Priority     int // 越大越优先，见 config.Model.Priority
	MaxCtx       int
	MaxOut       int
	Stream       bool
	Vision       bool // 该模型是否吃图片（多模态）。带图请求只会派发给这类目标
	Timeout      time.Duration
	Enabled      bool // 配置层开关

	mu sync.Mutex
	h  *Health
}

// TargetView 是 Target 配置字段的只读快照。
//
// 之前 Target 有 mu，但 Reload 持锁写可变字段，而 Call/buildPayload/ordered/
// pick/Stats/Chat 的日志全部裸读，锁形同虚设：string 字段可能读到撕裂的
// (ptr,len)（Authorization 乱码或 slice 越界），int 字段会在调度中途跳变，
// -race 稳定报警。统一在锁内取一份快照再往下传，是成本最低也最难写错的做法。
type TargetView struct {
	EndpointID   string
	EndpointName string
	BaseURL      string
	APIKey       string
	APIType      string
	Model        string
	Label        string
	Priority     int
	MaxCtx       int
	MaxOut       int
	Stream       bool
	Vision       bool
	Enabled      bool
	Timeout      time.Duration
}

// snap 在锁内取配置快照
func (t *Target) snap() TargetView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapLocked()
}

// snapLocked 取配置快照，调用方必须已持有 t.mu（同时要读健康度时用这个，避免二次加锁）
func (t *Target) snapLocked() TargetView {
	return TargetView{
		EndpointID:   t.EndpointID,
		EndpointName: t.EndpointName,
		BaseURL:      t.BaseURL,
		APIKey:       t.APIKey,
		APIType:      t.APIType,
		Model:        t.Model,
		Label:        t.Label,
		Priority:     t.Priority,
		MaxCtx:       t.MaxCtx,
		MaxOut:       t.MaxOut,
		Stream:       t.Stream,
		Vision:       t.Vision,
		Enabled:      t.Enabled,
		Timeout:      t.Timeout,
	}
}

// MaxOutTokens 该目标在调用方没给 max_tokens 时会用的输出上限。
func (t *Target) MaxOutTokens() int {
	if v := t.snap().MaxOut; v > 0 {
		return v
	}
	return defaultMaxOut
}

// Key 唯一标识
func (t *Target) Key() string { return t.EndpointID + "|" + t.Model }

// Health 返回健康度快照
func (t *Target) Health() Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *t.h
}

// snapshot 返回用于排序的快照（含 enabled 状态与冷却信息）
type snapshot struct {
	t       *Target
	view    TargetView // 配置快照，排序与展示一律读它，不裸读 Target
	key     string
	score   float64
	cooling bool
	remain  time.Duration
	dead    bool
	enabled bool
	health  Health
}

// exploreEpsilon 探路流量占比。设为变量是为了让测试能关掉它做确定性断言。
var exploreEpsilon = 0.08

// Router 中央调用器
type Router struct {
	store *config.Store

	mu       sync.RWMutex
	targets  map[string]*Target
	weights  Weight
	maxTry   int
	seq      uint64
	lastErr  string
	lastOK   time.Time
	totalReq int64
	failReq  int64

	// stats 统计持久化（可为 nil，nil 时全部跳过，便于测试与降级）。
	// 用独立原子指针而不是放在 mu 保护下：SetStats 只在启动时调一次，
	// 而 Chat 热路径每次都要读，不能再蹭路由锁。
	stats atomic.Pointer[statsdb.DB]
}

// NewRouter 创建中央调用器并载入配置
func NewRouter(store *config.Store) *Router {
	r := &Router{store: store, targets: map[string]*Target{}}
	r.Reload()
	return r
}

// SetStats 挂接统计库。启动时调用一次即可；传 nil 表示统计降级为纯内存。
func (r *Router) SetStats(d *statsdb.DB) {
	r.stats.Store(d)
	if d == nil {
		return
	}
	// 挂载后立刻清一遍孤立记录（比如上次运行期间改了 endpoint/model id）
	r.mu.RLock()
	keys := make([]string, 0, len(r.targets))
	for k := range r.targets {
		keys = append(keys, k)
	}
	r.mu.RUnlock()
	d.DeleteTargetsNotIn(keys)
}

// RestoreTargets 从统计库恢复目标健康度。
// 只回填当前配置中存在的 key；Cooldown/Dead 不恢复——重启即给目标一次重新证明自己的机会。
func (r *Router) RestoreTargets(m map[string]statsdb.TargetState) {
	if len(m) == 0 {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	restored := 0
	for key, s := range m {
		t, ok := r.targets[key]
		if !ok {
			continue
		}
		t.mu.Lock()
		t.h.Total = s.Total
		t.h.Fails = s.Fails
		t.h.ConsecFail = s.ConsecFail
		t.h.TTFTMS = s.TTFTMS
		t.h.LatencyMS = s.LatencyMS
		t.h.LastSuccess = s.LastSuccess
		t.h.LastUsed = s.LastUsed
		t.h.LastError = s.LastError
		// everSucceeded 不落盘（statsdb 没有这一列），用「有过成功」反推：
		// 没有成功时间戳、且一次都没成功过（Total 全部计入 Fails），都算没成功过。
		t.h.EverSucceeded = !s.LastSuccess.IsZero() || (s.Total > 0 && s.Fails < s.Total)
		if s.SuccessEWA > 0 {
			t.h.SuccessEWA = s.SuccessEWA
		}
		t.mu.Unlock()
		restored++
	}
	if restored > 0 {
		logx.Info("目标健康度已从统计库恢复", "数量", restored)
	}
}

// CheapestModel 返回当前可用的「最便宜」目标：接入点 id 与模型名。
//
// 配置里没有价格字段，所以这里的「便宜」是按本项目的档位约定推的：
// 主力模型 priority=1、免费兜底 priority=0，取 priority 最低的那档。
// 供优选任务这种粗活使用——它每 6 小时才跑一次，纯文本小模型足够。
//
// 打分只吃描述文本，**不需要**视觉能力——但也不能因此把 vision 模型全排掉。
//
// 原来这里写死 `if v.Vision { continue }`，而生产配置里 5 个启用模型
// 全是 vision=true，于是永远返回空串，优选任务一次都没跑过。
// 「挑不出纯文本模型就什么都不干」是最坏的降级：池子照样在收图，
// 只是再也不会清理，坏图一直占着名额。宁可退而用视觉模型
// （它吃的确实是纯文本，多花点钱而已），也不能让整条优选链路消失。
// 返回空串只保留给「一个可用目标都没有」的情况，调用方应跳过而不是硬调。
func (r *Router) CheapestModel() (endpointID, model string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// 两轮：先在纯文本档里选，没有再退回任意启用档。
	// 纯文本档不存在是常态（生产配置如此），所以退回那轮才是主力路径。
	for _, wantVision := range []bool{false, true} {
		best := 1 << 30
		var beID, beModel string
		for _, t := range r.targets {
			v := t.snap()
			if !v.Enabled || v.Model == "" || v.Vision != wantVision {
				continue
			}
			// 档位相同时按 id 字典序定序，保证同样的配置每次都选同一个
			if v.Priority < best || (v.Priority == best && v.EndpointID+v.Model < beID+beModel) {
				best, beID, beModel = v.Priority, v.EndpointID, v.Model
			}
		}
		if beModel != "" {
			return beID, beModel
		}
	}
	return "", ""
}

// FlushTargets 把当前全部目标的健康度快照写入统计库（周期 + 退出前调用）。
// 写入是异步入队的，本函数本身不阻塞。
func (r *Router) FlushTargets() {
	d := r.stats.Load()
	if d == nil {
		return
	}
	r.mu.RLock()
	targets := make([]*Target, 0, len(r.targets))
	for _, t := range r.targets {
		targets = append(targets, t)
	}
	r.mu.RUnlock()
	for _, t := range targets {
		h := t.Health()
		d.RecordTarget(statsdb.TargetState{
			Key:         t.Key(),
			Total:       h.Total,
			Fails:       h.Fails,
			ConsecFail:  h.ConsecFail,
			SuccessEWA:  h.SuccessEWA,
			TTFTMS:      h.TTFTMS,
			LatencyMS:   h.LatencyMS,
			LastSuccess: h.LastSuccess,
			LastUsed:    h.LastUsed,
			LastError:   h.LastError,
		})
	}
}

// Reload 依据最新配置重建目标池；已存在目标的健康指标会被保留
func (r *Router) Reload() {
	cfg := r.store.Get()

	// 统计库清理要用的 key 快照，以及要打的日志，都留到锁外去做。
	// statsdb 的连接池是 SetMaxOpenConns(1)，DeleteTargetsNotIn 又是同步写：
	// 写协程正独占那条连接跑事务时，这次调用会阻塞到 busy_timeout（3s），
	// 期间所有在途调用都卡在 r.mu.RLock() 上，配置热加载能把整个机器人卡死。
	// 本窗口内没观测到实际卡顿（target_state 只有 6 行），这是并发隐患而非已发生的故障。
	var keys []string
	var nNext, nAlive, nRemoved int
	var weights Weight
	var maxTry int

	func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		weights = Weight{
			TTFT:     cfg.LLM.Weights.TTFT,
			Failure:  cfg.LLM.Weights.Failure,
			Latency:  cfg.LLM.Weights.Latency,
			Cooldown: cfg.LLM.Weights.Cooldown,
		}
		// 权重全为 0 时给一组合理默认值
		if weights.TTFT == 0 && weights.Failure == 0 && weights.Latency == 0 && weights.Cooldown == 0 {
			weights = Weight{TTFT: 0.45, Failure: 0.30, Latency: 0.15, Cooldown: 0.10}
		}
		maxTry = cfg.LLM.MaxAttempts
		if maxTry <= 0 {
			maxTry = 4
		}

		anyStream := false
		next := map[string]*Target{}
		for _, ep := range cfg.LLM.Endpoints {
			for _, m := range ep.Models {
				t := &Target{
					EndpointID:   ep.ID,
					EndpointName: ep.Name,
					BaseURL:      ep.BaseURL,
					APIKey:       ep.APIKey,
					APIType:      ep.APIType,
					Model:        m.ID,
					Label:        m.Label,
					Priority:     m.Priority,
					MaxCtx:       m.MaxCtx,
					MaxOut:       m.MaxOut,
					Stream:       m.Stream,
					Vision:       m.Vision,
					Timeout:      time.Duration(ep.TimeoutMS) * time.Millisecond,
					Enabled:      ep.Enabled && m.Enabled,
				}
				if m.Stream {
					anyStream = true
				}
				key := t.Key()
				if old, ok := r.targets[key]; ok {
					old.mu.Lock()
					// 继承历史指标，只更新可变字段
					old.BaseURL = t.BaseURL
					old.APIKey = t.APIKey
					old.APIType = t.APIType
					old.Label = t.Label
					old.Priority = t.Priority
					old.MaxCtx = t.MaxCtx
					old.MaxOut = t.MaxOut
					old.Stream = t.Stream
					old.Vision = t.Vision
					old.Timeout = t.Timeout
					old.Enabled = t.Enabled
					old.EndpointName = t.EndpointName
					old.mu.Unlock()
					next[key] = old
					continue
				}
				t.h = newHealth()
				next[key] = t
			}
		}
		// 一个流式目标都没有时，TTFT 拿不到实测值：result() 只能用总耗时顶替，
		// 于是 ttft≡latency，0.45 与 0.15 压在同一个变量上重复计分，
		// 路由器也分不清「首字快但生成慢」和「首字慢但生成快」。
		// 与其指望运维去改配置，不如在这里把这部分权重并进 Latency：
		// 排序结果完全等价，面板上的分数也不再骗人。等哪天开了流式，配置权重自然恢复。
		if !anyStream && weights.TTFT > 0 {
			weights.Latency += weights.TTFT
			weights.TTFT = 0
		}
		r.weights = weights
		r.maxTry = maxTry

		for k := range r.targets {
			if _, ok := next[k]; !ok {
				nRemoved++
			}
		}
		r.targets = next
		nNext = len(next)
		keys = make([]string, 0, len(next))
		for k := range next {
			if next[k].snap().Enabled {
				nAlive++
			}
			keys = append(keys, k)
		}
	}()

	// 配置变更后清理统计库里的孤立目标记录（id 被改名/删除留下的旧 key）
	if d := r.stats.Load(); d != nil {
		d.DeleteTargetsNotIn(keys)
	}
	logx.Info("中央调用器已载入目标", "总数", nNext, "启用", nAlive, "移除", nRemoved, "单次最大尝试", maxTry,
		"权重TTFT", weights.TTFT, "权重Latency", weights.Latency)
}

// Chat 发起一次对话：按健康度排序依次尝试，直到成功或所有目标耗尽。
// 只要池中还有一个活着的（接入点, 模型）组合，请求就一定会被消费。
func (r *Router) Chat(ctx context.Context, req Request) (*Result, error) {
	r.mu.RLock()
	maxTry := r.maxTry
	r.mu.RUnlock()

	tried := map[string]bool{}
	var lastErr error
	var attempts int
	refused := 0 // 被内容审核拒绝的目标数，用来判定「是不是内容的问题」

	for i := 0; i < maxTry; i++ {
		// 剩余预算不够再跑一次有意义的尝试时就停手。外层 ctx 是整条链的总预算，
		// 内层超时只是每次尝试的上限：两者各拿满配置值（60s vs 60s）时，
		// 第一个目标一慢，第 2 个目标就永远等不到——发起注定超时的请求
		// 只会把预算彻底耗光，让后面连出场机会都没有。
		if ctx.Err() != nil {
			r.noteFail("上游上下文已取消")
			return nil, fmt.Errorf("请求被取消: %w", ctx.Err())
		}
		if !enoughBudget(ctx, minAttemptBudget) {
			r.noteFail("预算耗尽")
			return nil, fmt.Errorf("预算耗尽，无法再尝试下一个目标（已尝试 %d 个）: %w", attempts, lastErr)
		}

		t := r.pick(tried, req.NeedsVision)
		if t == nil {
			break
		}
		view := t.snap()
		key := t.Key()
		tried[key] = true
		attempts++

		start := time.Now()
		res, cerr := Call(ctx, t, req)
		if cerr == nil {
			t.recordSuccess(res)
			r.mu.Lock()
			r.totalReq++
			r.lastOK = time.Now()
			r.mu.Unlock()
			r.recordCall(t, true, res.TTFTMS, res.LatencyMS, res.PromptTokens, res.OutputTokens, "")
			if i > 0 {
				logx.Warn("调用经重试后成功", "endpoint", view.EndpointName, "model", view.Model,
					"第几次", i+1, "耗时ms", time.Since(start).Milliseconds())
			}
			return res, nil
		}
		elapsed := msSince(start)
		// 失败明细也必须带真实 token 数：上游在 HTTP 200 之后才被判定失败
		// （空内容/被截断）时同样计费，硬写 0 会让成本统计少记近三成。
		r.recordCall(t, false, 0, elapsed, cerr.PromptTokens, cerr.OutputTokens, cerr.Message)

		// 调用方自己的超时/取消不算目标的账。这里必须排在 recordFailure 之前：
		// 顺序反了，摘录调用（45s 预算）撞上 60s 超时的目标时，
		// 父 ctx 先到期却被写成「超过 1m0s 未响应」，健康目标被推进指数退避。
		if cerr.External || ctx.Err() != nil {
			r.noteFail("上游上下文已结束: " + cerr.Message)
			logx.Warn("调用因上游上下文结束而中止，不计入目标健康度",
				"endpoint", view.EndpointName, "model", view.Model, "err", cerr.Error())
			if ctx.Err() != nil {
				return nil, fmt.Errorf("请求被取消: %w", ctx.Err())
			}
			// 预算见底但父 ctx 还没到期（例如只剩 1s）：如实说预算不够，别谎称被取消
			return nil, fmt.Errorf("预算耗尽: %w", cerr)
		}
		// 请求本身不合法（上下文超长、字段非法）：换目标也是同样的坏请求。
		// 不记失败、不冷却，直接把错误交回调用方，别把健康目标一锅端。
		if cerr.Kind == ErrKindBadRequest {
			r.noteFail("请求不合法: " + cerr.Message)
			logx.Warn("请求被上游拒绝，不重试也不冷却",
				"endpoint", view.EndpointName, "model", view.Model, "err", cerr.Error())
			return nil, fmt.Errorf("请求被上游拒绝: %w", cerr)
		}

		// 内容被上游审核拦下：要换目标（各家口径不同，换一家可能就过了），
		// 但**绝不能冷却或降权**——目标本身好得很，是这次的内容踩了它的线。
		// 走 recordFailure 会把主力推进指数退避，只因为有人在群里说了句敏感词，
		// 代价与故障完全不成比例（2026-10-04 生产实测形态，见 refusal.go）。
		if cerr.Kind == ErrKindRefused {
			lastErr = cerr
			refused++
			logx.Warn("内容被上游拒绝，换下一个目标（不冷却本目标）",
				"endpoint", view.EndpointName, "model", view.Model, "err", cerr.Error())
			continue
		}

		backoff := t.recordFailure(cerr)
		lastErr = cerr
		logx.Warn("调用失败，切换到下一个目标", "endpoint", view.EndpointName, "model", view.Model,
			"err", cerr.Error(), "冷却", backoff.String())

		// 鉴权失败通常是整个接入点的问题，让同接入点的其他模型一起进冷却，避免无谓重试
		if cerr.Kind == ErrKindAuth {
			r.cooldownEndpoint(view.EndpointID, 5*time.Minute, cerr.Message)
		}
		// 模型不存在（免费模型经常下线）直接标记 dead。
		// 但带图请求被拒不是「模型下线」，只是这个模型实际不吃图——
		// markDead 只在全部目标都判死时才由 reviveAll 解开，否则本次进程内永远出局。
		if cerr.Kind == ErrKindNotFound && !visionRejected(req, cerr) {
			t.markDead(cerr.Message)
		}
	}

	if attempts == 0 {
		r.noteFail("没有可用目标")
		return nil, fmt.Errorf("没有可用的接入点与模型")
	}
	r.noteFail(fmt.Sprintf("已尝试 %d 个目标仍失败", attempts))
	// 每个目标都试过了、每一个都是被内容审核拦下——这是「内容问题」，不是「服务挂了」。
	// 上层要靠这个区分来决定是发一句嘴臭的兜底还是安静闭嘴，两者对用户的观感完全不同。
	if refused == attempts {
		return nil, fmt.Errorf("%w（已尝试 %d 个目标，全部被内容审核拒绝）", ErrAllRefused, attempts)
	}
	return nil, fmt.Errorf("所有接入点的所有模型均不可用（已尝试 %d 个）: %w", attempts, lastErr)
}

// ErrAllRefused 所有目标都以「内容被审核拒绝」告终。
//
// 单独一个哨兵错误，因为上层对它的处置和对「服务不可用」完全不同：
// 前者说明模型都活着、只是这轮内容过不去，发一句嘴臭的兜底比装死自然；
// 后者是真故障，闭嘴才是对的。用 errors.Is 判定，不要匹配错误文本。
var ErrAllRefused = errors.New("所有目标均拒绝该内容")

// noteFail 记一次整体失败。取消/预算耗尽/请求不合法都走这里，
// 保证 total_req 与 fail_req 的口径一致（早先取消分支只加 fail 不加 total，面板会少算）。
func (r *Router) noteFail(reason string) {
	r.mu.Lock()
	r.totalReq++
	r.failReq++
	r.lastErr = reason
	r.mu.Unlock()
}

// visionRejected 带图请求被拒时，「模型不存在」多半不是模型下线，
// 而是这个模型在配置里勾了 vision、实际却不吃图。永久下线太重，只让它正常冷却。
func visionRejected(req Request, cerr *CallError) bool {
	if !req.NeedsVision {
		return false
	}
	msg := strings.ToLower(cerr.Message)
	return strings.Contains(msg, "image") || strings.Contains(msg, "vision") || strings.Contains(msg, "multimodal")
}

// ordered 计算候选的调度顺序，是「实际挑谁」的唯一事实来源。
//
// pick（真正发请求）与 PreviewChain（管理端预览）都调它，
// 这样「管理端看到的顺序」必然等于「下一次调用真实尝试的顺序」。
// 早期这两处各写一套排序，天生会漂移，管理端就会骗人。
//
// exclude 里的 key 会被跳过（Chat 的重试循环用它排除已试过的目标）。
// needVision=true 时把没声明多模态的目标整个剔出候选：带图请求发给纯文本模型，
// 上游只会回 400/404（"No endpoints found that support image input"），
// 白赔一次调用，还可能把一个健康模型误判成已下线。
// jitter=true 时给分数叠一点随机扰动，避免并发请求全砸向同一个目标；
// 预览用 jitter=false，给出确定顺序。
//
// 本函数只读，不改任何状态（allDead/reviveAll 这类会改状态的动作留在 pick 里）。
// 返回：非冷却候选（按调度顺序）、冷却候选（按最快恢复排序）。
func (r *Router) ordered(exclude map[string]bool, jitter, needVision bool) (snaps, cooling []snapshot) {
	r.mu.RLock()
	cands := make([]*Target, 0, len(r.targets))
	for _, t := range r.targets {
		if exclude[t.Key()] {
			continue
		}
		cands = append(cands, t)
	}
	weights := r.weights
	r.mu.RUnlock()

	// map 遍历顺序是随机的：先按 key 排序，让「并列」也有确定结果，
	// 否则预览每次刷新顺序都可能跳来跳去，读者会以为配置在变。
	sort.Slice(cands, func(i, j int) bool { return cands[i].Key() < cands[j].Key() })

	now := time.Now()
	type entry struct {
		t    *Target
		view TargetView
		h    Health
	}
	entries := make([]entry, 0, len(cands))
	for _, t := range cands {
		t.mu.Lock()
		view := t.snapLocked()
		h := *t.h
		t.mu.Unlock()
		if !view.Enabled || h.Dead {
			continue
		}
		if needVision && !view.Vision {
			continue
		}
		entries = append(entries, entry{t: t, view: view, h: h})
	}
	if len(entries) == 0 {
		return nil, nil
	}

	hs := make([]Health, 0, len(entries))
	for _, e := range entries {
		hs = append(hs, e.h)
	}
	scale := computeScale(hs)

	for _, e := range entries {
		s := snapshot{
			t:       e.t,
			view:    e.view,
			key:     e.t.Key(),
			score:   e.h.Score(weights, now, e.h.Total > 0, scale),
			cooling: e.h.Cooling(now),
			remain:  e.h.CooldownRemaining(now),
			health:  e.h,
			enabled: true,
		}
		// 轻微随机抖动，避免多个请求同时打向同一个「最优」目标
		if jitter {
			s.score += rand.Float64() * 0.02
		}
		if s.cooling {
			cooling = append(cooling, s)
		} else {
			snaps = append(snaps, s)
		}
	}

	// 排序的第一优先级不是分数，而是「有没有被真正试过」。
	//
	// 原因是延迟分用的是候选集内的相对归一化：当只有 A 有数据、B 从未调用时，
	// 归一化区间会被人为拉开成 [A, A+50]，A 自己落在区间起点于是拿到满分，
	// 而 B 因为「乐观估值」同样拿满分——两者分数持平，
	// 胜负只能由成功率决定，A 反而稳赢。结果就是：
	// 一个从未被验证的目标永远排在慢目标后面，冷启动阶段谁先被随机选中
	// 谁就通吃后续流量。免费模型时好时坏，这种「赢者通吃」是致命的。
	//
	// 所以这里强制：没试过的一律排在试过的之前，组内再按分数排。
	//
	// 但在这一切之前先看优先级：priority 是「硬分层」，不是加权分。
	// 主力模型即使比免费的兜底慢，只要它还活着就该走主力——
	// 否则「我要用 deepseek」这种诉求会被延迟排序悄悄推翻。
	// 主力出事时不需要靠分数输给兜底：失败会立刻进冷却（见 observeFailure），
	// 冷却中的目标根本不在这个候选集里，于是自然落到下一档。
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].view.Priority != snaps[j].view.Priority {
			return snaps[i].view.Priority > snaps[j].view.Priority
		}
		ti, tj := snaps[i].health.Total > 0, snaps[j].health.Total > 0
		if ti != tj {
			return !ti
		}
		return snaps[i].score > snaps[j].score
	})
	// 全部在冷却中时，挑最快恢复的那个再试一次
	sort.SliceStable(cooling, func(i, j int) bool { return cooling[i].remain < cooling[j].remain })
	return snaps, cooling
}

// pick 选出当前最优且未尝试过的目标。needVision 见 ordered。
func (r *Router) pick(tried map[string]bool, needVision bool) *Target {
	// 只有当「启用中的目标全部被判定不可用」时才放开一轮重新探测。
	// 注意必须区别于「本次请求已把所有候选试过一遍」——后者应直接放弃，
	// 否则会对同一个失败目标反复重试，白白消耗时间与额度。
	if r.allDead() {
		r.reviveAll()
	}

	snaps, cooling := r.ordered(tried, true, needVision)
	if len(snaps) > 0 {
		// ε-贪心：留一小部分流量去探路。若永远只打当前最优，
		// 那些暂时落后又恢复的目标就再也没机会证明自己。
		//
		// 探路只在「最高优先级那一档」内部进行：探路的目的是不让同档目标被永久埋没，
		// 而不是把主力流量随机丢给低优先级的兜底模型。
		if len(snaps) > 1 && rand.Float64() < exploreEpsilon {
			top := snaps[0].view.Priority
			pool := make([]snapshot, 0, len(snaps))
			for _, s := range snaps {
				if s.view.Priority == top {
					pool = append(pool, s)
				}
			}
			return pool[rand.Intn(len(pool))].t
		}
		return snaps[0].t
	}
	// 全部在冷却中：挑最快恢复的那个再试一次（尝试本身会刷新状态，不会额外浪费）
	if len(cooling) > 0 {
		return cooling[0].t
	}
	return nil // 候选已在本轮重试中耗尽
}

// allDead 启用中的目标是否全部被判定不可用
func (r *Router) allDead() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	enabled := 0
	dead := 0
	for _, t := range r.targets {
		t.mu.Lock()
		en := t.Enabled
		d := t.h.Dead
		t.mu.Unlock()
		if en {
			enabled++
			if d {
				dead++
			}
		}
	}
	return enabled > 0 && enabled == dead
}

// reviveAll 放开全部不可用标记，给目标一轮重新证明自己的机会
func (r *Router) reviveAll() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, t := range r.targets {
		t.mu.Lock()
		if t.h.Dead {
			t.h.Dead = false
			t.h.ConsecFail = 0
			t.h.CooldownUntil = time.Time{}
			n++
		}
		t.mu.Unlock()
	}
	if n > 0 {
		logx.Warn("所有模型均被标记为不可用，本轮放开全部限制重新探测", "数量", n)
	}
}

// recordCall 把一次尝试写进统计库。异步入队、永不阻塞；stats 为 nil 时直接跳过。
func (r *Router) recordCall(t *Target, ok bool, ttftMS, latencyMS float64, promptTokens, outputTokens int, errMsg string) {
	d := r.stats.Load()
	if d == nil {
		return
	}
	d.RecordCall(statsdb.Call{
		TS:           time.Now(),
		EndpointID:   t.EndpointID,
		Model:        t.Model,
		OK:           ok,
		TTFTMS:       int64(ttftMS),
		LatencyMS:    int64(latencyMS),
		PromptTokens: promptTokens,
		OutputTokens: outputTokens,
		Err:          errMsg,
	})
}

func (t *Target) recordSuccess(res *Result) {
	t.mu.Lock()
	t.h.observeSuccess(res.TTFTMS, res.LatencyMS, time.Now())
	t.h.LastUsed = time.Now()
	t.mu.Unlock()
}

func (t *Target) recordFailure(cerr *CallError) time.Duration {
	t.mu.Lock()
	backoff := t.h.observeFailure(cerr.Message, time.Now())
	t.h.LastUsed = time.Now()
	t.mu.Unlock()
	return backoff
}

func (t *Target) markDead(msg string) {
	t.mu.Lock()
	t.h.Dead = true
	t.h.LastError = "已下线: " + msg
	view := t.snapLocked()
	t.mu.Unlock()
	logx.Warn("模型被判定不可用（可能是免费模型下线）", "endpoint", view.EndpointName, "model", view.Model)
}

// cooldownEndpoint 让同一接入点下的所有模型进入冷却
func (r *Router) cooldownEndpoint(endpointID string, d time.Duration, msg string) {
	r.mu.RLock()
	list := make([]*Target, 0, len(r.targets))
	for _, t := range r.targets {
		if t.snap().EndpointID == endpointID {
			list = append(list, t)
		}
	}
	r.mu.RUnlock()
	until := time.Now().Add(d)
	for _, t := range list {
		t.mu.Lock()
		if t.h.CooldownUntil.Before(until) {
			t.h.CooldownUntil = until
			t.h.LastError = "接入点级冷却: " + msg
		}
		t.mu.Unlock()
	}
	logx.Warn("接入点进入冷却", "endpoint", endpointID, "时长", d.String())
}

// TargetStats 供管理端展示的目标状态
type TargetStats struct {
	EndpointID   string  `json:"endpoint_id"`
	EndpointName string  `json:"endpoint_name"`
	Model        string  `json:"model"`
	Label        string  `json:"label"`
	Priority     int     `json:"priority"` // 越大越优先，主力 > 兜底
	Enabled      bool    `json:"enabled"`
	Vision       bool    `json:"vision"` // 带图请求只会派发给 vision=true 的目标
	Dead         bool    `json:"dead"`
	Cooling      bool    `json:"cooling"`
	CooldownSec  float64 `json:"cooldown_sec"`
	Score        float64 `json:"score"`
	TTFTMS       float64 `json:"ttft_ms"`
	LatencyMS    float64 `json:"latency_ms"`
	SuccessRate  float64 `json:"success_rate"`
	Total        int64   `json:"total"`
	Fails        int64   `json:"fails"`
	ConsecFail   int     `json:"consec_fail"`
	LastError    string  `json:"last_error"`
	LastUsed     string  `json:"last_used"`
	LastSuccess  string  `json:"last_success"`
}

// Stats 导出全部目标状态，按分数降序
func (r *Router) Stats() []TargetStats {
	r.mu.RLock()
	cands := make([]*Target, 0, len(r.targets))
	for _, t := range r.targets {
		cands = append(cands, t)
	}
	weights := r.weights
	r.mu.RUnlock()

	now := time.Now()
	hs := make([]Health, 0, len(cands))
	for _, t := range cands {
		t.mu.Lock()
		hs = append(hs, *t.h)
		t.mu.Unlock()
	}
	scale := computeScale(hs)

	out := make([]TargetStats, 0, len(cands))
	for _, t := range cands {
		t.mu.Lock()
		h := *t.h
		view := t.snapLocked()
		t.mu.Unlock()
		out = append(out, TargetStats{
			EndpointID:   view.EndpointID,
			EndpointName: view.EndpointName,
			Model:        view.Model,
			Label:        view.Label,
			Priority:     view.Priority,
			Enabled:      view.Enabled,
			Vision:       view.Vision,
			Dead:         h.Dead,
			Cooling:      h.Cooling(now),
			CooldownSec:  h.CooldownRemaining(now).Seconds(),
			Score:        h.Score(weights, now, h.Total > 0, scale),
			TTFTMS:       h.TTFTMS,
			LatencyMS:    h.LatencyMS,
			SuccessRate:  h.SuccessEWA,
			Total:        h.Total,
			Fails:        h.Fails,
			ConsecFail:   h.ConsecFail,
			LastError:    h.LastError,
			LastUsed:     relTime(h.LastUsed),
			LastSuccess:  relTime(h.LastSuccess),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// MaxOutTokens 候选目标里最大的输出上限，也就是调用方没给 max_tokens 时
// 单次请求实际上可能用到的天花板。
//
// brain.ContextBudget 要靠它给输出预留空间：它按固定值（400）预留，
// 而实际请求可能跑到 1024，历史就会被裁得比真正装得下的更少。
// 调用方应该用这个值（而不是配置里的 brain.max_out_tokens）来算预留。
func (r *Router) MaxOutTokens() int {
	r.mu.RLock()
	cands := make([]*Target, 0, len(r.targets))
	for _, t := range r.targets {
		cands = append(cands, t)
	}
	r.mu.RUnlock()

	best := 0
	for _, t := range cands {
		v := t.snap()
		if !v.Enabled {
			continue
		}
		if out := v.MaxOut; out > best {
			best = out
		}
	}
	if best <= 0 {
		return defaultMaxOut
	}
	return best
}

// PreviewEntry 管理端「实际调度顺序」里的一个环节
type PreviewEntry struct {
	Rank         int     `json:"rank"`
	EndpointID   string  `json:"endpoint_id"`
	EndpointName string  `json:"endpoint_name"`
	Model        string  `json:"model"`
	Label        string  `json:"label"`
	Priority     int     `json:"priority"` // 硬分层档位，大者先
	Score        float64 `json:"score"`
	Total        int64   `json:"total"`        // 累计调用次数，0 = 还没被验证过
	Cooling      bool    `json:"cooling"`      // 冷却中（正常路径里轮不到，只在候选耗尽时兜底）
	CooldownSec  float64 `json:"cooldown_sec"` //
	// Explore 标记「这一档里有多个候选，因此有概率被随机探路命中」。
	Explore bool `json:"explore"`
	// ExploreHit 只在排第一且 Explore 为真时有意义：说明「下一个」其实不是确定的。
	ExploreHit bool `json:"explore_hit"`
}

// ChainPreview 一次撮合链路的完整预览
type ChainPreview struct {
	Chain       []PreviewEntry `json:"chain"`
	MaxAttempts int            `json:"max_attempts"`
	ExploreRate float64        `json:"explore_rate"` // 探路概率，0.08 = 8%
	AllDead     bool           `json:"all_dead"`     // 启用中的目标全被判死
}

// PreviewChain 复刻 Chat 的重试链路：按真实调度顺序依次列出「会被尝试的目标」。
//
// 与 pick 共用 ordered()，所以这里列出的顺序就是实际尝试顺序；
// 唯一的例外是 ε-贪心探路（默认 8%，且只在最高优先级档内随机），
// 因此当最高档有多个候选时，第一名不是确定的——用 Explore 字段如实标出。
//
// 只读：不调用 allDead/reviveAll，预览绝不能改运行状态。
func (r *Router) PreviewChain() ChainPreview {
	r.mu.RLock()
	maxTry := r.maxTry
	r.mu.RUnlock()
	if maxTry <= 0 {
		maxTry = 4
	}

	tried := map[string]bool{}
	chain := make([]PreviewEntry, 0, maxTry)
	for i := 0; i < maxTry; i++ {
		// 预览不带图：不勾 vision 的目标本来就在真实链路里，不该从预览消失
		snaps, cooling := r.ordered(tried, false, false)
		var step *snapshot
		switch {
		case len(snaps) > 0:
			step = &snaps[0]
		case len(cooling) > 0:
			step = &cooling[0]
		}
		if step == nil {
			break // 候选耗尽（重试次数还没用完就没得试了）
		}
		tried[step.key] = true
		chain = append(chain, PreviewEntry{
			Rank:         len(chain) + 1,
			EndpointID:   step.view.EndpointID,
			EndpointName: step.view.EndpointName,
			Model:        step.view.Model,
			Label:        step.view.Label,
			Priority:     step.view.Priority,
			Score:        step.score,
			Total:        step.health.Total,
			Cooling:      step.cooling,
			CooldownSec:  step.remain.Seconds(),
		})
	}

	// 最高档内若有多个可用候选，pick 有 8% 概率在其中随机挑一个，
	// 也就是说「下一个命中的是谁」并不是确定的。如实标注，别让面板骗人。
	if len(chain) > 0 && !chain[0].Cooling {
		top := chain[0].Priority
		n := 0
		for _, e := range chain {
			if !e.Cooling && e.Priority == top {
				n++
			}
		}
		if n > 1 {
			for i := range chain {
				if !chain[i].Cooling && chain[i].Priority == top {
					chain[i].Explore = true
				}
			}
			chain[0].ExploreHit = true
		}
	}

	// 顺带报告「全被判死」这个异常状态：真要调用时会触发一轮全量复活重探。
	r.mu.RLock()
	enabled, dead := 0, 0
	for _, t := range r.targets {
		t.mu.Lock()
		en := t.Enabled
		d := t.h.Dead
		t.mu.Unlock()
		if en {
			enabled++
			if d {
				dead++
			}
		}
	}
	r.mu.RUnlock()

	return ChainPreview{
		Chain:       chain,
		MaxAttempts: maxTry,
		ExploreRate: exploreEpsilon,
		AllDead:     enabled > 0 && enabled == dead,
	}
}

// Summary 全局概览
type Summary struct {
	TotalReq    int64  `json:"total_req"`
	FailReq     int64  `json:"fail_req"`
	LastOK      string `json:"last_ok"`
	LastErr     string `json:"last_err"`
	AliveCount  int    `json:"alive_count"`
	TotalCount  int    `json:"total_count"`
	MaxAttempts int    `json:"max_attempts"`
}

// Summary 返回全局统计
func (r *Router) Summary() Summary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	alive := 0
	now := time.Now()
	for _, t := range r.targets {
		t.mu.Lock()
		ok := t.Enabled && !t.h.Dead && !t.h.Cooling(now)
		t.mu.Unlock()
		if ok {
			alive++
		}
	}
	return Summary{
		TotalReq:    r.totalReq,
		FailReq:     r.failReq,
		LastOK:      relTime(r.lastOK),
		LastErr:     r.lastErr,
		AliveCount:  alive,
		TotalCount:  len(r.targets),
		MaxAttempts: r.maxTry,
	}
}

// ResetTarget 清除某个目标的失败状态（管理端手动恢复）
func (r *Router) ResetTarget(endpointID, modelID string) bool {
	r.mu.RLock()
	t, ok := r.targets[endpointID+"|"+modelID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	t.mu.Lock()
	t.h.Dead = false
	t.h.ConsecFail = 0
	t.h.CooldownUntil = time.Time{}
	t.h.SuccessEWA = 0.5
	t.h.LastError = ""
	t.mu.Unlock()
	return true
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "从未"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d小时前", int(d.Hours()))
	}
	return t.Format("01-02 15:04")
}
