package brain

import (
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

// BlockTypeText / BlockTypeImg 是 blocks 里合法的类型
const (
	BlockTypeText = "text"
	BlockTypeImg  = "img"
)

// allowBlocks 归一化 blocks：去空、去非法类型、按预算截断。
//
// 返回值第二个是 true 表示「确实要发点东西」——全空时不该发言，
// 否则会在群里留下一条空白消息。
func allowBlocks(blocks []Block, text string, max int) ([]Block, bool) {
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
				out = append(out, Block{T: BlockTypeText, C: c})
			case BlockTypeImg:
				if b.ID <= 0 {
					continue // 没有合法 ID 的图片块没法解析
				}
				out = append(out, Block{T: BlockTypeImg, ID: b.ID})
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
func planDelivery(blocks []Block, maxSegChars, maxSent int) []Block {
	if maxSegChars <= 0 {
		maxSegChars = 40
	}
	if maxSent <= 0 {
		maxSent = 5
	}
	var out []Block
	for _, b := range blocks {
		if b.T == BlockTypeImg {
			if len(out) >= maxSent {
				break
			}
			out = append(out, b)
			continue
		}
		segs := SplitSegments(b.C, maxSegChars, maxSent)
		segs = dropEmpty(DedupeMentions(segs))
		for _, s := range segs {
			if len(out) >= maxSent {
				break
			}
			out = append(out, Block{T: BlockTypeText, C: s})
		}
		if len(out) >= maxSent {
			break
		}
	}
	return out
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
