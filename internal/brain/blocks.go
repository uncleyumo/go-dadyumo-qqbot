package brain

import (
	"regexp"
	"strings"
)

// 交付内容的块处理。
//
// 这一层是「模型说什么」到「实际发什么」之间唯一的闸门。
// 它存在的理由：模型可以把 blocks 填得很随意（一条回复塞十个块、
// 全是空文字、引用不存在的表情包 ID），而这些错一旦发出去就是真的发进群里了。
//
// **顺序在这里定死**：文字和表情包的先后由 blocks 的排列决定，
// 不由工具调用轮决定——工具轮只负责取上下文，绝不发送。

// BlockTypeText / BlockTypeImg / BlockTypeAt 是 blocks 里合法的类型
const (
	BlockTypeText = "text"
	BlockTypeImg  = "img"
	// BlockTypeAt 是「把这个人真艾特出来」。程序侧渲染成平台的
	// <qqbot-at-user id="member_openid" /> 内嵌标签，可与文字混排。
	//
	// 注意**不是** msg_type=5：那是 botgo 里频道（Channel）时代的遗留常量，
	// QQ 群/单聊的开放接口枚举里根本没有这个类型。
	BlockTypeAt = "at"
)

// allowBlocks 归一化 blocks：去空、去非法类型、按预算截断。
//
// atName 可为 nil：单元测试与部分调用路径不接群成员表。
// 非 nil 时，at 块的名字会在这里就解析成 openid（resolveAt 可为 nil 则只清洗不解析），
// 对不上就丢掉这个块——猜错 @ 人比不 @ 糟得多，
// 而模型填错名字是常事（它照抄的是渲染后的称呼，可能带群名片或重名后缀）。
//
// atOpenID 是**反向**校验：判断一个已经成形的 openid 是不是本群成员，
// 专门用来验模型照抄进来的标签（见 atTagInText 那段事故说明）。
// 为 nil 时那些标签一律当幻觉删掉。
func allowBlocks(blocks []Block, text string, max int,
	atName func(string) string, atOpenID func(string) bool) ([]Block, bool) {
	out := make([]Block, 0, len(blocks)+1)

	// blocks 为空是正常情况（模型用了旧格式），回落到 text
	if len(blocks) == 0 {
		if segs := textOnlyBlocks(text); len(segs) > 0 {
			out = append(out, segs...)
		}
	} else {
		for _, b := range blocks {
			switch b.T {
			case BlockTypeText:
				c := strings.TrimSpace(b.C)
				if c == "" {
					continue // 空文字块：发出去是空白消息，QQ 里会显示成一条空气
				}
				// 2026-10-05 生产事故：模型不按协议写 at 块，而是照抄**渲染后**的
				// 标签——它前几轮自己发过，聊天记录里就有现成的样例。标签混在 text
				// 块里，下面的 at 分支根本看不到，于是原样发进群，而那个 openid 是
				// 它自己编的。这里把它收编回 at 块：在成员表里就是一次合法艾特，
				// 不在就只删标签、把句子留下（猜错 @ 人比不 @ 糟得多）。
				if oid, ok := leadingAtOpenID(c); ok && atOpenID != nil && atOpenID(oid) {
					c = stripAtTags(c)
					out = append(out, Block{T: BlockTypeAt, C: oid})
					if c != "" {
						out = append(out, Block{T: BlockTypeText, C: c})
					}
					continue
				}
				c = stripAtTags(c)
				if c == "" {
					continue
				}
				out = append(out, Block{T: BlockTypeText, C: c, Q: b.Q})
			case BlockTypeImg:
				if b.ID <= 0 {
					continue // 没有合法 ID 的图片块没法解析
				}
				out = append(out, Block{T: BlockTypeImg, ID: b.ID})
			case BlockTypeAt:
				name := strings.TrimSpace(b.Name)
				if name == "" {
					continue
				}
				// @全体成员 这一类直接扔：平台不支持（官方标注
				// 「仅在文字子频道可用」，群聊等于不支持），
				// 而模型学聊天记录里的字样发出来只会露馅（2026-10-01 事故）。
				if stripAtAll(name) == "" {
					continue
				}
				openID := ""
				if atName != nil {
					openID = atName(name)
				}
				if openID == "" {
					// 对不上群成员表：宁可这个人不 @。
					// 挂错人比不挂糟得多——那等于当着全群艾特了一个不相干的人。
					continue
				}
				out = append(out, Block{T: BlockTypeAt, Name: name, C: openID})
			default:
				// 未知类型直接丢。宁可少发也不要让未知结构漏到发送层
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, true
}

// textOnlyBlocks 把纯文本按 \n 拆成多个 text 块。
//
// 为什么要拆成块而不是合成一块：SplitSegments 还要按字数和标点再拆，
// 拆成块只是把「哪些是独立消息」的判断前置到这里，
// 好让文字和图片的先后顺序在预算截断时也能算得清。
func textOnlyBlocks(text string) []Block {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []Block
	for _, line := range strings.Split(text, "\n") {
		if c := strings.TrimSpace(line); c != "" {
			out = append(out, Block{T: BlockTypeText, C: c})
		}
	}
	return out
}

// planDelivery 把 blocks 展开成最终的「按顺序、逐条可发」清单。
//
// maxSent 是硬上限（QQ 允许的被动回复次数）。
//
// at 块**不单独占一条**，而是渲染成下一条 text 的前缀——
// 真人不会单发一条只有 @ 的消息，「@张三 就这吧」是一句话。
// 于是这一层的条数预算与引入 at 之前完全一致。
func planDelivery(blocks []Block, maxSegChars, maxSent int) []Block {
	if maxSegChars <= 0 {
		maxSegChars = 40
	}
	if maxSent <= 0 {
		maxSent = 5
	}
	var out []Block
	// pendingAt 是还没挂到任何文字上的 @ 前缀。
	pendingAt := ""
	for _, b := range blocks {
		switch b.T {
		case BlockTypeImg:
			if len(out) >= maxSent {
				return out
			}
			// @ 后面紧跟一张图：这个 @ 没有可修饰的对象，
			// 丢掉它（而不是把标签孤零零塞在图前面——那在 QQ 上不成立）。
			pendingAt = ""
			out = append(out, b)
			continue
		case BlockTypeAt:
			if len(out) >= maxSent {
				return out
			}
			// 一个 text 块里只留第一个 @：第二条再 @ 就是「@甲 @乙 说句话」，
			// 群里看着像在挨个点名刷存在感。
			if pendingAt == "" {
				pendingAt = atTag(b.C)
			}
			continue
		}
		segs := SplitSegments(b.C, maxSegChars, maxSent)
		segs = dropEmpty(DedupeMentions(segs))
		for _, s := range segs {
			if len(out) >= maxSent {
				return out
			}
			if pendingAt != "" {
				s = pendingAt + " " + s
				pendingAt = ""
			}
			out = append(out, Block{T: BlockTypeText, C: s, Q: b.Q})
		}
		if len(out) >= maxSent {
			return out
		}
	}
	return out
}

// atTag 渲染平台的 @ 内嵌标签。
//
// QQ 的 @ 不是独立消息类型（msg_type=5 是 botgo 频道时代的遗留常量），
// 而是文本里的一个标签：<qqbot-at-user id="member_openid" />。
// 旧格式 <@userid> 官方标注「即将弃用」，且社区实测客户端已不再解析它
// （会原样显示成字面量），所以不能用。
func atTag(openID string) string {
	return `<qqbot-at-user id="` + openID + `" />`
}

// atTagInText 匹配「模型正文里出现的平台 @ 标签」。
//
// 为什么要单独认它：出口渲染出来的就是这个样子，而渲染结果会进聊天记录、
// 会进它自己的记忆，于是模型随时会把它当模板抄回来——它并不知道
// 「写 {"t":"at","name":...}」才是唯一的正确姿势。
//
// 2026-10-05 生产实况：模型写出了 `<qqbot-at-user id="D1DBAF…"/>` 这样一个
// 完整 openid。那串值不在任何上下文里，是它编的（碰巧对上了群成员）。
//
// 所以这个标签一律**不信任**：只有 allowBlocks 认得它、并拿成员表验过，
// 才转成正规的 at 块；否则只删标签保句子。
var atTagInText = regexp.MustCompile(`<qqbot-at-user\s+id\s*=\s*"([^"]*)"\s*/?>`)

// leadingAtOpenID 判断正文是否以 @ 标签开头，是则返回那个 openid。
//
// **只认句首**：夹在句子中间的标签提成 at 块就没地方放它前面的字了
// （planDelivery 只会把 @ 放句首），为了一个艾特丢掉半句话不划算，
// 那种情况交给 stripAtTags 只删标签。
func leadingAtOpenID(text string) (string, bool) {
	m := atTagInText.FindStringSubmatchIndex(text)
	if m == nil || m[0] != 0 {
		return "", false
	}
	openID := text[m[2]:m[3]]
	return openID, openID != ""
}

// stripAtTags 从正文里删掉所有 @ 标签，句子本身留下。
func stripAtTags(text string) string {
	if !atTagInText.MatchString(text) {
		return text
	}
	return strings.TrimSpace(atTagInText.ReplaceAllString(text, " "))
}

// textOf 提取全部文字块拼起来的文本，用来写进记忆与统计。
func textOf(blocks []Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.T == BlockTypeText {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(b.C)
		}
	}
	return strings.TrimSpace(sb.String())
}

// hasImage 判断这一轮是否含图片（统计与记忆留痕用）
func hasImage(blocks []Block) bool {
	for _, b := range blocks {
		if b.T == BlockTypeImg {
			return true
		}
	}
	return false
}
