// Package logx 提供结构化日志，同时向 stdout 与内存环形缓冲（供 Web 管理端实时查看）输出。
package logx

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Level 日志级别
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "INFO"
}

// Cat 日志分类。
//
// 为什么要有它：全部日志是单行 JSON 混在一起的，生产一天几千条。
// 排查「它为什么不回」时想看的只有决策链路那几十条，
// 但它们被夹在系统启动、回调入队、图片抓取之间，靠人眼翻不动。
// 分类让管理端能一键筛出「只看决策」。
//
// 分类是**正交的**：级别回答「严重吗」，分类回答「这是哪条链路的事」。
// 一条决策可以是 INFO 也可以是 ERROR，两者不互相替代。
type Cat string

const (
	// CatRuntime 未分类的常规日志（默认）。
	CatRuntime Cat = "runtime"
	// CatDecision 决策链路：为什么说话/为什么闭嘴。这是排查「它为什么不回」的全部。
	CatDecision Cat = "decision"
	// CatSpeak 实际发出的内容。
	CatSpeak Cat = "speak"
	// CatChat 群消息本体。
	CatChat Cat = "chat"
	// CatSystem 启动、配置加载、调度开关。
	CatSystem Cat = "system"
	// CatWebhook 回调入队与事件派发。
	CatWebhook Cat = "webhook"
	// CatModel LLM 调用与目标健康度。
	CatModel Cat = "model"
)

// Cats 管理端下拉里的固定顺序。
var Cats = []Cat{CatDecision, CatSpeak, CatChat, CatModel, CatWebhook, CatSystem, CatRuntime}

// ValidCat 判断分类是否在已知集合内。落盘与查询都靠它挡住脏值。
func ValidCat(c Cat) bool {
	for _, k := range Cats {
		if k == c {
			return true
		}
	}
	return c == ""
}

// Entry 一条日志
type Entry struct {
	TS string `json:"ts"`
	// Cat 是链路分类。空字符串当 runtime 处理，不写进 JSON——
	// 绝大多数日志都没分类，让 JSON 少一个字段能省不少字节。
	Cat   Cat            `json:"cat,omitempty"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	KV    map[string]any `json:"kv,omitempty"`
	raw   string
}

var (
	mu       sync.RWMutex
	ring     []Entry
	ringSize = 500
	minLevel = LevelInfo
	std      = log.New(os.Stdout, "", 0)
	// sink 是可选的落盘回调。装上之后每条日志会额外交给它一次。
	//
	// 为什么挂在 logx 而不是让每个调用方自己写：调用点有 80 多处，
	// 逐个改的话「哪些该落盘」这个判断会散得到处都是，
	// 而它恰恰是最需要集中的一条规则（见 shouldPersist）。
	sink func(Entry)
)

// persistCat 落盘时无视级别、强制保留的分类。
//
// 为什么需要它：决策链路里有一部分信息天然是 Debug 级
// （比如「消息已登记」这类每条消息一条的细节），但它们恰恰是
// 排查「它为什么不回」时最想看的。级别回答「严重吗」，
// 分类回答「哪条链路的事」——两者正交，这里就是让它们解耦的地方。
var persistCat = map[Cat]bool{CatDecision: true}

// SetSink 装上落盘回调。传 nil 摘掉。
//
// 调用方必须是「永不阻塞、永不 panic」的那种——它在日志的
// 热路径上，一旦卡住会把整个机器人拖慢。
func SetSink(f func(Entry)) {
	mu.Lock()
	sink = f
	mu.Unlock()
}

// shouldPersist 这条要不要落盘。
//
// 规则：Info 及以上全落；Debug 只在决策链路落。
// 反过来不落的是天气查询失败、botgo 第三方原始行、
// 群消息已发送这类高频低价值日志——它们能把库撑大好几倍，
// 而排查时用不上。
func shouldPersist(l Level, cat Cat) bool {
	if l >= LevelInfo {
		return true
	}
	return persistCat[cat]
}

func deliver(e Entry) {
	mu.RLock()
	f := sink
	mu.RUnlock()
	if f != nil {
		f(e)
	}
}

func init() {
	if v := os.Getenv("QQPAL_LOG_LEVEL"); v != "" {
		switch v {
		case "debug":
			minLevel = LevelDebug
		case "warn":
			minLevel = LevelWarn
		case "error":
			minLevel = LevelError
		}
	}
}

// SetLevel 设置最低输出级别
func SetLevel(l Level) {
	mu.Lock()
	minLevel = l
	mu.Unlock()
}

func output(l Level, msg string, kv map[string]any) {
	outputCat(l, CatRuntime, msg, kv)
}

func outputCat(l Level, cat Cat, msg string, kv map[string]any) {
	mu.Lock()
	if l < minLevel {
		mu.Unlock()
		return
	}
	e := Entry{
		TS:    time.Now().Format("2006-01-02 15:04:05.000"),
		Cat:   cat,
		Level: l.String(),
		Msg:   msg,
		KV:    kv,
	}
	if cat == CatRuntime {
		// runtime 是默认值，不占 JSON 字段
		e.Cat = ""
	}
	b, _ := json.Marshal(e)
	e.raw = string(b)
	ring = append(ring, e)
	if len(ring) > ringSize {
		ring = ring[len(ring)-ringSize:]
	}
	mu.Unlock()
	std.Println(e.raw)

	// 落盘判定放在解锁之后：sink 可能做 I/O，不能占着日志锁，
	// 否则一条慢查询会把全进程的日志卡住。
	if shouldPersist(l, e.Cat) {
		deliver(e)
	}
}

// Recent 返回最近 n 条日志（新的在前）
func Recent(n int) []Entry {
	mu.RLock()
	defer mu.RUnlock()
	if n <= 0 || n > len(ring) {
		n = len(ring)
	}
	out := make([]Entry, 0, n)
	for i := len(ring) - 1; i >= len(ring)-n; i-- {
		out = append(out, ring[i])
	}
	return out
}

func fmtKV(kv map[string]any) string {
	if len(kv) == 0 {
		return ""
	}
	b, _ := json.Marshal(kv)
	return string(b)
}

// Debug 调试日志
func Debug(msg string, kv ...any) { output(LevelDebug, msg, pack(kv)) }

// Info 常规日志
func Info(msg string, kv ...any) { output(LevelInfo, msg, pack(kv)) }

// Warn 警告日志
func Warn(msg string, kv ...any) { output(LevelWarn, msg, pack(kv)) }

// Error 错误日志
func Error(msg string, kv ...any) { output(LevelError, msg, pack(kv)) }

// Errorf 格式化错误日志
func Errorf(format string, args ...any) {
	output(LevelError, fmt.Sprintf(format, args...), nil)
}

// InfoCat 带分类的常规日志。
//
// 为什么不改造 Info 的签名：全仓 80+ 处调用，改签名要动每一处，
// 而绝大多数日志根本不关心分类。**保持原签名不动**、另开一个带分类的入口，
// 只有需要被筛选的那几条才用它。
func InfoCat(cat Cat, msg string, kv ...any) { outputCat(LevelInfo, cat, msg, pack(kv)) }

// DebugCat 带分类的调试日志。
func DebugCat(cat Cat, msg string, kv ...any) { outputCat(LevelDebug, cat, msg, pack(kv)) }

// WarnCat 带分类的警告日志。
func WarnCat(cat Cat, msg string, kv ...any) { outputCat(LevelWarn, cat, msg, pack(kv)) }

func pack(kv []any) map[string]any {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return m
}
