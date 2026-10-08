// Package agent 是事件入口：把 QQ 回调事件归一化后交给 brain 决策，
// 并负责把决策结果真正发出去。
//
// 这一层刻意保持很薄——所有「要不要说话、说什么、分几条发」的判断都在 brain 里，
// 这里只做协议适配（事件结构 → 内部 Event）和发送通道。
package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dadyumo/internal/brain"
	"dadyumo/internal/config"
	"dadyumo/internal/logx"
	"dadyumo/internal/memory"
	"dadyumo/internal/qqapi"
	"dadyumo/internal/webhook"
)

// c2cPrefix 单聊会话在内部也当成一个「群」来处理，用前缀区分
const c2cPrefix = "c2c:"

// Agent 实现 webhook.Handler 与 brain.Sender
type Agent struct {
	store  *config.Store
	qq     *qqapi.Client
	mem    *memory.Store
	engine *brain.Engine

	mu       sync.Mutex
	lastText map[string]string // 会话 -> 上次发出的内容，用于兜底去重
}

// New 创建 Agent
func New(store *config.Store, qq *qqapi.Client, mem *memory.Store, engine *brain.Engine) *Agent {
	return &Agent{
		store:    store,
		qq:       qq,
		mem:      mem,
		engine:   engine,
		lastText: map[string]string{},
	}
}

// SetEngine 回填决策引擎。
// Agent 既是 engine 的发送通道、又要调用 engine，构造上是个环，只能分两步装配。
func (a *Agent) SetEngine(e *brain.Engine) { a.engine = e }

// OnGroupMessage 处理群消息
func (a *Agent) OnGroupMessage(ev *webhook.GroupMessage, atMe bool) {
	if ev == nil || ev.Author == nil {
		return
	}
	cfg := a.store.Get()

	groupID := ev.GroupOpenID
	if groupID == "" {
		return
	}
	// 群名回调里没有，只能从配置里认；认不出来就用 openid 的尾号，日志里好分辨
	groupName := groupNameOf(cfg, groupID)
	if !groupEnabled(cfg, groupID) {
		logx.Debug("该群未启用，已忽略", "group", groupID)
		return
	}

	openID := ev.Author.MemberOpenID
	if openID == "" {
		openID = ev.Author.UserOpenID
	}
	if openID == "" {
		openID = ev.Author.ID
	}
	name := displayName(ev.Author.Username, openID)

	content := normalizeContent(ev)
	rawContent := content // 清洗前的平台原文，只用于日志对照
	mentionTarget := ""

	// 群主开启「全量消息」后，@机器人 也不再走 GROUP_AT_MESSAGE_CREATE 事件，
	// 而是混在 GROUP_MESSAGE_CREATE 里。判断「这条在跟谁说话」有两个来源：
	//
	//  1. mentions 数组——平台唯一结构化的「消息 @ 了谁」信号，最可靠。
	//     顺带把被 @ 到的其他群友登记进成员表：这是白得的「谁在群里叫什么」。
	//  2. content 里的 <@openid> 标签——平台只承诺剥掉 @机器人 的前缀，
	//     对 @他人 的残留格式没有任何承诺，只能当兜底。
	mentionsMe, others := parseMentions(ev.Mentions, cfg.QQ.SelfOpenID)
	if len(others) > 0 {
		g := a.mem.Group(groupID, groupName)
		for oid, nm := range others {
			g.TouchMemberCard(oid, displayName(nm, oid))
		}
		// 取「第一个被 @ 的人」时必须按平台给的顺序，不能遍历 others 这个 map——
		// Go 的 map 遍历顺序是随机的，一条 @ 了两个人的消息，
		// 上一轮回给 A、这一轮可能就回给 B 了。这里走 ev.Mentions 的原始下标。
		for _, u := range ev.Mentions {
			if u == nil {
				continue
			}
			oid := u.MemberOpenID
			if oid == "" {
				oid = u.UserOpenID
			}
			if oid == "" {
				oid = u.ID
			}
			if oid != "" && oid != cfg.QQ.SelfOpenID {
				if _, ok := others[oid]; ok {
					mentionTarget = oid
					break
				}
			}
		}
	}
	// mentions 是唯一可靠的「@了谁」，但只覆盖结构化字段。正文里的
	// <@openid>/<@all>/表情标记是平台塞进来的原文，必须翻译掉再往下走，
	// 否则模型看到的是 32 位十六进制和 base64，认不出人是谁、也读不出表情。
	// openid → 昵称。机器人自己映射成人设名，@到的群友用 mentions 里的
	// username；认不出来的返回空，cleanTags 会把那个标签整个删掉。
	nameOf := func(oid string) string {
		if oid == "" {
			return ""
		}
		if oid == cfg.QQ.SelfOpenID {
			return cfg.Persona.Name
		}
		if nm, ok := others[oid]; ok {
			return displayName(nm, oid)
		}
		return ""
	}
	content, atAll := cleanTags(content, nameOf)
	// @全体成员 平台既不给 mentions 条目、也不置 at=true，只在正文留一个
	// <@all> 标签。它包含机器人，但「群里有人 @ 全员」通常不是在问它，
	// 所以不当成 atMe——只把这个事实如实记进上下文，让模型自己判断要不要接。
	if mentionsMe && !atMe {
		atMe = true
	}
	// 机器人没被 @、但这条明确 @ 了别人：被 @ 的那个人才是这轮该回的对象，
	// 比「最后一条说话的人」准得多（群里常常是 A 在说、顺手 @ B 让他答）。
	// 若 @ 的正是机器人自己，回的是说话人而不是被 @ 的人，所以要排除。
	if !atMe && mentionTarget != "" && mentionTarget == cfg.QQ.SelfOpenID {
		mentionTarget = ""
	}

	logx.InfoCat(logx.CatChat, "群消息", buildGroupLogFields(groupName, name, atMe,
		content, rawContent, atAll, mentionTarget, nameOf,
		quotedPreview(ev.MsgElements))...)

	// 能收到这个群的消息说明机器人还在群里：清掉可能的「已退群」旧标记
	//（比如被移出后又被拉回来，但重新入群事件丢失的场景）
	if g := a.mem.Group(groupID, groupName); g != nil {
		if left, _ := g.Left(); left {
			g.SetLeft(false)
			logx.Info("群内再次收到消息，清除已退群标记", "group", groupName)
		}
	}

	// 只有「是机器人 且 openid 就是我自己」才算自己发的，才不进触发逻辑
	//（避免自我回复循环）。
	//
	// 原来只判 ev.Author.Bot。单人时成立——平台不会把自己的消息推回给自己，
	// 所以群里出现的 bot 消息必然是自己发的。同群跑第二个机器人（老爹/奶酱）
	// 之后这个假设就塌了：对方的每条消息 author.bot 也是 true，于是被记成
	// RoleBot，在 renderLines 里渲染成「· 你：」，模型把对方说的话当成自己说过
	// 的（连带 LastText 的复读抑制也作用到别人头上）。
	// 2026-10-08 生产实证：老爹日志里近 3 天有 80 条 username=羽沫奶酱 且
	// bot=true 的回调。
	//
	// 别的机器人一律按普通群友走：登记成员（TouchMember 用 author.username
	// 写昵称，管理端不再是「(无名)」）、记成 RoleUser 让它渲染成「· 名字：」，
	// 并且和真人一样能触发。
	//
	// 2026-10-08 补：触发权**降频**了，见 brain 的 PeerBotReplyRate。原来的
	// 「不做抑制」让两台形成自激回路——老爹 6 小时 60 条发言里 31 条是回奶酱的，
	// 群里每 5.6 分钟就有一条机器人在回另一个机器人。现在对方仍能触发，
	// 但要过一道与在线率串联的概率闸；被 @ / 被叫名字照旧无条件放行。
	if ev.Author.Bot && openID == cfg.QQ.SelfOpenID {
		a.mem.Group(groupID, groupName).Append(memory.Line{
			TS: parseTS(ev.Timestamp), Role: memory.RoleBot, Content: content, OpenID: openID,
		}, cfg.Brain.MaxHistory)
		return
	}

	// 说话人登记进成员表，用的是 **author 给的账号昵称**。
	//
	// 这一步以前没有，于是成员表里只剩群名片（只有被别人 @ 时才拿得到），
	// 主名是「群名片丁」而不是他自称的「群友乙」。
	// author 和 mentions 是平台两个不同字段给的两种名字，语义不同，
	// 必须分开写：见 Member.Card 的说明。
	a.mem.Group(groupID, groupName).TouchMember(openID, name)

	// 身份也是平台给的：author.member_role（member/admin/owner）。
	// 以前一路丢在 webhook 结构体里没人读，于是成员表里没有任何身份信息，
	// 「艾特一下群主」对模型就成了无解的题（2026-10-05 生产实况：
	// 模型答「不知道谁是群主」，还因为 JSON 残缺整条被按闭嘴处理）。
	if ev.Author.MemberRole != "" {
		a.mem.Group(groupID, groupName).TouchMemberRole(openID, ev.Author.MemberRole)
	}

	// 只有真人的消息才能作为被动回复的锚点。
	// 必须带上发送者：回复挂到谁的消息下，决定了群里看起来是在跟谁说话。
	// refIdx 是这条消息的引用 id（平台的 REFIDX_xxx==），有了它回复才能
	// 发成真正的引用气泡而不只是被动挂靠。拿不到时留空即可。
	a.qq.Anchors().Add(groupID, ev.ID, openID, name, refIdxOf(ev.MessageScene))

	// 被 @ 之外，直接叫名字也算在跟它说话
	if !atMe {
		atMe = mentions(content, cfg.Persona.Name)
	}

	imgs, quotedText, quotedPic, quotedOpenID, quotedName := parsePics(ev)

	// 每条收到的消息都按它自己的引用 id 记一笔：别人以后引用它时，
	// 回调里只带这个 id，作者和图片都得从这里反查。详见 qqapi.QuoteIndex。
	a.qq.Quotes().Add(groupID, refIdxOf(ev.MessageScene), qqapi.QuoteSrc{
		OpenID: openID, Name: name, Text: content, Images: imgs,
	})

	quotedText, quotedPic, quotedName, quotedOpenID = resolveQuoted(
		a.qq.Quotes(), groupID, refMsgIdxOf(ev.MessageScene),
		quotedText, quotedPic, quotedName, quotedOpenID)

	// 上面那行「群消息」日志里的「引用」字段是在反查**之前**打的，只反映平台给了什么。
	// 反查命中没有、补回了谁的图和话，只能靠这一条看——不记的话这个功能
	// 在日志里完全隐形，出了错只能靠模型说了什么去猜。
	if refMsgIdxOf(ev.MessageScene) != "" {
		who := quotedName
		if who == "" {
			who = "（认不出，平台没给作者）"
		}
		logx.InfoCat(logx.CatChat, "引用解析", "group", groupName, "引用者", name,
			"被引用者", who, "内容", truncate(quotedText, 60), "图", len(quotedPic))
	}

	// 视频/语音/引用都要记住是谁发的：模型要能说「这是谁发的」，
	// 发送层也要能把这轮回复挂回这个人。
	videos := make([]brain.MediaRef, 0, len(ev.Attachments))
	for _, u := range videoURLs(ev.Attachments) {
		videos = append(videos, brain.MediaRef{OpenID: openID, Name: name, URL: u})
	}
	voices := voiceRefs(ev.Attachments, openID, name)

	a.engine.OnMessage(&brain.Event{
		GroupID:            groupID,
		GroupName:          groupName,
		MsgID:              ev.ID,
		OpenID:             openID,
		Name:               name,
		Content:            content,
		Images:             imgs,
		QuotedPics:         quotedPic,
		Videos:             videos,
		Voices:             voices,
		Quoted:             quotedText,
		QuotedFrom:         name,
		QuotedFromOpenID:   openID,
		QuotedAuthor:       quotedName,
		QuotedAuthorOpenID: quotedOpenID,
		MentionTarget:      mentionTarget,
		AtAll:              atAll,
		AtMe:               atMe,
		IsBot:              false,
		// 走到这里还能是 bot 的，只可能是**同群的另一台机器人**：
		// 自己发的在上面那个分支就 return 了。置这个标记不是为了拦下它，
		// 而是让 brain 的 PeerBotReplyRate 闸能认出「这轮是对方把话头递过来」。
		IsPeerBot: ev.Author.Bot,
		TS:        parseTS(ev.Timestamp),
	})
}

// parsePics 从一条回调里把「这条消息自己带的图」和「被引用的那条消息里的图」
// 分成两路，顺带把引用文本和原作者带出来。
//
// **图必须分开，不能合并。** 下游只按「这条消息的发送人」给 Images 记账，
// 把引用的图混进去就等于告诉模型「这是他发的」——可他只是引用了别人。
// 2026-10-02 实况：有人引用别人半小时前发的一张图问「这谁？」，
// 机器人答「你自己发的你问我？」。
//
// 引用里的图归**被引用的原作者**（authorOpenID/authorName）。
// 原作者认不出来就留空，下游会显示成「有人」——宁可说不出是谁，
// 也不能赖到引用的人头上。
//
// 合成一个函数而不是拆成 extractQuoted + splitPics 两步：拆开时测试只能
// 直接调工具函数，调用点那句「谁传给谁」没人管——把合并改回去测试照样全绿
// （这个假绿真踩过一次）。
func parsePics(ev *webhook.GroupMessage) (imgs []string, quotedText string,
	quotedPics []brain.MediaRef, authorOpenID, authorName string) {

	quotedText, quotedImgs, oid, nm := extractQuoted(ev.MsgElements)
	authorOpenID, authorName = oid, nm
	imgs = imageURLs(ev.Attachments)
	quotedPics = make([]brain.MediaRef, 0, len(quotedImgs))
	for _, u := range quotedImgs {
		quotedPics = append(quotedPics, brain.MediaRef{OpenID: oid, Name: nm, URL: u})
	}
	return imgs, quotedText, quotedPics, authorOpenID, authorName
}

// imageURLs 从事件附件里挑出图片类附件的下载地址。
// 表情包、截图、照片在平台侧都是附件，contentType 以 image/ 开头。
func imageURLs(atts []*webhook.Attachment) []string {
	if len(atts) == 0 {
		return nil
	}
	out := make([]string, 0, len(atts))
	for _, a := range atts {
		if a == nil {
			continue
		}
		// 有些事件只给 contentType，有些只给文件名，两边都认
		isImg := strings.HasPrefix(strings.ToLower(a.ContentType), "image/")
		if !isImg {
			ext := strings.ToLower(filepath.Ext(a.FileName))
			isImg = ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".bmp"
		}
		if !isImg {
			continue
		}
		if a.URL != "" {
			out = append(out, a.URL)
		}
	}
	return out
}

// videoURLs 从事件附件里挑出视频附件的下载地址。
// 平台给视频的 contentType 形如 video/mp4；有的事件不带 contentType 只带文件名，
// 所以和图片一样两边都认。引用消息里的视频不收——引用链上的媒体本来就少见，
// 转写一份视频的成本又高，不值得。
func videoURLs(atts []*webhook.Attachment) []string {
	if len(atts) == 0 {
		return nil
	}
	out := make([]string, 0, len(atts))
	for _, a := range atts {
		if a == nil {
			continue
		}
		isVideo := strings.HasPrefix(strings.ToLower(a.ContentType), "video/")
		if !isVideo {
			ext := strings.ToLower(filepath.Ext(a.FileName))
			isVideo = ext == ".mp4" || ext == ".mov" || ext == ".avi" || ext == ".mkv" || ext == ".webm"
		}
		if !isVideo {
			continue
		}
		if a.URL != "" {
			out = append(out, a.URL)
		}
	}
	return out
}

// voiceRefs 挑出平台没给转写文本、但留了音频下载地址的语音消息，交给 ASR 兜底。
// 平台给了 asr_refer_text 的语音已经在 normalizeContent 里拼进正文了，这里不管。
// 转写不在这里做：回调是同步派发的，阻塞几秒做转写会拖慢回包被平台重推，
// 真正的转写攒到决策引擎 fire() 时才发生。
func voiceRefs(atts []*webhook.Attachment, senderOpenID, sender string) []brain.VoiceRef {
	if len(atts) == 0 {
		return nil
	}
	var out []brain.VoiceRef
	for _, a := range atts {
		if a == nil || a.ContentType != "voice" {
			continue
		}
		if strings.TrimSpace(a.ASRReferText) != "" || a.VoiceWavURL == "" {
			continue
		}
		out = append(out, brain.VoiceRef{OpenID: senderOpenID, Name: sender, URL: a.VoiceWavURL})
	}
	return out
}

// buildGroupLogFields 拼出「群消息」这行日志的 kv。
//
// 原文和清洗结果一起打：只看清洗后的没法判断是模型认错了还是这里解析错了，
// 只看原文又看不出模型到底读到了什么。控制台日志页把两栏上下排着，
// 出现平台标记（说明清洗漏了）或两边长度差很多，都一眼看得见。
//
// 单独抽成函数是为了能直接测：OnGroupMessage 依赖真实 qqapi 客户端做锚点登记，
// 为了验一行日志去起整个 Agent 不划算。
func buildGroupLogFields(group, from string, atMe bool,
	content, rawContent string, atAll bool, mentionTarget string,
	nameOf func(string) string, quoted ...string) []any {

	fields := []any{
		"group", group, "from", from, "at", atMe,
		"text", truncate(content, 200),
	}
	if rawContent != content {
		fields = append(fields, "原文", truncate(rawContent, 200))
	}
	if atAll {
		fields = append(fields, "@全体", true)
	}
	if mentionTarget != "" {
		if n := nameOf(mentionTarget); n != "" {
			fields = append(fields, "@给", n)
		}
	}
	if len(quoted) > 0 && quoted[0] != "" {
		fields = append(fields, "引用", truncate(quoted[0], 80))
	}
	return fields
}

// quotedPreview 生成一行给人看的引用摘要：谁引用了谁的什么话。
//
// 排查「回复挂错人」时最缺的就是这个：日志里只看到「某人发了条消息」，
// 看不到他引用的是谁。历史上认错人的案子有一大半是引用链没读对。
// 只给日志用，不进模型上下文（模型那边走 Event.Quoted 等结构化字段）。
func quotedPreview(els []*webhook.MsgElement) string {
	text, _, authorOpenID, authorName := extractQuoted(els)
	if text == "" {
		return ""
	}
	who := authorName
	if who == "" {
		who = shortID(authorOpenID)
	}
	if who == "" {
		return text
	}
	return who + "：「" + text + "」"
}

// maxQuotedChars 引用文本最多带多少字进上下文。引用通常是半句话的上下文，
// 但也可能有人引用一大段聊天记录——那对决策没什么用，纯粹烧 token。
const maxQuotedChars = 120

// extractQuoted 从 msg_elements 里把「被引用的内容」抠出来。
//
// 平台把引用/回复包装在消息元素里（可能还套聊天记录，所以递归走），
// 文本在 Content，图片在 Attachments，**被引用那条消息的作者在 Author**。
// 作者必须一起取出来：只给文本的话，模型会把被引用的内容当成「刚才有人在群里
// 说的」，于是回错了人——这在群友甩一段聊天记录出来让你看的时候最容易发生。
//
// 拿不到就返回空——引用只是辅助上下文，解析失败不值得让消息本身处理失败。
// 被引用那条的正文拿不到时用的占位。
//
// 三者都是「知道有这么个引用、但内容不完整」，区别只在缺哪一半。
// quotedUnknown 尤其要注意措辞：**不能写成「平台没给我」**——
// 那句话会教模型自曝（brain 的 quoteNote 里有详述）。
// 这里只陈述「被引用的是别人发的一条消息」，让它别把引用内容当成引用者自己发的。
const (
	quotedPicOnly = "（一张图）"
	quotedNoText  = "（一条没有文字的消息）"
	quotedUnknown = "（别人发的消息，内容未知）"
)

func extractQuoted(els []*webhook.MsgElement) (text string, imgs []string, authorOpenID, authorName string) {
	var texts []string
	var authors []string
	// 原始正文非空、清洗后变空 = 那条消息里装的是平台标记（最常见是单个表情）。
	// 用来区分「被引用的是个表情」和「这个元素里什么都没有」——
	// 后者如实报空，前者才敢说「一个表情」，不能凭空猜。
	rawNonEmpty := false
	var walk func(list []*webhook.MsgElement)
	walk = func(list []*webhook.MsgElement) {
		for _, el := range list {
			if el == nil {
				continue
			}
			// 引用里的正文**也要过 cleanTags**。它和这条消息自己的正文走的是
			// 同一个平台通道，标记格式完全一样——引用别人一个表情，元素里
			// 装的就是一整串 <faceType=6,faceId="0",ext="base64..."/>。
			// 以前只清洗本条消息的正文（OnGroupMessage 里那次），
			// 引用里的原样透给模型，模型看到的是一坨协议残渣。
			// 2026-10-02 实况：有人引用别人发的表情问「这条是谁发的？」，
			// 机器人答「你引用的那条我这儿看不见，截图发出来」——
			// 它不是看不见，是收到了一坨看不懂的 base64。
			raw := strings.TrimSpace(el.Content)
			c, _ := cleanTags(el.Content, nil)
			if c != "" {
				texts = append(texts, c)
			}
			if raw != "" {
				rawNonEmpty = true
			}
			if el.Author != nil {
				oid := el.Author.MemberOpenID
				if oid == "" {
					oid = el.Author.UserOpenID
				}
				if oid == "" {
					oid = el.Author.ID
				}
				nm := displayName(el.Author.Username, oid)
				// 只记第一个有名字的作者：多数引用只带一条，套聊天记录时后续作者多半是同一个人
				if oid != "" && authorOpenID == "" {
					authorOpenID, authorName = oid, nm
				}
				if nm != "" {
					authors = append(authors, nm)
				}
			}
			imgs = append(imgs, imageURLs(el.Attachments)...)
			walk(el.MsgElements)
		}
	}
	walk(els)
	if len(imgs) > 4 {
		imgs = imgs[:4]
	}
	if len(authors) > 1 {
		// 多个不同作者时把名字都带上，模型才知道这是一段对话的摘录
		authorName = strings.Join(dedupeStrings(authors), "、")
	}
	text = strings.Join(texts, "；")
	// 被引用的那条**可能一个字的正文都没有**——群里甩一张图过来问「这谁？」
	// 就是这种。这里必须给个非空占位，不能返回空串：
	// 调用方拿「文本为空」当「这条消息没有引用」，于是原作者是谁、
	// 「引用」这个动作本身，整段一起丢掉。模型只看到一张来路不明的图，
	// 2026-10-02 实况：它对着别人半小时前发的图说「你自己发的你问我？」。
	// 有元素才谈得上「引用」。els 为空是**根本没有引用**，
	// 这时给占位等于凭空造一段引用，测试和平时的普通消息都会中招。
	if len(els) > 0 && text == "" {
		switch {
		case len(imgs) > 0:
			text = quotedPicOnly
		case authorOpenID != "":
			text = quotedNoText
		case rawNonEmpty:
			// 原始正文非空、cleanTags 之后空了：正文里装的是平台标记，
			// 而标记没匹配上 faceTagRe（平台改了格式）。
			// 此时**不编**具体是什么——编成「表情」或「表情包」都是在猜，
			// 猜错会让模型按错的类型理解上下文。如实说「内容无法解析」，
			// 配合 quoteNote 里「平台没告诉我这条是谁发的」，它会如实说不确定。
			text = "（内容无法解析）"
		}
	}
	return text, imgs, authorOpenID, authorName
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// OnC2CMessage 处理单聊消息
func (a *Agent) OnC2CMessage(ev *webhook.C2CMessage) {
	if ev == nil || ev.Author == nil {
		return
	}

	openID := ev.Author.UserOpenID
	if openID == "" {
		openID = ev.Author.ID
	}
	if openID == "" {
		return
	}
	content := strings.TrimSpace(ev.Content)
	if content == "" {
		return
	}
	logx.Info("单聊消息", "from", openID, "text", truncate(content, 60))

	// 单聊不登记锚点：SendC2C 根本不查锚点池（主动通道已下线），
	// 登记了既没人用，又让 c2c:* 的键在 AnchorPool 里只增不减。
	a.engine.OnMessage(&brain.Event{
		GroupID:   c2cPrefix + openID,
		GroupName: "单聊",
		MsgID:     ev.ID,
		OpenID:    openID,
		Name:      displayName(ev.Author.Username, openID),
		Content:   content,
		AtMe:      true, // 单聊里每句话都是在跟它说话
		TS:        parseTS(ev.Timestamp),
	})
}

// OnGroupRobotEvent 处理机器人进出群、群主开关全量消息
func (a *Agent) OnGroupRobotEvent(eventType string, ev *webhook.GroupRobotEvent) {
	if ev == nil {
		return
	}
	switch eventType {
	case webhook.EventGroupMsgRecv:
		logx.Info("群主已开启全量消息", "group", ev.GroupOpenID)
	case webhook.EventGroupMsgReject:
		logx.Info("群主已关闭全量消息，后续只能收到 @ 消息", "group", ev.GroupOpenID)
	case webhook.EventGroupAddRobot:
		logx.Info("机器人被加入群", "group", ev.GroupOpenID)
		a.mem.Group(ev.GroupOpenID, "").SetLeft(false)
	case webhook.EventGroupDelRobot:
		logx.Info("机器人被移出群", "group", ev.GroupOpenID)
		// 平台没有群列表查询接口，这个事件是唯一能拿到的「已退群」信号，
		// 标记后管理端把它标灰，由人确认后手动移除
		a.mem.Group(ev.GroupOpenID, "").SetLeft(true)
	}
}

// SendGroup 实现 brain.Sender。内部按前缀分流到群消息 / 单聊。
// 发送成功后立刻把自己的话写回记忆——否则它下一轮会忘了自己说过什么。
func (a *Agent) SendGroup(ctx context.Context, sessionID, content string) error {
	return a.SendGroupTo(ctx, sessionID, content, "")
}

// SendGroupTo 实现 brain.Sender，replyToOpenID 指定这条回复要挂在谁的消息下。
//
// 这是「群里看起来在跟谁说话」的唯一决定点。replyToOpenID 为空时退化成
// 「挂到群里最新那条」，那在多人同时说话时必然有一部分挂错人。
func (a *Agent) SendGroupTo(ctx context.Context, sessionID, content, replyToOpenID string) error {
	return a.sendGroup(ctx, sessionID, content, replyToOpenID, false)
}

// SendGroupQuote 实现 brain.Sender：这条以真正的引用气泡发出去。
//
// 单聊没有引用气泡可言，直接按普通发送处理。
func (a *Agent) SendGroupQuote(ctx context.Context, sessionID, content, replyToOpenID string) error {
	return a.sendGroup(ctx, sessionID, content, replyToOpenID, true)
}

func (a *Agent) sendGroup(ctx context.Context, sessionID, content, replyToOpenID string, quote bool) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	var err error
	if strings.HasPrefix(sessionID, c2cPrefix) {
		err = a.qq.SendC2C(ctx, strings.TrimPrefix(sessionID, c2cPrefix), content)
	} else if quote {
		err = a.qq.SendGroupQuote(ctx, sessionID, content, replyToOpenID)
	} else {
		err = a.qq.SendGroupTo(ctx, sessionID, content, replyToOpenID)
	}
	if err != nil {
		return err
	}
	a.recordSent(sessionID, content)
	return nil
}

// recordSent 发送成功后的公共收尾：去重检查 + 写回自己的记忆
func (a *Agent) recordSent(sessionID, content string) {
	cfg := a.store.Get()
	a.mu.Lock()
	dup := a.lastText[sessionID] == content
	a.lastText[sessionID] = content
	a.mu.Unlock()
	if dup {
		logx.Warn("检测到重复发言", "session", sessionID, "text", truncate(content, 40))
	}
	a.mem.Group(sessionID, "").Append(memory.Line{
		TS: time.Now(), Role: memory.RoleBot, Content: content,
	}, cfg.Brain.MaxHistory)
}

// normalizeContent 把回调里的各种消息元素拼成一段可读文本。
// 纯文本之外的东西只留占位：便宜的模型大多不支持视觉输入，
// 硬塞图片 URL 只会白白烧 token，还可能让它对着链接胡说八道。
func normalizeContent(ev *webhook.GroupMessage) string {
	var sb strings.Builder
	text := strings.TrimSpace(ev.Content)
	if text != "" {
		sb.WriteString(text)
	}
	for _, at := range ev.Attachments {
		if at == nil {
			continue
		}
		switch {
		case strings.HasPrefix(at.ContentType, "image"):
			appendPart(&sb, "[图片]")
		case at.ContentType == "voice":
			// 语音优先用平台给的 ASR 文本，没有就只能标个占位
			if strings.TrimSpace(at.ASRReferText) != "" {
				appendPart(&sb, "[语音]"+strings.TrimSpace(at.ASRReferText))
			} else {
				appendPart(&sb, "[语音]")
			}
		case strings.HasPrefix(at.ContentType, "video"):
			appendPart(&sb, "[视频]")
		default:
			appendPart(&sb, "[文件]")
		}
	}
	return strings.TrimSpace(sb.String())
}

func appendPart(sb *strings.Builder, s string) {
	if sb.Len() > 0 {
		sb.WriteString(" ")
	}
	sb.WriteString(s)
}

// resolveQuoted 决定「被引用的那条」最终长什么样。
//
// 平台给的引用回调里**永远没有作者**，图片则时有时无
// （2026-10-08 实测一天 91 条含引用的回调：带被引用消息 author 的 0 条，
// 带被引用图片附件的只有 3 条，而且那 3 条同样没有作者）。所以先拿本机索引反查：
// 那条只要本机见过——收到过，或者就是自己发出去的——原作者、原文、图片
// 就全都能补回来，图片还带得回 CDN 地址直接喂给视觉模型。
//
// 反查不到时**不能就这么算了**：至少要把「这是一条引用别人的消息」报出去。
// 以前这里什么都不报，模型只看到一句「肉不肉麻啊……」，于是把被引用的图
// 当成引用者自己发的。
//
// 独立成一个函数而不是写在 OnGroupMessage 里，是为了能直接测这段判断：
// 它的几种结局在 OnGroupMessage 里构造不出来（那需要一个真的 qqapi.Client）。
// parsePics 当年因为拆开测而假绿过一次，所以这里连调用点一起测。
func resolveQuoted(idx *qqapi.QuoteIndex, groupID, ref, platformText string,
	platformPics []brain.MediaRef, authorName, authorOpenID string) (
	text string, pics []brain.MediaRef, name, openID string) {

	text, pics, name, openID = platformText, platformPics, authorName, authorOpenID
	if ref == "" {
		return // 这条消息没有引用，一个字都不许造
	}
	if src, ok := idx.Lookup(groupID, ref); ok {
		name, openID = src.Name, src.OpenID
		pics = make([]brain.MediaRef, 0, len(src.Images))
		for _, u := range src.Images {
			pics = append(pics, brain.MediaRef{OpenID: src.OpenID, Name: src.Name, URL: u})
		}
		if len(pics) == 0 {
			// 索引那条没存到图（收的时候附件没解析出来之类），而平台这次给了。
			// 图不能丢，但归属要改成索引里的原作者——平台给的那份没有作者，
			// 留着会让下游显示成「有人」，那比不知道还糟。
			for _, p := range platformPics {
				pics = append(pics, brain.MediaRef{OpenID: src.OpenID, Name: src.Name, URL: p.URL})
			}
		}
		switch {
		case src.Text != "":
			text = src.Text
		case len(src.Images) > 0:
			text = quotedPicOnly
		default:
			text = quotedNoText
		}
	}
	if text == "" {
		// 走到这里说明「确实是引用，但正文一个字都没有」。图还在就只说图，
		// 图也没有才落到那个最含糊的占位——但绝不能留空：
		// 留空等于告诉下游「这条消息没有引用」，整段一起丢。
		if len(pics) > 0 {
			text = quotedPicOnly
		} else {
			text = quotedUnknown
		}
	}
	return
}

// mentions 判断文本里是否叫了它的名字

// refIdxOf 从事件的 message_scene.ext 里取出这条消息的引用 id。
//
// 官方文档说 message_reference 填的 message_id 要从 `message_scene.ext` 取；
// webhook 那边把 ext 声明成 []string，说明它不是一个 JSON 对象而是若干个串。
//
// **它长什么样，2026-10-05 才在生产原文里看到过**（此前是猜的，猜错了两年）：
//
//	"message_scene":{"source":"default","ext":[
//	  "msg_idx=REFIDX_up/YiEUb8…ko51hdSHY",
//	  "auth_token=X-qanh…"]}
//
// ext 是若干个 key=value。我们要的是 `msg_idx=` 的**值**，也就是 `REFIDX_…`。
//
// 此前那两条规则（`HasPrefix(REFIDX)` 且 `HasSuffix("==")`）对着这个真实样本
// **同时落空**：前缀撞在 `msg_idx=` 上，结尾也没有 base64 补位（base64 补位
// 有 `==`/`=`/无 三种）。于是 message_reference 从来没被填出去过，
// 引用气泡一次都没发出来，而日志里 `引用:false` 看着像「平台没给 id」。
//
// 两种形式都认：裸的 `REFIDX_…`，以及 `msg_idx=` 带前缀的（后者是实测的形态）。
// 认不出就返回空：那只是这条回复不带引用气泡，绝不影响它发出去。
//
// 取「我引用了哪一条」用的是 refMsgIdxOf，两个 key 必须分开认——理由见那里。
func refIdxOf(scene *webhook.MessageScene) string { return extRefIdx(scene, "msg_idx=") }

// refMsgIdxOf 取这条消息**引用了哪一条**（被引用那条的引用 id）。
//
// 平台只给这个不透明的 id，作者和内容一个字都不给（实测，见 qqapi.QuoteIndex），
// 所以它唯一的用途是拿去 QuoteIndex 反查本机见过的那条消息。
//
// 与 refIdxOf 必须是两个函数、不能合成一个「哪个 key 都认」的：
// ext 里 `ref_msg_idx=…` 和 `msg_idx=…` 常常同时出现（引用别人的消息就同时有），
// 认错了就会把「我引用了谁」当成「我是谁」，发送层拿它填 message_reference
// 会让机器人引用到自己头上。
func refMsgIdxOf(scene *webhook.MessageScene) string { return extRefIdx(scene, "ref_msg_idx=") }

func extRefIdx(scene *webhook.MessageScene, key string) string {
	if scene == nil {
		return ""
	}
	for _, item := range scene.Ext {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		if val, ok := strings.CutPrefix(s, key); ok {
			s = strings.TrimSpace(val)
		} else if strings.Contains(s, "=") && !strings.HasPrefix(s, "REFIDX") {
			continue // ext 里别的 key=value（auth_token=…），不是引用 id
		}
		// 含空白的是被换行/截断拆开的残片，填进 message_reference 会被平台拒
		if strings.HasPrefix(s, "REFIDX") && s != "REFIDX" && !strings.ContainsAny(s, " \t\n") {
			return s
		}
	}
	return ""
}

// mentions 判断文本里是否叫了它的名字
func mentions(text, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return strings.Contains(text, name)
}

// parseMentions 解析平台给的 mentions 数组。
//
// 返回值：机器人自己是否被 @、以及这条消息 @ 到的其他人（openid → 昵称）。
//
// 为什么必须用它而不是解析 content：官方文档只承诺「content 已去除 @机器人 的前缀」，
// 对 @他人 在正文里残留成什么格式没有任何承诺，写正则去抠 openid 属于赌平台行为。
// mentions 是唯一结构化的信号。
//
// 注意 mentions 里可能出现机器人的 member_openid 或 user_openid（两个字段平台都可能填），
// 所以两个都要比。
func parseMentions(ms []*webhook.User, selfOpenID string) (mentionsMe bool, others map[string]string) {
	others = map[string]string{}
	selfOpenID = strings.TrimSpace(selfOpenID)
	for _, u := range ms {
		if u == nil {
			continue
		}
		oid := u.MemberOpenID
		if oid == "" {
			oid = u.UserOpenID
		}
		if oid == "" {
			oid = u.ID
		}
		if oid == "" {
			continue
		}
		if selfOpenID != "" && (oid == selfOpenID || u.ID == selfOpenID) {
			mentionsMe = true
			continue
		}
		others[oid] = u.Username
	}
	return mentionsMe, others
}

func groupNameOf(cfg config.Config, groupID string) string {
	for _, g := range cfg.Groups {
		if g.OpenID == groupID && g.Name != "" {
			return g.Name
		}
	}
	return shortID(groupID)
}

// groupEnabled 群白名单。配置里没列出的群默认是允许的——
// 否则每加一个群都要改配置重启，太笨重。
func groupEnabled(cfg config.Config, groupID string) bool {
	for _, g := range cfg.Groups {
		if g.OpenID == groupID {
			return g.Enabled
		}
	}
	return true
}

func displayName(username, openID string) string {
	if strings.TrimSpace(username) != "" {
		return strings.TrimSpace(username)
	}
	return shortID(openID)
}

func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// parseTS 解析回调时间戳。平台给的是 RFC3339；解析失败就用本地时间，
// 不能因为一个时间字段把整条消息丢掉。
func parseTS(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}
