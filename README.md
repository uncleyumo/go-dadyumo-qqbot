# go-dadyumo-qqbot · 羽沫老爹

一个常驻在服务器上的 **QQ 群聊 AI 群友**，人设叫「羽沫老爹」。

它的目标不是"问答助手"，而是群里那个四十来岁、嘴碎但有分寸的老东西：

- **会潜水**：不每条消息都回，攒一批消息再决定要不要开口
- **会闭嘴**：提示词里明确给了它 `act=quiet` 的权利，话少才像真人
- **会拆句**：把一段话拆成好几条一条条往外蹦，急了一个字一条
- **会拒绝**：被当工具使唤、被反复测试、被恶意刷，它不伺候
- **会认人**：对配置里的主人有明确的忠诚设定
- **会看气氛**：情绪检测前置，别人难受时自动收起嘴臭切共情
- **会自己找模型**：中央调用器按首字延迟/成功率/可用性排序，一个挂了立刻换下一个

交付形态：**一个无扩展名的 Go 二进制 + 一个 config.json**，外加一个内嵌的 Web 管理端（免外部静态文件）。

> **本项目以 GPL-3.0 协议开源**，Copyright (C) 2026 uncleyumo。详见 [LICENSE](LICENSE)（该文件为 GNU 官方原文，未作任何修改）。
>
> 你 fork、改动、重新分发时必须同样以 GPL-3.0 发布并保留本声明。若分发二进制或源码，你应当在自己的文件头写上 `Copyright (C) 2026 <你的名字>`。

---

## 目录结构

```
go-dadyumo-qqbot/
├── cmd/dadyumo-qqbot/main.go   入口：装配各组件、开两个 HTTP 服务、优雅退出
├── internal/
│   ├── webhook/                 QQ 回调接收：Ed25519 验签、op=13 地址验证、事件分发
│   ├── qqapi/                   QQ 发送：token 自愈刷新、被动消息（挂 msg_id）、锚点池、限流
│   ├── llm/                     中央调用器：chat/completions + responses 双方言、健康度排序、故障转移
│   ├── brain/                   决策层：冲动值、攒批调度、上下文裁剪、拆句、提示词、输出容错解析
│   ├── agent/                   事件入口，把 webhook 事件接到 brain
│   ├── memory/                  短期会话窗口 + 长期要点 + 成员画像（落盘）
│   ├── admin/                   Web 管理端：登录、在线改配置、连通性测试、群与主人、日志
│   ├── config/                  配置加载/校验/热更新/原子落盘
│   └── logx/                    结构化日志 + 内存环形缓冲（管理端实时看）
├── deploy/                      systemd 单元、Caddy 反代示例
├── scripts/                     编译与部署脚本
├── config.json.example          配置模板（含完整人设）
└── config.json                  真实配置（已在 .gitignore 中，绝不入库）
```

---

## 本地开发

```bash
git clone https://github.com/uncleyumo/go-dadyumo-qqbot.git
cd go-dadyumo-qqbot
cp config.json.example config.json      # 首次：从模板生成自己的配置
```

Windows（PowerShell）用 `Copy-Item config.json.example config.json` 代替 `cp`。

然后：

```bash
go test ./...                                # 跑测试
go run ./cmd/dadyumo-qqbot -config config.json -debug
```

Go 版本要求 1.21+。国内拉依赖建议：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

> **Windows 用户注意**：仓库通过 `.gitattributes` 强制 `.sh`/`.go` 等文本文件保持 LF 行尾。直接 clone 即可正常工作，**不要**开启 Git 的 `core.autocrlf=CRLF`。

---

## 部署到服务器

前置：本机 `~/.ssh/config` 里配好了你的服务器别名（下文统一写作 `your-server`）。

服务器路径：`/opt/dadyumo-qqbot`（若放在 `/root` 下，systemd 必须以 root 运行）。

### 首次

```bash
# 1) 建目录 + 装 systemd（Git Bash 执行）
HOST=your-server bash ./scripts/setup-server.sh

# 2) 上传你本地调好的配置（只做这一次，之后脚本不会覆盖它）
scp config.json your-server:/opt/dadyumo-qqbot/config.json

# 3) 编译并上线
HOST=your-server bash ./scripts/deploy.sh
```

### 日常更新

```bash
HOST=your-server bash ./scripts/deploy.sh
```

PowerShell 版：`.\scripts\deploy.ps1 -HostAlias your-server`

脚本做的事：交叉编译 linux/amd64 静态二进制 → scp 到 `.new` → 原子切换 → `systemctl restart` → 打最近 20 行日志。**它只换二进制，绝不动服务器上的 config.json。**

> **注意**：若服务器上已有服务正在运行，`systemctl stop` 必须先于 scp 覆盖，否则会报 `ETXTBSY: text file busy`。

### 服务器上的路径

| 路径 | 说明 |
| --- | --- |
| `/opt/dadyumo-qqbot/dadyumo-qqbot` | 二进制 |
| `/opt/dadyumo-qqbot/config.json` | 配置（管理端在线改的就是它） |
| `/opt/dadyumo-qqbot/data/memory.json` | 长期记忆（要点 + 成员画像） |

常用运维：

```bash
ssh your-server 'sudo systemctl status dadyumo-qqbot'
ssh your-server 'sudo journalctl -u dadyumo-qqbot -f'
ssh your-server 'curl -s 127.0.0.1:8080/healthz'
```

---

## 两个 HTTP 服务

| 服务 | 默认监听 | 用途 |
| --- | --- | --- |
| 回调 | `0.0.0.0:8080` | QQ 平台回调，路径 `/qq/callback`，必须 HTTPS 且端口 ∈ {80,443,8080,8443} |
| 管理端 | `127.0.0.1:8081` | 登录、在线改人设/接入点/预算、看群与主人、看日志，路径 `/admin` |

Caddy 反代配置见 `deploy/Caddyfile.example`（示例域名 `bot.example.com`，回调地址 `https://bot.example.com/qq/callback`，请替换成你自己的）。管理端监听回环地址，公网访问要么走反代（建议再加一层 basic auth），要么 SSH 隧道：

```powershell
ssh -L 8081:127.0.0.1:8081 your-server
# 然后浏览器打开 http://127.0.0.1:8081/admin
```

**管理端密码**：`config.json` 的 `admin.password_plain` 是初始明文密码，进程启动时转成 bcrypt；**首次登录成功后明文会自动从配置中清除**（用完即焚）。

---

## 怎么让它像个人

这一节是本项目最关键的部分，改配置前建议先读完。

### 1. 攒批（debounce）—— 它不会秒回

收到消息后不立刻问模型，而是先等 `brain.debounce_sec + 随机抖动` 秒，期间如果有人接着说话就继续等（上限 45 秒）。

这既是拟真（真人反应需要时间），也是省钱最有效的旋钮：把 10 秒调到 20 秒，调用次数大约减半。

### 2. 冲动值（impulse）—— 它有权不想说话

每条消息按规则打一个 0~1 的冲动分，低于 `brain.impulse_threshold` 就直接闭嘴，根本不调模型：

| 因素 | 权重 |
| --- | --- |
| 被 @ | +0.90 |
| 被叫名字 | +0.70 |
| 主人说话 | +0.50 |
| 有人在接它的话 | +0.30 |
| 有人提问 | +0.25 |
| 攒够 5 条新消息 | +0.15 |
| 已经连着说了 2 轮 / 4 轮 | −0.50 / −0.90 |
| 25 秒内刚发过言 | −0.80 |

### 3. 拆句发送 —— 一条一条往外蹦

模型按协议在 `text` 里用 `\n` 表示"我要分几条发"，程序拆开后按真人速度逐条发送：

```
我 / 真的 / 不 / 知 / 道 / ！
```

条间延迟 = `min_delay ~ max_delay` 随机 + `per_char_ms × 上一条字数`，上限 6 秒。开口前还会先"想一下"（`first_delay_ms`）。

一条超过 `max_seg_chars` 时按标点自动再拆；一次最多 `max_segments` 条（QQ 被动消息同一 msg_id 上限 5 次）。

### 4. 拒绝被当机器人使唤

`persona.refuse_rules` 里写死了几种它不伺候的情形：让它翻译/总结/算账/执行命令、反复测试它是不是机器人、刷屏 @ 它、让它做不合适的事。它要么怼回去，要么直接不回。

### 5. 闭嘴规则

`persona.silence_rules` 覆盖：插不上嘴的话题、两个人在私聊、自己说完没人接、纯表情接龙、重复表态、不知道的事。

---

## 主人识别（重要）

**QQ 开放平台的回调里只有 openid，拿不到真实 QQ 号。** 所以「认出主人」只能靠 openid 绑定，两条路：

1. **口令绑定**：在群里发 `#认主 <你在 master.bind_token 里设的口令>`，它回一句「记住你了」，你的 openid 就写进 `config.json` 的 `master.openids`。绑定成功后建议把 `master.bind_enabled` 改成 `false`。
2. **管理端手动绑定**：`群与主人` 页签里列出每个群最近说话的人（昵称 + openid），点「设为主人」即可。

绑好之后，`persona.loyalty` 那段"对开发者的态度"才认得出人；`master.nickname` 也会进提示词。

---

## 上下文与成本

### 上下文本身完全够用

`brain.max_ctx_tokens`（默认 4000）只约束**历史部分**，人设提示词另算约 2400 token，单次请求总计约 6~7K token。

即使将来换到只剩 **32K** 的小模型，也绰绰有余——`ContextBudget` 会取「配置预算」和「模型标称上下文 − 输出预留 − 1/4 余量」中的**较小值**做硬裁剪，换小模型不会炸。

### 模型优先级（`priority`）

路由**不是**纯拼速度：每个模型可以设 `priority`，数值越大越优先，它是**硬分层**而不是加权分。

- 同档内才比健康度；主力即使比免费的兜底慢，只要还活着就走主力
- 主力一旦报错会立刻进冷却（指数退避 5s → 10s → 20s…），自动被踢出候选集，流量落到下一档
- 探路（ε-贪心）只在最高档内部进行，不会把主力流量随机丢给兜底

主力模型与兜底模型的 priority 数值关系决定流量分配。`config.json.example` 里给的是「主力 priority 2 + 免费兜底 priority 0」的配置；想切回免费主力，把两者的 priority 对调即可，不用删配置。

### 成本测算

单次请求按：输入约 4000 token（典型）/ 6400 token（上限），输出约 120 token（典型）/ 400 token（上限）。

| 模型 | 单价（每百万 token） | 单次典型 | 150 次/天 | 300 次/天（预算硬顶） |
| --- | --- | --- | --- | --- |
| `stealth/space-bunny-alpha` | 输入 $0 / 输出 $0 | **$0** | **¥0** | **¥0** |
| `deepseek/deepseek-v4.1-flash` | 输入 $0.30 / 输出 $1.20 | $0.0013 | ≈ ¥1.4/天 · ¥43/月 | ≈ ¥5.1/天 · ¥153/月 |

> 汇率按 1 USD ≈ 7.1 CNY 估算。价格取自 OpenRouter 公开价目，随时可能变动。

**结论**：主力模型设`priority = 1`，免费兜底模型设 `priority = 0`（仍然免费）。两个 10 人群的正常聊天频率下，日成本大约 **¥1~1.5、月 ¥40~45**；`brain.daily_budget = 300` 是硬闸门，即便被打满月成本封顶约 ¥150。

> 想再省，按顺序调这三个旋钮最有效：`brain.debounce_sec`（攒批拉长）→ `brain.impulse_threshold`（调高更沉默）→ `brain.daily_budget`（硬顶）。

---

## 配置要点速查

| 字段 | 说明 |
| --- | --- |
| `qq.app_id` / `app_secret` | QQ 开放平台凭证。**管理端不允许改这两项**，防止把自己锁在外面 |
| `qq.sandbox` | 生产必须 `false` |
| `llm.endpoints[].api_type` | `chat_completions` 或 `responses`，**OpenRouter 两种都提供，选错会 404** |
| `llm.max_attempts` | 单次请求最多试几个模型，默认 4 |
| `persona.*` | 人设：背景、风格、口头禅、闭嘴规则、拒绝规则、嘴臭边界、红线、忠诚、单条字数上限 |
| `brain.daily_budget` | 每日 LLM 调用硬预算，超了只对主人和被 @ 放行 |
| `brain.debounce_sec` | 攒批静默窗口，最有效的省钱旋钮 |
| `brain.max_ctx_tokens` | 历史部分的 token 预算 |
| `master.*` | 主人昵称、QQ 备注、已绑定 openid、认主口令 |
| `speak.*` | 拆句条数、条间延迟、打字速度、开口前思考时间 |

---

## 进度

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P0 | 回调链路：验签、op=13、事件解析、被动回复 | 完成 |
| P1 | 中央调用器：双协议方言、健康度排序、故障转移、TTFT 测量 | 完成 |
| P2 | Web 管理端：登录、在线改配置、连通性测试、群与主人、实时日志 | 完成 |
| P3 | 大脑层：冲动值、攒批调度、上下文裁剪、拆句发送、预算闸门 | 完成 |
| P4 | 记忆系统：长期要点、成员画像、认主绑定、落盘 | 完成 |
| P5 | 部署交付：systemd、Caddy、脚本 | 完成 |
| P6 | 语音与视频理解：视频抽帧 + 音轨转写、语音消息兜底 | 完成 |
| P7 | 在线时段调度：按时间段概率在线，避免整点突然失联 | 完成 |

---

## 已知坑（都在代码里处理过了）

- **botgo 的 `StartRefreshAccessToken` 会 panic**：首次取 token 失败会直接返回错误，后台连续失败 10 次直接崩。这里没用它，改成自己跑刷新循环，失败只记日志 + 指数退避，凭证写错也能先把服务起来。
- **官方文档的签名 DEMO 对不上**：它给的 `signature` 和自己给的 `body` 算不出来。代码以官方给的**公钥字节**为准做了黄金测试，测试注释里记了这笔勘误。
- **延迟必须用微秒精度**：`time.Since().Milliseconds()` 是整数截断，极快响应会记成 0，和"从未测量"撞值导致排序失效。
- **配置 `Get()` 必须深拷贝**：管理端会先给 API Key 打码再下发，浅拷贝会把真实 Key 冲掉并落盘。有回归测试盯着。
- **健康度排序曾出现"赢者通吃"**：延迟分用候选集内相对归一化，当只有一个目标有数据时，它自己落在区间起点拿满分，与"从未测量"的目标并列，胜负只能靠成功率——结果冷启动阶段谁先被随机选中谁就通吃。已改为"未试过的一律排在试过的之前"，并有回归测试。
- **随机密码曾生成后立刻抹掉明文**：用户根本来不及去读 config.json。已改为首次登录成功后才清除。
- **Responses API 的 system 提示词要放 `instructions`**：混进 `input` 会被判 role 非法。有测试盯着。
- **流式响应里的 reasoning 摘要不能当输出**：只认 `type=message` 的条目，否则它会把"想了一下"也发到群里。
- **管理端前端是编译进二进制的**（`//go:embed assets`）：改了 `internal/admin/assets/index.html` 必须重新编译换二进制才生效。报"界面还是旧的"时先怀疑没部署，其次才是浏览器缓存（Ctrl+Shift+R）。
- **管理端 CSS 的 ID 选择器会压过 `.hide`**：`#ovl { display:block }` 的特异性高于 `.hide { display:none }`，`classList.add('hide')` 会失效。任何用 `.hide` 切显隐的元素都要配一条 `#id.hide{display:none}`。
- **平台已下线主动推送**：腾讯 QQ 开放平台自 2025-04 起不再支持机器人主动发消息（错误码 `40034105`）。机器人只能被动回复，且被动消息必须挂一条 5 分钟内、使用未满 5 次的用户消息 `msg_id` 作为锚点。所以它只能「回应」，不能「起头」。
- **群里没开全量消息时它会显得不理人**：没开的群只有 @ 的消息会推过来。需群主在手机 QQ → 群设置 → 群机器人里选「获取群内全部消息」。

---

## 已知限制

- **上下文预算有限**：`brain.max_ctx_tokens` 只约束历史部分。模型最小上下文窗口建议 12K 以上，8K 会撞（`ContextBudget` 需 `maxCtx - maxOut - 25% ≥ 3200` 才给满额历史预算）。
- **成本随活跃度线性增长**：`daily_budget` 是硬闸门但不是省钱手段，真正的旋钮是攒批时长与冲动阈值。
- **单进程单配置**：没有多实例水平扩展，每个群的消息窗口与成员画像都存在本地 `data/`。
- **依赖 QQ 开放平台的 webhook 模式**：没有 websocket 长连选项，平台侧回调配置和群侧权限开关缺一不可。

---

## 参与贡献

欢迎提Issue 和 PR。动手前请先读 [CONTRIBUTING.md](CONTRIBUTING.md)；安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，不要开公开 Issue。

## 致谢

- [腾讯 QQ 开放平台](https://q.qq.com/) 提供机器人 webhook 能力
- [tencent-connect/botgo](https://github.com/tencent-connect/botgo) QQ 机器人 Go SDK（本项目只用了它的协议层，token 刷新自己实现）
- [OpenRouter](https://openrouter.ai/) 多模型聚合接入
