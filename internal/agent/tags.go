package agent

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// QQ 把两类平台内部标记直接塞在 content 正文里，原样透给模型是有害的：
//
//  1. 表情：<faceType=1,faceId="5",ext="eyJ0ZXh0Ijoi5rWB6In0="/>
//     faceId 是无序编号，没有可查的对应关系，正则里只当任意数字跳过。
//     表情名全靠 ext 实时解出来：base64 → {"text":"流泪"}，没有任何映射表，
//     所以平台新增表情、老表情改名都不用改代码。
//     不还原的话，模型看到的是一串 XML 残渣，只能回「搁这刷表情包呢」——
//     这正是 2026-10-01 群里实际发生的事。
//
//  2. @：<@openid>、<@!openid>、<@all>
//     机器人自己的标签有专门逻辑替换，但 @别人 的裸 openid 一直原样进上下文：
//     32 个十六进制字符，模型既认不出是谁、也学着在回复里写出这种乱码。
//
// 统一在这里翻译成中文，模型看到的上下文就和人读的群记录一样了。
var (
	// 结尾那个可选的符号/表情字符是平台冗余附带的：<faceType=...>👍。
	// 名字已经从 ext 解出来了，跟着留一个重复的 emoji 反而碍眼，一并吃掉。
	faceTagRe = regexp.MustCompile(
		`<faceType=\d+,faceId="\d+",ext="([^"]*)"\s*/?>` +
			`[\p{So}\p{Sk}\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}]?`)

	mentionTagRe = regexp.MustCompile(`<@!?([0-9A-Za-z_\-]+)>`)
)

// faceExt 是 ext 字段解出来的 JSON
type faceExt struct {
	Text string `json:"text"`
}

// cleanTags 把 content 里的平台标记翻译成可读文本。
//
// nameOf 用来把 openid 换成昵称：机器人自己传 cfg.Persona.Name，
// 群友传 mentions 数组里的 username。查不到的 openid 直接丢掉标签本身——
// 留着 32 位十六进制对模型零价值，纯烧 token。
//
// atAll 回报正文里有没有 @全体成员。平台不保证把它放进 mentions 数组，
// 所以只能从正文标签认；它本身不构成「@ 了机器人」，
// 是否回应交给 brain 按语境判断，不在这里当成提及。
func cleanTags(content string, nameOf func(openID string) string) (out string, atAll bool) {
	if content == "" {
		return "", false
	}
	out = faceTagRe.ReplaceAllStringFunc(content, faceName)

	var sb strings.Builder
	last := 0
	for _, loc := range mentionTagRe.FindAllStringSubmatchIndex(out, -1) {
		oid := out[loc[2]:loc[3]]
		if oid == "all" {
			atAll = true
			sb.WriteString(out[last:loc[0]])
			sb.WriteString(markMention("全体成员"))
			last = loc[1]
			continue
		}
		name := ""
		if nameOf != nil {
			name = strings.TrimSpace(nameOf(oid))
		}
		if name == "" {
			// 认不出来的 @ 整段删掉，别留个光秃秃的 openid 给模型模仿
			sb.WriteString(out[last:loc[0]])
			last = loc[1]
			continue
		}
		sb.WriteString(out[last:loc[0]])
		sb.WriteString(markMention(name))
		last = loc[1]
	}
	sb.WriteString(out[last:])
	return strings.TrimSpace(sb.String()), atAll
}

// 提到底 这些字符是群名片，不是聊天内容。
const (
	mentionOpen  = "〔"
	mentionClose = "〕"
)

// markMention 把「@某人」包成结构化标记再交给模型。
//
// 为什么不能直接输出 `@昵称`：群名片是不可控的。生产实况里有个群友的
// 昵称就叫「...」，他 @ 别人时上下文里只剩「@...」三个点，模型读到的
// 就是一句莫名其妙的省略号——2026-10-02 实况，机器人回的是
// 「大半夜发个省略号，你这是困傻了还是省电模式」。
//
// 包起来之后上下文里是 `〔@...〕`，方括号告诉模型「这是一次 @ 动作，
// 括号里是那个人的群名片」，「...」于是重新变回一个名字而不是三个点。
//
// 用 〔〕 而不是 【】：sanitizeChatText 会把群友正文里的 【】 换成 〖〗
// 来防伪造分段，如果这里也用 【】，同一段文本会被改写两次而且读起来
// 跟被伪造的分段没区别。〔〕 不在那个替换表里，且不像 【】 那样
// 是这个系统里已被征用的分段符号。
func markMention(name string) string {
	return mentionOpen + "@" + name + mentionClose
}

// faceName 把一个表情标签换成「（中文名）」。
//
// 这里刻意用圆括号、不是方括号：[图片] / [语音] / [视频] 在这个系统里
// 本来就是「这有一条媒体附件、不是文字」的占位符，模型已经学会把它们
// 归成一类「图片和表情包」。第一版用 [流泪] 的时候它就是这么理解的——
// 2026-10-01 实况：连发一串表情后它回「表情包批发呢你」「有事说事，别光发图」，
// 说明它压根没读出情绪，只看出「又是一堆附件」。
// 换成圆括号才能在视觉上和语义上都跟附件占位符划清界限。
func faceName(tag string) string {
	m := faceTagRe.FindStringSubmatch(tag)
	if m == nil {
		return "（表情）"
	}
	if name := faceText(m[1]); name != "" {
		return "（" + name + "）"
	}
	// 解不出名字时不能一律叫「表情」。
	//
	// 内置小表情（faceType=1）的 ext 里一定有名字（「流泪」「呲牙」…），
	// 解不出来说明平台改格式了，这时叫「表情」是安全的。
	//
	// 但空 ext 还有另一类东西：**群友发的表情包**。2026-10-02 生产实况，
	// 有人引用别人发的那张猫，问「这条是谁发的？」，被引用的元素里装的是
	//
	//	<faceType=6,faceId="0",ext="eyJ0ZXh0IjoiIn0="/>   → {"text":""}
	//
	// 平台把它当图片处理（所以不给 attachments），却在引用时只塞一个空标签，
	// 既没有名字也没有地址。说成「表情」会让模型以为对方甩了个小符号，
	// 而它面对的其实是一张**它看不到、平台也没告诉它发送者**的图片——
	// 于是它答「我这儿看不见，截图发出来」。
	//
	// 注意判据是**ext 为空**、不是 faceType=6：6 还包括有名字的魔法表情
	// （见 faceMagic → 幽灵），那种必须照常显示名字，不能一律叫表情包。
	if extIsNameless(m[1]) {
		return "（表情包）"
	}
	return "（表情）"
}

// extIsNameless 判断 ext 是不是「解得开但里面没有名字」。
//
// 只认这一种可判定的情况：base64 解得开、是合法 JSON、text 字段是空的。
// 其余（解不开 base64、不是 JSON、ext 干脆是空串）一律不算——
// 那多半是平台改了格式，此时叫「表情包」是在编。
func extIsNameless(ext string) bool {
	ext = strings.TrimSpace(ext)
	if ext == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(ext)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(ext)
		if err != nil {
			return false
		}
	}
	var fe faceExt
	if err := json.Unmarshal(raw, &fe); err != nil {
		return false
	}
	// 名字可能带 [/] 壳，解完是空的也算没有名字
	name := strings.TrimSuffix(strings.TrimPrefix(fe.Text, "[/"), "]")
	return strings.TrimSpace(name) == ""
}

// faceText 从 ext 解出中文表情名。
//
// 返回空串表示「解不出来」，由 faceName 决定回退成什么。
// **不要在这里统一回退成「表情」**：解不出来和「本来就没名字」是两回事，
// 而 faceType=6 里前者才是常态（见 faceName 的说明）。
func faceText(ext string) string {
	ext = strings.TrimSpace(ext)
	if ext == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(ext)
	if err != nil {
		// 有些 ext 不带 padding
		raw, err = base64.RawStdEncoding.DecodeString(ext)
		if err != nil {
			return ""
		}
	}
	var fe faceExt
	if err := json.Unmarshal(raw, &fe); err != nil {
		return ""
	}
	name := strings.TrimSpace(fe.Text)
	// faceType=6 的魔法表情名字自带 [//] 壳：「[/皱眉]」，去掉壳更好读
	name = strings.TrimPrefix(name, "[/")
	name = strings.TrimSuffix(name, "]")
	name = strings.TrimSpace(name)
	// 平台偶尔给超长名字（自定义表情），截断防着点烧 token
	if utf8.RuneCountInString(name) > 12 {
		name = string([]rune(name)[:12])
	}
	return name
}
