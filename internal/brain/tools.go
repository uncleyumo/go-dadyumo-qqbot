package brain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
)

// 工具轮的实现。
//
// 唯一原则：**工具轮只取上下文，绝不发送**。
// 工具调用的返回值是「给模型看的字符串」，任何一条路径都不会碰发送通道。
// 这样最终轮失败时重试是幂等的——不会因为「工具轮已经发过了」而重复发言。

// poolOf 取当前表情包池。没启用或没配好时返回 nil。
func (e *Engine) poolOf(cfg config.Config) *memepool.Pool {
	if e.memes == nil || !cfg.MemePool.Enabled {
		return nil
	}
	return e.memes
}

// toolDefs 声明本轮可用的工具。
//
// 池子是空的、或功能没开，就**不给这个工具**——
// 给模型一个必然返回空列表的工具，它还会去调，白烧一次往返。
func (e *Engine) toolDefs(g *memory.Group) []llm.Tool {
	cfg := e.store.Get()
	pool := e.poolOf(cfg)
	if pool == nil || pool.Len() == 0 {
		return nil
	}
	return []llm.Tool{{
		Name: "browse_meme_pool",
		Description: "查看你现在可以发的表情包列表，返回每张的短 ID 和文字描述。" +
			"只有你真的想发一张时才调它；只是想说话就不要调。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mood": map[string]any{
					"type":        "string",
					"description": "你想表达的情绪，如「无语」「兴奋」「嘲讽」。只用于你自己挑图，不影响排序。",
				},
			},
			"required": []any{},
		},
	}}
}

// runTool 执行一次工具调用，返回给模型的文本结果。
//
// 未知工具、参数坏掉、池子没开——统统返回一个说明性的字符串而不是错误。
// 工具层报错不该中断整轮决策：模型看到「这个工具用不了」就会改用纯文字，
// 那本来就是能接受的结果。
func (e *Engine) runTool(ctx context.Context, cfg config.Config, g *memory.Group, tc llm.ToolCall) string {
	switch tc.Name {
	case "browse_meme_pool":
		return e.browseMemePool(cfg, tc.Arguments)
	default:
		return "没有叫 " + tc.Name + " 的工具。"
	}
}

// browseMemePool 返回表情包列表。
//
// **排序就是全部的「心情表达」**：模型只看到顺序，看不到任何分数。
// 把权重、评分、时间波动写进上下文只会增加它的认知负担，
// 还挤占它该花在聊天记录上的注意力。提示词里只说一句
// 「排越前的越愿意用」就够了。
func (e *Engine) browseMemePool(cfg config.Config, args string) string {
	pool := e.poolOf(cfg)
	if pool == nil {
		return "表情包功能没开。直接用文字回答就行。"
	}
	mood := parseMoodArg(args)

	items := pool.Browse(time.Now())
	if len(items) == 0 {
		return "表情包池现在是空的。直接用文字回答就行。"
	}

	var sb strings.Builder
	if mood != "" {
		fmt.Fprintf(&sb, "你现在想表达的情绪：%s\n", mood)
	}
	sb.WriteString("可以发的表情包（从上到下，排越前的你此刻越愿意用）：\n")
	for _, m := range items {
		fmt.Fprintf(&sb, "id %d：%s\n", m.ID, m.Descr)
	}
	// 编号和行号必须说清是两回事。ID 是自增短号，按好感度排序后
	// 第 1 行的 id 可能是 12——不点破的话模型会顺手写 {"id":1}，
	// 发出去的是另一张图，或者干脆哪张都发不出。
	sb.WriteString("\n想发哪张就在 blocks 里写 {\"t\":\"img\",\"id\":那个数字}。")
	sb.WriteString("数字是上面每行开头的 id，不是行号；只能用这份列表里的 id。")

	logx.Debug("表情包池已浏览", "池大小", len(items), "情绪", mood)
	return strings.TrimSpace(sb.String())
}

// parseMoodArg 从工具参数里取出情绪。
//
// 解析失败返回空串：情绪只是给日志看的，模型不给也不影响它按顺序挑图。
func parseMoodArg(args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "{}" {
		return ""
	}
	// 只做最轻的提取，不引 JSON 解析：参数格式不对不该让整轮失败
	for _, key := range []string{`"mood"`, `'mood'`} {
		i := strings.Index(args, key)
		if i < 0 {
			continue
		}
		rest := args[i+len(key):]
		c := strings.IndexAny(rest, ":")
		if c < 0 {
			continue
		}
		v := strings.TrimSpace(rest[c+1:])
		v = strings.Trim(v, `"'`)
		if j := strings.IndexAny(v, `,}`); j >= 0 {
			v = v[:j]
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if len([]rune(v)) > 16 {
			v = string([]rune(v)[:16])
		}
		return v
	}
	return ""
}
