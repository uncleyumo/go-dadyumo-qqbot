package qqapi

import (
	"fmt"

	botgolog "github.com/tencent-connect/botgo/log"

	"dadyumo/internal/logx"
)

// botgo 的日志是包级变量 DefaultLogger，没有任何级别过滤：
// log/log.go 全文只有一个转发函数和一个 var，没有 SetLogLevel 之类的 API。
// 于是它在生产环境原样打印两类东西：
//
//  1. token_source.go 的 Debug —— **把 appSecret 与 access_token 明文写进 journald**。
//     appSecret 永不过期，拿到就能以「羽沫老爹」的身份向它所在的群发任意消息。
//     这是本次审计的头号问题，而且关不掉：没有级别开关可关。
//  2. openapi.go 的 Infof —— 把每次发言的完整 req/resp 和真实群 openid 打进日志，
//     占全天日志体积的近一半，群聊内容明文留存。
//
// 所以只能整体换掉 DefaultLogger。三档处理：
//
//	Debug    —— 直接丢弃。里面只有凭据与调用细节，没有诊断价值。
//	Info     —— 降到 logx.Debug。生产默认不输出，带 -debug 启动时仍然可见，
//	             保留排查能力，又不让群聊内容整天躺在 journald 里。
//	Warn/Error —— 原样转发到 logx。这两类是真问题，必须留着。
type botgoLogger struct{}

// 编译期确认实现了 SDK 要求的全部方法
var _ botgolog.Logger = botgoLogger{}

// installBotgoLogFilter 在创建任何 botgo 客户端之前调用。
func installBotgoLogFilter() {
	botgolog.DefaultLogger = botgoLogger{}
}

func (botgoLogger) Debug(...any)          {}
func (botgoLogger) Debugf(string, ...any) {}

func (botgoLogger) Info(v ...any) { logx.Debug("botgo: " + fmt.Sprint(v...)) }
func (botgoLogger) Infof(f string, a ...any) {
	logx.Debug("botgo: " + fmt.Sprintf(f, a...))
}

func (botgoLogger) Warn(v ...any) { logx.Warn("botgo: " + fmt.Sprint(v...)) }
func (botgoLogger) Warnf(f string, a ...any) {
	logx.Warn("botgo: " + fmt.Sprintf(f, a...))
}

func (botgoLogger) Error(v ...any) { logx.Error("botgo: " + fmt.Sprint(v...)) }
func (botgoLogger) Errorf(f string, a ...any) {
	logx.Error("botgo: " + fmt.Sprintf(f, a...))
}

func (botgoLogger) Sync() error { return nil }
