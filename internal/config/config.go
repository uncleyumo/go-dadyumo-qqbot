// Package config 负责 config.json 的加载、校验、热更新与原子持久化。
// 所有 Web 管理端的修改都通过 Store 生效并写回磁盘，保证「exe + config.json」的交付形态不变。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dadyumo/internal/logx"
)

// Config 根配置
type Config struct {
	// Paused 总开关：true = 机器人彻底关闭，不接收、不记录、不调模型。
	//
	// 与 Schedule.Enabled 不是一回事：那个答的是「这会儿人在不在电脑前」，
	// 按概率放行、随时段自己变；这个答的是「这台机器人现在上不上班」，
	// 按下就锁死，用于同机部署第二台机器人、或临时让它闭嘴的场景。
	//
	// 放在根配置而不是某个子配置里，是因为它管的不止对话：表情包优选的后台
	// 定时器、语音转写、视频理解都得跟着停，那些分属不同子系统。
	//
	// ⚠️ 这个字段**不允许在管理端的配置表单里修改**：handleSaveConfig 会用服务端的
	// 现值覆盖客户端传来的值。否则页面里那份「加载时快照」（paused:false）会在你
	// 关停之后、随便点一次「保存并生效」时把它**悄悄开机**——现场表现是
	// 「我明明关了它怎么又说话了」，且没有任何报错。只能走 POST /api/pause。
	//
	// 闸门分布在 brain.Engine.OnMessage / fire / maybeSummarize 与
	// memepool.Curator.runOnce 四处，见各处的注释。
	Paused   bool           `json:"paused"`
	Server   ServerConfig   `json:"server"`
	QQ       QQConfig       `json:"qq"`
	Admin    AdminConfig    `json:"admin"`
	LLM      LLMConfig      `json:"llm"`
	Persona  PersonaConfig  `json:"persona"`
	Brain    BrainConfig    `json:"brain"`
	Master   MasterConfig   `json:"master"`
	Speak    SpeakConfig    `json:"speak"`
	Groups   []GroupConfig  `json:"groups"`
	Storage  StorageConfig  `json:"storage"`
	Schedule ScheduleConfig `json:"schedule"` // 在线时段调度（省钱 + 拟真作息）
	ASR      ASRConfig      `json:"asr"`      // 语音转文字（视频音轨 + 群语音兜底）
	Compact  CompactConfig  `json:"compact"`  // 超长转写压缩（纯文本小模型，不进中央调度器）
	MemePool MemePoolConfig `json:"meme_pool"` // 表情包池（MinIO 存储 + 工具调用）
}

// MemePoolConfig 表情包池配置。
//
// 默认关闭。它依赖 MinIO 与 QQ 富媒体上传两条外部链路，
// 任何一条没通都不该让机器人带着这个功能上线。
type MemePoolConfig struct {
	Enabled bool   `json:"enabled"`
	MinIO   MinIOConfig `json:"minio"`
	// MaxPool 池子上限。每次工具调用会把整个池子返回给模型、直接烧 token，
	// 所以这个数不是「能存多少」而是「模型一次能扫完并选准」的上限。
	MaxPool int `json:"max_pool"`
	// MaxResidencyDays 单图最长驻留天数。到期无条件淘汰。
	// 硬要求是「没有任何图永不过期」——否则池子会凝固成一堆老面孔，
	// 新图永远挤不进来。
	MaxResidencyDays int `json:"max_residency_days"`
	// OptIntervalHours 优选任务间隔（小时）。
	OptIntervalHours int `json:"opt_interval_hours"`
	// MaxToolRounds 工具调用轮数上限。
	// 注意这不是「正常要走几轮」——正常情况一轮工具调用就够，
	// 没有 function_call 就是交付轮。这个值只用来兜住发癫的模型。
	MaxToolRounds int `json:"max_tool_rounds"`
	// MaxImagesPerReply 单条回复最多发几张图。
	// 硬上限受制于 QQ：同一条用户消息最多回 5 次，文字和图共用这个额度。
	MaxImagesPerReply int `json:"max_images_per_reply"`
}

// MinIOConfig 对象存储连接参数
type MinIOConfig struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	// StateFile 池子元数据的落盘路径（相对 data 目录）
	StateFile string `json:"state_file"`
}

// ASRConfig 语音识别配置。
//
// 用途只有一处：把群友发的视频里的音轨转成文字，让机器人「听得到」视频里说了什么。
// SiliconFlow 的 SenseVoiceSmall 对免费用户 0 元、中文效果好，是国内场景的首选。
// APIKey 留空时自动跳过转写（视频仍然抽帧看画面），不影响其它功能。
type ASRConfig struct {
	Provider string `json:"provider"` // siliconflow（目前唯一实现）/ none
	APIKey   string `json:"api_key"`  // 留空 = 不做语音转写
	Model    string `json:"model"`    // 默认 FunAudioLLM/SenseVoiceSmall
}

// CompactConfig 超长转写压缩配置。
//
// 两分钟的语音/视频能转出上千字，原样塞进提示词既烧 token 又稀释重点，
// 所以超过 ThresholdChars 就交给一个极致便宜的纯文本小模型压成要点。
// 这个模型**故意不进 llm 中央调度器**：它不支持多模态，质量也只配干压缩
// 这种粗活，进调度器会被当成大脑用。APIKey 留空 = 不压缩（长转写硬截断）。
type CompactConfig struct {
	BaseURL        string `json:"base_url"`        // OpenAI 风格接口地址（不含 /chat/completions）
	APIKey         string `json:"api_key"`         // 留空 = 关闭压缩
	Model          string `json:"model"`           // 默认 qwen3.5-flash
	ThresholdChars int    `json:"threshold_chars"` // 转写超过这么多字才压缩
	TargetChars    int    `json:"target_chars"`    // 压缩目标字数
	MaxOutTokens   int    `json:"max_out_tokens"`
}

// MasterConfig 开发者身份。
//
// ⚠️ **字段名保留 master 是历史原因**（改动会让现网 config.json 里已绑定的
// openids 全部失效），但**语义已改为「开发者」**：所有面向人的文案、提示词、
// 管理端标签都不再使用「主人」这个词。
//
// # 认人和特权是两件事
//
// **DevEnabled 是总开关，默认关闭。** 关闭时开发者的消息与群友**完全一样**：
// 不过在线率闸、不豁免预算、不用快节奏、提示词里不标身份。
// 这个开关是本项目「像人」这个目标的关键——开着它时，作为开发者的人享受
// 更高的优先级，于是**你看到的「它今天话好多」可能只是它对你话多**，
// 拿这个有偏的样本去判断行为是查不出问题的。
//
// 关闭它**不影响绑定**：认主口令、管理端「设为开发者」照常可用，
// OpenIDs 列表照常维护。绑定管的是「认得出谁」，开关管的是「区别对待谁」，
// 两者可以也应该分开。
type MasterConfig struct {
	// DevEnabled 开发者特权总开关。**默认关**。
	// 关着时开发者的消息和群友一个待遇；开着时它会无条件回应、超预算也照应、
	// 说话节奏更快，并在提示词里被标出来。切换后已进攒批窗口的那批不受影响
	// （以消息到达时刻为准），这是正确行为——攒批本来就有 10~18 秒的自然延迟。
	DevEnabled bool `json:"dev_enabled"`

	Nickname    string   `json:"nickname"`     // 开发者昵称，如「张三」；留空则提示词里只说「你开发者」
	QQ          string   `json:"qq"`           // 真实 QQ 号，仅作备注展示
	OpenIDs     []string `json:"openids"`      // 已绑定的 openid 列表
	BindToken   string   `json:"bind_token"`   // 认主口令，群里发「#认主 <token>」即绑定
	BindEnabled bool     `json:"bind_enabled"` // 是否开启口令绑定（绑定成功后建议关掉）
}

// SpeakConfig 发言节奏：把一段话拆成若干条、按真人速度逐条发出去
type SpeakConfig struct {
	MaxSegments  int `json:"max_segments"`   // 一次最多发几条（QQ 被动消息同一 msg_id 上限 5 次）
	MinDelayMS   int `json:"min_delay_ms"`   // 条间最小间隔
	MaxDelayMS   int `json:"max_delay_ms"`   // 条间最大间隔
	PerCharMS    int `json:"per_char_ms"`    // 按字数追加的「打字时间」
	MaxSegChars  int `json:"max_seg_chars"`  // 单条字数上限，超过则按标点再拆
	FirstDelayMS int `json:"first_delay_ms"` // 第一条之前的「看到消息到开始回」的思考时间

	// EagerScale 被 @ / 被叫名字 / 主人说话时的节奏缩放系数。
	//
	// 为什么要它：这三种场景下群友在等，晚几秒就当你掉线了；
	// 而自己插话时没人等，可以按真人节奏慢慢来。
	// 不配第二套 min/max/per_char/lead 四件套——那要 4 个字段、4 处 UI、4 处校验，
	// 实际只需要一个「急不急」的档位开关。
	EagerScale float64 `json:"eager_scale"`

	// TailRamp 多条连发时，后面几条的间隔按 (1-TailRamp×序号) 递减。
	//
	// 为什么需要它：真人连着发消息是**越说越快**的，最后一两句几乎连发。
	// 而固定间隔下 5 条 × 2.6 秒 = 13 秒，观感是「一条一条慢慢爬」——
	// 那比秒回更像机器人，因为它规律得不自然。
	// 递减排掉了这个规律性：前面慎重、后面加速。
	// 0 = 不递减（固定间隔）。
	//
	// 注意它只影响**条与条之间**，不影响第一条的首字延迟。
	// 首字延迟由 FirstDelayMS 管，而那个值在本项目里其实意义有限——
	// 攒批窗口（debounce_sec 8 秒）加 LLM 调用（实测均延迟 4.4 秒）
	// 决定了首字最快也在 12 秒后，600ms 淹没在里面。
	TailRamp float64 `json:"tail_ramp"`
}

// ServerConfig HTTP 服务监听配置
type ServerConfig struct {
	PublicAddr   string `json:"public_addr"`    // QQ 回调监听地址，如 0.0.0.0:8080
	WebhookPath  string `json:"webhook_path"`   // 回调路径，默认 /qq/callback
	AdminAddr    string `json:"admin_addr"`     // 管理端监听地址，建议 127.0.0.1:8081
	AdminEnabled bool   `json:"admin_enabled"`  // 是否启用管理端
	AdminPrefix  string `json:"admin_base_url"` // 管理端根路径，默认 /admin
}

// QQConfig QQ 开放平台凭证
type QQConfig struct {
	AppID            string `json:"app_id"`
	AppSecret        string `json:"app_secret"`
	Sandbox          bool   `json:"sandbox"`           // true 走 sandbox.api.sgroup.qq.com
	RequireSignature bool   `json:"require_signature"` // 是否强制校验回调签名，生产必须 true
	SendTimeoutMS    int    `json:"send_timeout_ms"`   // 发消息超时
	PreferPassive    bool   `json:"prefer_passive"`    // 优先使用被动消息（挂 msg_id）
	// SelfOpenID 机器人自己的 openid。群主开「全量消息」后，@机器人 只在 content 里
	// 留 <@openid> 标签，平台不单独告知"被 @"，需要拿它来比对。留空则不判标签。
	// 获取方式：被 @ 一次后从日志「群消息」的 text 里能看到这个标签。
	SelfOpenID string `json:"self_openid"`
}

// AdminConfig 管理端登录配置
type AdminConfig struct {
	Username       string `json:"username"`
	PasswordBcrypt string `json:"password_bcrypt"` // bcrypt 哈希，空则首次启动用 password_plain 初始化
	PasswordPlain  string `json:"password_plain"`  // 仅用于初始化，启动后应删除
	SessionSecret  string `json:"session_secret"`  // cookie 签名密钥
}

// LLMConfig 中央调用器配置
type LLMConfig struct {
	Endpoints   []Endpoint `json:"endpoints"`
	MaxAttempts int        `json:"max_attempts"` // 单次请求最多尝试多少个目标，默认 4
	// 排序权重，用于健康度打分
	Weights WeightConfig `json:"weights"`
}

// WeightConfig 健康度排序权重
type WeightConfig struct {
	TTFT     float64 `json:"ttft"`     // 首字延迟权重
	Failure  float64 `json:"failure"`  // 失败率权重
	Latency  float64 `json:"latency"`  // 端到端延迟权重
	Cooldown float64 `json:"cooldown"` // 冷却剩余惩罚权重
}

// APIType 接入点的协议方言。
// OpenAI 生态现在有两套主流格式：
//   - chat_completions：POST {base}/chat/completions，经典格式
//   - responses：POST {base}/responses，新的 Responses API（OpenRouter 也提供）
//
// 两者路径与报文结构都不同，必须按接入点区分，否则会拿到 404。
const (
	APIChatCompletions = "chat_completions"
	APIResponses       = "responses"
)

// Endpoint 一个接入点：一个 key + 若干 OpenAI 格式模型
type Endpoint struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BaseURL   string  `json:"base_url"` // 如 https://openrouter.ai/api/v1
	APIKey    string  `json:"api_key"`
	APIType   string  `json:"api_type"` // chat_completions（默认） / responses
	Enabled   bool    `json:"enabled"`
	TimeoutMS int     `json:"timeout_ms"`
	Models    []Model `json:"models"`
}

// Model 接入点下的一个模型
type Model struct {
	ID      string `json:"id"`    // 如 qwen/qwen3.8-27b:free
	Label   string `json:"label"` // 展示名
	Enabled bool   `json:"enabled"`
	// Priority 优先级，数值越大越优先（默认 0）。
	// 主力模型给 1，免费兜底给 0：只要主力健康就永远走主力，
	// 主力连续失败时扣分会盖过优先级加成，流量自动切到兜底。
	Priority int  `json:"priority"`
	MaxCtx   int  `json:"max_ctx"` // 上下文上限，用于裁剪历史
	MaxOut   int  `json:"max_out"` // 最大输出 token
	Stream   bool `json:"stream"`  // 是否用流式（用于测 TTFT，仍整体返回）
	// Vision 该模型是否吃图片（多模态）。不勾的模型，带图请求会自动降级为纯文本，
	// 免得上游直接报 400 浪费一次调用。
	Vision bool `json:"vision"`
}

// PersonaConfig 人设
type PersonaConfig struct {
	Name         string   `json:"name"`
	Background   string   `json:"background"`
	Style        string   `json:"style"`
	RoastRules   []string `json:"roast_rules"`
	RedLines     []string `json:"red_lines"`
	Catchphrases []string `json:"catchphrases"`
	MaxChars     int      `json:"max_chars"` // 单条发言最大字数

	// ReplyRules 是「怎么回话」的正向分寸。
	//
	// 为什么必须独立成节：它是**正向**指导（该接话时怎么接），
	// 而 RoastRules / SilenceRules / RefuseRules / RedLines 全是**禁令**。
	//
	// 混在一张卡里的代价是实打实的（2026-10-05 用户反馈）：
	// 「很多对于回复的指导找不到地方，放哪里都不合适，只得拆进行为边界中」。
	// 根因是没有正向那一格——那些规则是按「什么时候不做什么」组织的，
	// 于是「该说什么、怎么说」这类指导只能硬塞进去，
	// 而模型读到一整节禁令时，会把它当成消极清单。
	ReplyRules []string `json:"reply_rules"`

	// 以下是「活人感」的三块关键约束，缺一个就会退化成问答机器人
	SilenceRules []string `json:"silence_rules"` // 什么时候该闭嘴
	RefuseRules  []string `json:"refuse_rules"`  // 什么时候该拒绝（被当工具使唤、恶意调戏）

	// FallbackLines 是「所有模型都拒绝这轮内容」时随机发一句的兜底话术。
	//
	// **刻意不复用 Catchphrases**：那一项会被拼进系统提示词的【你的口头禅】，
	// 拿来当兜底池等于把这些句子同时变成模型日常的口头禅——
	// 而人设 v2（2026-10-04）费力把 catchphrases 清空过，正是为了不让模型
	// 照抄固定串（生产实测「绷」7 天 15 次、「图」占 9%）。
	// 兜底话术要的是「有事才说一句」，与日常口头禅是两种用途，必须分开。
	FallbackLines []string `json:"fallback_lines"`

	// BusyLines 同样是「全拒时发一句」，但只在**被指名艾特**的那一轮用。
	//
	// 与 FallbackLines 分开是因为语义不同，混池必然错配：
	// 被人点名却回一句「牛逼」很怪，回「在忙」才自然；
	// 而没人点名时说「在忙」又莫名其妙。两池互斥，一轮只发一句。
	BusyLines []string `json:"busy_lines"`
	// 这里曾有一个 Loyalty（对开发者的态度），2026-10-03 删掉。
	// 它在固定段无条件注入，等于开了一个绕过特权的泄漏口：即使把
	// dev_enabled 关掉，模型仍能从「那是把你做出来的人，给他点面子」里
	// 知道谁是开发者，而程序侧已按普通人处理——两边不一致，模型会照着
	// 提示词偏向那个人。开发者身份现在**只由 master.dev_enabled 控制**。
}

// BrainConfig 决策引擎
type BrainConfig struct {
	DailyBudget  int     `json:"daily_budget"`  // 每日 LLM 调用预算
	MaxHistory   int     `json:"max_history"`   // 短期记忆条数
	Temperature  float64 `json:"temperature"`
	MaxOutTokens int     `json:"max_out_tokens"`

	// 注意：这里曾有一个 ImpulseThreshold（冲动值阈值），
	// 连同整套权重机制已于 2026-10-03 废除。理由见 internal/brain 的包注释——
	// 它是在替模型判断「这条值不值得回」，而真人没有这个内心过程。
	// 现在控制「说不说话」的是 schedule（在线率：「在不在电脑前」），
	// 剩下的只有静默期、最小发言间隔、每日预算这三道与技术性限流有关的闸。

	// 攒批（debounce）：新消息到了不立刻问模型，先等一小会儿看有没有人接着说。
	// 这既是「真人反应需要时间」的拟真，也是控制调用次数的最主要手段。
	DebounceSec       int `json:"debounce_sec"`        // 攒批基础静默窗口（秒）
	DebounceJitterSec int `json:"debounce_jitter_sec"` // 在其上叠加的随机抖动（秒）

	// 上下文预算：给历史留多少 token。
	// 免费模型上下文可能只有 32K，甚至更小，这里必须做硬裁剪，
	// 否则一次请求就能把额度烧穿或直接被上游拒绝。
	MaxCtxTokens int `json:"max_ctx_tokens"`

	// 这里曾有一个 MinSpeakIntervalSec（两次发言的最小间隔），2026-10-03 删掉。
	//
	// 它有两个致命问题，删掉比修好更划算：
	//  1. **它会丢消息**。fire() 在函数开头就把攒批状态清空了（st.newCount=0、
	//     st.timer=nil），走到这道闸时 return，而 defer 里的补救条件
	//     `pending && timer==nil && newCount>0` 不成立 → 那批消息彻底消失，
	//     从来没进过模型。生产实况：17:36:41 进的群消息、17:36:58 被这道闸拦掉。
	//  2. **与攒批窗口语义重复**。窗口本来就是 10~18 秒，间隔设 15 秒，
	//     两者几乎相等，叠加后变成「每两轮就可能扔一批」。
	//
	// 防刷屏另有三道且都不丢消息：拆句发送的分段延迟、speak.max_segments
	// 一次最多 N 条、QQ 平台 passiveMaxSends=5 的硬限制。

	// 滚动摘要：每积累这么多条新消息，就把更早的部分压成一段「前文提要」。
	// 这是让上下文在长期运行下不失控的关键——没有它，历史只会越喂越多，
	// 要么烧穿 token 预算，要么被模型上下文上限截断。
	// 设为 0 表示关闭（历史纯靠滑动窗口，适合不太活跃的群）。
	SummaryEvery int `json:"summary_every"`

	// MaxImagesPerCall 单次决策最多带几张图进上下文。
	// 图片按 token 计价比文本贵得多（一张普通截图几百到上千 token），
	// 群里刷图时必须封顶，否则几张图就能吃掉整轮预算。
	MaxImagesPerCall int `json:"max_images_per_call"`

	// MaxFacts 每个群最多记住多少条长期要点（模型自己写的结论，
	// 比如「他叫老张，在苏州做监理」）。满了按 LRU 淘汰最旧没被改写的那条。
	//
	// 与 MaxHistory（短期原话条数）是**两个独立的池子**，别混在一起算：
	// 30 条原话和 24 条要点是两回事——前者存「谁说了什么」，后者存「结论」。
	//
	// 要点会进提示词，所以它同时占 token 预算。建议 12~30，
	// 再多容易把模型带跑偏（它会把每条都当成事实）。
	// 2026-10-03 之前这个上限硬编码在 memory 包里（24），配不了。
	MaxFacts int `json:"max_facts"`

	// ImageMaxSide 图片长边压缩上限（像素）。
	// 群里的手机截图动辄 1080×2400、PNG 一两 MB，原图内联纯属烧钱；
	// 压到长边 1024 的 JPEG 通常只剩一两百 KB，模型照样看得清。
	// 设为 0 表示关闭压缩（原图直传，仅用于调试）。
	ImageMaxSide int `json:"image_max_side"`

	// GIFFrames 一张动图最多按总时长均匀拆成几帧送给模型看。
	//
	// 为什么需要：GIF 在 imgproc 里被显式豁免压缩（原样透传，见那里的理由），
	// 发出去的是完整的多帧字节，而**上游 vision 模型只看得到首帧**——
	// 群里大量表情包是动图，模型只能看到开场。视频早就抽帧了
	// （brain/video.go 的 extractFrames），GIF 一直没有对应实现。
	//
	// **这个额度独立于 MaxImagesPerCall**，不占普通图片的名额：
	// 一张动图不该把同批次里其它静图全挤掉（用户 2026-10-05 定的）。
	// 代价是最坏一轮 MaxImagesPerCall + GIFFrames 张图，token 约为原来的 2.7 倍。
	//
	// 设为 0 表示不拆帧，原样透传（与本改动之前的行为一致）。
	// 表情包池的入池路径**不受影响**：它拿的始终是原始 GIF，
	// 拆帧只作用于「给模型看」这一侧。
	GIFFrames int `json:"gif_frames"`

	// 视频理解：≤ VideoMaxSec 的视频抽 VideoFrames 帧进上下文（复用图片管道），
	// 音轨交给 ASR 转文字；超过 VideoMaxSec 的直接不处理——超长视频的转写
	// 既费流量又费 ASR 额度，而群友互相分享的视频几乎都在半分钟以内。
	// 被要求分析超长视频时，提示词会引导它自然搪塞（没流量/懒得看），不暴露处理方式。
	VideoMaxSec    int `json:"video_max_sec"`    // 视频时长上限（秒），超过就只留一句搪塞提示
	VideoMaxMB     int `json:"video_max_mb"`     // 视频下载体积上限（MB），防超大文件拖垮机器
	VideoFrames    int `json:"video_frames"`     // 一个视频抽几帧（0 = 关闭视频理解）
	VideoFrameSide int `json:"video_frame_side"` // 帧的长边像素，视频帧压得比截图更狠（信息密度低）

	// 环境感知：让机器人知道自己活在哪个时间、哪个地方。
	// 天气一天查一次（换日后的第一次发言时），地点留空则不查。
	WeatherLat   float64 `json:"weather_lat"`   // 纬度
	WeatherLon   float64 `json:"weather_lon"`   // 经度
	WeatherPlace string  `json:"weather_place"` // 地名，进提示词（如「杭州西湖区」）；留空则不查天气
	// SpecialDays 额外的特殊日子，key 为 MM-DD，value 为叫法（如「12-25」「你的生日」）。
	// 内置了常见公历节日、周末和疯狂星期四，这里配的是你自己的纪念日。
	SpecialDays map[string]string `json:"special_days"`
}

// baseOnlineRateDefault 非窗口时段（「平时」）的默认在线率。
// 0.2 是「省钱优先」的取值，白天会明显变安静；默认档白天有独立窗口，
// 所以这个值只影响凌晨那几个小时，正好也是没人说话的时候。
const baseOnlineRateDefault = 0.20

// ScheduleWindow 一个在线时段：从几点到几点，这个时段里以多大概率「在线」。
//
// 为什么是在线率而不是开关：硬开关会出现「8:30 整它突然不说话了」这种机械感，
// 概率则表现成「白天偶尔冒泡、夜里话多一点」，更像真人的作息浮动。
type ScheduleWindow struct {
	From  string  `json:"from"`  // HH:MM，支持 24:00
	To    string  `json:"to"`    // HH:MM，支持跨午夜（如 18:00-02:00）
	Rate  float64 `json:"rate"`  // 0~1 在线率
	Label string  `json:"label"` // 展示用说明
	// Days 限定这条窗口在星期几生效：逗号分隔的 1~7（1=周一 … 7=周日），
	// 支持区间如 "1-5"。空 = 每天，也就是不写这个字段的旧配置语义完全不变。
	//
	// 存在的唯一理由：DeepSeek 的峰时段只覆盖工作日，而 deepseek_offpeak 档
	// 就是照着它的峰谷定价做的（见 brain.schedulePresets）。别的档位用不上它。
	// 写错格式（如 "mon-fri"）时该窗口永不命中——见 brain.daysMatch 的说明。
	Days string `json:"days,omitempty"`
}

// ScheduleConfig 在线时段调度。
//
// 内置档位里有 deepseek_offpeak（谷时段 80%、峰时段回落 base_rate），但**不建议**当长期默认：
// DeepSeek 的峰时段是工作日 09:00-12:00 与 14:00-18:00，正好是群里最热闹的时候；
// 把这几段的在线率压到 20% 只会让机器人在你真正在聊的时段装死。所以默认档用 daytime——
// 白天几乎全在线，凌晨（本来就没人的时候）才降下来，省钱和体感两头都照顾到。
//
// ⚠️ 「全天」有两个档，别按名字猜：always 是 0.90（日常档，仍有 10% 概率不接话），
// always_strict 才是 1.00（调试/特殊场景，任何时候都必应）。
// 2026-10-03 拆分——之前 always 的显示名是「全天在线」，暗示 100% 实际 0.90。
type ScheduleConfig struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"` // daytime / deepseek_offpeak / night_owl / always(0.9) / always_strict(1.0) / random_daily / custom
	// AtGraceSec 被 @ 之后的实时宽限：这段时间内不再按概率过滤，一律实时回应。
	// 被人点名了还按概率装死，是最伤体验的。
	AtGraceSec int `json:"at_grace_sec"`
	// BaseRate 没命中任何窗口时的在线率。这是「平时活跃度」的总旋钮：
	// 觉得机器人太安静就调高它，觉得话太多/太费钱就调低。
	//
	// ⚠️ 自定义窗口（mode=custom）里把某条 rate 填 0 **不是「关掉这个时段」**，
	// 而是回落成 BaseRate（未配置时为 0.20）。要真静音得把 BaseRate 也调下去。
	BaseRate float64          `json:"base_rate"`
	Windows  []ScheduleWindow `json:"windows"` // 仅 mode=custom 时生效
}

// GroupConfig 群配置
type GroupConfig struct {
	OpenID  string `json:"openid"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	// 这里曾有两个字段 GroupMinSpeakIntervalSec 与 ImpulseBias，都是死字段：
	// 全仓没有任何读取点。ImpulseBias 随冲动值机制一起废掉了；
	// GroupMinSpeakIntervalSec 则是在 brain.min_speak_interval_sec 也被删掉之后
	// 才彻底没人读的（2026-10-03）。两个都清干净——留着它们只会让后人以为
	// 「群里可以单独配一个门限」，然后花时间找它为什么不生效。
}

// StorageConfig 存储
type StorageConfig struct {
	DataDir string `json:"data_dir"`
}

// Store 持有运行中配置，支持热替换
type Store struct {
	mu     sync.RWMutex
	cfg    *Config
	path   string
	saveMu sync.Mutex
	rev    int64 // 配置版本号，每次改动自增；管理端靠它判断要不要重画表单
}

// NewDefaultStore 造一个带默认值的 Store，不碰磁盘。
//
// 给测试用：配置驱动的逻辑（表情包上限、轮次上限那些）要能直接构造出来，
// 不必为每个用例写一份临时 config.json。
func NewDefaultStore() *Store {
	cfg := Default()
	// Validate 会返回 error，但默认配置本来就是合法的——
	// 这里忽略返回值只为给测试一个开箱即用的 Store。
	_ = cfg.Validate()
	return &Store{cfg: cfg}
}

// NewStoreFrom 按默认值构造后再改几项，不落盘。
//
// Update 会写文件，测试里不能用它；这个是纯内存版本，
// 用来造「表情包开/关」「上限调到 1」这类变体。
func NewStoreFrom(tweak func(*Config)) *Store {
	cfg := Default()
	if tweak != nil {
		tweak(cfg)
	}
	_ = cfg.Validate()
	return &Store{cfg: cfg}
}

// Load 从文件加载配置，缺失字段用默认值补齐
func Load(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := Default()
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg, path: path}, nil
}

// Default 返回带默认值的配置
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			PublicAddr:   "0.0.0.0:8080",
			WebhookPath:  "/qq/callback",
			AdminAddr:    "127.0.0.1:8081",
			AdminEnabled: true,
			AdminPrefix:  "/admin",
		},
		QQ: QQConfig{
			RequireSignature: true,
			SendTimeoutMS:    10000,
			PreferPassive:    true,
		},
		Admin: AdminConfig{Username: "admin"},
		LLM: LLMConfig{
			MaxAttempts: 4,
			Weights: WeightConfig{
				TTFT:     0.45,
				Failure:  0.30,
				Latency:  0.15,
				Cooldown: 0.10,
			},
		},
		Brain: BrainConfig{
			DailyBudget:         300,
			MaxHistory:          30,
			Temperature:         0.95,
			MaxOutTokens:        400,
			DebounceSec:         10,
			DebounceJitterSec:   8,
			MaxCtxTokens:        4000,
			SummaryEvery:        24,
			MaxImagesPerCall:    3,
			MaxFacts:            24,
			ImageMaxSide:        1024,
			VideoMaxSec:         120,
			VideoMaxMB:          50,
			VideoFrames:         2,
VideoFrameSide:      768,
		// 天气默认关闭：留空即不查（refreshEnv 只在 WeatherPlace 非空时请求）。
		// 想开天气就填自己所在地的经纬度与地名，见 config.json.example。
		WeatherPlace: "",
	},
	Persona: PersonaConfig{MaxChars: 120},
	Master:  MasterConfig{BindEnabled: true},
		Speak: SpeakConfig{
			MaxSegments: 5,
			// 间隔按真人速度给：真人从打完上一句到发出下一句，
			// 犹豫 1 秒左右、再按每字 150ms 打字。原来的 300/1100/45
			// 是「机器人打字机」的速度——肉眼可辨的节奏信号只有条间间隔，
			// 这一项不像人，一眼就能看出来。
			MinDelayMS:   900,
			MaxDelayMS:   2600,
			PerCharMS:    150,
			MaxSegChars:  40,
			FirstDelayMS: 600,
			EagerScale:   0.5,
			// 递减系数 0.18：第 4 条间隙 ×(1-0.18×3)=0.46，
			// 5 条总耗时从 13 秒降到约 8 秒，且尾部呈连发感。
			TailRamp: 0.18,
		},
		Storage: StorageConfig{DataDir: "./data"},
		Schedule: ScheduleConfig{
			Enabled:    true,
			Mode:       "daytime",
			AtGraceSec: 300,
			BaseRate:   baseOnlineRateDefault,
		},
		Compact: CompactConfig{
			BaseURL:        "https://slb-v1.api.fan/v1",
			Model:          "qwen3.5-flash",
			ThresholdChars: 300,
			TargetChars:    150,
			MaxOutTokens:   300,
		},
		MemePool: MemePoolConfig{
			// 默认关闭：依赖 MinIO 与 QQ 富媒体上传两条外部链路，
			// 哪条没通都不该带着它上线。
			Enabled:            false,
			MaxPool:            20,
			MaxResidencyDays:   30,
			OptIntervalHours:   6,
			MaxToolRounds:      3,
			MaxImagesPerReply:  1,
			MinIO: MinIOConfig{
				Region:    "us-east-1",
				StateFile: "memes.json",
			},
		},
	}
}

// Validate 校验必要字段
func (c *Config) Validate() error {
	if c.QQ.AppID == "" {
		return errors.New("qq.app_id 不能为空")
	}
	if c.QQ.AppSecret == "" {
		return errors.New("qq.app_secret 不能为空")
	}
	if c.Server.PublicAddr == "" {
		c.Server.PublicAddr = "0.0.0.0:8080"
	}
	if c.Server.WebhookPath == "" {
		c.Server.WebhookPath = "/qq/callback"
	}
	if c.LLM.MaxAttempts <= 0 {
		c.LLM.MaxAttempts = 4
	}
	if c.Brain.MaxHistory <= 0 {
		c.Brain.MaxHistory = 40
	}
	if c.Persona.MaxChars <= 0 {
		c.Persona.MaxChars = 220
	}
	if c.Storage.DataDir == "" {
		c.Storage.DataDir = "./data"
	}
	if c.Brain.DebounceSec <= 0 {
		c.Brain.DebounceSec = 10
	}
	if c.Brain.MaxCtxTokens <= 0 {
		c.Brain.MaxCtxTokens = 4000
	}
	if c.Brain.SummaryEvery <= 0 {
		c.Brain.SummaryEvery = 24
	}
	if c.Brain.MaxImagesPerCall <= 0 {
		c.Brain.MaxImagesPerCall = 3
	}
	// 长期要点上限。<=0 时回落 24（与 memory.MaxFacts 的初值一致）。
	// 上限不设天花板：要点本身是短文本，token 成本远低于原话，
	// 而多记几条换来的是「模型知道群里的事」，这个交换划算。
	if c.Brain.MaxFacts <= 0 {
		c.Brain.MaxFacts = 24
	}
	if c.Brain.ImageMaxSide <= 0 {
		c.Brain.ImageMaxSide = 1024
	}
	// GIFFrames 刻意**不兜底**。其余字段都是「<=0 就给默认值」，
	// 这里不能照做：0 在这一项上是「不拆帧」的明确语义，
	// 而 Go 分不出「配置里没写这个键」与「写了 0」。
	// 若按惯例兜底成 5，想关掉拆帧的人就永远关不掉；
	// 若兜底成 0，老配置升级上来会静默保持旧行为、看着像改动没生效。
	// 所以约定：默认值写在 config.json.example 里（当前 5），
	// 缺这一键的老配置一律视为 0（不拆），改行为要在配置里显式加。
	//
	// 它**不进管理控制台**：与 max_images_per_call 不同，这一项基本不用调，
	// 而控制台每个表单项都要在 index.html 的三处接上（框/读回/存回）并配套
	// wiring 测试。为一个几乎不改的值付这个维护成本不划算。
	if c.Brain.VideoMaxSec <= 0 {
		c.Brain.VideoMaxSec = 120
	}
	if c.Brain.VideoMaxMB <= 0 {
		c.Brain.VideoMaxMB = 50
	}
	if c.Brain.VideoFrames <= 0 {
		c.Brain.VideoFrames = 2
	}
	if c.Brain.VideoFrameSide <= 0 {
		c.Brain.VideoFrameSide = 768
	}
	if c.ASR.Provider == "" {
		c.ASR.Provider = "siliconflow"
	}
	if c.ASR.Model == "" {
		c.ASR.Model = "FunAudioLLM/SenseVoiceSmall"
	}
	if c.Compact.BaseURL == "" {
		c.Compact.BaseURL = "https://slb-v1.api.fan/v1"
	}
	if c.Compact.Model == "" {
		c.Compact.Model = "qwen3.5-flash"
	}
	if c.Compact.ThresholdChars <= 0 {
		c.Compact.ThresholdChars = 300
	}
	if c.Compact.TargetChars <= 0 {
		c.Compact.TargetChars = 150
	}
	if c.Compact.MaxOutTokens <= 0 {
		c.Compact.MaxOutTokens = 300
	}
	if c.Schedule.Mode == "" {
		c.Schedule.Mode = "daytime"
	}
	if c.Schedule.AtGraceSec <= 0 {
		c.Schedule.AtGraceSec = 300
	}
	if c.Schedule.BaseRate <= 0 {
		c.Schedule.BaseRate = baseOnlineRateDefault
	} else if c.Schedule.BaseRate > 1 {
		c.Schedule.BaseRate = 1
	}
	// 天气：既没填地名也没填坐标时保持关闭，不要回填任何预设地点——
	// 否则每个没配天气的用户都会拿到同一个地方的天气，提示词里的环境信息是错的。
	if c.Brain.WeatherPlace == "" {
		c.Brain.WeatherLat = 0
		c.Brain.WeatherLon = 0
	}
	if c.Speak.MaxSegments <= 0 {
		c.Speak.MaxSegments = 5
	}
	if c.Speak.MaxSegChars <= 0 {
		c.Speak.MaxSegChars = 40
	}
	if c.Speak.MaxDelayMS <= 0 {
		c.Speak.MaxDelayMS = 1100
	}
	if c.Speak.MinDelayMS <= 0 {
		c.Speak.MinDelayMS = 300
	}
	// 节奏的两个缩放系数：0 是「关闭」，>1 是荒谬值，一律按默认档补。
	// 这两个值是**乘数**，落到 0 意味着「间隔恒为 0」（机器人连发），
	// 落到 >1 意味着「慢到群里以为掉线」，都不是能接受的配置。
	if c.Speak.EagerScale <= 0 || c.Speak.EagerScale > 1 {
		c.Speak.EagerScale = 0.5
	}
	if c.Speak.TailRamp < 0 || c.Speak.TailRamp > 0.9 {
		// 上限 0.9 而不是 1：留一点间隔给最后一条。
		// 系数 =1 时最后一条的间隔是 0，等于两条消息同一毫秒发出去——
		// 平台会把它们合并成一条，那模型本来想分开发的东西就没了。
		c.Speak.TailRamp = 0.18
	}
	// 表情包池的默认值。enabled 不给默认值——它必须由人显式打开。
	if c.MemePool.MaxPool <= 0 {
		c.MemePool.MaxPool = 20
	}
	if c.MemePool.MaxResidencyDays <= 0 {
		c.MemePool.MaxResidencyDays = 30
	}
	if c.MemePool.OptIntervalHours <= 0 {
		c.MemePool.OptIntervalHours = 6
	}
	if c.MemePool.MaxToolRounds <= 0 {
		c.MemePool.MaxToolRounds = 3
	}
	if c.MemePool.MaxImagesPerReply <= 0 {
		c.MemePool.MaxImagesPerReply = 1
	}
	if c.MemePool.MinIO.Region == "" {
		c.MemePool.MinIO.Region = "us-east-1"
	}
	if c.MemePool.MinIO.StateFile == "" {
		c.MemePool.MinIO.StateFile = "memes.json"
	}
	// 池子上限不能超过 QQ 的硬限制：同一条用户消息最多回 5 次，
	// 文字和图片共用。配得再大也发不出去。
	if c.MemePool.MaxPool > 20 {
		c.MemePool.MaxPool = 20
	}
	seen := map[string]bool{}
	for i := range c.LLM.Endpoints {
		ep := &c.LLM.Endpoints[i]
		if ep.ID == "" {
			return fmt.Errorf("接入点 #%d 缺少 id", i)
		}
		if seen[ep.ID] {
			return fmt.Errorf("接入点 id 重复: %s", ep.ID)
		}
		seen[ep.ID] = true
		if ep.TimeoutMS <= 0 {
			ep.TimeoutMS = 60000
		}
		// 留空按经典格式处理；填了不认识的值直接拒绝，避免上线后才发现路径拼错
		switch ep.APIType {
		case "":
			ep.APIType = APIChatCompletions
		case APIChatCompletions, APIResponses:
		default:
			return fmt.Errorf("接入点 %s 的 api_type 非法: %s（只能是 %s 或 %s）",
				ep.ID, ep.APIType, APIChatCompletions, APIResponses)
		}
		for j := range ep.Models {
			if ep.Models[j].Priority < 0 {
				ep.Models[j].Priority = 0
			}
		}
	}
	return nil
}

// Get 返回当前配置快照。返回的是深拷贝，调用方可以随意修改而不影响 Store 内部状态。
// 这一点很关键：管理端会先给 API Key 打码再下发给浏览器，若这里返回浅拷贝，
// 打码操作会通过共享的 slice 直接改写真实配置并落盘，把 Key 冲掉。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Clone 深拷贝一份配置
func (c *Config) Clone() Config {
	out := *c
	if c.Groups != nil {
		out.Groups = append([]GroupConfig(nil), c.Groups...)
	}
	if c.LLM.Endpoints != nil {
		out.LLM.Endpoints = make([]Endpoint, len(c.LLM.Endpoints))
		for i, e := range c.LLM.Endpoints {
			ne := e
			if e.Models != nil {
				ne.Models = append([]Model(nil), e.Models...)
			}
			out.LLM.Endpoints[i] = ne
		}
	}
	if c.Persona.RoastRules != nil {
		out.Persona.RoastRules = append([]string(nil), c.Persona.RoastRules...)
	}
	if c.Persona.RedLines != nil {
		out.Persona.RedLines = append([]string(nil), c.Persona.RedLines...)
	}
	if c.Persona.Catchphrases != nil {
		out.Persona.Catchphrases = append([]string(nil), c.Persona.Catchphrases...)
	}
	if c.Persona.FallbackLines != nil {
		out.Persona.FallbackLines = append([]string(nil), c.Persona.FallbackLines...)
	}
	if c.Persona.BusyLines != nil {
		out.Persona.BusyLines = append([]string(nil), c.Persona.BusyLines...)
	}
	if c.Persona.SilenceRules != nil {
		out.Persona.SilenceRules = append([]string(nil), c.Persona.SilenceRules...)
	}
	if c.Persona.RefuseRules != nil {
		out.Persona.RefuseRules = append([]string(nil), c.Persona.RefuseRules...)
	}
	if c.Persona.ReplyRules != nil {
		out.Persona.ReplyRules = append([]string(nil), c.Persona.ReplyRules...)
	}
	if c.Master.OpenIDs != nil {
		out.Master.OpenIDs = append([]string(nil), c.Master.OpenIDs...)
	}
	return out
}

// Path 配置文件路径
func (s *Store) Path() string { return s.path }

// Rev 配置版本号。管理端轮询时只在它变化时才重画表单，
// 否则每 5 秒重写一次 input.value，用户根本没法编辑。
func (s *Store) Rev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rev
}

// IsMaster 判断某个 openid 是否为已绑定的开发者。
//
// 注意它只回答「认得出谁」，不回答「要不要给他特权」——那是 Master.DevEnabled
// 的事。认人与特权分开，是这个开关能存在的前提。
// 群里回调只给 openid，所以这是「认人」的唯一可靠依据。
// 无论 DevEnabled 开没开，这个判断都照常工作。
func (c *Config) IsMaster(openID string) bool {
	if openID == "" {
		return false
	}
	for _, id := range c.Master.OpenIDs {
		if id == openID {
			return true
		}
	}
	return false
}

// BindMaster 绑定一个 openid 为主人并落盘。返回 false 表示已绑定过，无需重复写盘。
func (s *Store) BindMaster(openID string) (bool, error) {
	if openID == "" {
		return false, nil
	}
	added := false
	err := s.Update(func(c *Config) (bool, error) {
		for _, id := range c.Master.OpenIDs {
			if id == openID {
				return false, nil
			}
		}
		c.Master.OpenIDs = append(c.Master.OpenIDs, openID)
		added = true
		return true, nil
	})
	return added, err
}

// UnbindMaster 解绑一个 openid
func (s *Store) UnbindMaster(openID string) error {
	return s.Update(func(c *Config) (bool, error) {
		kept := c.Master.OpenIDs[:0:0]
		changed := false
		for _, id := range c.Master.OpenIDs {
			if id == openID {
				changed = true
				continue
			}
			kept = append(kept, id)
		}
		if !changed {
			return false, nil
		}
		c.Master.OpenIDs = kept
		return true, nil
	})
}

// Update 在锁内修改配置副本。fn 返回 true 表示配置有变化：
// 只有有变化时才校验、替换内存配置、推进版本号并落盘；
// 返回 false 表示什么都没改，draft 直接丢弃（不 rev++，不落盘）。
func (s *Store) Update(fn func(c *Config) (changed bool, err error)) error {
	s.mu.Lock()
	// 深拷贝：浅拷贝时 fn 对 draft 共享切片的修改会泄漏进 Store 内部配置，
	// 一旦随后 Validate 失败，内存已被改但 rev 未增、未落盘——内存/磁盘/前端三方不一致
	draft := s.cfg.Clone()
	changed, err := fn(&draft)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if !changed {
		s.mu.Unlock()
		return nil
	}
	if err := draft.Validate(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cfg = &draft
	s.rev++
	s.mu.Unlock()
	return s.Save()
}

// Save 原子写回 config.json
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(s.path)
	tmp := filepath.Join(dir, "."+filepath.Base(s.path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// 保留一份最近备份，防止写坏
	bak := filepath.Join(dir, filepath.Base(s.path)+".bak")
	if old, err := os.ReadFile(s.path); err == nil {
		_ = os.WriteFile(bak, old, 0o600)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	logx.Info("配置已写入", "path", s.path, "at", time.Now().Format("15:04:05"))
	return nil
}
