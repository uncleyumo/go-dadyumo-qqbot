package memepool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
)

// Curator 是优选任务：定期给池子里的表情包打分，把垃圾清出去。
//
// 为什么需要它：入池的门槛只有「有描述」，而描述是模型自己写的，
// 质量参差。20 个名额如果只靠「用量低就淘汰」，会留下
// 一堆发不出去但也没人扔的垃圾——它们占着位置，真正好用的挤不进来。
//
// 它不是必需品：Judge 为 nil 时整个任务跳过，池子照样工作，
// 只是退化到「按用量和到期时间淘汰」。
type Curator struct {
	pool   *Pool
	router *llm.Router
	store  ConfigSource

	// optEvery 多久跑一次
	optEvery time.Duration
	// optBatch 一批过多少张。只给描述、不给图——判断「这张图配不配得上
	// 当表情包」靠描述就够了，给图会让每轮 token 翻几倍。
	optBatch int
	optMaxOut int
}

// ConfigSource 提供当前配置（优选任务要用最便宜的模型）
type ConfigSource interface {
	// CheapestModel 返回当前可用的最便宜模型名与它的接入点。
	// 没有可用目标时返回空串。
	CheapestModel() (endpointID, model string)
	// Paused 是否处于总开关关停状态。
	//
	// 这个接口是 Curator 唯一能看到配置的窗口，所以关停判定必须从这里透出去：
	// 优选是**唯一一个不看群消息、自己到点就调模型**的后台任务，
	// 少了这个口子，用户按下「关闭」之后它照样每 6 小时烧一次 token。
	Paused() bool
}

// NewCurator 构造优选任务
func NewCurator(p *Pool, router *llm.Router, src ConfigSource) *Curator {
	return &Curator{
		pool:     p,
		router:   router,
		store:    src,
		optEvery: 6 * time.Hour,
		optBatch: 20,
		// 6000 而不是几百：打分模型是 reasoning 型，token 先被思考吃掉，
		// 剩下的才轮到正文。实测 deepseek-flash 在 900 的上限下
		// status=incomplete、output 里只剩 reasoning，连 message 都没有——
		// 而这是**不报错**的失败：日志一切正常，只是这轮没清垃圾。
		// 宁可多花点 token，也不能让「清理」静默失效。
		optMaxOut: 6000,
	}
}

// SetInterval 调整跑批间隔（配置项用）
func (c *Curator) SetInterval(d time.Duration) {
	if d > 0 {
		c.optEvery = d
	}
}

// Start 在后台循环跑优选，直到 ctx 取消。
//
// 首次不立即跑：刚启动时池子里的图都是新鲜的没什么可清的，
// 等一个间隔再开始更合理。
func (c *Curator) Start(ctx context.Context) {
	if c == nil || c.pool == nil || c.router == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(c.optEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.runOnce(ctx)
			}
		}
	}()
}

// runOnce 跑一轮。导出给测试直接调用。
func (c *Curator) runOnce(ctx context.Context) {
	// 总开关（闸门四）。判在 runOnce 而不只是 ticker 里，是因为测试与将来的
	// 手动触发都直接调这里——只在 ticker 上拦截，绕过去就漏。
	if c.store.Paused() {
		logx.Info("表情包优选跳过：总开关已关闭")
		return
	}
	pool := c.pool.Browse(time.Now())
	if len(pool) == 0 {
		return
	}

	scores, err := c.judge(ctx, pool)
	if err != nil {
		// 优选失败不是致命问题：池子照常工作，只是这轮没清垃圾
		logx.Warn("表情包优选本轮跳过", "err", err.Error(), "条数", len(pool))
		return
	}
	c.pool.ApplyQuality(scores)

	dropped := c.pool.PruneLowQuality()
	if len(dropped) > 0 {
		logx.Info("优选淘汰了低质量表情包", "数量", len(dropped), "剩余", c.pool.Len())
	}
}

// judge 让模型给一批描述打分。
func (c *Curator) judge(ctx context.Context, memes []Meme) (map[int64]float64, error) {
	endpointID, model := c.store.CheapestModel()
	if endpointID == "" || model == "" {
		return nil, fmt.Errorf("没有可用的打分模型")
	}

	var sb strings.Builder
	sb.WriteString("下面是群里要当表情包用的图，每行一个，只有文字描述。\n")
	sb.WriteString("给每行打一个 0~1 的分：\n")
	sb.WriteString("1.0 = 一看就想发，梗强、通用、看懂的都会笑\n")
	sb.WriteString("0.5 = 勉强能用\n")
	sb.WriteString("0.0 = 没用（描述空洞、只是图/风景/文字截图、看不出梗）\n")
	sb.WriteString("严格按 JSON 输出，键是行号，值是分数，不要有别的话。\n\n")
	for i, m := range memes {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, m.Descr)
	}

	res, cerr := c.router.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: sb.String()},
		},
		MaxTokens:   c.optMaxOut,
		Temperature: 0.1,
	})
	if cerr != nil {
		return nil, cerr
	}
	return parseScores(res.Content, memes)
}