// Command dadyumo-qqbot 群聊智能体服务端：接收 QQ 开放平台回调，经中央调用器驱动 LLM，在群里像一个真人一样发言。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	adminpkg "dadyumo/internal/admin"
	"dadyumo/internal/agent"
	"dadyumo/internal/brain"
	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
	"dadyumo/internal/qqapi"
	"dadyumo/internal/statsdb"
	"dadyumo/internal/webhook"
)

// version 由构建时注入：go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

// memoryFlushInterval 长期记忆落盘周期
const memoryFlushInterval = 5 * time.Minute

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	debug := flag.Bool("debug", false, "开启 debug 日志")
	showVer := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	if *showVer {
		fmt.Printf("dadyumo-qqbot %s (linux/amd64)\n", version)
		return
	}

	if *debug {
		logx.SetLevel(logx.LevelDebug)
	}
	// 这里不能写具体的机器人名。同机跑第二台（羽沫奶酱）时，
	// 两台的启动日志都会写着「羽沫老爹启动中」，排查时第一步就被带偏。
	// 名字要等配置读进来才知道，所以放在下面那行「配置已加载」里带出去。
	logx.Info("进程启动中", "version", version)

	store, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}
	cfg := store.Get()
	botName := cfg.Persona.Name
	if botName == "" {
		botName = "（人设未命名）"
	}
	logx.Info("配置已加载", "机器人", botName, "path", *cfgPath,
		"appid", cfg.QQ.AppID, "sandbox", cfg.QQ.Sandbox)
	// 档位名不认识时必须显式告警：windowsFor 对未知 mode 是静默回落 daytime 的，
	// 现场表现只是「话变少了」，没人会想到是档位名写错或被改名。
	// 校验放在启动时而不是 config.Validate 里，是因为 brain 已 import config，
	// 反向 import 会成环——这里 main 同时看得见两边。
	// 总开关处于关停状态时必须显眼地喊一声。
	//
	// 这个状态是**持久化**的（关掉之后重新部署、重启、甚至机器重启都还是关的），
	// 于是最常见的现场问题是「它怎么一句话都不说」——而进程活着、QQ 通道正常、
	// 配置也没错，唯一的原因就藏在 config.json 的一个 bool 里。
	// 启动日志是排查的第一站，这一行必须让人不用翻配置文件就能找到答案。
	if cfg.Paused {
		logx.Warn("总开关处于关闭状态：本次启动不会调用任何模型",
			"说明", "消息不接收、不记录、不回话，表情包优选也停",
			"恢复", "打开管理端控制台总览页，点「启动」；或改 config.json 的 paused=false")
	}
	if !brain.KnownScheduleMode(cfg.Schedule.Mode) {
		logx.Warn("schedule.mode 不认识，将按 daytime 处理",
			"mode", cfg.Schedule.Mode,
			"提示", "档位名拼错了，或该档位在某个版本被改名。可用：daytime/deepseek_offpeak/night_owl/always/always_strict/random_daily/custom")
	}
	// 长期要点上限从配置写进 memory 包。上限住在 memory 包里是因为淘汰逻辑
	// 与 facts 同处一地（SetFact 要拿它判断该不该 LRU），而配置在 config 包——
	// 只能由 main 这个同时看得见两边的地方做交接。
	//
	// **必须在建 Engine / 读 memory.json 之前写**，否则加载进来的旧要点
	// 会按默认值算过一次淘汰统计。
	memory.MaxFacts = cfg.Brain.MaxFacts
	// 在线时段落一行日志：重启后能一眼确认档位和当前在线率，不用猜它为什么不说话
	if rate, label := brain.OnlineRate(cfg.Schedule, time.Now()); cfg.Schedule.Enabled {
		logx.Info("在线时段调度已启用", "档位", cfg.Schedule.Mode,
			"当前时段", label, "在线率", fmt.Sprintf("%.0f%%", rate*100),
			"平时在线率", fmt.Sprintf("%.0f%%", cfg.Schedule.BaseRate*100),
			"@宽限(秒)", cfg.Schedule.AtGraceSec)
	} else {
		logx.Info("在线时段调度未启用（全天候在线）")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	qqClient, err := qqapi.New(ctx, store)
	if err != nil {
		logx.Error("QQ 客户端初始化失败", "err", err.Error())
		os.Exit(1)
	}

	router := llm.NewRouter(store)

	// 统计库：调用明细与健康度持久化。打开失败只降级为纯内存统计，不影响主流程。
	var sdb *statsdb.DB
	if d, err := statsdb.Open(filepath.Join(cfg.Storage.DataDir, "stats.db")); err != nil {
		logx.Warn("统计库打开失败，统计功能降级为纯内存（重启后清零）", "err", err.Error())
	} else {
		sdb = d
		router.SetStats(sdb)
		if m, err := sdb.LoadTargets(); err == nil {
			router.RestoreTargets(m)
		} else {
			logx.Warn("统计库读取目标状态失败", "err", err.Error())
		}
		// 事件日志落盘。装在 logx 上而不是各个调用点，
		// 是因为「哪些该落盘」这条规则必须集中一处（见 logx.shouldPersist）。
		// RecordEvent 走队列且满则丢弃，日志堵了不会拖慢主流程。
		logx.SetSink(sdb.LogSink())
	}

	// 长期记忆：跨重启保留要点、成员画像、滚动摘要，以及最近十几条会话记录。
	// 短期窗口以前是随进程消亡的，那个判断被推翻了——真人是记得昨天聊过什么的。
	mem := memory.New(cfg.Brain.MaxHistory)
	memPath := filepath.Join(cfg.Storage.DataDir, "memory.json")
	if err := mem.LoadFrom(memPath); err != nil {
		logx.Warn("长期记忆恢复失败（将重新开始记忆）", "err", err.Error())
		// 原文件先隔离，绝不能留在原位：memory.SaveTo 每 5 分钟会把这个
		// 空 store 写回去，一旦覆盖就是 62 条长期要点永久消失且没有备份。
		if q, qerr := memory.Quarantine(memPath); qerr == nil {
			logx.Error("原文件已隔离，请人工检查后恢复", "path", q)
		} else {
			logx.Error("原文件隔离失败，请立刻手工备份", "path", memPath, "err", qerr.Error())
		}
	}

	// 后台循环统一挂一个 WaitGroup：退出时必须等它们真的返回，再去关统计库。
	// 早先 cancel() 之后直接 sdb.Close()，而 statsFlushLoop 可能正在 FlushTargets，
	// statsdb 的 channel 一关就变成向已关闭 channel 发送——必定 panic。
	var bg sync.WaitGroup

	bot := agent.New(store, qqClient, mem, nil)
	engine := brain.NewEngine(store, router, mem, bot)
	bot.SetEngine(engine)

	// 表情包池。默认关闭；开启需要 MinIO 与 QQ 富媒体上传两条外部链路都通。
	// 任一条没通就整体降级为「不发图」，绝不影响文字路径。
	var memePool *memepool.Pool
	var memePoolPath string
	if pool, curator, poolPath, err := setupMemePool(ctx, router, cfg); err != nil {
		logx.Warn("表情包池未启用，本次只走纯文本", "err", err.Error())
	} else if pool != nil {
		engine.SetMemePool(pool, qqClient)
		curator.Start(ctx) // 内部自己开 goroutine
		memePool, memePoolPath = pool, poolPath
		bg.Add(1)
		go func() {
			defer bg.Done()
			memeSaveLoop(ctx, pool, poolPath)
		}()
		logx.Info("表情包池已就绪", "池大小", pool.Len(), "上限", cfg.MemePool.MaxPool)
	}

	// 回调服务：事件原文先落盘再回 200，后台 worker 消费。
	// 这样「回 200」在平台侧才真的等价于「收到了」，而不是「骗过了它然后把消息扔了」。
	wh := webhook.NewWithQueue(store, bot, filepath.Join(cfg.Storage.DataDir, "callback-queue"))
	if err := wh.Start(ctx); err != nil {
		// 队列建不起来还能靠 ServeHTTP 的同步降级跑，只是丢了持久化，不至于起不来
		logx.Error("回调队列初始化失败，回调将退化为同步处理（进程重启会丢未处理事件）", "err", err.Error())
	}

	bg.Add(2)
	go func() {
		defer bg.Done()
		flushLoop(ctx, mem, memPath)
	}()
	// 健康度快照周期落库：明细是实时记的，target_state 只需要崩溃恢复精度
	go func() {
		defer bg.Done()
		statsFlushLoop(ctx, router)
	}()

	mux := http.NewServeMux()
	mux.Handle(cfg.Server.WebhookPath, wh)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	pubSrv := &http.Server{
		Addr:              cfg.Server.PublicAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	go func() {
		logx.Info("回调服务启动", "addr", cfg.Server.PublicAddr, "path", cfg.Server.WebhookPath)
		if err := pubSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Error("回调服务异常退出", "err", err.Error())
			os.Exit(1)
		}
	}()

	// 管理端：默认只监听回环地址，公网访问请自行用 Caddy/Nginx 反代或 SSH 隧道
	var adminSrv *http.Server
	if cfg.Server.AdminEnabled {
		admin, err := adminpkg.New(store, router, engine, mem, sdb)
		if err != nil {
			logx.Error("管理端初始化失败", "err", err.Error())
			os.Exit(1)
		}
		// 让管理端改完群记忆能立即落盘，而不是等 5 分钟的周期 flush
		admin.SetMemoryPersist(func() error { return mem.SaveTo(memPath) })
		admin.SetMemoryPath(memPath)
		if memePool != nil {
			path := memePoolPath
			admin.SetMemePool(memePool, func() error { return memePool.Save(path) })
		}
		prefix := cfg.Server.AdminPrefix
		if prefix == "" {
			prefix = "/admin"
		}
		warnIfExposedAdmin(cfg.Server.AdminAddr)
		adminSrv = &http.Server{
			Addr:              cfg.Server.AdminAddr,
			Handler:           admin.Handler(prefix),
			ReadHeaderTimeout: 10 * time.Second,
			// 管理端同样要防 slowloris：只配 ReadHeaderTimeout 的话，
			// 对手发完请求头再以 1 B/s 灌 body，连接和 goroutine 能一直占着。
			// WriteTimeout 给 30s 而不是 15s：/admin/api/test 是同步真调 LLM 的，实测最长 13.6s。
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
		go func() {
			logx.Info("管理端启动", "addr", cfg.Server.AdminAddr, "prefix", prefix)
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// 和回调服务一样：管理端挂了就 os.Exit(1)，别带着「回调还在收、面板已经没了」
				// 的半死状态继续跑，那比直接退出更难排查。
				logx.Error("管理端异常退出", "err", err.Error())
				os.Exit(1)
			}
		}()
	}

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logx.Info("收到退出信号，正在关闭…")

	// 顺序是硬要求，不能调换：
	//   1. cancel()            —— 通知所有后台循环停止
	//   2. Shutdown 两个 HTTP  —— 等在途请求跑完，期间还可能产生新的 recordCall
	//   3. 等后台循环退出      —— statsFlushLoop 可能正好在 FlushTargets
	//   4. sdb.Close()         —— 最后关，因为 Close 会 close(d.ops)，
	//                            任何还在往 statsdb 写的 goroutine 都会变成
	//                            「向已关闭 channel 发送」——Go 里必定 panic，
	//                            select 带 default 也救不了。
	// 早先是 cancel → SaveTo → sdb.Close → Shutdown，顺序颠倒，进程每次退出都带栈崩。
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := pubSrv.Shutdown(shutdownCtx); err != nil {
		logx.Warn("回调服务关闭不彻底", "err", err.Error())
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			logx.Warn("管理端关闭不彻底", "err", err.Error())
		}
	}

	bg.Wait() // flushLoop / statsFlushLoop 都已返回，不会再往统计库写

	if err := mem.SaveTo(memPath); err != nil {
		logx.Warn("长期记忆保存失败", "err", err.Error())
	} else {
		logx.Info("长期记忆已保存", "path", memPath)
	}
	if sdb != nil {
		// brain 的攒批定时器是 time.AfterFunc，不持有 ctx，cancel() 对它无效；
		// 退出前排上的那些仍可能再 fire 一次 decide → recordCall。
		// 这里留一小段沉降时间把它们排掉，再关统计库。
		settleBrainTimers()
		router.FlushTargets() // 异步入队，紧接着的 Close 会排空写完
		if err := sdb.Close(); err != nil {
			logx.Warn("统计库关闭失败", "err", err.Error())
		} else {
			logx.Info("统计库已关闭")
		}
	}
	logx.Info("已退出")
}

// brainTimerSettle brain 的攒批定时器是不受 ctx 约束的 time.AfterFunc，
// 退出时给它们一点时间自然 fire 完，免得在统计库关闭后再写一次。
const brainTimerSettle = 2 * time.Second

func settleBrainTimers() {
	time.Sleep(brainTimerSettle)
}

// warnIfExposedAdmin 管理端默认只该监听回环地址。
//
// 最阴的一种配错是 admin_addr 留空：net.Listen("tcp", "") 会绑到 [::]:80，
// 等于把没有认证的改记忆/调 LLM 的管理面板开到公网 80 端口，而且全程静默。
// 这里不静默降级，只打一条足够扎眼的 WARN，让部署的人在日志里一眼看见。
func warnIfExposedAdmin(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		logx.Warn("admin_addr 为空！net.Listen 会绑到 [::]:80，管理端将直接暴露在公网 80 端口，请立即改为 127.0.0.1:8081")
		return
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return
	}
	logx.Warn("admin_addr 不是回环地址，管理端将暴露在该地址上，请确认前面有反代与鉴权！",
		"admin_addr", addr, "host", host)
}

// flushLoop 定期把长期记忆落盘，避免异常退出时白记一场
func flushLoop(ctx context.Context, mem *memory.Store, path string) {
	ticker := time.NewTicker(memoryFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := mem.SaveTo(path); err != nil {
				logx.Warn("长期记忆落盘失败", "err", err.Error())
			}
		}
	}
}

// statsFlushLoop 定期把目标健康度快照写进统计库
func statsFlushLoop(ctx context.Context, router *llm.Router) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			router.FlushTargets()
		}
	}
}
