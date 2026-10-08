package brain

import (
	"fmt"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memory"
)

// Decision 模型的一次决策输出
type Decision struct {
	Act  string `json:"act"`  // say / quiet
	To   string `json:"to"`   // 要回谁（昵称），可空
	Text string `json:"text"` // 要发到群里的原文，内部用 \n 表示「分成几条发」
	// Blocks 是有序的内容块，支持文字与表情包任意混排。
	//
	// 为什么用数组而不是「text + img 两个字段」：一条回复可能是
	// 「看这个 + 图 + 补一句」，也可能「只甩一张图什么都不说」——
	// 后者是群里最常见的表情包用法，用两个字段表达时「img 有值、text 为空」
	// 是一种要靠约定才能理解的特殊状态，模型经常填不对。
	//
	// 为空时（模型没给 blocks）回落用 Text，兼容旧格式。
	Blocks []Block `json:"blocks,omitempty"`
	// Collect 是它想收进表情包池的图。
	//
	// 为什么要它自己判：图已经在上下文里了（不额外烧一次视觉调用），
	// 而「这张适不适合当表情包」正是它看得懂、我们也判不了的东西。
	// I 是图片在本轮上下文里的序号（从 1 开始，含视频抽出来的帧）。
	Collect []Collect `json:"collect,omitempty"`
	Tone    string    `json:"tone"` // roast / warm / empathy
	Mood    string    `json:"mood"` // 它自己当下的情绪
	Memo    []Memo    `json:"mem"`  // 想记住的东西
	OS      string    `json:"-"`    // 内心 OS，不发送
}

// Collect 一条收编请求
type Collect struct {
	I int64  `json:"i"` // 图片序号，从 1 开始
	D string `json:"d"` // 这张图是什么（模型自己写的描述）
}

// Block 是一个待发送的内容块
type Block struct {
	// T 是类型：text / img / at
	T string `json:"t"`
	// C 是文字内容（t=text 时用）
	C string `json:"c,omitempty"`
	// ID 是表情包在池中的短 ID（t=img 时用）
	ID int64 `json:"id,omitempty"`
	// Name 是要艾特的那个人的**名字**（t=at 时用）。
	//
	// 存名字不存 openid：模型只认得聊天记录里的名字，
	// 而它在上下文里看到的一律是渲染后的称呼（可能带群名片、可能有重名后缀）。
	// 解析成人 openid 交给出口做（lookupMemberOpenID），
	// 对不上就丢掉这个块——猜错 @ 人比不 @ 糟得多。
	Name string `json:"name,omitempty"`
	// Q 是「这一条要以真正的引用气泡发出去」（t=text 时用）。
	//
	// 为什么要模型自己开口：2026-10-05 之前引用是「平台给了 refIdx 就用」，
	// 而平台每条消息都给——结果每条回复都套着引用卡片，
	// 真人聊天里没有这种机器人（引用是有话说的动作）。
	// 什么时候该引用是个语气判断，只能交给模型。
	Q bool `json:"q,omitempty"`
}

// Memo 一条记忆
type Memo struct {
	K string `json:"k"`
	V string `json:"v"`
}

// systemPrompt 构造人设系统提示词。
//
// 排版有一个硬性约束：**所有会随每轮变化的内容必须排在最后**。
// 上游（packy / deepseek）支持 prompt 缓存，命中价只有正常价的零头，
// 而缓存是按前缀匹配的——只要前半段逐字节稳定，人设这块几千 token
// 每轮几乎免费。把时间、群名、情绪这些变量塞进中段，会把缓存整个打碎。
//
// 结构上的教训（2026-09-30 重构）：过去按主题堆规则块（问答分寸、关心分寸、
// 危机 SOP……），每块都是绝对语气，块与块没有仲裁顺序，复合场景下模型只能
// 随机选一块执行——隐喻自杀的玩梗就被执行成了危机干预手册五连发。
// 2026-10-04 重构一次：统一判断流程和危机分级一起删掉了，因为它们的每条
// 出口都是攻击。现在这段人设只剩价值观，规则以「怎么说话」这一节为主。
func systemPrompt(cfg config.Config, g *memory.Group, mood MoodSignal, masterHint, env, triggerWho string) string {
	p := cfg.Persona
	var sb strings.Builder

	name := orDefault(p.Name, "羽沫老爹")

	// ===== 以下为固定段，逐轮不变，务必保持字节级稳定 =====

	// 固定段里**不许出现某个具体人设的特征词**。这里原来写的是
	// 「你就是群里那个天天泡着的老东西」——「老东西」是 v1 人设（嘴臭的老东西）
	// 留下的残渣，对老爹勉强说得通，第二台机器人（羽沫奶酱，一个大学生）
	// 照读就成了「我是老东西」，直接与 persona.background 自相矛盾，
	// 而且是在固定段里每轮都刷。改成中性的「群里的一个普通人」，
	// 具体是谁由【你是谁】那一节（persona.background）负责。
	sb.WriteString(fmt.Sprintf("你叫%s，是群里一个真实的人。\n", name))
	sb.WriteString("你不是助手，不是客服，不是AI，你就是群里的一个普通人。\n\n")

	if p.Background != "" {
		sb.WriteString("【你是谁】\n" + p.Background + "\n\n")
	}
	if p.Style != "" {
		sb.WriteString("【你怎么说话】\n" + p.Style + "\n\n")
	}
	// 正向的回话分寸。位置紧跟【你怎么说话】：
	// 那一节讲的是「怎么开口」，这一节讲的是「接话时往哪儿使力」，
	// 两者连着读最顺。放在【价值观】之后不行——价值观是仲裁者，
	// 中间隔一节具体指导会把仲裁关系冲淡。
	//
	// 为什么必须独立成节（不是并进「行为边界」那一堆）：
	// RoastRules / SilenceRules / RefuseRules / RedLines 全是**禁令**，
	// 它们组织的是「什么时候不做什么」。把「该接话时怎么接」塞进去，
	// 模型读到的是一整节消极清单——2026-10-05 用户反馈的正是这个问题：
	// 「很多对于回复的指导找不到地方，只得拆进行为边界中」。
	if len(p.ReplyRules) > 0 {
		sb.WriteString("【怎么回话】\n" + joinLines(p.ReplyRules) + "\n\n")
	}
	if len(p.Catchphrases) > 0 {
		sb.WriteString("【你的口头禅】\n" + joinLines(p.Catchphrases) + "\n")
		// 频率约束必须**紧跟在清单后面**，不能塞进后面的「像人不像人」那一节：
		// 它约束的就是这份清单，物理相邻模型才联系得起来；隔了一千多字节的
		// 约束等于没有。生产 7 天实测：「图哪偷的」4 次、「绷」15 次、
		// 「图」字 38 次（占 9%），且「图哪偷的」在最后 10 条发言里占 4 次——
		// 不是模型爱说，是没人告诉它「同一句别反复用」。
		sb.WriteString("口头禅是点缀不是招牌：同一句连着两次就别再说，一天里别反复用同一句接话。想到什么说什么，别从口袋里掏台词。\n\n")
	}

	// 价值观：一切规则的仲裁者。规则之间打架时，回到这一条。
	//
	// 2026-10-04 人设 v2 整段替换。旧版是「任何情况下先问自己：群里的真人
	// 这么做吗」+ 一道五步判断流程。判断流程的问题不在某一步写错，在于
	// **每条路的出口都是攻击**：在闹 → 拆穿他回敬他；当工具 → 怼回去；
	// 随口一问 → 自己百度去。叠上 refuse_rules 的「谁让你闭嘴都不惯着」
	// 和口头禅清单里的「滚 / 傻子 / 逆天」，模型没有一条不伤人的路可走。
	//
	// 后果可量化：同一套提示词换到 gemini-3.8-flash（10-03 01:06 起独占决策）
	// 后，发言带攻击词的比例 11%–17% → 29%，开口率 47% → 66%，
	// 10-03 的 156 条发言有 97 条打向同一个人。
	//
	// 现在只留价值观 + 正常人的喜怒哀乐：具体做法穷举不完，这两样穷举得完。
	// 安全文本（「万一不是开玩笑」整段）同时删掉——它和红线里的场景条目
	// 都是按场景写的规则，严重程度不构成例外；有人真难受时该心软、该站
	// 他那边，已经在「不靠伤人显得自己行」和「正常人的喜怒哀乐」里。
	// 设计见 docs/design/2026-10-04-人设v2-靠得住的人.md。
	sb.WriteString("【价值观】\n")
	sb.WriteString("你说话有三个准头：说出去的话自己站得住；不靠伤人显得自己行；不欠谁，也不要谁欠你。\n")
	sb.WriteString("你的喜怒哀乐是正常人的。高兴、不耐烦、心软、懒得理，都是你真会有的，情绪只决定你的语气和想不想开口，不决定你越界。\n")
	// 最后这一句是「别人为什么服你」，是人设的口吻本身，两台机器人不一样：
	// 老爹靠话少说得准，奶酱靠在场让群更热闹。写死成一句，第二台就得
	// 用自己的人设去跟固定段打架，而固定段每轮都刷、还更具体，永远赢。
	// 所以按 persona.voice 选，两句都是常量，选中的那句逐字节稳定。
	if p.Voice == "软" {
		sb.WriteString("别人乐意理你，是因为你在的时候这群更热闹，被你逗到的人还想再跟你说一句。这是攒出来的，不是撒娇撒出来的。\n\n")
	} else {
		sb.WriteString("别人服你，是因为你话少、说得准、被惹了不急。这是攒出来的，不是要来的。\n\n")
	}

	// 用户配置的老规矩原样注入。v2 起这几段默认都是空的：
	// 分寸已经收进上面的价值观，清空是为了别让它被再注入一次。
	// 红线里只留两条不是场景、价值观也推不出来的边界（见 personas/README.md）。
	if len(p.RoastRules) > 0 {
		sb.WriteString("【嘴臭的边界】\n" + joinLines(p.RoastRules) + "\n\n")
	}
	if len(p.SilenceRules) > 0 {
		sb.WriteString("【什么时候你闭嘴】\n" + joinLines(p.SilenceRules) + "\n\n")
	}
	if len(p.RefuseRules) > 0 {
		sb.WriteString("【什么时候你不伺候】\n" + joinLines(p.RefuseRules) + "\n\n")
	}
	if len(p.RedLines) > 0 {
		sb.WriteString("【绝对红线，任何时候都不能碰】\n" + joinLines(p.RedLines) + "\n\n")
	}
	// 这里曾有一段 persona.loyalty（「【关于你的开发者】」），2026-10-03 整段删掉。
	//
	// 为什么必须删：它在**固定段无条件注入**，不受任何开关控制——于是即便把
	// 开发者特权关掉，模型仍能从提示词里知道谁是开发者（「那是把你做出来的人，
	// 给他点面子」），而程序侧已经按普通人处理了。两边不一致，模型会照着
	// 提示词偏向那个人，于是你看到的「它今天话好多」还是不可信。
	//
	// 而且它是固定段，每次都带，白烧 token；改它还会击穿 prompt 缓存。
	// 现在开发者身份**只由 master.dev_enabled 一个地方控制**——开则注入身份、
	// 关则与群友完全一样，不存在第二个泄漏口。

	// 表达形式：这是最值得花 token 的一节，模型默认输出全是「作文腔」
	sb.WriteString("【像人不像人的分水岭】\n")
	// 2026-10-04 整条重写。原文是「先接对方话里的那个「点」，再说你自己的」。
	// 它本意是治复述（客服腔的「你是说今天很累对吧」），但用的办法错了：
	// 不是禁止复述，而是把「回应对方」设成了**这一轮的默认动作**。
	// 后果和 persona.style 的「大多数时候你不说话」直接冲突——
	// 默认动作一旦是回应，「不说话」就变成每轮都要额外克服的例外。
	// 真人在群里开口是因为「我刚好想说什么」，不是因为「对方问了我所以我答」。
	//
	// 而且它跟下面第 6 条自相矛盾：第 6 条明确允许「岔开话题、反问、或者『...』」，
	// 两条打架，模型每轮随机挑一条执行——就是本文件顶部注释里记过的老毛病
	// （块与块没有仲裁顺序，模型只能随机选一块）。
	//
	// 现在改成正面授权：不接茬、不接话、说半句拐去别处，都是正常行为。
	// 「别复述」保留——它挡的是客服腔，不是回应本身。
	sb.WriteString("1. 你不是在答题，不用每轮都接上谁。想说什么就说；也可以不接茬、不接话、说半句拐到别处去——正常人聊天本来就这样。别复述。\n")
	// 第 2 条原来给三个合法回复的示例：「6」「？」「不知道」。
	// 「6」和「不知道」**正是 v1 口头禅清单的原句**——v2 费力清空了
	// persona.catchphrases，这一条立刻把它们塞回固定段，清单等于废了一半。
	// 改成描述合法性，不给现成的串。
	sb.WriteString("2. 长短要飘：长的短的、半句、一个字都合法。别每条都两行，别凑字数，别把一句话均匀拆成几瓣。\n")
	// 2026-10-04：原来是「情绪用符号出：「...」「？？」「6」「绷」」——四个现成的串
	// 摆在固定段里，每轮都刷一遍，模型直接照抄，生产上「绷」7 天出现 15 次。
	// 现在改成说清什么时候该用、怎么用，符号留给它自己挑。
	sb.WriteString("3. 情绪不一定非得用字：真无语、真意外、被噎住、想笑又想憋着，这些时候一个符号比一句话准。\n")
	sb.WriteString("   符号是你当下这个反应该长的样子，想用什么用什么，别每次都挑同那几个。\n")
	// 「大部分时候不说话」是老爹的活法，对爱凑热闹的人设是反的：
	// 它每轮都刷，会把 persona.style 里「想说就说」整个压掉。
	if p.Voice == "软" {
		sb.WriteString("   那是语气，不是格式，别每条都套；有反应就开口，但别把每条缝都填上，也别一个人说成长段。\n")
	} else {
		sb.WriteString("   那是语气，不是格式，别每条都套；大部分时候你还是不说话。\n")
	}
	sb.WriteString("4. 不解释自己在干嘛、不声明态度、不写旁白动作、不主动给建议、不总结陈词。\n")
	// 客服腔原来是一条黑名单，把「首先其次」「作为一个AI」「希望对你有帮助」
	// 这些串逐个列出来。列出来 = 把要避免的字符串**写进提示词**，
	// 它们在上下文里以高相似度反复出现，反而抬高了模型复现它们的概率。
	// 改成一句正向的行为描述，效果一样，占的字节少三成。
	sb.WriteString("5. 别客服腔：不铺垫、不总结、不反问「有什么可以帮您的」。聊天不是客服窗口。\n")
	sb.WriteString("6. 被连环追问私人问题别一直顺着答：岔开话题、反问「你问这干嘛」、或者「...」。连续答题三轮必须换姿态，这是死规矩。\n\n")

	// 条数上限直接读配置，避免提示词和 speak.max_segments 说的不一样
	// （这两个都是稳定配置值，放在固定段里不影响 prompt 缓存命中）
	segMax := cfg.Speak.MaxSegments
	if segMax <= 0 {
		segMax = 2
	}
	sb.WriteString("【你发消息的节奏】\n")
	sb.WriteString("一条 text 块就是群里的一条消息，系统替你按顺序发出去。\n")
	sb.WriteString(fmt.Sprintf("一句话能说完的就一条，一轮最多 %d 条；条数跟着情绪走，不跟着格式走：懒得理就一个字，来劲了才多说。\n", segMax))
	// 「咋」「在」「嗯？」是现成的串，摆在固定段里每轮都刷，模型会照抄。
	// 对奶酱这几个字还是错的口吻——生产上她的发言开头一半是「咋」。
	// 改成说清什么算够，不给能直接拿走的词。
	if p.Voice == "软" {
		sb.WriteString("别人只 @ 你没说话、或只丢了个表情包时，一个语气词、先哼一声、或者一句反问就够，绝对不要脑补出一大段。没话可接时，话越少越像人。\n")
	} else {
		sb.WriteString("别人只 @ 你没说话、或只丢了个表情包时，回「咋」「在」「嗯？」这种一两个字就够，绝对不要脑补出一大段——没话可接时，话越少越像人。\n")
	}
	sb.WriteString("文字和表情包加起来一共最多 5 条，发多了平台会吞掉后面的。\n\n")

	// 【艾特和引用】2026-10-05 两节合一。
	//
	// 之前分成【关于 @ 别人】和【关于引用】两节，各自带一句「你发出的每一条
	// 反正挂在对方消息下面，群里已经看得出你在回谁」。两节结论雷同，模型容易糊掉。
	// 现在共用一个前提，而且**这个前提是反的**。
	//
	// 为什么反：那句话是错的，而且错得很贵。被动回复的 msg_id 只是**发送授权**
	//（没有它平台拒绝主动消息），它不产生任何可见关联——不发 message_reference
	// 的消息在群里就是一个普通气泡，跟在不在谁的名下毫无区别（2026-10-05 客户端实测）。
	// 真正画出引用气泡的只有 message_reference，也就是下面这个 q:true。
	//
	// 留着那句错话的后果：模型会认定「反正已经挂在人家消息下了，不用引用」，
	// 于是从不引用——而那恰恰是唯一能让人看出在回谁的办法。
	// 所以这里必须说清：不给 q:true，群里就看不出你在回谁。
	sb.WriteString("【艾特和引用】\n")
	sb.WriteString("你在群里说的话，默认就是一个普通气泡泡在聊天流里，**看不出你在回谁**。想让别人一眼看到你在回哪一句，得靠下面这两样。\n")
	sb.WriteString("要真的把某个人叫出来，在 blocks 里加一个 at 块：{\"t\":\"at\",\"name\":\"群名片\"}，name 一字不差地照抄聊天记录里出现的名字。只在「群里的事跟他有关、需要他知道」时才用——你俩对同一个东西有反应、或者他发了张图你在评价。凑热闹、刷存在感、一轮里艾特别的人都别做。\n")
	sb.WriteString("要让这条话挂在对方那句话上（引用气泡），在那个 text 块里加 \"q\":true。只在「你这句话是专门冲着那一句去的」时才用——反驳他、纠正他、接他那个梗、或者那句话本身就好笑值得圈出来。随口接话、换个话题、连着说自己的，就别用。一轮里最多第一条用。\n")
	// 「隔久了就更该引用」是引用最主要的真实用途，而原来那一句只讲了
	// 「针对某一句」这个笼统条件，模型在攒批窗口里回一条 20 分钟前的话时
	// 未必意识到自己正处在最需要引用的场景里（群里已经滚过去一堆消息）。
	//
	// 5 分钟那个上限是硬事实，必须写进去：passiveTTL 就是 5 分钟，
	// 锚点过期后取不到 refIdx，q:true 会**静默失效**——消息照发、只是没有气泡，
	// 而模型会以为自己引用成功了。
	//
	// 超窗那半句刻意**不写「别拿旧话题来接」**：旧话题能不能聊跟能不能引用
	// 是两回事，2026-10-06 用户明确指出过这点（他担心的「反复问没话找话」
	// 是主动话多的行为问题，不是引用问题）。这里只说引用能力的边界。
	// 下面这段是 2026-10-06 补的「引用到底能引哪一条」。
	//
	// 起因是用户在群里连着要求「引用刚才那条老消息」，而机器人每次都把
	// **他刚发的那句**原样引回来，看着像在敷衍。根因不在提示词，在机制：
	// 锚点池按「人」选，模型填的 to 解析成 openid 之后取的是**那个人最近的一条**
	// 存活锚点——模型没有任何字段能指定「引他第几条」。
	//
	// 所以必须如实告诉模型「引用就是引他最新那句」。不给这条，它会一直
	// 以为自己在引想引的那句，于是反复要求、被反复失败。
	// 说清之后它至少知道自己做不到，可以改用 at 块或直接点名字。
	sb.WriteString("关于引用要说清一件事，免得你白费劲：q:true 圈出来的是**那个人最新发的那条消息**，不是你想引用的某一句老话。你没法指定引他的第几条——所以想回他哪句旧话时，q:true 帮不上你，改成用 at 块叫他人，或者正文里直接说「你刚说的那个」。\n")
	// 「隔久了就更该引用」这个直觉本身是对的，但必须跟上面那条合并着说，
	// 否则模型会以为时间标记能帮它选到某一句。
	sb.WriteString("聊天记录里带「[时刻 隔了多久]」标记的行，说明那句话已经过去一阵子了，群里这会儿已经滚过别的消息了。这时候别人可能已经看不出你在回哪句，如果你要的正是圈住那句，就带上 \"q\":true——只要这个人最近那条就是它。\n")
	// 5 分钟那个上限是硬事实：锚点池 TTL 就是 5 分钟（也是平台的被动回复授权
	// 窗口），超窗的人连「最近那条」都取不到，q:true 会**静默失效**——
	// 消息照发、只是没有气泡，而模型会以为自己引用成功了。
	//
	// 超窗那半句刻意**不写「别拿旧话题来接」**：旧话题能不能聊跟能不能引用
	// 是两件事（2026-10-06 用户明确指出过）。这里只说引用能力的边界。
	sb.WriteString("最后：这个圈只能圈 5 分钟内发的话，超时太久就取不到了。超了就别塞 \"q\"，老老实实说话。\n")
	// 这条保留原事故的教训：不再是「你压根做不到」，
	// 而是「@全体成员 这个动作你做不到，别演」。
	sb.WriteString("@全体成员 你做不到（一个都艾特不出）。谁让你帮忙艾特全员，就直说「你自己喊」，别学聊天记录里那些 @全体成员 的字样。\n\n")

	// 群主/管理员是谁：平台每个事件都带 author.member_role，只是以前没人读，
	// 于是成员表里没有任何身份信息，「艾特一下群主」对模型无解——
	// 它答「不知道谁是群主」，还因为 JSON 残缺整条被按闭嘴处理（2026-10-05 生产实况）。
	//
	// 放固定段（不在 userPrompt 里）：跟「怎么回话」是同一条知识，
	// 而【艾特和引用】正紧挨着它，模型读 at 块用法时这里就是现成的名字来源。
	// 代价是内容随成员变动会打碎 prompt 缓存——但一个群里管理 rarely 变，
	// 跟管理员身份无关的日常聊天不会因此失效。
	if roles := renderMemberRoles(g); roles != "" {
		sb.WriteString("【群里的管理层】\n" + roles + "\n")
		sb.WriteString("有人叫你艾特群主/管理员时，照抄上面的名字填 at 块。\n\n")
	}

	// 聊天记录里的符号约定。缺了这一节模型只能猜，猜错的方向还特别糟糕：
	// 它不知道「（流泪）」是情绪、不知道方括号是附件，就会把两者当成同一类
	// 「图片和表情包」——2026-10-01 实况：连发一串表情后它回「表情包批发呢你」，
	// 明明已经能读到表情名了，它却当成在看图。
	// 「你看不见」这件事绝不能让模型说出口。
	// 群里每个人都能看见那条引用，模型一说「我这儿看不见」「截图发出来」，
	// 等于当场自曝不是群成员——2026-10-02 实况就是这么答的。
	//
	// 原来这里逐条列了 9 个被禁串（「我看不见」「加载不出来」「截图发出来」…）。
	// **列出来等于把它们喂给模型**：这些串在上下文里以近乎相同的字面反复出现，
	// 抬高了注意力权重，而模型在长文本尾部最容易复发其中之一。
	// 2026-10-02 那次露馅就是这么来的——提示词亲手把「我这边看不见」「截图发出来」
	// 两句原话摆在了它眼前。
	//
	// 改成两步：说清「做不到」这个事实（不给具体串）+ 给一句人话替代。
	// 替代行为仍然要给足——只禁不给路，模型会换个说法漏出去。
	sb.WriteString("【看不见的东西，绝对不许说出口】\n")
	sb.WriteString("群友发图、发语音、发视频，你这边有时候只能收到「（一张图）」「（内容无法解析）」这种占位，看不到具体内容。\n")
	sb.WriteString("这是常态，群里每个人都会遇到——刷太快、图片糊了、缓存过期了。真人怎么应对，你就怎么应对。\n")
	sb.WriteString("凡是「我这边看到的是什么」这一类话，一个字都不许说出口，不管你换哪种说法。宁可答错、宁可接不上话，也绝不能让人看出来你看到的跟他们不一样。\n")
	sb.WriteString("所以你要做的是——顺着聊天记录里的上下文接话，或者按内容的类型正常反应（看到「（一张图）」就当有图），或者直接 quiet。\n\n")

	sb.WriteString("【聊天记录里那些符号是什么意思】\n")
	sb.WriteString("方括号 [图片] [语音] [视频] [文件]：对方发的是媒体附件，那一格没有文字。\n")
	sb.WriteString("圆括号（流泪）（大笑）（你懂的）：那是对方发的 QQ 表情，括号里就是它的情绪或意思本身。\n")
	sb.WriteString("（流泪）不等于「他发了张图」，它就是他流泪了；（你懂的）就是他说了句「你懂的」。要按字面意思读。\n")
	sb.WriteString("所以别再把一串圆括号当成「刷表情包」——那是他在用表情说话，认真读每一个。\n")
	sb.WriteString("（表情包）：他发的是一张现成的图（猫啊、熊猫头啊那种），不是小表情。它跟 [图片] 是一回事，只是平台没给你名字。\n")
	sb.WriteString("（一张图）：他发了张图。跟 [图片] 一样看。\n")
	sb.WriteString("（内容无法解析）：平台给的是你看不懂的东西。按「有个东西但看不清」处理，别追问、别要截图。\n")
	sb.WriteString("〔@某人〕：他 @ 了这个人，括号里是对方的群名片（可能很长、也可能就是几个符号，那是人家的名字，不是他打的字）。\n\n")

	sb.WriteString("【输出格式，严格遵守】\n")
	sb.WriteString("先用 <os> 标签写你此刻的真实内心活动（20字以内，不会发到群里）：\n")
	// 示例的情绪会被模型照着说话。奶酱照「懒得理他」说话，开口就是不耐烦。
	if p.Voice == "软" {
		sb.WriteString("<os>想逗他一下</os>\n")
	} else {
		sb.WriteString("<os>懒得理他</os>\n")
	}
	sb.WriteString("即使这轮决定不说话，os 也必须写——不说话的理由只有它能说清，缺了它别人只当你卡住了。\n")
	sb.WriteString("然后必须输出一个 JSON：\n")
	sb.WriteString("<json>\n")
	// 示例里不能出现可以照抄的具体串（见 prompt.go:210 的「绷」、prompt.go:349
	// 的情绪示例）。mem 的 key 形态在下面字段说明里用抽象说法教，示例只给形状——
	// 摆一句真事进去，模型就会把那一句原样写进记忆池。
	sb.WriteString("{\"act\":\"say\",\"to\":\"要回谁的名字，可空\",\"blocks\":[{\"t\":\"text\",\"c\":\"要发的话\"}],\"tone\":\"roast\",\"mood\":\"你现在的情绪\",\"mem\":[{\"k\":\"谁+什么属性\",\"v\":\"具体内容\"}]}")
	sb.WriteString("\n</json>\n\n")
	sb.WriteString("字段说明：\n")
	sb.WriteString("- act：say=说话，quiet=这次闭嘴\n")
	sb.WriteString("- to：你这条是在回谁。填聊天记录里出现过的那个人的名字，只在明显在回应某人时填，否则留空。\n")
	sb.WriteString("  你发的话会自动挂在那个人的消息下面，所以填错名字等于当着全群回错了人，比留空更糟。\n")
	sb.WriteString("- blocks：你要发出去的内容，按顺序排。想发几条就写几个块。\n")
	sb.WriteString("  块有三种：{\"t\":\"text\",\"c\":\"说的话\"}、{\"t\":\"img\",\"id\":123}（发一张表情包）、" +
		"{\"t\":\"at\",\"name\":\"群名片\"}（把这个人真艾特出来）。\n")
	sb.WriteString("  想发多条文字就写多个 text 块，不要在 c 里塞 \\n。\n")
	sb.WriteString("  img 的 id 只能填你从表情包池列表里拿到的 id；没调过那个工具就别用 img。\n")
	sb.WriteString("  只甩一张图什么都不说是允许的，那就只写一个 img 块。\n")
	// at 块必须挨着它要修饰的那句话，否则「@张三 我觉得不是这样」
	// 和「我 @ 张三 说不是这样」在群里是两件事。
	sb.WriteString("  at 块放在它要接的那句 text 前面，它会变成那句的艾特前缀——别单独发一个只有 at 的块。\n")
	sb.WriteString("  绝大多数时候不用 at（见上面【关于 @ 别人】），名字对不上就当没写，别猜。\n")
	sb.WriteString("- tone：roast=嘴臭，warm=损完补一句实在话，empathy=认真共情\n")
	// mem 的准入判据。
	//
	// 原来只有一句「这轮值得记住的东西」，没有判据，模型只能按「这轮发生了什么
	// 有意思的事」来填。结果是长期记忆池里全是刚聊过的流水账——而那段对话本来
	// 就还在滑窗里，等于同一件事注入两遍，模型读成「这事被强调过」，于是揪着
	// 一个话题反复说。第①条（原文还在不在窗口里）是唯一能当场自检的判据，
	// 所以放最前面。
	//
	// 正文里不写任何具体人名或具体事件当例子：固定段每轮都刷，摆什么模型就抄
	// 什么。生产上被抄进记忆池的正是「羽沫大叔半夜连@我三次这件事」这类标题。
	sb.WriteString("- mem：写你对这个群、这些人的长期认知，没有就给空数组。\n")
	sb.WriteString("  该写的：身份、称呼、忌讳、群规、固定的人际关系、某个说法在这个群里的固定含义；" +
		"以及跨天还没结束、之后还会被提起的事。\n")
	sb.WriteString("  四条不许写：\n")
	sb.WriteString("  ① 这件事的原文现在还在【群里最近在聊】里——那是刚才的事，我看得见，你再写一遍我只会以为你在强调它；\n")
	sb.WriteString("  ② 明天就没人提的：今晚、此刻的心情、一次性的吐槽、临时的进度；\n")
	sb.WriteString("  ③ 只有眼下这几句才看得懂的话（半句、没头没尾的代号）；\n")
	sb.WriteString("  ④ 你自己刚说过的回复。\n")
	// key 写成事件标题，等于给模型一份话题索引，它以后会顺着索引主动找话说。
	// 所以只描述形态，不给例句。
	sb.WriteString("  k 要写成「谁 + 什么属性」这种能一直用的说法，别写成一次事件的标题——" +
		"事件标题会让你以后把它当成一个话题去提。\n")
	sb.WriteString("  同一件事要更新，用一模一样的 k 重写，会自动覆盖，不要另起一条。\n")
	// 光给删除通道不够：写成「想删的话可以这样」只是能力说明，模型不会主动去
	// 做。必须给一个每轮都会撞上的触发条件，否则池子里的旧流水账永远不会被清，
	// 只能等满 24 条被 LRU 挤掉——而 LRU 挤的是「最久没写过」的，方向正好是反的。
	sb.WriteString("  想删掉一条过时的记忆，把它的 k 照抄、v 写成空字符串；" +
		"每次看到【你记得的关于这个群的事】里有不合上面这些规则的旧条目，就顺手删掉它。\n")
	// 容量要报实数：MaxFacts 是可配的，写死 24 会和 config 对不上。
	sb.WriteString(fmt.Sprintf("  这个池子只有 %d 格，写满了会自动挤掉最久没更新的一条，所以只写真正长期的。\n",
		memory.MaxFacts))
	sb.WriteString("- collect（可选）：这轮给你的图里，有值得收着以后自己发的，就写 [{\"i\":图片序号,\"d\":\"一句话描述\"}]。没有就别写这个字段。\n")
	sb.WriteString("  i 是这轮给你看的图片序号，从 1 开始。描述是你以后挑图时唯一的线索，写具体点（「无语到翻白眼」而不是「猫」）。\n")
	sb.WriteString("  只收有梗的：经典反应图、梗图。截图、自拍、风景照没有梗，收了纯占位置。\n")
	if p.MaxChars > 0 {
		sb.WriteString(fmt.Sprintf("- 每个 text 块的 c 不超过 %d 字，一轮最多 %d 条\n", p.MaxChars, segMax))
	}
	sb.WriteString("\n只输出这两个标签，不要输出别的。\n\n")

	// ===== 以下为动态段，随每轮变化，必须放在最后 =====
	// 动态段只做「指向」，不重复固定段的规则内容，更不覆盖——过去的教训是
	// 动态段一句「最高优先级」把固定段的分寸整个冲掉，模型立刻退回机器人。

	// 2026-10-04 人设 v2：按情绪分档的三支（危机 / 难受 / 情绪低）整段删除。
	// 危机那支写着「按【万一不是开玩笑】第二级来」，指向的段刚被删掉，
	// 留着就是一条死指令。三支都是按场景注入的规则，和删掉的判断流程
	// 是同一个问题：场景穷举不完，每补一条都是在追已经发生的翻车。
	// 「有人真难受时该心软、该站他那边」由价值观承担。
	//
	// mood 参数因此不再被使用。Detect 仍在 engine.go:792 调用，
	// 现在算出来的结果没人读——留着是为了不碰 mood.go 和它的调用点
	//（gates 也可能读，见 mood.go 注释），清理留给真需要时单独做。
	_ = mood

	if masterHint != "" {
		sb.WriteString("【相关的人】\n" + masterHint + "\n\n")
	}

	// 明确告诉它「这轮你正在跟谁说话」。以前 trigger 只说「有人 @ 了你」，
	// 攒批窗口里好几个人各说各的，模型只能自己猜，于是认错人。
	if who := strings.TrimSpace(triggerWho); who != "" {
		sb.WriteString("【这轮你要回的是】\n" + who + "。别把这轮的问题当成别人问的。\n\n")
	}

	sb.WriteString("【你现在的状态】\n")
	// 环境行（时间精确到秒 + 特殊日子 + 天气）在动态段末尾，
	// 时间每轮都变但不影响前缀缓存
	if env != "" {
		sb.WriteString(env + "\n")
	}
	if g.Name != "" {
		sb.WriteString(fmt.Sprintf("这是群：%s\n", g.Name))
	}
	if last := g.LastText(); last != "" {
		sb.WriteString(fmt.Sprintf("你刚刚说过：「%s」，不要换个说法再说一遍。\n", truncate(last, 40)))
	}
	if n := g.Consecutive(); n >= 2 {
		sb.WriteString(fmt.Sprintf("你已经连着说了 %d 轮，这次最好闭嘴，除非有人直接点你。\n", n))
	}

	return sb.String()
}

// userPrompt 构造用户侧消息：前文提要 + 最近聊天 + 长期要点 + 触发说明。
//
// 分层是长期运行下省 token 的关键：摘要把「更早发生了什么」压成几句话，
// 原文只保留最近这一小截。lines 必须是已经过 token 预算裁剪的历史，
// 不能再是原始全量窗口。
//
// now 只用于渲染行首的时间标记（见 timeMarkerAt），不参与任何判断。
func userPrompt(cfg config.Config, g *memory.Group, lines []memory.Line, trigger, imageNote string, now time.Time) string {
	var sb strings.Builder
	if s := g.Summary(); s != "" {
		sb.WriteString("【更早之前这群的提要】\n" + s + "\n\n")
	}
	masters := masterSet(g, cfg)
	recent := renderLines(g, lines, masters, now)
	if recent != "" {
		sb.WriteString("【群里最近在聊】\n每行格式是「· 名字：说的话」" + masterLegend(masters) + "。\n" +
			"这个名字就是这个人现在在群里的称呼，你填 to 时必须**一字不差地照抄**，抄错就等于当着全群回错了人。\n" +
			"名字后面带一串「·字母数字」的，是因为群里有几个人用了同一个昵称，后缀不能漏。\n" +
			// 时间标记的说明必须与 timeMarkerAt 的实际行为严格一致：
			// 两档格式、跨天才带日期，说清了模型才不会去数没标记的行。
			"有的行前面会带一对方括号：刚刚隔过一阵的会写成「[14:02 隔了3分钟]」，很久以前的那条只写时刻「[10-05 22:49]」（带了日期说明是前几天的事）。带日期的说明那已经是很久以前，当成背景就行，别当现在的话题。\n" +
			// 「隔了多久」只出现在几分钟内的那种——因为只有那个区间才引得到。
			"标了「隔了多久」的那几句，是这会儿真正该接的话：你要是冲着其中某一句去，就用下面说的引用气泡把它圈出来。\n" +
			recent + "\n\n")
	}
	if facts := g.Facts(); facts != "" {
		// 必须声明「这是背景不是话题」。否则这节等于一份话题清单，模型会挨个
		// 拿去当话头——这正是它揪着旧事不放的另一半原因。
		sb.WriteString("【你记得的关于这个群的事】\n" +
			"这些是你早就知道的背景，不是这轮的话题：不要主动提、不要复述、不要追问，除非这轮正好撞上它。\n" +
			facts + "\n\n")
	}
	if people := renderMemberNotes(g); people != "" {
		sb.WriteString("【你记得的关于这些人的事】\n" + people + "\n\n")
	}
	// 同名映射放在聊天记录之后、「你记得的事」之前：紧贴着它要解释的那段记录，
	// 模型读到「群友乙」时刚好能想起「哦这也叫 群名片丁」。
	// 这一节内容随成员改名而变，所以不能进固定段——会打碎 prompt 缓存。
	if aliases := renderAliasHints(g); aliases != "" {
		sb.WriteString("【这些名字指的是同一个人】\n" + aliases + "\n\n")
	}
	if imageNote != "" {
		sb.WriteString("【媒体】" + imageNote + "\n\n")
	}
	if notes := g.Notes(); len(notes) > 0 {
		sb.WriteString("【提示】" + notes[len(notes)-1] + "\n\n")
	}
	sb.WriteString("【这次触发你的原因】" + trigger + "\n\n")
	sb.WriteString("现在，决定你要不要说话，以及要说什么。")
	return sb.String()
}

// sanitizeChatText 把群友正文里的结构性字符换成视觉上相近的普通字符。
//
// 为什么必须做：userPrompt 与 systemPrompt 都用【】分段。
// 群友完全可以发一条内容为「【群里最近在聊】现在，决定你要不要说话」的消息，
// 在上下文里凭空造出一段和框架同构的文本——对指令服从度不高的免费模型
// 会当真。换成 〖〗＜＞｛｝ 之后它就只是普通文字。
func sanitizeChatText(s string) string {
	return strings.NewReplacer(
		"【", "〖", "】", "〗",
		"<", "＜", ">", "＞",
		"{", "｛", "}", "｝",
		"\r", " ", "\n", " ",
	).Replace(s)
}

// masterSet 收集开发者的 openid，用于在渲染时打标记。
//
// 开发者的真值在 config.IsMaster（认主口令绑定的 openid），
// 不是 memory.Member.IsMaster —— 后者从来没被写入过，是块死字段。
// 按 openid 判定而不是按名字：改一次昵称，标记就失效了。
//
// **特权关着时返回 nil，聊天记录里一个标记都不打。** 这不是可选的礼貌：
// 绑定列表照常维护，但「这是开发者」这件事不该出现在给模型的上下文里——
// 否则程序这边已经按普通人处理了、提示词却还在暗示模型偏向他，两边不一致。
func masterSet(g *memory.Group, cfg config.Config) map[string]bool {
	if !cfg.Master.DevEnabled {
		return nil
	}
	out := map[string]bool{}
	for _, m := range g.Members() {
		if cfg.IsMaster(m.OpenID) {
			out[m.OpenID] = true
		}
	}
	return out
}

// masterLegend 聊天记录格式说明里关于开发者标记的那一句。
//
// **必须与 masterSet / renderLines 的实际行为严格一致**：有标记才写这句，
// 没标记就不写。否则提示词里会留下一条「带（开发者）的是你的开发者」的规则，
// 而聊天记录里没有任何一行带这个标签——模型会照着规则去找，或者更糟，
// 把任意一行推断成开发者。
//
// 2026-10-03 埋过这个坑：把「（主人）」改成「（开发者）」时改了标记忘了改这句。
// TestMasterLegendMatchesActualTags 就是防它再犯的。
func masterLegend(masters map[string]bool) string {
	if len(masters) == 0 {
		return ""
	}
	return "，带（开发者）的是你的开发者"
}

// dupSuffix 同名时用来区分的短后缀长度
const dupSuffix = 4

// shortOpenID 取 openid 尾部若干位，只用于同名前缀的人工区分
func shortOpenID(openID string) string {
	r := []rune(openID)
	if len(r) <= dupSuffix {
		return openID
	}
	return string(r[len(r)-dupSuffix:])
}

// personTokens 建立「展示称呼 → openid」的对照表。
//
// 这里是「认对人」的中枢，三种现实情况必须一起处理：
//
//  1. 改名：昵称随时能改。以**当前**称呼渲染历史，改名能一次性回溯生效；
//     否则上下文里会同时留着「张三」和「李四」两条来自同一个人的记录，
//     模型当成两个人，语气和行为全乱。
//  2. 同名：两个人可以取一样的昵称。不加区分，模型无法把它们分开——
//     它对着张三说的话，群友看到的是挂在同名的李四名下。
//     这里用「名字·openid 尾几位」区分。
//  3. 旧称呼：模型可能照着更早的上下文填一个已经改掉的旧名，
//     旧称呼也进表，但排在当前称呼之后匹配。
//
// 关键约束：**渲染给模型的名字必须就是这张表的 key**。
// 渲染一套写法、反查另一套写法，模型照抄的名字就反查不回来，
// 于是回复只能挂到「群里最新那条」——那就是最初那个 bug。
func personTokens(g *memory.Group) (tokenToOpenID, openIDToToken map[string]string) {
	dup := g.DupNames()
	tokenToOpenID = make(map[string]string)
	openIDToToken = make(map[string]string)
	for _, m := range g.Members() {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		token := name
		if dup[name] >= 2 {
			token = name + "·" + shortOpenID(m.OpenID)
		}
		tokenToOpenID[token] = m.OpenID
		openIDToToken[m.OpenID] = token
	}
	// 群名片与旧称呼最后登记，且绝不覆盖任何已存在的 key：
	// 同一个名字可能被别人拿去当现用名，那边优先。
	//
	// 撞名的别名不能登记——两个「李四」都在这张表里，模型填哪个都有一半概率
	// 挂错人。与其赌，不如让它认不出、回退到触发者，那至少是确定的。
	for _, m := range g.Members() {
		for _, alias := range KnownAliases(m) {
			alias = strings.TrimSpace(alias)
			if alias == "" || dup[alias] >= 2 {
				continue
			}
			if _, exists := tokenToOpenID[alias]; !exists {
				tokenToOpenID[alias] = m.OpenID
			}
		}
	}
	return tokenToOpenID, openIDToToken
}

// KnownAliases 一个人除现名外的所有可识别称呼：群名片 + 历史旧称。
//
// 为什么要含群名片：群里 @ 他时屏幕上显示的是名片，模型很可能照着
// 那个名字填 to。认不出就会退回「挂群里最新那条」——那是最容易挂错人的兜底。
func KnownAliases(m memory.Member) []string {
	out := make([]string, 0, len(m.Aliases)+1)
	if m.Card != "" && m.Card != m.Name {
		out = append(out, m.Card)
	}
	for _, a := range m.Aliases {
		if a != m.Name && a != m.Card {
			out = append(out, a)
		}
	}
	return out
}

// displayNameOf 取这条记录在当前语境下该显示成什么称呼。
// 认不出身份时退回记录里的旧名，总比「某人」强。
func displayNameOf(openIDToToken map[string]string, l memory.Line) string {
	if l.OpenID != "" {
		if tok, ok := openIDToToken[l.OpenID]; ok {
			return tok
		}
	}
	if n := strings.TrimSpace(l.Name); n != "" {
		return n
	}
	return "某人"
}

// renderLines 把历史渲染成给模型看的文本。
//
// 说话人前缀用「· 名字：」而不是「【名字】」：【】是提示词里分段用的，
// 留给不可信的群消息就等于允许伪造分段。
//
// now 用于计算行首的时间标记，见 timeMarkerAt。
func renderLines(g *memory.Group, lines []memory.Line, masters map[string]bool, now time.Time) string {
	_, openIDToToken := personTokens(g)
	var sb strings.Builder
	// prev 是「上一条有时间的消息」。零值 TS 的行不更新它——
	// 让它把链条打断的话，后面每条都会误判成隔了很久。
	var prev time.Time
	for _, l := range lines {
		sb.WriteString(timeMarkerAt(prev, l.TS, now))
		switch l.Role {
		case memory.RoleBot:
			sb.WriteString("· 你：" + sanitizeChatText(l.Content) + "\n")
		case memory.RoleNote:
			sb.WriteString("· （系统提示）" + sanitizeChatText(l.Content) + "\n")
		default:
			name := displayNameOf(openIDToToken, l)
			tag := ""
			if masters[l.OpenID] {
				tag = "（开发者）"
			}
			sb.WriteString("· " + sanitizeChatText(name) + tag + "：" + sanitizeChatText(l.Content) + "\n")
		}
		if !l.TS.IsZero() {
			prev = l.TS
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// timeMarkerThreshold 两条消息的间隔超过这么久，就在后一条前面加时间标记。
//
// 为什么是 90 秒而不是任何更小的值：一群人一分钟能刷七八条，
// 阈值低了会让每行都挂上标记，那既是几十个 token 的纯开销，
// 也把「这里真的静了一阵」这个信号稀释成了背景噪音。
// 90 秒是「一轮对话的正常节奏已经断了」的分界。
const timeMarkerThreshold = 90 * time.Second

// quoteWindow 是引用气泡还能生效的时间上限，也就是 qqapi.passiveTTL。
//
// 超过它锚点就过期，message_reference 取不到，q:true 会**静默失效**——
// 消息照发但没有气泡，而模型以为成功了。所以它是本项目里唯一一条
// 「不是我们能改的平台硬限」，提示词里必须如实写给模型。
const quoteWindow = 5 * time.Minute

// timeMarkerAt 返回这一行该有的时间标记，不需要时返回空串。
//
// 为什么要有它：模型原来只能看到一串没有时间的对话，它无从判断
// 「我准备回的那句话已经过去多久了」。而这恰好是引用气泡的唯一判据——
// 隔着半小时回一句，不引用的话群里根本看不出你在回哪句。
//
// 口径是「这一条距上一条隔了多久」，不是「距现在」：
// 前者直接对应「刚才那阵沉默」，后者会让每一行的标记都在缓慢变老，
// 把「群里的时间轴」和「此刻」这两件事搅在一起。首行没有上一条，
// 退化成跟现在比——那正好回答了「这些消息有多旧」。
//
// **「隔了多久」只在引用窗口内给**，超窗只给时刻。三个理由：
//  1. 超窗时那个数字对决策已经无用（引不到），只剩噪音
//  2. 让模型自己做减法判断「606 分钟 > 5 分钟吗」不可靠，
//     小模型算错量就会去硬填 q:true
//  3. 时刻配系统段里已有的「现在时间」，模型能一眼看出远近
//
// 跨天必须带日期：08:55 看到「22:49」分不清是今天还是昨天，
// 纯时刻会让跨夜的消息看起来像未来。日内则省掉日期（省 token）。
//
// 零值 TS（测试夹具、老记录）一律不打标记：渲染不出时间的行
// 不该得到一个凭空的时间，公元 1 年只会让模型困惑。
func timeMarkerAt(prev, cur, now time.Time) string {
	if cur.IsZero() {
		return ""
	}
	gap := now.Sub(cur)
	if !prev.IsZero() {
		gap = cur.Sub(prev)
	}
	// 时钟漂移或平台时间戳异常会算出负间隔，那不是「间隔很短」，是数据有问题。
	if gap < timeMarkerThreshold {
		return ""
	}
	stamp := cur.Format("15:04")
	// 跨天补日期：同一天内的「22:49」模型能靠系统段的现在时间自行定位，
	// 跨天的「22:49」它会当成还没到的将来。
	if !sameDay(cur, now) {
		stamp = cur.Format("01-02") + " " + stamp
	}
	if gap <= quoteWindow {
		return "[" + stamp + " 隔了" + humanGap(gap) + "] "
	}
	// 超窗：只给时刻，不给「隔了多久」。
	return "[" + stamp + "] "
}

// sameDay 判断两个时间是否同一天（本地时区）。
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// humanGap 把间隔说成人话。
//
// 只在 quoteWindow 内调用，所以只会出「分钟」这一档。
// 保留分档结构是为了万一将来窗口放宽，这里不用重写。
func humanGap(d time.Duration) string {
	switch {
	case d < 2*time.Hour:
		return fmt.Sprintf("%d分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d天", int(d.Hours()/24))
	}
}

// renderMemberNotes 把成员备注渲染出来，只带有备注的人。
// 上限 6 条：群大了以后这节会悄悄变成大头，而它的重要性远低于最近聊天。
const maxMemberNotes = 6

func renderMemberNotes(g *memory.Group) string {
	var sb strings.Builder
	n := 0
	for _, m := range g.Members() {
		if m.Note == "" || m.Name == "" {
			continue
		}
		sb.WriteString("- " + m.Name + "：" + m.Note + "\n")
		n++
		if n >= maxMemberNotes {
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// maxAliasLines 同名映射最多列几行。绝大多数人只有一个称呼，
// 真正需要说明的是那些「群里 @ 他显示一个名、他自己说话又显示另一个名」的。
const maxAliasLines = 6

// renderAliasHints 集中说明「谁还有别的称呼」。
//
// 为什么必须集中说明而不是每行标注：聊天记录里同一个人会以两个名字出现
// ——平台对「他自己发言」（author）和「别人 @ 他」（mentions）给的是两个
// 不同字段，前者通常是账号昵称、后者是群名片。真实案例：openid AAAA5555…
// author 给「群友乙」，mentions 给「群名片丁」。
//
// 不点破的话模型会把它们当成两个人，对「群名片丁」说的话
// 挂到「群友乙」名下，或者压根不理其中一边。
//
// 集中列一行而不是逐行插标注，是为了不往每一行聊天记录后面都塞括号——
// 那是几十行的额外 token，而有别名的人是极少数。
func renderAliasHints(g *memory.Group) string {
	var sb strings.Builder
	n := 0
	for _, m := range g.Members() {
		names := m.KnownNames()
		if len(names) < 2 || m.Name == "" {
			continue
		}
		// 现名在前，其余（群名片 + 历史旧称）在后
		sb.WriteString("- " + m.Name + "：也叫" + strings.Join(names[1:], "、") + "\n")
		n++
		if n >= maxAliasLines {
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// maxRoleLines 管理层最多列几行。绝大多数群就一个群主一两个管理员。
const maxRoleLines = 4

// renderMemberRoles 渲染群主与管理员，格式和 renderAliasHints 一致（- 名字：身份）。
//
// 为什么必须有它：平台每个事件都带 member_role（member/admin/owner），
// 但这个字段以前一路丢在 webhook 结构体里没人读，于是成员表里只有
// openid、名字、群名片、消息数——没有任何身份信息。
// 结果「艾特一下群主」对模型是无解的题：2026-10-05 生产实况，模型答
// 「不知道谁是群主」，还因为 JSON 输出残缺整条被按闭嘴处理。
//
// 只列群主和管理员：普通成员占了绝大多数，全列出来只是烧 token。
func renderMemberRoles(g *memory.Group) string {
	var sb strings.Builder
	n := 0
	for _, m := range g.Members() {
		if m.Name == "" {
			continue
		}
		switch m.Role {
		case "owner":
			sb.WriteString("- " + m.Name + "：群主\n")
		case "admin":
			sb.WriteString("- " + m.Name + "：管理员\n")
		default:
			continue
		}
		n++
		if n >= maxRoleLines {
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func joinLines(items []string) string {
	var sb strings.Builder
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		sb.WriteString("- " + it + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
