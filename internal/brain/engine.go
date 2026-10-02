// Package brain 的调度中枢：把「群里发生了什么」翻译成「要不要说话、说什么、分几条发」。
//
// 三条设计主线：
//  1. 攒批（debounce）——消息不是逐条触发模型，而是攒一小会儿再决定，
//     既像真人反应需要时间，也是控制调用次数最有效的手段
//  2. 冲动值（impulse）——用规则先挡掉明显不该开口的情形，
//     只有值得考虑的才去问模型，省下的都是钱
//  3. 硬预算——每日调用次数与上下文 token 双重封顶，
//     免费娱乐项目的第一要务是不失控
package brain

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/imgproc"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
)

// Sender 发送通道，由上层（agent）注入，方便测试时替换。
//
// 关于 @ 的平台行为（踩过的坑，别再改回去）：
// 带 msg_id 的被动回复会被 QQ 客户端渲染成「@原发送者」。所以拆句发送的每一条
// 都会自带一个 @，拆 5 条就是 5 个 @——观感像客服派单。
// 曾经试过让后续句子走主动消息绕开这个渲染，但本机器人没有主动消息权限
// （平台返回 40034105），走那条路等于一条都发不出去。
// 因此现在的策略是：全部走被动，靠 speak.max_segments 把条数压到 1~2 条，
// 只保留「我在回你」这一次自然的 @。
type Sender interface {
	SendGroup(ctx context.Context, groupID, content string) error
	// SendGroupTo 指定这条回复要挂在谁的息下（replyToOpenID 为空则挂最新那条）。
	// 多人同时说话时不指定，群里就分不清它到底在回谁。
	SendGroupTo(ctx context.Context, groupID, content, replyToOpenID string) error
}

// MediaRef 带发送人的媒体引用。
//
// 为什么要带人：模型得知道「这个视频是谁发的」，发送层也得知道该把回复挂给谁。
// 只给一串 URL 的话，攒批窗口里几个人的视频混在一起就彻底对不上人了。
type MediaRef struct {
	OpenID string
	Name   string
	URL    string
}

// Event 一条进来的群消息，已归一化为调度层需要的形态
type Event struct {
	GroupID   string
	GroupName string
	MsgID     string
	OpenID    string
	Name      string
	Content   string
	Images []string   // 这条消息**自己**带的图片 URL（表情包/截图都算），可为空
	Videos []MediaRef // 消息里带的视频附件（不含引用里的），可为空
	Voices []VoiceRef // 平台没给转写文本、留了音频地址的语音（兜底转写用），可为空
	Quoted string     // 被引用的消息文本（有人引用着说话时才有），可为空

	// QuotedPics 是**被引用的那条消息里**的图片，带原作者。
	//
	// 必须和 Images 分开：Images 里的图归属就是这条消息的发送人，
	// 把引用的图混进去等于告诉模型「这是他发的」。2026-10-02 实况：
	// 有人引用别人半小时前发的图问「这谁？」，机器人答「你自己发的你问我？」。
	QuotedPics []MediaRef

	// QuotedFrom 是「引用了别人」的那个人，Quoted 是被他引用的内容。
	// QuotedAuthor 是被引用的那条消息的原作者（可能与 QuotedFrom 不是同一人）。
	// 三者必须分开：只给文本的话，模型会把被引用的内容当成「刚才有人在群里说的」，
	// 于是回错了人——群友甩一段聊天记录出来的时候最容易发生。
	QuotedFrom         string
	QuotedFromOpenID   string
	QuotedAuthor       string
	QuotedAuthorOpenID string

	// MentionTarget 是这条消息 @ 到的、但不是机器人的那个人。
	//
	// 群里的常态是「A 在说话，顺手 @ B 让他答」。这时该回的是 B，不是 A，
	// 光靠「谁发的」永远会把话头挂错人。平台给的 mentions 数组是唯一可靠来源。
	MentionTarget string

	// AtAll 表示这条消息 @ 了全体成员。
	//
	// 平台对 @全体 的处理很特殊：mentions 数组里没有条目、事件也不置 at=true，
	// 只在正文里塞一个 <@all> 标签。所以它既不是「明确在问机器人」，
	// 也不该被当成没看见——如实告诉模型「他 @ 了全员」，由它判断要不要接。
	AtAll bool

	AtMe  bool
	IsBot bool
	TS    time.Time
}

// c2cPrefix 单聊会话的内部前缀（与 agent 保持一致）
const c2cPrefix = "c2c:"

// imgSpamCount 同一个人在一个攒批窗口内发图超过这个数，就算刷屏。
// 真人群里连发一大串图的只有两种人：刷表情包玩的和故意灌水的，
// 正常人看这种都是一句「你发这么多干啥」，没必要逐张认真看。
const imgSpamCount = 5

// maxImagesPerSender 单人在一个攒批窗口内最多记录多少张，防止有人真刷几百张把内存顶爆
const maxImagesPerSender = 12

// maxVideosTracked 单个攒批窗口最多记几个视频 URL。视频处理比图片贵一个量级
// （下载 + ffmpeg + 转写），留 4 个做上限只为应对「连发好几条」的场面，
// 真正进上下文的只会是最后一条。
const maxVideosTracked = 4

// imageFrom 一个人在本批攒到的图片
type imageFrom struct {
	openID string
	name   string
	urls   []string
}

// maxBatchWait 攒批的最长等待时间。
// 群里聊得热火朝天时会不断重置 debounce，没有这个上限它就永远等不到触发，
// 表现成「机器人突然不说话了」，而且很难排查。
const maxBatchWait = 45 * time.Second

// Engine 决策引擎
type Engine struct {
	store  *config.Store
	router *llm.Router
	mem    *memory.Store
	sender Sender

	mu     sync.Mutex
	states map[string]*groupState

	statMu sync.Mutex
	stat   DailyStat

	envMu sync.Mutex
	env   EnvInfo // 特殊日子 + 天气，一天懒刷新一次

	// memes 表情包池。没启用时是 nil，工具层会因此不给模型这个工具。
	memes *memepool.Pool
	// imgSender 发送图片的能力。brain 只依赖这个窄接口，
	// 实际实现是 qqapi.Client——这样 brain 的测试不需要真实客户端。
	imgSender ImageSender
}

// ImageSender 发图片的能力
type ImageSender interface {
	SendImage(ctx context.Context, groupOpenID string, data []byte, mime, replyToOpenID string) error
}

// SetMemePool 挂上表情包池与图片发送通道。
func (e *Engine) SetMemePool(pool *memepool.Pool, img ImageSender) {
	e.memes = pool
	e.imgSender = img
}

// DailyStat 当日用量统计，同时用于管理端展示与预算拦截
type DailyStat struct {
	Day          string  `json:"day"`
	Calls        int     `json:"calls"`
	Said         int     `json:"said"`
	Quiet        int     `json:"quiet"`
	PromptTokens int64   `json:"prompt_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	EstCostUSD   float64 `json:"est_cost_usd"`
	LastModel    string  `json:"last_model"`
	LastErr      string  `json:"last_err"`
	LastAt       string  `json:"last_at"`
}

// groupState 单个群的攒批与触发状态
type groupState struct {
	mu      sync.Mutex
	timer   *time.Timer
	firstAt time.Time

	atMe       bool
	nameCalled bool
	fromMaster bool
	question   bool
	replyToBot bool
	newCount   int
	impulse    float64
	reasons    []string
	firing     bool

	// 本批的「主触发者」——@ 它、或者叫它名字的那个人。
	// 没有它，trigger 只能说「有人 @ 了你」，模型不知道该回谁，
	// 发送层也只能把回复挂到群里最新那条上——这就是认错人的根源。
	triggerOpenID string
	triggerName   string

	// atAll：本批里有人 @ 了全体成员。
	// 它不是「在跟机器人说话」，所以不参与 recordTrigger 的优先级，
	// 只作为一条事实写进 trigger，让模型知道刚才有人喊了全员。
	atAll bool

	// atOthers：本批里有人明确 @ 了别人（@给的不是机器人）。
	//
	// 为什么要单记：群里的常态是「A 说话、顺手 @ B 让他答」。
	// 那种对话跟机器人没关系，插进去就是抢话——2026-10-01 实况：
	// 有人 @ 了另一位群友（群名片写着「多端智能体」，只是个昵称），
	// 机器人没被 @，却因为「群里有闲聊 + 攒够一批」凑到 0.40 挤过门限，
	// 抢着回了一句。
	//
	// 这里只降冲动值、不做硬闸：抢话和「恰到好处的接话」只差一个量，
	// 机械地一刀切会把合法的插话也毙掉，交给阈值继续权衡。
	// 判断依据只有平台的 mentions 数组，与对方是人还是机器人无关。
	atOthers bool

	// faceSpam：本批里全是纯表情、没有一个字。
	// 刷屏不是对话，不该触发插话。
	faceSpam bool

	// pendingFire：上一轮 fire 还在跑时又攒了新消息。
	// 早退时置位，等当前这轮结束时补跑一次，否则这批消息永远不会被处理。
	pendingFire bool

	imgSenders       []imageFrom // 这一批按发送人归拢的图片
	quoted           string      // 本批最后一条带引用的消息的引用内容
	quotedAuthor     string      // 被引用的那条消息的原作者
	quotedAuthorOpen string      // 同上，openid
	videos           []MediaRef  // 这一批攒到的视频（带发送人）
	voices           []VoiceRef  // 这一批攒到的待兜底转写语音
}

// NewEngine 创建决策引擎
func NewEngine(store *config.Store, router *llm.Router, mem *memory.Store, sender Sender) *Engine {
	return &Engine{
		store:  store,
		router: router,
		mem:    mem,
		sender: sender,
		states: map[string]*groupState{},
		stat:   DailyStat{Day: time.Now().Format("2006-01-02")},
	}
}

func (e *Engine) stateOf(groupID string) *groupState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.states[groupID]
	if !ok {
		st = &groupState{}
		e.states[groupID] = st
	}
	return st
}

// OnMessage 群消息入口。它只做登记与排程，真正的决策在 debounce 到期后异步发生。
func (e *Engine) OnMessage(ev *Event) {
	if ev == nil || ev.GroupID == "" {
		return
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	cfg := e.store.Get()
	g := e.mem.Group(ev.GroupID, ev.GroupName)

	content := strings.TrimSpace(ev.Content)
	if ev.IsBot {
		// 自己说过的话也要进上下文，否则它会忘了自己刚说了啥、开始复读。
		// 注意不能 MarkUserTurn：那会清掉连续发言计数，让它以为自己刚被搭理过。
		g.Append(memory.Line{TS: ev.TS, Role: memory.RoleBot, Content: content}, cfg.Brain.MaxHistory)
		return
	}

	isMaster := cfg.IsMaster(ev.OpenID)
	g.TouchMember(ev.OpenID, ev.Name)
	// OpenID 一起存：昵称会改、会撞名，身份只能靠 openid。
	g.Append(memory.Line{
		TS: ev.TS, Role: memory.RoleUser, Name: ev.Name, OpenID: ev.OpenID,
		Content: content, MsgID: ev.MsgID,
	}, cfg.Brain.MaxHistory)
	g.MarkUserTurn()

	// 认主口令：群里回调只给 openid，这是把「我的 QQ 号」和 openid 对上的唯一途径
	if cfg.Master.BindEnabled && strings.HasPrefix(content, "#认主") {
		e.handleBind(cfg, ev)
		return
	}

	nameCalled := mentionsName(content, cfg.Persona.Name)
	question := LooksLikeQuestion(content)
	replyToBot := e.looksLikeReplyToBot(g, content)

	// 被 @ 或被直接叫名字：开启一段实时宽限，这段时间里不再按在线时段概率过滤
	if ev.AtMe || nameCalled {
		g.MarkAtHit()
	}

	st := e.stateOf(ev.GroupID)
	st.mu.Lock()
	// 纯表情/纯附件的连发是刷屏，不是在跟它说话。
	// 图片刷屏有 imgSpamCount 兜着，表情走的是正文文本路径、完全绕过那个机制，
	// 结果连发 8 个表情就把攒批条数顶上去、推高冲动值、挤进门限发言——
	// 2026-10-01 实况：没被 @ 也回了两句（「表情包批发呢你」「有事说事，别光发图」）。
	// 这里单独记一笔，后面不把它算进「攒了一批」和「群里有闲聊」。
	onlyFaces := IsOnlyPlaceholders(content) && len(ev.Images) == 0 && len(ev.Videos) == 0
	st.newCount++
	st.recordAtOthers(ev.AtMe, ev.MentionTarget != "" && !ev.AtMe && !nameCalled)
	// 攒批条数在这里才已知：最后一条消息的冲动值把整批的热闹程度算进去
	imp := ComputeImpulse(ImpulseInput{
		AtMe:        ev.AtMe,
		NameCalled:  nameCalled,
		Question:    question,
		ReplyToBot:  replyToBot,
		FromMaster:  isMaster,
		Chatter:     true,
		NewCount:    st.newCount,
		Consecutive: g.Consecutive(),
		SinceSpeak:  g.SinceLastSpeak(),
		Idle:        0,
	})
	if onlyFaces {
		// 刷屏不该是「值得插一句」的热闹。把这两项抹掉，只留下真正的触发理由。
		// 但被 @ / 被叫名字 / 在提问时照常回——用户 @ 完只发一个表情，
		// 那也是明确在叫它，不该被当成刷屏忽略掉。
		if !ev.AtMe && !nameCalled && !question && !replyToBot {
			imp.Score -= wChatter + wManyNew
			if imp.Score < 0 {
				imp.Score = 0
			}
			imp.Reasons = append(imp.Reasons, "对方在刷表情，不是跟你说话")
			st.faceSpam = true
		}
	} else {
		// 这一批里混进了有实质内容的消息。刷屏标记必须撤销——
		// 不撤销的话「连发七个表情 + 一句正经话」会被当成纯刷屏给毙掉。
		st.faceSpam = false
	}
	if ev.AtMe {
		st.atMe = true
	}
	if nameCalled {
		st.nameCalled = true
	}
	if isMaster {
		st.fromMaster = true
	}
	if ev.AtAll {
		st.atAll = true
	}
	st.recordTrigger(g, ev.OpenID, ev.Name, ev.AtMe, nameCalled, isMaster, ev.MentionTarget)
	if question {
		st.question = true
	}
	if replyToBot {
		st.replyToBot = true
	}
	if len(ev.Images) > 0 {
		st.addImages(ev.OpenID, ev.Name, ev.Images)
	}
	// 引用里的图记到**原作者**名下。原作者认不出来（openid 和名字都空）时
	// addImages 会归到一个空 key 上，提示词里显示成「有人」——
	// 这仍然远好过赖到引用的人头上。
	for _, q := range ev.QuotedPics {
		st.addImages(q.OpenID, q.Name, []string{q.URL})
	}
	if len(ev.Videos) > 0 && len(st.videos) < maxVideosTracked {
		st.videos = append(st.videos, ev.Videos...)
	}
	if len(ev.Voices) > 0 {
		st.voices = append(st.voices, ev.Voices...)
		if len(st.voices) > maxVoicesPerBatch {
			st.voices = st.voices[len(st.voices)-maxVoicesPerBatch:]
		}
	}
	registerQuoted(st, ev.Quoted, ev.QuotedAuthor, ev.QuotedAuthorOpenID)
	if imp.Score > st.impulse {
		st.impulse = imp.Score
		st.reasons = imp.Reasons
	}
	st.applyBatchPenalties()
	if st.firstAt.IsZero() {
		st.firstAt = time.Now()
	}
	e.scheduleLocked(st, cfg, ev.GroupID)
	st.mu.Unlock()

	logx.Debug("消息已登记", "group", ev.GroupID, "from", ev.Name, "at", ev.AtMe,
		"impulse", fmt.Sprintf("%.2f", imp.Score), "攒批", st.newCount)
}

// addImages 按发送人归拢本批的图片。同一个人重复发的同一张图不重复记。
// 调用方必须持有 st.mu。
// registerQuoted 登记本批的引用信息。调用方必须持有 st.mu。
//
// 门槛是「有没有引用」，不是「引用文本长不长」。以前只看 Quoted 非空，
// 于是引用一条纯图片消息（正文为空）时整段被丢掉，原作者是谁也跟着没了。
// 2026-10-02 实况：有人引用别人 23:53 发的图问「这谁？」，机器人答
// 「你自己发的你问我？」——日志里那条群消息连「引用」字段都没有。
//
// 现在 agent 侧对纯图片引用会给出 "[图片]" 占位，但这里仍然两个字段都判：
// 上游哪天漏了占位，损失的是「引用者问的是谁」这个最关键的信息，不该静默。
func registerQuoted(st *groupState, quoted, author, authorOpenID string) {
	if quoted == "" && author == "" {
		return
	}
	st.quoted = quoted
	st.quotedAuthor = author
	st.quotedAuthorOpen = authorOpenID
}

func (st *groupState) addImages(openID, name string, urls []string) {
	idx := -1
	for i := range st.imgSenders {
		if st.imgSenders[i].openID == openID {
			idx = i
			break
		}
	}
	if idx < 0 {
		st.imgSenders = append(st.imgSenders, imageFrom{openID: openID, name: name})
		idx = len(st.imgSenders) - 1
	}
	slot := &st.imgSenders[idx]
	if slot.name == "" {
		slot.name = name
	}
	for _, u := range urls {
		if u == "" || len(slot.urls) >= maxImagesPerSender {
			continue
		}
		dup := false
		for _, have := range slot.urls {
			if have == u {
				dup = true
				break
			}
		}
		if !dup {
			slot.urls = append(slot.urls, u)
		}
	}
}

// pickImages 决定这一批里到底带哪几张图进上下文，以及要不要附一句刷屏提示。
//
// 刷屏规则：同一个人连发超过 imgSpamCount 张时，只取第一张和最后一张——
// 中间那些既烧 token 又没什么新信息。同时给模型一句提示让它自己决定
// 怎么吐槽（不写死回复，写死了就假了）。
func (st *groupState) pickImages() (images []string, spamNote, whoNote string) {
	if len(st.imgSenders) == 0 {
		return nil, "", ""
	}
	var senders []string
	for _, f := range st.imgSenders {
		if len(f.urls) == 0 {
			continue
		}
		who := f.name
		if who == "" {
			who = "有人"
		}
		if len(f.urls) > imgSpamCount {
			images = append(images, f.urls[0], f.urls[len(f.urls)-1])
			spamNote = fmt.Sprintf(
				"%s一口气刷了 %d 张图，这里只给你看了第一张和最后一张，中间的全当他刷屏。"+
					"正常人会嫌烦，会怼他两句或者懒得理，别一本正经地逐张分析。", who, len(f.urls))
			continue
		}
		images = append(images, f.urls...)
		senders = append(senders, fmt.Sprintf("%s 发了 %d 张图/表情包", who, len(f.urls)))
	}
	// 图是谁发的是归属信息，不是备注：群里有好几个人同时在发图时，
	// 模型得能分清「这是张三发的」而不是笼统的「有人发了图」。
	if len(senders) > 0 {
		whoNote = "这一批的图片来自：" + strings.Join(senders, "；") + "。"
	}
	return images, spamNote, whoNote
}

// recordAtOthers 记下「这批里有人明确 @ 了别人」。调用方必须持有 st.mu。
//
// 判断只看平台给的 mentions 数组——@ 的对象是不是机器人、昵称里有没有
// 「智能体」三个字，通通不看。2026-10-01 就是把群名片「群名片丁」
// 当成了机器人身份，结论完全错了：那是个人，那次 @ 的是他。
//
// 后面有人 @ 我或叫了我名字时立刻撤销：攒批窗口里
// 「先 @ 别人、后 @ 我」很常见，那时该回的是我。
func (st *groupState) recordAtOthers(atMe, atOthers bool) {
	switch {
	case atOthers && !atMe:
		st.atOthers = true
	case atMe:
		st.atOthers = false
	}
}

// applyBatchPenalties 在**批次层面**统一压分。调用方必须持有 st.mu。
//
// 为什么必须在这里、而不能扣在单条消息上：st.impulse 取的是整批最高分
// （`if imp.Score > st.impulse`）。给「@ 别人」那条消息单独扣分，
// 攒批窗口里后面跟的两句闲聊（各 0.25）会把分数盖回来，
// 于是 0.25+0.15-0.50 → 0，仍 ≥ 0.40 阈值，机器人照样抢话。
// 2026-10-01 实况就是这样抢了一句「煮面呢 咋了」。
//
// 抢话是整批的性质，不是某一条消息的性质，所以扣分必须发生在合计之后。
func (st *groupState) applyBatchPenalties() {
	if !st.atOthers {
		return
	}
	// 被 @ / 被叫名字 / 主人说话时，那段对话是顺带的，不该被压
	if st.atMe || st.nameCalled || st.fromMaster {
		return
	}
	st.impulse += pAtOthers
	if st.impulse < 0 {
		st.impulse = 0
	}
	st.reasons = append(st.reasons, "有人在跟别人说话")
}

// recordTrigger 记下本批的「主触发者」。调用方必须持有 st.mu。//
// 攒批窗口里好几个人各说各的，这一个 openid 同时决定了模型「在回谁」和
// 发送层「把回复挂给谁」——认错人就是这里没记对。
//
// 优先级，从高到低：
//  1. 说话的人被 @ 了机器人 / 叫了机器人名字 / 是开发者 → 就是他
//  2. 说话的人 @ 的是别人（群里最常见的「A 说话顺手 @ B」）→ 被 @ 的那个人
//  3. 都没有 → 第一条说话的人
//
// 三级都落空才留空，而留空意味着发送层只能退回「挂群里最新那条」，
// 那是最容易挂错的兜底。
func (st *groupState) recordTrigger(g *memory.Group, openID, name string, atMe, nameCalled, isMaster bool, mentionTarget string) {
	switch {
	case atMe || nameCalled || isMaster:
		st.triggerOpenID, st.triggerName = openID, name
	case mentionTarget != "" && mentionTarget != st.triggerOpenID:
		if n := g.NameOfByOpenID(mentionTarget); n != "" {
			st.triggerOpenID, st.triggerName = mentionTarget, n
		} else if st.triggerOpenID == "" {
			st.triggerOpenID, st.triggerName = openID, name
		}
	case st.triggerOpenID == "":
		st.triggerOpenID, st.triggerName = openID, name
	}
}

// scheduleLocked 设置或重置攒批定时器。调用方必须持有 st.mu。
func (e *Engine) scheduleLocked(st *groupState, cfg config.Config, groupID string) {
	now := time.Now()
	// 已经等太久了就不再顺延，直接按剩余时间触发
	wait := time.Duration(cfg.Brain.DebounceSec) * time.Second
	if wait <= 0 {
		wait = 10 * time.Second
	}
	jitter := time.Duration(rand.Intn(maxi(1, cfg.Brain.DebounceJitterSec))) * time.Second
	wait += jitter

	deadline := st.firstAt.Add(maxBatchWait)
	if now.Add(wait).After(deadline) {
		wait = deadline.Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
	}

	if st.timer != nil {
		// 重置而非新建：群里连续说话时不断后移触发点，才叫「攒批」
		if !st.timer.Stop() {
			select {
			case <-st.timer.C:
			default:
			}
		}
		st.timer.Reset(wait)
		return
	}
	st.timer = time.AfterFunc(wait, func() { e.fire(groupID) })
}

// fire 攒批到期，做一次真正的决策
func (e *Engine) fire(groupID string) {
	st := e.stateOf(groupID)
	cfg := e.store.Get()
	g := e.mem.Group(groupID, "")

	st.mu.Lock()
	if st.firing {
		// 上一轮决策还在跑（最长 60s LLM + 90s 音视频管道），而这个定时器先到期了。
		// 早退时必须记一笔：否则这批消息留在 st 里却再没有定时器会叫它，
		// 群里一安静，@ 它的消息就永远不会被处理。
		st.pendingFire = true
		st.mu.Unlock()
		return
	}
	st.firing = true
	atMe := st.atMe
	nameCalled := st.nameCalled
	fromMaster := st.fromMaster
	question := st.question
	replyToBot := st.replyToBot
	newCount := st.newCount
	impulse := st.impulse
	reasons := st.reasons
	images, imgSpamNote, imgWhoNote := st.pickImages()
	quoted := st.quoted
	quotedAuthor := st.quotedAuthor
	videos := st.videos
	voices := st.voices
	triggerOpenID := st.triggerOpenID
	triggerName := st.triggerName
	atAll := st.atAll
	atOthers := st.atOthers
	faceSpam := st.faceSpam
	// eager = 「有人在等这条回复」。被 @ / 被叫名字 / 主人说话这三种对节奏的
	// 要求完全一样：晚几秒群友就当你掉线了。合成一个布尔，是为了让下游
	// （deliver / segDelay）只需要知道「急不急」，不必知道「为什么急」。
	//
	// 只用 atMe 不够——nameCalled 和 fromMaster 同样是有人在等，
	// 而 atMe 把它们漏掉了，那两种场景会按「没人等」的慢节奏发。
	eager := atMe || nameCalled || fromMaster
	// 清空攒批，后续新消息会重新起一轮
	st.atMe, st.nameCalled, st.fromMaster, st.question, st.replyToBot = false, false, false, false, false
	st.newCount, st.impulse, st.reasons = 0, 0, nil
	st.imgSenders, st.quoted, st.videos, st.voices = nil, "", nil, nil
	st.quotedAuthor, st.quotedAuthorOpen = "", ""
	st.triggerOpenID, st.triggerName, st.atAll = "", "", false
	st.faceSpam = false
	st.atOthers = false
	st.timer = nil
	st.firstAt = time.Time{}
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		st.firing = false
		pending := st.pendingFire
		st.pendingFire = false
		// 还有排着的定时器就别重复排——它自己会来叫这一轮。
		if pending && st.timer == nil && st.newCount > 0 {
			e.scheduleLocked(st, cfg, groupID)
		}
		st.mu.Unlock()
	}()

	// 下面这六道闸过去全是 logx.Debug，而生产默认 Info —— 一条都看不见。
	// 「群里毛也不回，我不知道为什么」就是这么来的：不是没记录，是记录了看不见。
	//
	// 所以全部提到 Info 并归入 decision 分类，同时**补齐缺失的参数**：
	// 光说「冲动值不足」没有用，得看得到「0.35 < 0.40，差 0.05」；
	// 光说「不在在线时段」更没用——那是个概率判定，同一时刻下次可能就中了，
	// 必须把本次摇到的数和当时的阈值都记下来（见 schedule.go 的 ScheduleDecision）。

	if muted := g.MutedUntil(); time.Now().Before(muted) {
		logx.InfoCat(logx.CatDecision, "跳过：该群处于静默期",
			"group", groupLabel(g), "静默至", muted.Format("15:04:05"),
			"剩余", time.Until(muted).Truncate(time.Second).String())
		return
	}

	// 冲动值门限：被 @ 或主人说话时无条件放行，其余按阈值
	if !atMe && !fromMaster && impulse < cfg.Brain.ImpulseThreshold {
		logx.InfoCat(logx.CatDecision, "跳过：冲动值不足",
			"group", groupLabel(g), "冲动", fmt.Sprintf("%.2f", impulse),
			"阈值", fmt.Sprintf("%.2f", cfg.Brain.ImpulseThreshold),
			"差", fmt.Sprintf("%.2f", cfg.Brain.ImpulseThreshold-impulse),
			"构成", strings.Join(reasons, "+"), "本批条数", newCount)
		return
	}

	// 刷屏硬闸：整批只有表情、一个字都没有，且没有任何明确叫它的理由。
	// 光靠冲动值还不够——攒批窗口里只要混进一条别的消息，阈值就被顶过去了，
	// 于是「连发八个表情 + 顺带一句闲聊」还是会插嘴。这里是最后一道。
	if faceSpam && !atMe && !fromMaster {
		logx.InfoCat(logx.CatDecision, "跳过：整批只有表情，没人在跟它说话",
			"group", groupLabel(g), "条数", newCount,
			"冲动", fmt.Sprintf("%.2f", impulse))
		return
	}

	// 在线时段：非 @ 的闲聊按当前时段概率放行。
	// 这是省钱主力——白天（非半价）只留两成在线率，@ 与宽限期内不受影响。
	if !atMe && !fromMaster && !strings.HasPrefix(groupID, c2cPrefix) {
		d := ScheduleDecide(cfg.Schedule, time.Now(), false, false, g.SinceAtHit())
		if !d.Allowed {
			logx.InfoCat(logx.CatDecision, "跳过：本次摇骰子没上线",
				"group", groupLabel(g), "摇到", fmt.Sprintf("%.3f", d.Roll),
				"在线率", fmt.Sprintf("%.2f", d.Rate), "档位", d.Label)
			return
		}
	}

	// 最小发言间隔：防止它自己连着刷屏
	minGap := time.Duration(cfg.Brain.MinSpeakIntervalSec) * time.Second
	if d := g.SinceLastSpeak(); d < minGap && !atMe {
		logx.InfoCat(logx.CatDecision, "跳过：距上次发言太近",
			"group", groupLabel(g), "距上次", d.Truncate(time.Second).String(),
			"要求", minGap.String(), "还差", (minGap - d).Truncate(time.Second).String())
		return
	}

	// 预算：超了就只对主人和被 @ 的情况放行
	if !e.allowCall(cfg, atMe, fromMaster) {
		logx.WarnCat(logx.CatDecision, "跳过：今日预算已用尽",
			"group", groupLabel(g), "预算", cfg.Brain.DailyBudget)
		return
	}

	// 视频/语音处理放在所有闸门之后：冲动值不够、预算用尽、静默期都不该为它花流量。
	// 用独立 ctx 而不是 decide 的 60s——下载+ffmpeg+转写可能要几十秒，
	// 不能挤占模型调用的超时预算。
	var videoFrames []string
	var mediaNote string
	if len(videos) > 0 || len(voices) > 0 {
		vctx, vcancel := context.WithTimeout(context.Background(), videoPipelineTimeout)
		if len(voices) > 0 {
			mediaNote = transcribeVoices(vctx, cfg, voices)
		}
		if len(videos) > 0 {
			vres := handleVideos(vctx, cfg, videos)
			videoFrames = vres.frames
			mediaNote += vres.note
		}
		vcancel()
	}

	trigger := buildTrigger(atMe, nameCalled, fromMaster, question, replyToBot, newCount, triggerName, atAll)
	// 话头不在自己身上时，把这件事如实告诉模型。只靠压低冲动值不够：
	// 冲动值过了门限之后模型看到的是「有人刚在群里说了话」，
	// 很容易当成有人在跟它说话而插嘴。2026-10-01 实况就是这样抢了一句。
	if atOthers {
		trigger += "；这批里有人在明确 @ 别人，那段对话不是跟你说的"
	}
	e.decide(cfg, g, trigger, triggerOpenID, triggerName, atMe, fromMaster, reasons, impulse,
		images, imgSpamNote, imgWhoNote, quoted, quotedAuthor, videoFrames, mediaNote, eager)
}

// quoteNote 生成给模型看的「谁引用了谁的什么」说明，没有引用时返回空串。
//
// 三个信息缺一不可——只给原文的话，模型会把被引用的内容当成「刚才有人在群里说的」，
// 对象就错了；群友甩一段聊天记录让你看的时候尤其容易错。
//
// **点名原作者是刚性要求**，不是锦上添花。2026-10-02 实况：引用别人发的图问
// 「这谁？」，少了这句话模型就认定是引用者自己发的，答了「你自己发的你问我？」。
func quoteNote(who, author, quoted string) string {
	q := strings.TrimSpace(quoted)
	a := strings.TrimSpace(author)
	if q == "" && a == "" {
		return ""
	}
	who = orDefault(who, "刚才那个人")
	// 原作者认不出来时，**绝不能把「平台没告诉我」写进提示词**。
	// 那句话是在教模型自曝：它会照着说「我这边看不到是谁发的」。
	// 2026-10-02 实况就是这么答的——「你引用的那条我这儿看不见，截图发出来」。
	//
	// 群里每个人都能看见那条引用，模型看不见是它自己的事，不该转述。
	// 这里只描述「有一条引用、内容是什么」，让它按内容正常反应；
	// 万一它真的需要提，就当不知道发送者——那是正常人的状态（没注意、记不清）。
	if a == "" {
		return fmt.Sprintf("%s引用了一条消息，原文是：「%s」。", who, truncate(q, 100))
	}
	return fmt.Sprintf(
		"%s引用了%s之前说的，原文是：「%s」。他可能是在回应这句话，也可能只是顺手引一下。",
		who, a, truncate(q, 100))
}

// allowCall 预算闸门
func (e *Engine) allowCall(cfg config.Config, atMe, fromMaster bool) bool {
	e.statMu.Lock()
	defer e.statMu.Unlock()
	e.rollDayLocked()
	if cfg.Brain.DailyBudget <= 0 {
		return true
	}
	if e.stat.Calls < cfg.Brain.DailyBudget {
		return true
	}
	// 超预算后只保留最必要的应答，避免彻底装死显得掉线
	return atMe || fromMaster
}

func (e *Engine) rollDayLocked() {
	today := time.Now().Format("2006-01-02")
	if e.stat.Day != today {
		e.stat = DailyStat{Day: today}
		logx.Info("已进入新的一天，用量统计归零", "day", today)
	}
}

// decide 构造提示词 → 调模型 → 解析 → 发送
func (e *Engine) decide(cfg config.Config, g *memory.Group, trigger, triggerOpenID, triggerName string, atMe, fromMaster bool, reasons []string, impulse float64, images []string, imgSpamNote, imgWhoNote, quoted, quotedAuthor string, videoFrames []string, mediaNote string, eager bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now()
	// 时间每次现取；日子和天气只在换日后刷一次（内部有日期判断）
	e.refreshEnv(now, cfg)

	// persona.max_chars 在 parse 包里是个进程级开关（parse.go 里 SetMaxChars），
	// 每轮从当前配置推一次，配置热重载后立刻生效。
	SetMaxChars(cfg.Persona.MaxChars)

	// 情绪检测前置：免费小模型自己判断气氛的能力不稳，这里先兜一道。
	// 必须带上说话人：systemPrompt 的动态段会写「他刚才的话像是真的要伤害自己」，
	// 扫的文本里只有内容没有名字的话，模型根本不知道「他」是谁。
	recent := g.Recent(cfg.Brain.MaxHistory)
	if len(recent) > 12 {
		recent = recent[len(recent)-12:]
	}
	var scan strings.Builder
	for _, l := range recent {
		if l.Role == memory.RoleUser && l.Name != "" {
			scan.WriteString(l.Name)
			scan.WriteString("：")
		}
		scan.WriteString(l.Content)
		scan.WriteString("\n")
	}
	mood := Detect(scan.String())

	masterHint := ""
	if fromMaster {
		// 用本轮实际发言者的名字，不用配置里的昵称常量：
		// 群昵称随时能改，配置里的昵称只会让模型把别人认成主人。
		masterHint = fmt.Sprintf("刚刚叫你的这个人叫%s（你在配置里记住的开发者昵称是%s），给他面子。",
			orDefault(triggerName, "不知道"), orDefault(cfg.Master.Nickname, "你开发者"))
	}

	system := systemPrompt(cfg, g, mood, masterHint, composeEnvLine(now, e.envSnapshot(), cfg.Brain.WeatherPlace), triggerName)

	// 上下文裁剪：按最小可用模型上下文来算预算，保证换到小模型也不会炸
	minCtx := 0
	for _, ep := range cfg.LLM.Endpoints {
		if !ep.Enabled {
			continue
		}
		for _, m := range ep.Models {
			if !m.Enabled || m.MaxCtx <= 0 {
				continue
			}
			if minCtx == 0 || m.MaxCtx < minCtx {
				minCtx = m.MaxCtx
			}
		}
	}
	// 输出预留必须跟「实际会发出去的 max_out」对齐。
	// cfg.Brain.MaxOutTokens 只是调用方的意向，llm.Call 会在调用方没给时才用它；
	// 一旦路由侧的目标配了更大的上限，按旧值预留就会把上下文撑爆（context_length_exceeded）。
	budget := ContextBudget(cfg.Brain.MaxCtxTokens, minCtx, e.router.MaxOutTokens())
	lines := TrimHistory(g.Recent(cfg.Brain.MaxHistory), budget)

	// 图片：先按配置封顶，再抓成 data URI（抓的过程中按需压缩）。
	// 抓失败就当没这张图，一张图挂掉不值得让整轮决策失败——
	// 但要在提示词里告诉它有人发了图，否则它会一脸茫然。
	maxImg := cfg.Brain.MaxImagesPerCall
	wantImages := len(images) > 0 && maxImg > 0 && hasVisionModel(cfg)
	var notes []string
	if imgSpamNote != "" {
		notes = append(notes, imgSpamNote)
	}
	if imgWhoNote != "" {
		notes = append(notes, imgWhoNote)
	}
	if mediaNote != "" {
		notes = append(notes, mediaNote)
	}
	if n := quoteNote(triggerName, quotedAuthor, quoted); n != "" {
		notes = append(notes, n)
	}
	var dataURLs []string
	// 视频帧排最前面：发视频的人，视频就是话题本体，
	// 宁可挤掉普通图片的名额也要保住它。
	if len(videoFrames) > 0 {
		dataURLs = append(dataURLs, videoFrames...)
	}
	if len(images) > 0 {
		// 这三条都只是「这轮没图」，措辞必须和 quoteNote 同一原则：
		// 不告诉模型「你看不到」，更不能让它转述成「我看不见」。
		// 群里每个人都可能没注意到图，它要表现得像没注意，而不是像没能力。
		if !wantImages {
			notes = append(notes, fmt.Sprintf(
				"%s发了 %d 张图片/表情包。", orDefault(triggerName, "有人"), len(images)))
		} else if imgBudget := maxImg - len(videoFrames); imgBudget <= 0 {
			notes = append(notes, fmt.Sprintf(
				"%s发了 %d 张图片/表情包，你这轮先顾视频。",
				orDefault(triggerName, "有人"), len(images)))
		} else {
			imgGot := e.fetchImages(images, imgBudget, cfg.Brain.ImageMaxSide)
			dataURLs = append(dataURLs, imgGot...)
			if len(imgGot) == 0 {
				notes = append(notes, "有人发了图片/表情包。")
			} else if len(imgGot) < len(images) {
				notes = append(notes, fmt.Sprintf("一共 %d 张图，这里只给你看了 %d 张。", len(images), len(imgGot)))
			}
		}
	}
	// 有池子的时候才告诉它「序号」怎么算：收图这件事只有池子开着才有意义，
	// 没有它就别在上下文里多塞一句模型用不上的规则。
	if len(dataURLs) > 0 && e.poolOf(cfg) != nil {
		notes = append(notes, fmt.Sprintf(
			"这轮给你看了 %d 张图，序号 1 到 %d（按你看到的顺序）。看到有梗的，用 collect 报给我。", len(dataURLs), len(dataURLs)))
	}

	user := userPrompt(cfg, g, lines, trigger, strings.Join(notes, ""))

	start := time.Now()
	res, rounds, err := e.chatWithTools(ctx, cfg, system, user, dataURLs, g)

	// 记账必须跟着每一次实际调用走。原来只有一次 Chat，统计写在固定位置；
	// 现在循环里每次调用都要记，否则多轮的工具调用会漏记——
	// 漏记的直接后果是每日预算形同虚设。
	e.statMu.Lock()
	e.rollDayLocked()
	e.stat.Calls += rounds
	if err != nil {
		e.stat.LastErr = err.Error()
		e.statMu.Unlock()
		logx.Warn("决策调用失败", "group", groupLabel(g), "err", err.Error(),
			"cost_ms", time.Since(start).Milliseconds(), "轮数", rounds)
		return
	}
	e.stat.LastModel = res.Model
	e.stat.LastAt = now.Format("15:04:05")
	e.stat.LastErr = ""
	e.statMu.Unlock()

	dec, issue := ParseDecision(res.Content)

	switch {
	case issue == ParseNoise:
		// 剥完标签剩下的还是协议残渣（比如模型被 max_tokens 截断在 `<json` 上），
		// 历史上这里会把字面量 `<json` 当发言发进真人群，必须留一条可见的日志。
		logx.Warn("模型输出是协议残渣，已拦下不发", "group", groupLabel(g), "raw", truncate(res.Content, 80))
	case issue == ParseFallback && dec.Act == "say":
		// 拿不到 JSON 时把原文当发言，风险是它可能把内心 OS 也发出去了
		logx.Warn("未解析到合法 JSON，已降级为直接发言", "group", groupLabel(g), "raw", truncate(res.Content, 60))
	}

	// 记住它想记住的东西
	for _, m := range dec.Memo {
		g.SetFact(m.K, m.V)
	}

	// 交付内容：blocks 优先，没有就回落 text。
	// 归一化在 allowBlocks 里做——空块、非法类型、超预算全在那里拦。
	//
	// 必须在日志之前算：这一轮到底发没发出去，取决于它，判据就是这里。
	blocks, hasContent := allowBlocks(dec.Blocks, dec.Text, cfg.Speak.MaxSegments)

	// **一次模型调用只落一条决策日志。**
	//
	// 过去这里是两条：「决策已得出」记模型的原始 act，紧接着
	// 「决策：模型选择闭嘴」再记一遍。两行 os 逐字相同、理由相同，
	// 平铺在日志里就是「一次思考出了两个结论」，看着像 bug——用户为此
	// 反复来问是不是同一轮记了两遍。
	//
	// act 记模型的原始判断（say/quiet，那是模型的决定，不该被后处理改写），
	// 结果记这一轮最终发没发出去（两者可以不一致：模型说 say 但内容全被
	// allowBlocks 拦下时 act=say、结果=无内容，这是最需要被看见的那种情况，
	// 过去它在日志里完全看不出来）。
	outcome := "发言"
	switch {
	case dec.Act != "say":
		outcome = "闭嘴"
	case !hasContent:
		outcome = "无内容"
	}
	// os 是模型自己写的内心活动（不发送）。它能让你看出它当时在想什么——
	// 「懒得理他」和「怕说错」是完全不同的两种闭嘴。
	logx.InfoCat(logx.CatDecision, "决策已得出",
		"group", groupLabel(g), "act", dec.Act, "结果", outcome,
		"model", res.Model, "os", truncate(dec.OS, 40),
		"冲动", fmt.Sprintf("%.2f", impulse), "理由", strings.Join(reasons, "+"),
		"耗时ms", time.Since(start).Milliseconds(),
		"解析", string(issue), "轮数", rounds)

	if dec.Act != "say" || !hasContent {
		// 收图与发不发言无关：闭嘴那轮照样可能看见一张值得留的梗图。
		e.collectMemes(cfg, dec.Collect, dataURLs)
		e.statMu.Lock()
		e.stat.Quiet++
		e.statMu.Unlock()
		return
	}

	// 回给谁：模型填的 to 优先（它知道自己在回谁），填不出来就用本轮的触发者。
	// 两者都拿不到才退到「挂群里最新那条」，那是最容易挂错人的兜底。
	replyToOpenID := e.resolveReplyTarget(g, cfg, dec.To, triggerOpenID)

	sent := e.deliver(cfg, g, blocks, replyToOpenID, eager)

	// 放在发完之后：收图要走一次 MinIO 上传，挂在发消息前面只会让群里多等。
	e.collectMemes(cfg, dec.Collect, dataURLs)

	// 统计只认真正送达的条数：Said 原来在发送之前自增，
	// 一条都没发出去的轮次照样计数，管理端展示的那个数字是虚高的。
	if sent > 0 {
		e.statMu.Lock()
		e.stat.Said += sent
		e.statMu.Unlock()
	}

	// 滚动摘要异步跑：本轮的回复已经发出去了，摘要慢一点不影响体验
	go e.maybeSummarize(cfg, g)
}

// chatWithTools 跑决策调用，模型可以先调工具取上下文，再给出最终答案。
//
// **工具轮绝不发送任何消息**，这是整套设计的地基：
// 工具轮发出去的东西在最终轮失败时无法回滚，重试就会重复发言。
// 内容和发送顺序一律由最终轮决定，所以失败重试是幂等的。
//
// 轮次上限不是「正常流程要走几轮」，而是「发癫了要拦在哪」：
// 没有 function_call 就是交付轮。达到上限时按当前拿到的内容交付，
// 而不是静默丢弃——那会表现为「它明明该说话却突然哑了」。
func (e *Engine) chatWithTools(ctx context.Context, cfg config.Config,
	system, user string, images []string, g *memory.Group) (*llm.Result, int, error) {

	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user, Images: images},
	}
	tools := e.toolDefs(g)
	maxRounds := cfg.MemePool.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = 2
	}

	rounds := 0
	for {
		res, cerr := e.router.Chat(ctx, llm.Request{
			Messages:    msgs,
			NeedsVision: len(images) > 0,
			Tools:       tools,
			Temperature: cfg.Brain.Temperature,
			MaxTokens:   cfg.Brain.MaxOutTokens,
		})
		rounds++
		e.recordTokens(res, system, user)
		if cerr != nil {
			return nil, rounds, cerr
		}
		// 没有工具调用 = 交付轮
		if len(res.ToolCalls) == 0 {
			return res, rounds, nil
		}
		if rounds >= maxRounds {
			logx.Warn("工具轮次用尽，按当前内容交付", "group", groupLabel(g),
				"轮数", rounds, "上限", maxRounds)
			return res, rounds, nil
		}
		// 把模型发起的那次调用原样回传，再追加结果。
		// 不回传的话它下一轮看不到自己调过什么，会重复调同一个工具。
		for _, tc := range res.ToolCalls {
			result := e.runTool(ctx, cfg, g, tc)
			msgs = append(msgs, llm.Message{
				Role: llm.RoleAssistant, CallID: tc.ID, ToolName: tc.Name,
				Content: tc.Arguments,
			}, llm.Message{
				Role: llm.RoleTool, CallID: tc.ID, Content: result,
			})
		}
		logx.Debug("工具轮完成", "group", groupLabel(g), "轮数", rounds, "调用数", len(res.ToolCalls))
	}
}

// recordTokens 把一次调用的用量记进统计。
func (e *Engine) recordTokens(res *llm.Result, system, user string) {
	if res == nil {
		return
	}
	promptTok := res.PromptTokens
	outTok := res.OutputTokens
	if promptTok <= 0 {
		promptTok = EstimateTokens(system) + EstimateTokens(user)
	}
	if outTok <= 0 {
		outTok = EstimateTokens(res.Content)
	}
	e.statMu.Lock()
	e.stat.PromptTokens += int64(promptTok)
	e.stat.OutputTokens += int64(outTok)
	e.statMu.Unlock()
}

// resolveReplyTarget 决定这条回复挂在谁的消息下。
//
// 三级：模型填的 to（昵称，需在群成员里能对上）→ 本轮触发者 → 空（发送层挂最新那条）。
// 昵称匹配不上很正常（改名、表情包昵称、模型自己写错），所以必须优雅退级，
// 绝不能因为匹配失败就把整个决策丢掉。
func (e *Engine) resolveReplyTarget(g *memory.Group, cfg config.Config, to, triggerOpenID string) string {
	to = strings.TrimSpace(to)
	if to == "" {
		return triggerOpenID
	}
	// 模型偶尔会带 @ 前缀或把昵称写全了
	to = strings.TrimPrefix(to, "@")
	if oid, ok := lookupMemberOpenID(g, to); ok {
		return oid
	}
	return triggerOpenID
}

// lookupMemberOpenID 按称呼在群成员里找 openid。
//
// 与渲染共用 personTokens 这张表——这是刻意的：
// 模型看见什么名字，我们就要能反查回同一个人，反查表和渲染表必须是同一份，
// 否则「模型照抄的名字反查不到」就等于又退回「挂最新那条」。
//
// 匹配分三级，越精确的优先：
//  1. 整个 to 就是一个称呼（可带 @ 和引号）
//  2. to 里完整包含某个称呼，取**最长的**那个匹配
//     ——必须取最长：群里有「张三」和「张三丰」时，说「张三丰」不能匹配到「张三」
//     单字称呼和纯符号称呼不参与这一级（见下面两处 continue）
//  3. 不做模糊匹配。宁可匹配不上退回触发者，也不要猜：
//     猜错就是把回复挂给一个不相干的人，比挂给触发者糟糕得多。
func lookupMemberOpenID(g *memory.Group, to string) (string, bool) {
	to = strings.TrimSpace(strings.Trim(strings.TrimSpace(to), "「」『』\"'【】"))
	to = strings.TrimPrefix(to, "@")
	to = strings.TrimSpace(to)
	if to == "" {
		return "", false
	}
	tokenToOpenID, _ := personTokens(g)

	if oid, ok := tokenToOpenID[to]; ok {
		return oid, true
	}

	// 包含匹配：取最长的命中，长度相同再按 openid 定序，保证结果稳定。
	best := ""
	bestOpenID := ""
	for token, oid := range tokenToOpenID {
		if len([]rune(token)) < 2 {
			// 单字昵称（「6」「好」）在句子里撞上的概率太高，命中等于乱挂
			continue
		}
		// 纯符号昵称同样不能参与包含匹配。生产实况（2026-10-02）：
		// 测试群一号 有一位群友的**昵称和群名片都叫「...」**。
		// 「...」有 3 个 rune，躲得过上面那道长度闸，于是任何带三个点的 to
		// 都会命中他——模型填「笑死...」这种顺口的收尾，回复就挂给了
		// 一个不相干的人。精确匹配不受影响：模型真的填「...」时照样能对上。
		//
		// 判据用 hasWord（至少有一个字母或数字）而不是「是不是标点」：
		// 颜文字、纯 emoji 昵称也一并挡掉，它们在句中的偶然命中率同样很高。
		if !hasWord(token) {
			continue
		}
		if !strings.Contains(to, token) {
			continue
		}
		if len(token) > len(best) || (len(token) == len(best) && oid < bestOpenID) {
			best, bestOpenID = token, oid
		}
	}
	if best != "" {
		return bestOpenID, true
	}
	return "", false
}

// hasVisionModel 当前是否至少有一个启用的多模态模型。
// 一个都没有时直接放弃带图，否则上游必然 400，白烧一次调用。
func hasVisionModel(cfg config.Config) bool {
	for _, ep := range cfg.LLM.Endpoints {
		if !ep.Enabled {
			continue
		}
		for _, m := range ep.Models {
			if m.Enabled && m.Vision {
				return true
			}
		}
	}
	return false
}

// fetchImages 把图片 URL 抓成 data URI（顺带压缩），最多取 limit 张。
// 单张失败跳过：群里的图经常是过期链接或超大截图。
//
// 失败记 **Warn 而不是 Debug**：生产环境跑的是 Info 级别（没设 QQPAL_LOG_LEVEL），
// Debug 在那里等于不存在。2026-10-02 那次「机器人说看不见图」就是这么查不下去的——
// 日志里干干净净，什么都看不出来，只能靠猜。抓图失败是**用户可见的功能缺陷**
// （模型确实少看了东西），必须留下痕迹。
func (e *Engine) fetchImages(urls []string, limit, maxSide int) []string {
	out := make([]string, 0, limit)
	for _, u := range urls {
		if len(out) >= limit {
			break
		}
		d, err := imgproc.FetchAsDataURL(u, maxSide)
		if err != nil {
			logx.Warn("图片抓取失败，跳过", "url", truncate(u, 60), "err", err.Error())
			continue
		}
		out = append(out, d)
	}
	return out
}

// maybeSummarize 把「更早之前的聊天」压成一段提要，替换掉会越喂越长的原始历史。
//
// 为什么必须有这一步：滑动窗口只能保住最近几十条，超出的部分会被直接丢掉，
// 群聊久了它就变成只有七秒记忆的人。但把全量历史塞进 prompt 又会烧穿预算、
// 撞上模型上下文上限。折中办法是定期把旧段落压成几句话——
// 一次摘要大约几百 token，换掉的是后续每轮都少喂几千 token。
func (e *Engine) maybeSummarize(cfg config.Config, g *memory.Group) {
	every := cfg.Brain.SummaryEvery
	if every <= 0 {
		return
	}
	if g.PendingSummary() < every {
		return
	}
	if !e.allowCall(cfg, false, false) {
		return
	}

	lines := g.Recent(cfg.Brain.MaxHistory)
	if len(lines) < every {
		return
	}
	// 留出最近这几条不做摘要：它们还在滑动窗口里，下一轮会以原文形式进上下文
	keepTail := every / 3
	if keepTail < 4 {
		keepTail = 4
	}
	if len(lines) <= keepTail {
		return
	}
	old := lines[:len(lines)-keepTail]

	var sb strings.Builder
	for _, l := range old {
		sb.WriteString(renderOneLine(l))
		sb.WriteString("\n")
	}
	prev := g.Summary()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	req := "把下面这段群聊记录压缩成 3~5 句「前文提要」，只写给后面读它的人看：谁说了什么、聊到哪了、有什么梗或结论。用口语，不要编号，不要评价。"
	if prev != "" {
		req += "\n\n之前的提要：\n" + prev + "\n\n（把新内容并进去，仍然控制在 5 句以内）"
	}
	req += "\n\n群聊记录：\n" + sb.String()

	e.statMu.Lock()
	e.rollDayLocked()
	e.stat.Calls++
	e.statMu.Unlock()

	res, err := e.router.Chat(ctx, llm.Request{
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: req}},
		Temperature: 0.3,
		MaxTokens:   300,
	})
	if err != nil {
		e.statMu.Lock()
		e.stat.LastErr = err.Error()
		e.statMu.Unlock()
		logx.Warn("摘要失败", "group", groupLabel(g), "err", err.Error())
		return
	}
	summary := strings.TrimSpace(res.Content)
	if summary == "" {
		return
	}
	if len([]rune(summary)) > 400 {
		summary = string([]rune(summary)[:400])
	}
	e.statMu.Lock()
	e.stat.PromptTokens += int64(res.PromptTokens)
	e.stat.OutputTokens += int64(res.OutputTokens)
	e.statMu.Unlock()
	g.SetSummary(summary)
	logx.Info("已更新前文提要", "group", groupLabel(g), "长度", len([]rune(summary)))
}

// renderOneLine 渲染单条历史（与 renderLines 同格式，供摘要用）。
// 摘要是唯一会被长期保留下来的压缩物，所以说话人必须留在里面——
// 丢了名字，剩下的提要就读不出「谁说的」，模型照样会认错人。
func renderOneLine(l memory.Line) string {
	switch l.Role {
	case memory.RoleBot:
		return "· 你：" + sanitizeChatText(l.Content)
	case memory.RoleNote:
		return "· （系统提示）" + sanitizeChatText(l.Content)
	default:
		name := l.Name
		if name == "" {
			name = "某人"
		}
		return "· " + sanitizeChatText(name) + "：" + sanitizeChatText(l.Content)
	}
}

// deliver 按 blocks 的顺序逐条发出，返回**真正送达**的条数。
//
// 三件事必须以「送达」为准，不能以「尝试」为准：
//   - 记忆里的「你刚刚说过」——写没送出去的内容，模型下一轮会以为自己说过了
//   - 连续发言计数——按失败的轮次累加，它会误以为群里没人在理它而一直闭嘴
//   - 管理端展示的 Said——历史上它在发送之前就自增，虚高得离谱
//
// 文字和表情包**共用**「同一条用户消息最多回 5 次」的额度，
// 所以 planDelivery 会把总条数压到上限内——超了会在那里截断，
// 不能指望平台替我们拦（那表现为「话说一半」）。
//
// replyToOpenID 决定这些消息挂在谁的名下，空则退化为「挂群里最新那条」。
func (e *Engine) deliver(cfg config.Config, g *memory.Group, blocks []Block, replyToOpenID string, eager bool) int {
	plan := planDelivery(blocks, cfg.Speak.MaxSegChars, passiveMaxSends)
	if len(plan) == 0 {
		logx.Info("清理后没有可发内容，放弃发言", "group", groupLabel(g),
			"原文", truncate(textOf(blocks), 60))
		g.MarkAttempted()
		return 0
	}
	ctx := context.Background()

	// 第一条之前的「想了一下」
	lead := time.Duration(cfg.Speak.FirstDelayMS) * time.Millisecond
	if lead <= 0 {
		lead = 600 * time.Millisecond
	}
	lead += time.Duration(rand.Intn(400)) * time.Millisecond
	time.Sleep(lead)

	var sentTexts []string
	sent := 0
	imgUsed := 0
	maxImg := cfg.MemePool.MaxImagesPerReply
	if maxImg <= 0 {
		maxImg = 1
	}

	for i, b := range plan {
		if i > 0 {
			prev := plan[i-1]
			time.Sleep(segDelay(cfg, prev.C, eager, i, len(plan)))
		}
		var err error
		switch b.T {
		case BlockTypeImg:
			if imgUsed >= maxImg {
				// 配置说一条最多发几张就几张。超了不是错误——
				// 模型想多发点很正常，不值得打断它。
				continue
			}
			err = e.sendOneImage(ctx, g, b.ID, replyToOpenID)
			if err == nil {
				imgUsed++
				sent++
			}
		default:
			err = e.sender.SendGroupTo(ctx, g.OpenID, b.C, replyToOpenID)
			if err == nil {
				sentTexts = append(sentTexts, b.C)
				sent++
			}
		}
		if err != nil {
			logx.Error("发言发送失败", "group", groupLabel(g),
				"类型", b.T, "内容", truncate(b.C, 40), "err", err.Error())
			// 单条失败不中断：让它把剩下的说完，比戛然而止自然
		}
	}

	if sent == 0 {
		// 一条都没发出去也要推进状态，否则下一条消息进来会立刻再打一次，
		// 变成对着一个必然失败的接口反复烧钱（历史上真实发生过 76 次连续失败）。
		g.MarkAttempted()
		return 0
	}

	// 记忆里只留文字。图片另起一条留痕——
	// 不留的话模型下一轮不知道自己发过图，会在同一段对话里反复甩同一张。
	g.MarkBotSpoke(strings.Join(sentTexts, " "))
	if imgUsed > 0 {
		g.Append(memory.Line{
			TS: time.Now(), Role: memory.RoleBot,
			Content: "（发了个表情包）",
		}, 50)
	}

	// 群名和收件人都打昵称，不打 openid。
	// openid 是内部主键，日志里看到一串 32 位十六进制毫无意义——
	// 而「这条回复到底挂给了谁」恰恰是排查认错人时最想确认的一件事，
	// 看到「回给 群友甲」比看到一串十六进制有用得多。
	toName := replyToOpenID
	if replyToOpenID != "" {
		if n := g.NameOfByOpenID(replyToOpenID); n != "" {
			toName = n
		} else {
			toName = shortOpenID(replyToOpenID)
		}
	}
	logx.InfoCat(logx.CatSpeak, "已发言",
		"group", groupLabel(g), "条数", sent, "图", imgUsed,
		"回给", toName, "内容", truncate(textOf(plan), 80), "急", eager)
	return sent
}

// sendOneImage 发一张表情包。
//
// 池子不可用或发送失败都只记日志不返错——
// 表情包是锦上添花，它挂了就该退化成纯文本，而不是让整轮回复泡汤。
func (e *Engine) sendOneImage(ctx context.Context, g *memory.Group, id int64, replyToOpenID string) error {
	if e.memes == nil || e.imgSender == nil {
		return nil
	}
	raw, mime, err := e.memes.Data(id)
	if err != nil {
		logx.Warn("表情包取不到，跳过", "group", groupLabel(g), "id", id, "err", err.Error())
		return nil
	}
	if err := e.imgSender.SendImage(ctx, g.OpenID, raw, mime, replyToOpenID); err != nil {
		logx.Warn("表情包发送失败，退化为纯文本", "group", groupLabel(g),
			"id", id, "err", err.Error())
		return nil
	}
	e.memes.MarkUsed(id, time.Now())
	return nil
}

// passiveMaxSends QQ 的硬限制：同一条用户消息最多回 5 次，
// 文字和图片共用。这里独立成常量是因为它属于平台约束而非可调项。
const passiveMaxSends = 5

// speak 是纯文本路径的薄封装，保留给既有调用与测试。
// speak 是纯文本路径的薄封装，保留给既有调用与测试。
//
// eager 传 true：它只被测试当「立刻回一句」用，传 true 语义更贴。
func (e *Engine) speak(cfg config.Config, g *memory.Group, text, replyToOpenID string) int {
	return e.deliver(cfg, g, textOnlyBlocks(text), replyToOpenID, true)
}

// groupLabel 日志里显示的群名。
//
// 群 openid 是 32 位十六进制，日志里看到它对排查毫无帮助——
// 群名配置里有就用人看的名字（agent 那边一直是这么打的），
// 没配过才退回 openid，至少还能和回调日志对上。
func groupLabel(g *memory.Group) string {
	if g == nil {
		return ""
	}
	return orDefault(g.Name, g.OpenID)
}

// segDelay 计算两条之间的间隔：基础的犹豫时间 + 按上一条字数估算的「打字时间」。
//
// 这是**唯一肉眼可辨的节奏信号**。首字延迟（FirstDelayMS）在本项目里意义有限——
// 攒批窗口 8 秒加 LLM 调用（实测均延迟 4.4 秒）决定了首字最快也在 12 秒后，
// 那几百毫秒淹没在里面。条与条之间的间隔才是「像不像人在打字」的判据。
//
// eager：被 @ / 被叫名字 / 主人说话时群友在等，整体提前（见 SpeakConfig.EagerScale）。
//
// idx/total：第几条到第几条（共 total 条）。真人连着发消息是越说越快的——
// 前面慎重、后面连发。固定间隔下 5 条 × 2.6 秒 = 13 秒，观感是「一条一条
// 慢慢爬」，那比秒回更像机器人，因为它规律得不自然。递减系数（TailRamp）
// 消掉这个规律性。
func segDelay(cfg config.Config, prev string, eager bool, idx, total int) time.Duration {
	base := time.Duration(cfg.Speak.MinDelayMS) * time.Millisecond
	if base <= 0 {
		base = 300 * time.Millisecond
	}
	max := time.Duration(cfg.Speak.MaxDelayMS) * time.Millisecond
	if max < base {
		max = base
	}
	span := max - base
	d := base
	if span > 0 {
		d += time.Duration(rand.Int63n(int64(span)))
	}
	if cfg.Speak.PerCharMS > 0 {
		d += time.Duration(len([]rune(prev))*cfg.Speak.PerCharMS) * time.Millisecond
	}
	if eager {
		d = scaleDelay(d, cfg.Speak.EagerScale)
	}
	d = rampDelay(d, cfg.Speak.TailRamp, idx, total)

	// 上限从配置推导，而不是写死。
	//
	// 原来写死 6 秒是配「每字 45ms」那套参数调的：36 字 × 45ms = 1.6 秒，
	// 加 1.1 秒上限也就 2.7 秒，6 秒根本碰不到，纯属摆设。
	// 换成真人速度（每字 150ms）后合法最长间隔变成 36×150 + 2600 = 8 秒，
	// 写死的 6 秒就会在**最该慢的场合**（发一条长消息）把它砍掉，
	// 而那正是「像人在认真打字」最明显的时刻。
	cap := max + time.Duration(len([]rune(prev))*cfg.Speak.PerCharMS)*time.Millisecond
	if cap < base {
		cap = base
	}
	if d > cap {
		d = cap
	}
	return d
}

// scaleDelay 按系数整体缩放间隔。
func scaleDelay(d time.Duration, scale float64) time.Duration {
	if scale <= 0 {
		scale = 0.5
	}
	return time.Duration(float64(d) * scale)
}

// rampDelay 多条连发时让间隔递减：真人后面几句越说越急。
//
// **递减本身也带随机**，这是关键：递减只是**倾向**，不是规律。
// 真人发消息时「越来越急」是体感，具体第 3 条等了多久每次都不一样。
// 如果递减系数是确定的（idx=3 就固定 ×0.46），那么摇出来的随机值上
// 再乘一个固定系数，整条曲线仍然是可预测的——而「可预测」正是
// 机器人感的来源。所以这里对系数本身再摇一次随机。
//
// 下限 minRampFloor：完全递减到 0 会让最后两条同一毫秒发出去，
// 平台会把它们合并成一条，那模型本来想分开发的东西（一条文字 + 一张图）就没了。
func rampDelay(d time.Duration, ramp float64, idx, total int) time.Duration {
	if ramp <= 0 || total <= 1 || idx <= 0 {
		return d
	}
	const minRampFloor = 0.25
	// 按名义系数先摇一次，再让实际系数在它上下浮动 ±40%：
	// 偶尔第 3 条比第 2 条还慢一点（真人也会这样），
	// 但整体仍然是「越说越急」的趋势。
	target := 1 - ramp*float64(idx)
	if target < minRampFloor {
		target = minRampFloor
	}
	spread := target * rampRandomSpread
	factor := target + (rand.Float64()*2-1)*spread
	if factor < minRampFloor {
		factor = minRampFloor
	}
	if factor > 1 {
		factor = 1
	}
	return time.Duration(float64(d) * factor)
}

// rampRandomSpread 递减系数的随机幅度。
// 0.4 = 在名义值上下浮动四成。太大则「越说越急」的规律消失，
// 太小则随机性形同虚设。
const rampRandomSpread = 0.4

// Stats 返回当日用量统计（副本）
func (e *Engine) Stats() DailyStat {
	e.statMu.Lock()
	defer e.statMu.Unlock()
	e.rollDayLocked()
	return e.stat
}

// handleBind 处理认主口令。
//
// 安全语义：主人只有第一个绑上的人。口令是明文发在群里的，
// 任何翻聊天记录的人都能拿到，所以「已有主人时一律拒绝并回骂」，
// 口令错误也回骂——被冒认的企图本身就该怼回去。
func (e *Engine) handleBind(cfg config.Config, ev *Event) {
	fields := strings.Fields(ev.Content)
	if len(fields) < 2 || cfg.Master.BindToken == "" {
		return
	}
	if fields[1] != cfg.Master.BindToken {
		logx.Warn("认主口令不正确", "group", ev.GroupID, "from", ev.Name)
		e.replyOne(ev.GroupID, ev.OpenID, "口令都不对还想认主？")
		return
	}
	if cfg.IsMaster(ev.OpenID) {
		return
	}
	if len(cfg.Master.OpenIDs) > 0 {
		logx.Warn("已有人绑定主人，拒绝新的绑定", "group", ev.GroupID, "from", ev.Name, "openid", ev.OpenID)
		e.replyOne(ev.GroupID, ev.OpenID, "主人已经有人当了，轮不到你")
		return
	}
	if _, err := e.store.BindMaster(ev.OpenID); err != nil {
		logx.Error("绑定主人失败", "err", err.Error())
		return
	}
	logx.Info("已绑定主人", "openid", ev.OpenID, "name", ev.Name)
	e.replyOne(ev.GroupID, ev.OpenID, "记住你了")
}

// replyOne 快速回一句单条消息（用于认主这类固定应答）。
// 挂到 ev.OpenID 名下——口令是这个人敲的，回复也该挂在他那条消息下。
func (e *Engine) replyOne(groupID, replyToOpenID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.sender.SendGroupTo(ctx, groupID, text, replyToOpenID); err != nil {
		// 群名优先。这里没法用 engine 里的 g（这是独立的一次性应答），
		// 所以现查一次记忆表拿名字
		logx.Error("固定应答发送失败", "group", groupLabel(e.mem.Group(groupID, "")), "err", err.Error())
	}
}

// looksLikeReplyToBot 粗判：机器人刚说过话，紧接着有人发言，就算在接它的话
func (e *Engine) looksLikeReplyToBot(g *memory.Group, content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	d := g.SinceLastSpeak()
	return d > 0 && d < 3*time.Minute
}

// buildTrigger 生成给模型看的「这次为什么叫你」。
//
// who 是本轮触发者的昵称——必须报出名字，不能只说「有人」。
// 以前这里全是「有人 @ 了你」「有人在问问题」，模型只能自己从上下文里猜
// 是在回谁，攒批窗口里好几个人各说各的时就必然猜错。
func buildTrigger(atMe, nameCalled, fromMaster, question, replyToBot bool, newCount int, who string, atAll bool) string {
	who = strings.TrimSpace(who)
	if who == "" {
		who = "有人"
	}
	var parts []string
	switch {
	case atMe:
		parts = append(parts, who+" @ 了你，在直接跟你说话")
	case nameCalled:
		parts = append(parts, who+"叫了你的名字")
	case fromMaster:
		parts = append(parts, who+"是你的开发者，他说话了")
	case question:
		parts = append(parts, who+"在问问题")
	case replyToBot:
		parts = append(parts, who+"在接你刚才的话")
	default:
		parts = append(parts, who+"刚在群里说了话")
	}
	if newCount > 1 {
		// 同时说清这批里还有谁，否则模型会把别人的话也当成对它的提问
		parts = append(parts, fmt.Sprintf("这批一共 %d 条新消息，可能不止一个人在说话，注意分清每句是谁说的", newCount))
	}
	// @全体成员 如实说明，但必须点破「他喊的是所有人」——
	// 不点破的话，模型会以为这条是在跟它说话，然后用「你自己@all不就完了」
	// 这种把系统标记当话术的蠢话回复（2026-10-01 群里真实发生过）。
	if atAll {
		parts = append(parts, who+"刚才 @ 了全体成员，是在喊所有人，不是专门在跟你说话")
	}
	return strings.Join(parts, "；")
}

// mentionsName 判断是否提到了它的名字
func mentionsName(text, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return strings.Contains(text, name)
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
