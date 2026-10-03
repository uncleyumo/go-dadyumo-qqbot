// Package admin 提供内置 Web 管理端：在线查看调用器健康度、修改接入点与模型配置，
// 所有改动即时生效并原子写回 config.json。
package admin

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dadyumo/internal/brain"
	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
	"dadyumo/internal/statsdb"
)

//go:embed assets
var assetsFS embed.FS

// Server 管理端
type Server struct {
	store     *config.Store
	router    *llm.Router
	engine    *brain.Engine
	mem       *memory.Store
	stats     *statsdb.DB // 可为 nil：统计库打开失败时分时段统计降级为「未启用」
	limiter   *loginLimiter
	startTime time.Time

	// memes 表情包池，可为 nil（功能未启用或 MinIO 没配通时）。
	memes *memepool.Pool
	// memePersist 池子元数据落盘回调，由 main 注入。
	// 手动删图后立刻写盘，免得等两分钟周期 flush 时进程正好挂了。
	memePersist func() error

	// memPersist 长期记忆落盘回调，由 main 注入（admin 不持有 data 目录路径）。
	// 管理端改完记忆立即调用它，用户才不会遇到「改完重启就没了」。
	// 为 nil 时静默跳过（依赖 5 分钟周期 flush 兜底）。
	memPersist func() error
	// memPath 长期记忆文件路径，由 main 注入。
	// 只有 handleMemoryClear 用得上：清记忆前要把原文件挪走备份，
	// 而 memPersist 只是个闭包，拿不到路径。
	memPath string
}

// New 创建管理端
func New(store *config.Store, router *llm.Router, engine *brain.Engine, mem *memory.Store, stats *statsdb.DB) (*Server, error) {
	if err := initCredentials(store); err != nil {
		return nil, err
	}
	return &Server{
		store:     store,
		router:    router,
		engine:    engine,
		mem:       mem,
		stats:     stats,
		limiter:   newLoginLimiter(),
		startTime: time.Now(),
	}, nil
}

// SetMemoryPersist 注入长期记忆的即时落盘回调（main 里传 mem.SaveTo(memPath)）
func (s *Server) SetMemoryPersist(fn func() error) { s.memPersist = fn }

// SetMemoryPath 注入长期记忆文件路径，供清记忆前备份原文件用。
func (s *Server) SetMemoryPath(p string) { s.memPath = p }

// SetMemePool 挂上表情包池并注入其落盘回调。池为 nil 时管理端显示「未启用」。
func (s *Server) SetMemePool(p *memepool.Pool, persist func() error) {
	s.memes = p
	s.memePersist = persist
}

// persistMemes 立刻写盘池子元数据；没注入回调就跳过（周期 flush 兜底）。
func (s *Server) persistMemes() {
	if s.memePersist == nil {
		return
	}
	if err := s.memePersist(); err != nil {
		logx.Warn("管理端触发表情包池落盘失败", "err", err.Error())
	}
}

// persistMemory 立刻把长期记忆写盘。失败只告警不回滚——内存已经改了，
// 回滚反而让「界面显示了但实际没改」更迷惑；周期 flush 会兜底重试。
func (s *Server) persistMemory() {
	if s.memPersist == nil {
		return
	}
	if err := s.memPersist(); err != nil {
		logx.Warn("管理端触发长期记忆落盘失败", "err", err.Error())
	}
}

// Handler 返回挂在管理端前缀下的处理器
func (s *Server) Handler(prefix string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/api/login", s.handleLogin)
	mux.HandleFunc(prefix+"/api/logout", s.handleLogout)
	mux.HandleFunc(prefix+"/api/state", s.auth(s.handleState))
	mux.HandleFunc(prefix+"/api/config", s.auth(s.handleSaveConfig))
	mux.HandleFunc(prefix+"/api/toggle", s.auth(s.handleToggle))
	mux.HandleFunc(prefix+"/api/reset", s.auth(s.handleReset))
	mux.HandleFunc(prefix+"/api/test", s.auth(s.handleTest))
	mux.HandleFunc(prefix+"/api/reload", s.auth(s.handleReload))
	mux.HandleFunc(prefix+"/api/bind", s.auth(s.handleBindMaster))
	mux.HandleFunc(prefix+"/api/unbind", s.auth(s.handleUnbindMaster))
	mux.HandleFunc(prefix+"/api/mute", s.auth(s.handleMute))
	mux.HandleFunc(prefix+"/api/usage_series", s.auth(s.handleUsageSeries))
	mux.HandleFunc(prefix+"/api/recent_calls", s.auth(s.handleRecentCalls))
	mux.HandleFunc(prefix+"/api/stats/status", s.auth(s.handleStatsStatus))
	mux.HandleFunc(prefix+"/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc(prefix+"/api/group/alias", s.auth(s.handleGroupAlias))
	mux.HandleFunc(prefix+"/api/group/remove", s.auth(s.handleGroupRemove))
	mux.HandleFunc(prefix+"/api/memory/clear", s.auth(s.handleMemoryClear))
	mux.HandleFunc(prefix+"/api/group/fact/set", s.auth(s.handleGroupFactSet))
	mux.HandleFunc(prefix+"/api/group/fact/delete", s.auth(s.handleGroupFactDelete))
	mux.HandleFunc(prefix+"/api/memes", s.auth(s.handleMemeList))
	mux.HandleFunc(prefix+"/api/meme/img", s.auth(s.handleMemeImage))
	mux.HandleFunc(prefix+"/api/memes/delete", s.auth(s.handleMemeDelete))

	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	// 必须剥离前缀：内嵌 FS 的根就是 assets 目录本身，
	// 不做 StripPrefix 的话会把 /admin/login.html 拿去查 assets/admin/login.html
	fileSrv := http.StripPrefix(prefix, http.FileServer(http.FS(sub)))
	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		// 前端是 //go:embed 编进二进制的，而 http.FileServer 对零 modtime 的内嵌文件
		// 不会发 Last-Modified/ETag，浏览器可能启发式缓存住旧页面——
		// 表现为「换了二进制但界面还是旧的」。管理端是内部工具，直接禁用缓存最省心。
		w.Header().Set("Cache-Control", "no-store")
		// 登录页本身必须放行，否则未登录访问它会被重定向回自己，形成死循环
		if r.URL.Path == prefix+"/login.html" {
			fileSrv.ServeHTTP(w, r)
			return
		}
		if !s.authed(r) {
			http.Redirect(w, r, prefix+"/login.html", http.StatusFound)
			return
		}
		// 访问目录根时直接给首页（FileServer 不会自动回 index.html）
		if r.URL.Path == prefix+"/" || r.URL.Path == prefix {
			b, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "index 缺失", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
			return
		}
		fileSrv.ServeHTTP(w, r)
	})
	return mux
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	cfg := s.store.Get()
	user, ok := parseSession(cfg.Admin.SessionSecret, c.Value)
	return ok && user == cfg.Admin.Username
}

// auth 接口级鉴权中间件
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "未登录"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	ip := clientIP(r)
	if ok, wait := s.limiter.allow(ip); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok": false, "msg": "登录失败次数过多，请稍后再试", "wait_sec": int(wait.Seconds()),
		})
		return
	}
	// 登录接口完全公开，没上限的 body 就能几个 2GB 请求把进程 OOM 掉
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}
	cfg := s.store.Get()
	if !verifyPassword(cfg, body.User, body.Pass) {
		s.limiter.fail(ip)
		logx.Warn("管理端登录失败", "ip", ip, "user", body.User)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "用户名或密码错误"})
		return
	}
	s.limiter.reset(ip)
	// 已经证明本人能登录，明文密码不再有必要留在磁盘上
	clearPlainPassword(s.store)
	payload := body.User + "|" + strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	token := signSession(cfg.Admin.SessionSecret, payload)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true,
		Secure:   true, // 全站 HTTPS，缺 Secure 等于给同网域的 http 链接留了个抄 cookie 的口子
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	logx.Info("管理端登录成功", "ip", ip, "user", body.User)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", Secure: true, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// state 管理端首页数据
type state struct {
	OK bool `json:"ok"`
	// Rev 配置版本号。前端靠它判断「配置到底有没有变」——
	// 只有它变化时才重画表单，否则 5 秒一次的轮询会不停重写 input.value，
	// 用户根本没法在页面上编辑任何东西。
	Rev     int64             `json:"rev"`
	Summary llm.Summary       `json:"summary"`
	Targets []llm.TargetStats `json:"targets"`
	// Chain 下一次调用真实的尝试顺序（复刻 Chat 的重试链路）。
	// 健康度表是按分数排序的，不是实际命中顺序——两者不一样，必须分开呈现。
	Chain    llm.ChainPreview `json:"chain"`
	Logs     []logEntry       `json:"logs"`
	Config   config.Config    `json:"config"`
	Uptime   string           `json:"uptime"`
	QQ       qqStatus         `json:"qq"`
	Usage    brain.DailyStat  `json:"usage"`
	Groups   []groupView      `json:"groups"`
	Master   masterView       `json:"master"`
	Schedule scheduleView     `json:"schedule"`
}

// groupView 一个群的运行状态
type groupView struct {
	OpenID      string            `json:"openid"`
	Name        string            `json:"name"`  // 记忆里的名字（可能只是 openid 尾 8 位代号）
	Alias       string            `json:"alias"` // 管理端设置的别名（config.groups），展示时优先于 Name
	Left        bool              `json:"left"`  // 机器人已被移出该群（GROUP_DEL_ROBOT 事件标记）
	Recent      int               `json:"recent"`
	Idle        string            `json:"idle"`
	MutedUntil  int64             `json:"muted_until"` // 静默截止时刻（Unix 秒），0 = 没在静默
	LastBotText string            `json:"last_bot_text"`
	// Summary 是「前文提要」：模型压缩出来、已被 recent 窗口裁掉的那段历史。
	// prompt.go 每轮都注入 user prompt，是模型知道「聊到哪了」的唯一来源——
	// 也就是**最影响说话、却曾经完全不可见**的一块记忆。
	// 控制台只能看不能改：它是模型生成的，改了下一次压缩又会被覆盖回去，
	// 想真正清掉它只能用「清空全部记忆」。
	Summary string `json:"summary"`
	// TotalLines 是历史累计条数。Recent 会被裁，所以 Recent 变小不代表
	// 聊天记录变少——两个一起显示才看得出「窗口里剩多少 / 一共聊过多少」。
	TotalLines int `json:"total_lines"`
	FactsItems  []memory.FactItem `json:"facts_items"` // 结构化长期要点（可编辑），按最后写入时间倒序
	FactsMax    int               `json:"facts_max"`   // 上限，前端显示「已用 n/上限」
	Members     []memory.Member   `json:"members"`
}

// masterView 开发者绑定与特权情况
type masterView struct {
	Nickname string   `json:"nickname"`
	QQ       string   `json:"qq"`
	OpenIDs  []string `json:"openids"`
	// DevEnabled 开发者特权开关。关着时（默认）开发者的消息与群友完全一样，
	// 绑定列表照常维护——认人与特权是两件事。
	DevEnabled bool `json:"dev_enabled"`
}

// logEntry 是 /api/state 里带的内存日志条目。
//
// 它现在的唯一用途是**降级通道**：统计库没打开时日志仍然可见
// （此时落盘的 events 表不存在，前端的 /api/logs 会返回 enabled:false）。
// 正常路径上前端不再读这个字段——日志有自己的接口和轮询。
type logEntry struct {
	TS string `json:"ts"`
	// Cat 与 /api/logs 的字段同名，共用一套分类枚举。
	Cat   string         `json:"cat,omitempty"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	KV    map[string]any `json:"kv,omitempty"`
}

type qqStatus struct {
	Sandbox bool   `json:"sandbox"`
	AppID   string `json:"app_id"`
}

// scheduleView 当前在线时段的实时情况，管理端用来做预览
type scheduleView struct {
	Enabled bool    `json:"enabled"`
	Mode    string  `json:"mode"`
	Rate    float64 `json:"rate"`  // 当前在线率 0~1
	Label   string  `json:"label"` // 命中哪个时段
	AtGrace int     `json:"at_grace_sec"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	// 掩码后再下发，避免 key 通过接口泄露（Get 返回深拷贝，改 safe 不污染真实配置）
	safe := cfg
	for i := range safe.LLM.Endpoints {
		safe.LLM.Endpoints[i].APIKey = maskKey(safe.LLM.Endpoints[i].APIKey)
	}
	// 会话密钥、密码哈希、认主口令与各渠道 API Key 同样不能明文下发：
	// 拿到 session_secret 就能自签 cookie 伪造会话，鉴权形同虚设
	safe.Admin.SessionSecret = maskKey(safe.Admin.SessionSecret)
	safe.Admin.PasswordBcrypt = maskKey(safe.Admin.PasswordBcrypt)
	safe.Admin.PasswordPlain = ""
	safe.Master.BindToken = maskKey(safe.Master.BindToken)
	safe.QQ.AppSecret = maskKey(safe.QQ.AppSecret) // 少掩一个，前面那份「逐字段掩码」就白做了
	safe.ASR.APIKey = maskKey(safe.ASR.APIKey)
	safe.Compact.APIKey = maskKey(safe.Compact.APIKey)
	safe.MemePool.MinIO.AccessKey = maskKey(safe.MemePool.MinIO.AccessKey)
	safe.MemePool.MinIO.SecretKey = maskKey(safe.MemePool.MinIO.SecretKey)
	entries := logx.Recent(200)
	logs := make([]logEntry, 0, len(entries))
	for _, e := range entries {
		logs = append(logs, logEntry{TS: e.TS, Cat: string(e.Cat), Level: e.Level, Msg: e.Msg, KV: e.KV})
	}
	uptime := time.Since(s.startTime).Truncate(time.Second).String()

	groups := make([]groupView, 0)
	if s.mem != nil {
		// 别名表：管理端给群起的人间名字，展示时优先于记忆里的代号
		aliases := map[string]string{}
		for _, gc := range cfg.Groups {
			if gc.Name != "" {
				aliases[gc.OpenID] = gc.Name
			}
		}
		for _, g := range s.mem.All() {
			members := g.Members()
			if len(members) > 20 {
				members = members[:20]
			}
			left, _ := g.Left()
			groups = append(groups, groupView{
				OpenID:      g.OpenID,
				Name:        g.Name,
				Alias:       aliases[g.OpenID],
				Left:        left,
				Recent:      len(g.Recent(0)),
				Idle:        g.IdleFor().Truncate(time.Second).String(),
				// 已经过期的静默一律报 0：前端只靠这个字段决定画不画「静默中」，
				// 报一个过去的截止时间会让界面显示「剩 -3 分钟」。
				MutedUntil:  muteDeadline(g.MutedUntil()),
				LastBotText: g.LastText(),
				Summary:     g.Summary(),
				TotalLines:  g.TotalLines(),
				FactsItems:  g.FactsList(),
				FactsMax:    memory.MaxFacts,
				Members:     members,
			})
		}
	}

	var usage brain.DailyStat
	if s.engine != nil {
		usage = s.engine.Stats()
	}

	writeJSON(w, http.StatusOK, state{
		OK:      true,
		Rev:     s.store.Rev(),
		Summary: s.router.Summary(),
		Targets: s.router.Stats(),
		Chain:   s.router.PreviewChain(),
		Logs:    logs,
		Config:  safe,
		Uptime:  uptime,
		QQ:      qqStatus{Sandbox: cfg.QQ.Sandbox, AppID: cfg.QQ.AppID},
		Usage:   usage,
		Groups:  groups,
		Master: masterView{
			Nickname:   cfg.Master.Nickname,
			QQ:         cfg.Master.QQ,
			OpenIDs:    cfg.Master.OpenIDs,
			DevEnabled: cfg.Master.DevEnabled,
		},
		Schedule: func() scheduleView {
			rate, label := brain.OnlineRate(cfg.Schedule, time.Now())
			return scheduleView{
				Enabled: cfg.Schedule.Enabled,
				Mode:    cfg.Schedule.Mode,
				Rate:    rate,
				Label:   label,
				AtGrace: cfg.Schedule.AtGraceSec,
			}
		}(),
	})
}

// handleBindMaster 手动把某个 openid 绑成开发者。
// 群里回调只给 openid，认人只能靠这个列表。
func (s *Server) handleBindMaster(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpenID string `json:"openid"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	added, err := s.store.BindMaster(body.OpenID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logx.Info("管理端绑定开发者", "openid", body.OpenID, "name", body.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
}

func (s *Server) handleUnbindMaster(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpenID string `json:"openid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	if err := s.store.UnbindMaster(body.OpenID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logx.Info("管理端解绑主人", "openid", body.OpenID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMute 让它在某个群闭嘴一段时间，或者（minutes=0）立刻取消静默。
//
// minutes 用指针而不是 int，是为了区分「没传这个字段」和「传了 0」：
// 前者沿用 30 分钟默认，后者是取消静默的明确指令。以前这里是 int，
// 0 会被当成「没给」而重新静默 30 分钟——想取消反而取消不掉。
func (s *Server) handleMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpenID  string `json:"openid"`
		Minutes *int   `json:"minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	if s.mem == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "记忆未初始化"})
		return
	}
	g := s.mem.Group(body.OpenID, "")
	// 没传 minutes 才用默认；传了就按传的来，0 = 取消静默。
	if body.Minutes == nil || *body.Minutes < 0 {
		def := 30
		body.Minutes = &def
	}
	if *body.Minutes == 0 {
		g.Unmute()
		logx.Info("管理端已取消静默", "group", body.OpenID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": false, "until": 0})
		return
	}
	g.Mute(time.Duration(*body.Minutes) * time.Minute)
	until := g.MutedUntil()
	logx.Info("管理端已设置静默", "group", body.OpenID, "分钟", *body.Minutes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": true, "until": until.Unix()})
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	var incoming config.Config
	// 信封格式 {rev, config}：rev 是前端拿到的配置版本号，用于乐观锁；
	// 旧版页面直接 POST 裸 config（无 rev），兼容放行
	var clientRev int64 = -1
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20) // 整包配置，允许比登录大一些但仍然有上限
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "读取请求失败"})
		return
	}
	var envelope struct {
		Rev    *int64          `json:"rev"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Config) > 0 && string(envelope.Config) != "null" {
		if err := json.Unmarshal(envelope.Config, &incoming); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "配置格式错误: " + err.Error()})
			return
		}
		if envelope.Rev != nil {
			clientRev = *envelope.Rev
		}
	} else if err := json.Unmarshal(raw, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "配置格式错误: " + err.Error()})
		return
	}
	// 乐观锁：页面基于的配置版本落后于当前版本时拒绝保存，
	// 否则多开标签页/多人同时编辑会静默互相覆盖（revhint 只提示不拦，拦在这里）
	if clientRev >= 0 && clientRev < s.store.Rev() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "msg": "配置已在别处被修改（页面数据已过期），请刷新后重新编辑再保存", "rev": s.store.Rev(),
		})
		return
	}
	// 前端拿到的 key 是掩码，直接回传会覆盖真实值，这里做还原
	if err := s.store.Update(func(c *config.Config) (bool, error) {
		for i := range incoming.LLM.Endpoints {
			if !isMasked(incoming.LLM.Endpoints[i].APIKey) {
				continue
			}
			restored := false
			for _, old := range c.LLM.Endpoints {
				if old.ID == incoming.LLM.Endpoints[i].ID {
					incoming.LLM.Endpoints[i].APIKey = old.APIKey
					restored = true
					break
				}
			}
			// id 在前端可编辑：改了 id 后按 id 必然匹配失败，
			// 按下标回退到同一位置的旧接入点——张冠李戴（401 可发现）好过静默丢 key
			if !restored && i < len(c.LLM.Endpoints) {
				incoming.LLM.Endpoints[i].APIKey = c.LLM.Endpoints[i].APIKey
			}
		}
		// QQ 凭证与会话密钥不允许在管理端修改（避免把自己锁在外面）
		incoming.QQ.AppID = c.QQ.AppID
		incoming.QQ.AppSecret = c.QQ.AppSecret
		incoming.Admin.SessionSecret = c.Admin.SessionSecret
		incoming.Admin.PasswordBcrypt = c.Admin.PasswordBcrypt
		incoming.Admin.PasswordPlain = c.Admin.PasswordPlain
		// base_url 同理锁死：改了它，/api/test 就会拿着真实 API Key 去请求攻击者指定的地址
		// （既是 SSRF，也是密钥外发通道）。要换接入点请改 config.json。
		// 匹配方式与上面的 api_key 一致：先按 id，改过 id 就按下标回退。
		for i := range incoming.LLM.Endpoints {
			restored := false
			for _, old := range c.LLM.Endpoints {
				if old.ID == incoming.LLM.Endpoints[i].ID {
					incoming.LLM.Endpoints[i].BaseURL = old.BaseURL
					restored = true
					break
				}
			}
			if !restored && i < len(c.LLM.Endpoints) {
				incoming.LLM.Endpoints[i].BaseURL = c.LLM.Endpoints[i].BaseURL
			}
		}
		// 开发者绑定列表漏传时保留原值：把自己解绑了就再也认不回来了
		if len(incoming.Master.OpenIDs) == 0 {
			incoming.Master.OpenIDs = c.Master.OpenIDs
		}
		if isMasked(incoming.Master.BindToken) {
			incoming.Master.BindToken = c.Master.BindToken
		}
		// ASR 与视频参数目前没有管理端表单，前端回传的是零值或掩码；
		// 直接覆盖会把手工写进 config.json 的配置抹掉（allow_proactive 的教训）。
		// 等哪天这些配置有了编辑入口，这里的保留逻辑要跟着撤。
		if isMasked(incoming.ASR.APIKey) {
			incoming.ASR = c.ASR
		}
		if isMasked(incoming.Compact.APIKey) {
			incoming.Compact = c.Compact
		}
		if incoming.Brain.VideoMaxSec == 0 {
			incoming.Brain.VideoMaxSec = c.Brain.VideoMaxSec
		}
		if incoming.Brain.VideoMaxMB == 0 {
			incoming.Brain.VideoMaxMB = c.Brain.VideoMaxMB
		}
		if incoming.Brain.VideoFrames == 0 {
			incoming.Brain.VideoFrames = c.Brain.VideoFrames
		}
		if incoming.Brain.VideoFrameSide == 0 {
			incoming.Brain.VideoFrameSide = c.Brain.VideoFrameSide
		}
		// 天气三件套没有管理端表单，前端整包保存时按零值回传。
		// 必须整块保留，否则手写进 config.json 的坐标与地名会被清掉——
		// 而 normalize() 已不再回填任何默认地点（不该给所有用户塞同一个城市），
		// 一旦清零就没有自愈路径了，只能手动改配置文件。
		// 无表单字段被整对象替换清零的教训见 ASR/Compact。
		if incoming.Brain.WeatherPlace == "" {
			incoming.Brain.WeatherPlace = c.Brain.WeatherPlace
			incoming.Brain.WeatherLat = c.Brain.WeatherLat
			incoming.Brain.WeatherLon = c.Brain.WeatherLon
		}
		// 表情包池的 MinIO 段含密钥且没有管理端表单，前端整块不回传。
		// 无表单字段被整对象替换清零的教训见 ASR/Compact。
		if isMasked(incoming.MemePool.MinIO.AccessKey) {
			incoming.MemePool.MinIO = c.MemePool.MinIO
		}
		// 群别名表走 /api/group/alias 单独维护，整包保存时前端可能没回传（nil）；
		// 无表单字段被整对象替换清零的教训见 ASR/Compact。注意区分 nil（没回传，保留）
		// 与显式空数组（用户就是要清空，放行）。
		if incoming.Groups == nil {
			incoming.Groups = c.Groups
		}
		*c = incoming
		return true, nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "保存失败: " + err.Error()})
		return
	}
	s.router.Reload()
	logx.Info("管理端已更新配置并重载调用器")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndpointID string `json:"endpoint_id"`
		Model      string `json:"model"`
		Enabled    bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}
	if err := s.store.Update(func(c *config.Config) (bool, error) {
		for i := range c.LLM.Endpoints {
			if c.LLM.Endpoints[i].ID != body.EndpointID {
				continue
			}
			if body.Model == "" {
				c.LLM.Endpoints[i].Enabled = body.Enabled
				return true, nil
			}
			for j := range c.LLM.Endpoints[i].Models {
				if c.LLM.Endpoints[i].Models[j].ID == body.Model {
					c.LLM.Endpoints[i].Models[j].Enabled = body.Enabled
					return true, nil
				}
			}
			return false, nil
		}
		return false, nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	s.router.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndpointID string `json:"endpoint_id"`
		Model      string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.router.ResetTarget(body.EndpointID, body.Model) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "目标不存在"})
}

// handleTest 用指定模型发一句话，验证这个（接入点, 模型）是否真的可用
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndpointID string `json:"endpoint_id"`
		Model      string `json:"model"`
		Prompt     string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}
	if body.Prompt == "" {
		body.Prompt = "用一句话打个招呼"
	}
	cfg := s.store.Get()
	var target *llm.Target
	for _, ep := range cfg.LLM.Endpoints {
		if ep.ID != body.EndpointID {
			continue
		}
		for _, m := range ep.Models {
			if m.ID != body.Model {
				continue
			}
			target = &llm.Target{
				EndpointID:   ep.ID,
				EndpointName: ep.Name,
				BaseURL:      ep.BaseURL,
				APIKey:       ep.APIKey,
				APIType:      ep.APIType,
				Model:        m.ID,
				MaxOut:       m.MaxOut,
				Stream:       m.Stream,
				Timeout:      time.Duration(ep.TimeoutMS) * time.Millisecond,
				Enabled:      true,
			}
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "未找到该模型"})
		return
	}
	start := time.Now()
	res, cerr := llm.Call(context.Background(), target, llm.Request{
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: body.Prompt}},
		Temperature: 0.7,
		MaxTokens:   256,
	})
	if cerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": cerr.Error(), "cost_ms": msSince(start),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"content":  res.Content,
		"ttft_ms":  res.TTFTMS,
		"cost_ms":  res.LatencyMS,
		"endpoint": res.Endpoint,
		"model":    res.Model,
	})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.router.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUsageSeries 分时段 × 模型的调用统计（近 60 分钟逐分钟 / 今日逐小时 / 近 7 天逐日）
func (s *Server) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	switch r.URL.Query().Get("range") {
	case "60m":
		buckets, err := s.stats.MinutelyN(60)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "range": "60m", "buckets": buckets})
	case "", "today":
		buckets, err := s.stats.HourlyToday()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "range": "today", "buckets": buckets})
	case "7d":
		buckets, err := s.stats.DailyN(7)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "range": "7d", "buckets": buckets})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "range 只支持 60m / today / 7d"})
	}
}

// handleRecentCalls 最近若干条调用明细（精确到秒）。
// 聚合图回答「分布」，这张表回答「刚刚到底发生了什么」。
func (s *Server) handleRecentCalls(w http.ResponseWriter, r *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.stats.RecentCalls(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "calls": rows})
}

// handleStatsStatus 统计库运行状态（降级时可观测）
func (s *Server) handleStatsStatus(w http.ResponseWriter, r *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": true,
		"dropped": s.stats.Dropped(), "db_size_bytes": s.stats.SizeBytes(),
	})
}

// handleLogs 查询事件日志。
//
// 独立于 /api/state：那个接口每次返回掩码后的完整 config + targets + chain +
// groups + usage + schedule，日志只是其中一个小字段。为了它拖着整套重数据
// 每 5 秒拉一次不合理——而日志恰恰是唯一需要独立刷新节奏的东西。
//
// 参数：range(60m/today/7d/30d) / cat(逗号分隔) / level(逗号分隔) / q(关键字) / limit
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "rows": []any{}})
		return
	}
	q := r.URL.Query()
	f := statsdb.EventFilter{
		Since:  logRangeSince(q.Get("range")),
		Cats:   splitCSV(q.Get("cat")),
		Levels: splitCSV(q.Get("level")),
		Q:      q.Get("q"),
	}
	// limit 在 QueryEvents 里有硬上限（MaxEventLimit），
	// 这里不再重复校验——handler 和查询层对同一个数字负责会漂移。
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	rows, err := s.stats.QueryEvents(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	total, err := s.stats.CountEvents(f)
	if err != nil {
		total = len(rows) // 数不出来就退回本次返回的条数，别让整个页面报错
	}
	cats, err := s.stats.EventCats()
	if err != nil {
		cats = map[string]int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": true, "rows": rows,
		"total": total, "cats": cats,
	})
}

// logRangeSince 把 range 参数翻译成起始时间。
// 认不出来的值当「不限」——返回零值让 SQL 那边不加 ts 条件。
func logRangeSince(rng string) time.Time {
	var d time.Duration
	switch rng {
	case "60m":
		d = time.Hour
	case "today":
		now := time.Now()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "7d":
		d = 7 * 24 * time.Hour
	case "30d":
		d = 30 * 24 * time.Hour
	default:
		return time.Time{}
	}
	return time.Now().Add(-d)
}

// splitCSV 拆逗号分隔的参数，顺手丢掉空项。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// handleGroupAlias 给群设置别名。QQ 平台不暴露群名，别名是管理端唯一的「人话」来源。
// 别名写进 config.groups（长期有效），同时刷新记忆里的群名（纠正历史固化的代号）。
func (s *Server) handleGroupAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	var body struct {
		OpenID string `json:"openid"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if err := s.store.Update(func(c *config.Config) (bool, error) {
		for i := range c.Groups {
			if c.Groups[i].OpenID == body.OpenID {
				c.Groups[i].Name = body.Name
				return true, nil
			}
		}
		c.Groups = append(c.Groups, config.GroupConfig{OpenID: body.OpenID, Name: body.Name, Enabled: true})
		return true, nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	if s.mem != nil && body.Name != "" {
		s.mem.RefreshName(body.OpenID, body.Name)
	}
	s.persistMemory()
	logx.Info("管理端设置群别名", "group", body.OpenID, "alias", body.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGroupRemove 彻底移除一个群的全部记忆数据（用于清理已退群的死数据）。
// config.groups 里的别名保留：机器人哪天被拉回同一个群，别名直接复活。
func (s *Server) handleGroupRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	var body struct {
		OpenID string `json:"openid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	if s.mem == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "记忆未初始化"})
		return
	}
	s.mem.RemoveGroup(body.OpenID)
	s.persistMemory()
	logx.Info("管理端已移除群数据", "group", body.OpenID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMemoryClear 清空全部群的记忆，用于切人设。
//
// 只切人设不重置记忆会出事：记忆里的事实、成员画像、摘要是按**旧人设**的
// 判断流程攒下来的，新人设读到它们只会把已经废掉的行为学回来。
//
// 顺序是「备份 → 移走原文件 → Clear → 写回空快照」，不是简单的 Clear +
// SaveTo。原因是 memory.SaveTo 里的 guardAgainstEmptyOverwrite：磁盘上
// 有群、快照 0 群时它会拒绝覆盖（那个守卫存在的目的正是拦住空覆盖），
// 于是清完内存、写盘被拒，进程一重启记忆原样回来——按钮看着成功了，
// 其实没生效。移走原文件之后守卫读不到文件，放行，写的还是空快照。
//
// 备份不是可选项：这一下是不可逆的，没备份就没有回头路。
func (s *Server) handleMemoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	if s.mem == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "记忆未初始化"})
		return
	}
	backup := ""
	if s.memPath != "" {
		if _, err := os.Stat(s.memPath); err == nil {
			backup = s.memPath + ".bak-" + time.Now().Format("20060102-150405")
			if err := os.Rename(s.memPath, backup); err != nil {
				// 挪不走就整个不继续。宁可让按钮报错，也不能出现
				// 「内存清了、文件还在」的半吊子状态。
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "备份原记忆文件失败，已放弃清空：" + err.Error()})
				return
			}
		}
	}
	n := s.mem.Clear()
	// 这里不走 persistMemory()：它失败只告警，而这次失败必须让用户看见。
	if s.memPersist != nil {
		if err := s.memPersist(); err != nil {
			logx.Error("清空记忆后写盘失败", "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "清空已生效但写盘失败，重启会变回来：" + err.Error()})
			return
		}
	}
	logx.Info("管理端已清空全部记忆", "groups", n, "backup", backup)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": n, "backup": backup})
}
// 满了会按「最久没被写过」淘汰一条，被淘汰的 key 通过 evicted 返回，界面好提示。
// handleGroupFactSet 新增或修改一条群长期要点。
func (s *Server) handleGroupFactSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	var body struct {
		OpenID string `json:"openid"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid"})
		return
	}
	if strings.TrimSpace(body.Key) == "" || strings.TrimSpace(body.Value) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "要点与内容都不能为空"})
		return
	}
	if s.mem == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "记忆未初始化"})
		return
	}
	g := s.mem.Group(body.OpenID, "")
	evicted := g.SetFact(body.Key, body.Value)
	s.persistMemory()
	logx.Info("管理端写入群长期要点", "group", body.OpenID, "key", body.Key, "淘汰", evicted)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "evicted": evicted})
}

// handleGroupFactDelete 删除一条群长期要点
func (s *Server) handleGroupFactDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	var body struct {
		OpenID string `json:"openid"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OpenID == "" || strings.TrimSpace(body.Key) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 openid 或 key"})
		return
	}
	if s.mem == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "记忆未初始化"})
		return
	}
	s.mem.Group(body.OpenID, "").DelFact(body.Key)
	s.persistMemory()
	logx.Info("管理端删除群长期要点", "group", body.OpenID, "key", body.Key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// muteDeadline 把静默截止时刻转成 Unix 秒，已过期返回 0。
func muteDeadline(until time.Time) int64 {
	if !until.After(time.Now()) {
		return 0
	}
	return until.Unix()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

const maskPrefix = "***"

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 8 {
		return maskPrefix
	}
	// 按 rune 截取，否则多字节字符会被截成乱码
	return maskPrefix + string(r[len(r)-4:])
}

func isMasked(k string) bool {
	return k == "" || strings.HasPrefix(k, maskPrefix)
}
