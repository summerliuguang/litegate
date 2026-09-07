# LiteGate —— 轻量级 AI 网关 研究报告与设计方案

> 调研日期：2026-09-04 ｜ 状态：M1–M5 已全部交付；M6+ 按需推进
> 一句话定位：**把 LiteLLM 的网关能力和 cc-switch 的"供应商切换"体验，装进一个 ~20MB 的单二进制 + 内嵌 Web UI 里。**

---

## 1. 项目背景

### 1.1 现状与痛点

当前使用大模型 API 的典型场景中，普遍存在以下痛点：

1. **供应商碎片化**：一个开发者往往同时持有 OpenAI / Anthropic / Gemini / DeepSeek / Kimi 以及多个中转站（Relay）的 Key，不同协议（OpenAI Chat、Anthropic Messages、Gemini native）、不同计费、不同限额，管理成本高。
2. **Coding Agent 依赖单一供应商**：Claude Code、Codex、Gemini CLI、OpenCode 等 CLI 工具的供应商配置写死在本地配置文件里，切换供应商 = 手工改配置文件，容易出错、无法灰度、无故障切换。
3. **现有网关太重**：LiteLLM 功能最全，但 Python 技术栈 + 可选 Postgres/Redis，空载内存通常 300MB+，启动慢，个人/小团队在 NAS、软路由、小 VPS 上部署吃力。
4. **现有切换器没有网关能力**：cc-switch 等桌面工具只负责改写本地配置，请求仍直连供应商——没有统一日志、没有用量统计、没有故障转移、没有多 Key 轮询。

### 1.2 项目目标

做一个**自托管的轻量 AI 网关**，核心指标：

| 目标 | 指标 |
|---|---|
| 资源占用 | 单二进制 ≤ 25MB，空载内存 ≤ 50MB，冷启动 < 1s |
| 部署形态 | 单个二进制直接运行（裸跑 / systemd），默认 SQLite 零外部依赖，**不依赖 Docker** |
| 管理方式 | 内嵌 Web UI（go:embed），打开浏览器即管理 |
| 协议 | 下游同时暴露 OpenAI / Anthropic / Gemini 三种原生协议；上游适配同三种 + 中转站 |
| Agent 友好 | 为 Claude Code / Codex / Gemini CLI 一键生成接入配置（吸收 cc-switch 的体验） |

**非目标（明确不做）**：多租户 SaaS 计费体系、团队权限树、插件市场、模型微调托管。保持"个人/小团队自用"的克制范围，是轻量的前提。

---

## 2. 项目调研

以下数据来自 GitHub API 实时抓取（2026-09-04）。

### 2.1 网关类（服务端，统一 API 入口）

| 项目 | Stars | 技术栈 | 特点 | 与本项目的关系 |
|---|---|---|---|---|
| [BerriAI/litellm](https://github.com/BerriAI/litellm) | 58.0k | Python + Rust 核心 | 100+ Provider、虚拟密钥、预算/限流、Admin UI，生态最全 | 功能标杆；但重（Python 栈，常配 Postgres/Redis），不适合小设备 |
| [QuantumNous/new-api](https://github.com/QuantumNous/new-api) | 47.3k | Go | one-api 增强版：聚合分发、计费、多协议、Web UI | 偏"分发/计费站"场景，体量大、概念多 |
| [songquanpeng/one-api](https://github.com/songquanpeng/one-api) | 36.7k | Go + JS | 单二进制 + SQLite + 内嵌 UI，业界普及度最高 | 轻量路径的先行者；功能迭代趋缓，协议覆盖偏 OpenAI |
| [musistudio/claude-code-router](https://github.com/musistudio/claude-code-router) | 37.1k | TypeScript | 本地控制面：为 Claude Code 等做模型路由/能力编排 | 证明了"Coding Agent 专用路由"需求旺盛；Node 栈、面向单机 |
| [Portkey-AI/gateway](https://github.com/Portkey-AI/gateway) | 12.9k | TypeScript | 1600+ 模型、50+ 守护栏、极快 | 开源版无内嵌 UI，管理靠托管云 |
| [tensorzero/tensorzero](https://github.com/tensorzero/tensorzero) | 11.7k | Rust | 网关 + 观测 + 评估 + 微调的 LLMOps 平台 | 偏 ML 平台工程，超出个人网关范畴 |
| [coaidev/coai](https://github.com/coaidev/coai) | 9.3k | TypeScript | 多租户一站式、内置管理与计费 | SaaS 化路线，重 |
| [maximhq/bifrost](https://github.com/maximhq/bifrost) | 7.8k | Go | 主打性能（宣称 50x LiteLLM）、插件体系、集群模式 | 性能路线；企业向，部署概念多 |
| [tbphp/gpt-load](https://github.com/tbphp/gpt-load) | 6.5k | Go | **多渠道多凭证**：API Key + 订阅账号统一调度，内嵌 UI，SQLite/MySQL/PG，本地凭证加密 | **最接近本项目的现有产品**，重点对标 |
| [looplj/axonhub](https://github.com/looplj/axonhub) | 5.2k | Go | 任意 SDK 调 100+ 模型，failover、负载均衡、成本跟踪、内置 UI | 对标参考 |
| [envoyproxy/ai-gateway](https://github.com/envoyproxy/ai-gateway) | 2.0k | Go (Envoy) | 基于 Envoy Gateway 的 K8s 生态 AI 网关 | 基础设施路线，过重 |
| [APIParkLab/APIPark](https://github.com/APIParkLab/APIPark) | 1.8k | TypeScript | 云原生 API/LLM 管理门户 | 企业门户路线，过重 |
| [bricks-cloud/BricksLLM](https://github.com/bricks-cloud/BricksLLM) | 1.2k | Go | 按 Key 的成本/限额管理 | 企业 Key 管控细分场景 |

### 2.2 切换器 / Agent 代理类（客户端，改配置或本地代理）

| 项目 | Stars | 技术栈 | 特点 |
|---|---|---|---|
| [farion1231/cc-switch](https://github.com/farion1231/cc-switch) | 131.0k | Rust (Tauri 2) | 桌面 All-in-One：切换 Claude Code / Codex / Gemini CLI / OpenCode 等的供应商配置，MCP & Skills 管理。**本质是配置管理器，不改请求路径** |
| inflaborg/ccrelay | 17 | TypeScript | VS Code 扩展 + 桌面应用形态的本地代理 |
| swobuforge/swobu | 8 | Go | Claude Code/Codex 的供应商路由与兼容代理 |
| Ike-li/ccs | 4 | Shell | 极简供应商切换脚本 |
| muhammadhaseebiqbal-dev/ClaudeRouter | 5 | Python | Claude Code 自动故障转移代理 |
| kotoyuuko/cc-switch-cli | 1 | Go | 命令行版 cc-switch |

> 备注：早期同类项目 Yuyz0112/uni-api 目前在 GitHub 已检索不到（可能已归档或改名）。

### 2.3 调研结论与机会点

1. **两条产品线泾渭分明**：网关类（one-api/new-api/gpt-load）管"请求怎么走"，切换器类（cc-switch）管"配置怎么写"。**没有轻量级产品把两者合在一起**：网关不管 Agent CLI 的接入体验，切换器不管请求的可观测性与容灾。
2. **轻量档位有空缺**：one-api 老化、new-api/coai 走 SaaS 计费路线、gpt-load/bifrost 面向多凭证企业调度。面向"个人开发者 + 2~3 台设备 + 小 VPS/NAS"的**极轻、三协议、Agent 一键接入**的网关尚未形成统治级产品。
3. **技术栈验证充分**：Go 单二进制 + SQLite + go:embed 内嵌 UI 的可行性已被 one-api、gpt-load 反复验证（gpt-load 单二进制 + SQLite 即可跑生产）。
4. **差异化定位**：
   - 比 LiteLLM：轻 10 倍以上，零 Python 依赖；
   - 比 one-api/new-api：去掉计费分发，聚焦自用 + Agent 接入体验；
   - 比 gpt-load：更克制（无企业多凭证调度复杂度），补上 CLI 配置生成（cc-switch 核心体验）；
   - 比 cc-switch：从"改配置"升级为"走网关"，换来日志、统计、failover、多 Key 轮询。

---

## 3. 项目定位

**LiteGate = 面向个人/小团队的自托管轻量 AI 网关**
- 一个入口（`http://nas:8080`）接管所有大模型请求；
- 三种原生协议进出，任何 OpenAI/Anthropic/Gemini 生态的客户端零改造接入；
- Claude Code / Codex / Gemini CLI 在 Web UI 上点一下即完成接入配置；
- 单二进制 + SQLite，能跑在任何能跑 Go 程序的地方。

---

## 4. 总体设计

### 4.1 架构

```
┌────────────────────────────────────────────────────────────────┐
│                        客户端（下游）                            │
│  Claude Code │ Codex │ Gemini CLI │ OpenAI SDK │ 任意兼容应用     │
└──────┬─────────────────┬──────────────────┬────────────────────┘
       │ Anthropic       │ OpenAI           │ Gemini
       │ /v1/messages    │ /v1/chat/...     │ /v1beta/...
┌──────▼─────────────────▼──────────────────▼────────────────────┐
│                        LiteGate (单二进制)                       │
│                                                                │
│  ┌──────────┐  ┌──────────┐  ┌───────────┐  ┌───────────────┐  │
│  │ 协议解析  │→ │ 鉴权/限流 │→ │ 路由/故障转移 │→ │ 上游协议适配器  │  │
│  │ (入站)    │  │ (虚拟Key) │  │ (渠道选择)   │  │ (出站)        │  │
│  └──────────┘  └──────────┘  └───────────┘  └───────────────┘  │
│        ↑              ↑              ↑               ↑         │
│  ┌─────┴──────────────┴──────────────┴───────────────┴───────┐ │
│  │         管理面：Admin API  +  内嵌 Web UI (go:embed)        │ │
│  │   渠道管理│密钥管理│日志│用量/成本看板│CLI配置生成│系统设置      │ │
│  └───────────────────────────┬───────────────────────────────┘ │
│                              │                                  │
│                      SQLite (WAL) ── 渠道/密钥/日志/价格/设置     │
└──────┬─────────────────────────────────────────────────────────┘
       │
┌──────▼─────────────────────────────────────────────────────────┐
│              上游（渠道）：OpenAI / Anthropic / Gemini /          │
│              DeepSeek / Kimi / OpenRouter / 各类中转站 …          │
└────────────────────────────────────────────────────────────────┘
```

核心原则：
- **数据面与管理面同进程、同端口**（可选分离端口），SSE 流式全程透传、不缓冲整包；
- 渠道（Channel）是核心抽象：`一个渠道 = 一种上游协议 + 一组凭证 + 路由策略 + 模型映射表`。

### 4.2 技术选型（推荐：Go + Vue3）

| 层 | 选型 | 理由 |
|---|---|---|
| 语言/运行时 | **Go 1.22+** | 单静态二进制、交叉编译、协程天然适合代理转发；one-api/gpt-load 已验证该路线 |
| HTTP 框架 | chi（或标准库 + echo） | 轻、中间件模型清晰，避免 gin 的反射开销 |
| 数据库 | **modernc.org/sqlite**（纯 Go、无 CGO）+ WAL | 零 CGO 才能 effortless 交叉编译；接口抽象预留 MySQL/PG 扩展 |
| 前端 | **Vue 3 + Vite + TypeScript + Naive UI** | 中文生态好、产物小（gzip 后 ~200KB 级）；构建产物 `go:embed` 进二进制 |
| 凭证加密 | AES-256-GCM，主密钥来自环境变量/首次启动生成 | 对标 gpt-load 的本地凭证加密 |
| 部署 | 单一静态二进制：裸跑 / systemd / Windows 服务；GitHub Releases 提供跨平台交叉编译产物 | 不使用 Docker |

> 备选方案：Rust（axum + leptos）更极致省内存，但开发效率与生态成熟度不如 Go，且 SQLite 纯 Go 方案已足够把内存控制在 50MB 内。不推荐 Node/Python——与"低占用"目标冲突。

### 4.3 数据模型（核心表）

```
channels        渠道：name, type(openai|anthropic|gemini), base_url, credentials(加密JSON),
                models(JSON: 上游模型→别名映射), weight, priority, status,
                cooldown_until, max_conns, remark
api_keys        下游虚拟密钥：key(sk-...), name, allowed_models, quota, expires_at,
                rpm_limit, enabled
request_logs    请求日志：ts, api_key_id, channel_id, model, protocol, prompt/completion tokens,
                cost, latency_ms, status, stream(bool), error(截断)
model_prices    价格表：model, input_per_1m, output_per_1m, cache_read/write(可选)
settings        系统设置：KV(管理员密码哈希、全局并发、重试策略、代理出口…)
cli_profiles    CLI 接入配置模板：为 Claude Code/Codex/Gemini CLI 生成的配置片段
```

### 4.4 关键 API 设计

数据面（兼容已有生态，客户端零改造）：

```
POST /v1/chat/completions          # OpenAI 协议（含 stream）
POST /v1/messages                  # Anthropic 协议（含 stream，供 Claude Code）
POST /v1beta/models/{m}:generateContent | :streamGenerateContent   # Gemini 协议
POST /v1/embeddings
GET  /v1/models
```

管理面（`/api/admin/*`，Bearer 会话）：

```
POST /api/admin/login
CRUD /api/admin/channels           # + POST /:id/test 连通性测试
CRUD /api/admin/keys
GET  /api/admin/logs?model=&channel=&status=&q=
GET  /api/admin/dashboard          # 聚合：今日请求/token/成本/成功率/渠道健康
GET  /api/admin/prices  PUT /api/admin/prices
GET  /api/admin/cli-config?tool=claude-code   # 返回该 CLI 指向网关的配置片段/一键命令
GET/PUT /api/admin/settings
GET  /healthz
```

### 4.5 路由与容灾策略（v1 范围）

1. 按请求模型名 → 匹配声明该模型的可用渠道集合；
2. `priority` 优先、同优先级按 `weight` 加权轮询；失败按 429/5xx/超时触发**自动重试下一个渠道**（流式请求在首字节前才可重试）；
3. 连续失败进入**冷却**（指数退避），恢复由半开探测决定；
4. 可选**会话亲和**：按请求头哈希使同一会话固定渠道（减少上游缓存失效损失）。

### 4.6 Web UI 页面规划

| 页面 | 内容 |
|---|---|
| 仪表盘 | 今日请求/Token/成本、成功率、渠道健康灯、7 日趋势图 |
| 渠道管理 | 渠道列表（健康/权重/冷却状态）、新建向导（选类型→填 Key→拉取模型列表→勾选暴露的模型）、一键测活 |
| 密钥管理 | 虚拟 Key 签发、额度/过期/模型白名单 |
| 请求日志 | 时间/模型/渠道/耗时/Token/成本/状态，点开看错误详情，可重放 |
| CLI 接入 | 选 Claude Code / Codex / Gemini CLI → 展示一键接入命令与配置片段（cc-switch 体验的 Web 化） |
| 价格与设置 | 模型价格表维护、全局参数、出口代理设置、管理员密码 |

---

## 5. 功能清单（分期）

### M1 —— 核心数据面（✅ 已完成 2026-09-04，二进制 ~11MB、空载内存 ~12MB）
- [x] OpenAI 协议入站 + OpenAI / Anthropic 两种上游适配器（非流式 + SSE 流式逐段透传）；Gemini 上游适配顺延至 M4
- [x] 渠道 CRUD + 多 Key + 加权随机路由（优先级分组）+ 手动启停
- [x] 自动故障转移：网络错误 / 408 / 429 / 5xx 时换下一个渠道，确定性错误（401/400/404）直接透传
- [x] 下游虚拟 Key 鉴权（`sk-lg-` 格式，Bearer 与 x-api-key 双通道）
- [x] SQLite 存储、凭证 AES-GCM 加密、管理员登录（会话 Token）
- [x] 内嵌 Web UI 骨架（状态页）、`GET /v1/models` 聚合（60s 缓存）、`/healthz`
- [x] 请求日志（模型/协议/状态/耗时/错误）+ 仪表盘聚合统计

### M2 —— 可观测（✅ 已完成 2026-09-04）
- [x] 请求级 Token 计量：usage 从上游响应被动提取，不改透传字节；Anthropic 流式从
  `message_start`/`message_delta` 合并，OpenAI 流式自动注入 `stream_options.include_usage`
  （上游 400 不识别时自动去掉重试一次）；日志新增 tokens/cost/ttfb_ms 列（旧库自动迁移）
- [x] 模型价格表（美元/百万 token）+ 成本核算：精确匹配 → 最长段边界前缀回退
  （`gpt-4o` 覆盖 `gpt-4o-2024-08-06`）；价格缓存 60s
- [x] 仪表盘完善：今日请求/错误/token/费用、近 7 天逐日趋势、按模型/按渠道 Top10
- [x] 日志过滤分页：channel_id/api_key_id/model/status/since/until + offset，返回 total
- [x] 虚拟密钥模型白名单（`allowed_models`，按 key 限定可用模型，`/v1/models` 取交集）

### M2.5 —— Web 管理台与安全加固（✅ 已完成 2026-09-07）
- [x] 内嵌管理台六页：仪表盘、渠道管理、虚拟密钥、请求日志、模型价格、登录（go:embed，零前端构建）
- [x] 渠道模型启停：`models` + `disabled_models` 双列表，通配渠道可只禁部分模型；界面勾选管理
- [x] 虚拟密钥管控：列表打码、`/reveal` 按需查看明文、`allowed_models` 白名单、key 级启停
- [x] 安全加固：登录失败限速（5 次锁 60s）、下游错误信息脱敏（渠道名/上游地址只进服务端日志）、
  弱管理密码拒绝启动（空/"admin"）、数据面 8080 收敛 127.0.0.1（局域网统一走 nginx 29xxx HTTPS）
- [x] P0/P1 缺陷修复：停用渠道不再接流量、通配渠道 EnableModel 不被打穿、流式 usage 大整数精度、
  查询串与 `Anthropic-Beta` 等头透传、价格缓存即时失效；新增 6 个回归测试
- [x] 部署配置：systemd unit、nginx 29xxx HTTPS 反代（SSE 不缓冲）、密钥文件 gitignore

### M3 —— 协议补齐（✅ 已完成 2026-09-08）
> 2026-09 调研结论：OpenAI 已将 Chat Completions 归入 Legacy、新能力只给 Responses API，Codex CLI
> 仅支持 `wire_api = responses`；prompt caching 已是各家标配，网关不透传 cache token 计费会让
> 客户端成本可见性失真。gpt-load / new-api / LiteLLM / OpenRouter 均已上线 /v1/responses。
- [x] `POST /v1/responses`：请求转 chat 走既有渠道路由（加权/优先级/故障转移/密钥白名单全复用），
      响应与 SSE 转回 Responses 格式（reasoning/message/function_call 三类 output item、
      `response.created→…→response.completed` 事件序列、usage 记账）；无状态
      （store 忽略，previous_response_id 报 400，GET/DELETE 对象报 501）；流式自动注入
      include_usage，上游 400 时同渠道去字段重试；chat↔responses 桥接即本实现本体，
      纯透传模式无必要（部署内渠道全部为 chat 兼容中转）
- [x] `POST /v1/messages/count_tokens`：anthropic 渠道可服务该模型时转发上游精确值，
      否则本地启发式估算（ASCII≈4 字符/token、CJK≈1 token/字符、每消息 +4），不记请求日志
- [x] Prompt caching 计费：提取 OpenAI `prompt_tokens_details.cached_tokens` 与 Anthropic
      `cache_read/cache_creation_input_tokens`（归一化为总输入口径），缓存读按输入价 1/10、
      缓存写按 1.25 倍折算成本；日志新增 `cache_tokens` 列（旧库自动迁移）
- [x] 回归测试 +8：请求转换、端到端非流式（含缓存折算断言）、流式桥接事件序列、
      previous_response_id 拒绝、count_tokens 本地/转发、CostOf 缓存价

### M4 —— 渠道与密钥治理（✅ 已完成 2026-09-08）
- [x] 渠道多 key 池：`channel_keys` 表（旧单 key 列自动迁移），渠道内按健康密钥随机轮换；
      多 key 渠道把上游 401/403 视为密钥问题自动换 key/渠道，单 key 保持透传原语义
- [x] 密钥健康管理：连续失败 ≥2 次进入指数冷却（1min 起步、封顶 30min），冷却结束半开恢复；
      后台巡检协程每 2 分钟对失败密钥发 GET /models 探测，成功即复位；手动启停持久化，
      渠道更新不重置
- [x] 模型映射/别名：渠道级 `model_map`（对外名 → 上游真实名），请求侧改写、日志统计保留对外名
- [x] 虚拟密钥增强：RPM/TPM 滑动窗口限速、日/月预算（美元或 token，超限 429 + Retry-After）、
      过期时间（当日结束失效返回 401）；预算启动时从日志表回填、请求完成增量累计
- [x] 管理 API/UI：渠道密钥打码列表 + 逐 key 连通性测试 + 单 key 启停端点；
      密钥编辑弹窗限额/预算/有效期字段
- 会话亲和（sticky）顺延至 M6+（家庭场景多 key 池主要摊限额，亲和收益小）

### M5 —— 可观测与运维（✅ 已完成 2026-09-08）
- [x] 仪表盘增强：延迟 P50/P95（今日成功请求）、实时 RPM/TPM（最近 60 秒）、平均生成速度
      （输出 token/s，由 ttfb 推算生成时长）
- [x] 应用归因：`X-LiteGate-App` 请求头（截断 64 字节）入日志 `app` 列，仪表盘"今日按应用分摊"，
      日志接口支持 `app=` 过滤
- [x] SQLite 运维包：`POST /api/admin/db/backup` 在线备份（VACUUM INTO，存数据库同级 backups/）+
      备份列表；日志保留策略 `LITEGATE_LOG_RETENTION_DAYS`（默认 0 永久，>0 时启动+每 6 小时清理）；
      深度 healthz（`?deep=1` 返回版本/运行时长/数据库状态，失败 503）；版本注入
      （ldflags `-X litegate/internal/api.Version`，`-version` 可打印）
- [x] 配置导入导出：`GET /api/admin/config/export`（渠道+价格 JSON，默认密钥打码，
      `include_keys=1` 才含明文——产出等同凭证）；`POST /api/admin/config/import`（渠道按 name
      upsert，未给密钥沿用原值）；渠道模型自动发现 `GET /api/admin/channels/{id}/discover`
      + 管理页"从上游拉取"按钮
- Prometheus `/metrics` 仍为可选项未实现（本机暂无 Prometheus 抓取端，需要时再加）

### M6+ —— 观察后按需
- [ ] `/mcp` 最小透传聚合（streamable HTTP 转发 + 按 key 工具白名单）
- [ ] rerank 透传、精确匹配响应缓存（请求 hash + TTL + 命中统计）
- [ ] Claude Code / Codex / Gemini CLI 一键接入配置生成（自原 M3 顺延）
- [ ] 多模态消息透传验证、跨平台二进制发布流水线与压测报告
- [ ] 订阅账号渠道**不自研**：需要时旁路部署 CLIProxyAPI，在 LiteGate 注册为 OpenAI 兼容渠道

### 明确不做（2026-09 调研后重申）
语义缓存、Wasm/插件运行时、K8s/分布式状态、用户系统/充值/兑换码、全链路 trace（需要时外接
Langfuse）、AI 智能路由、Vue3 重写、i18n、Gemini 原生出站适配。

---

## 6. 风险与对策

| 风险 | 对策 |
|---|---|
| 协议细节繁杂（tool use / thinking / 多模态在三种协议间不对等） | v1 先做"同协议透传 + 模型名映射"，跨协议转换仅覆盖核心字段；每种协议配契约测试集（用真实客户端录制回放） |
| 订阅账号（OAuth）渠道合规与实现复杂度高 | 不自研；需要时旁路部署 CLIProxyAPI 并注册为 OpenAI 兼容渠道，先以 API Key 类渠道覆盖 90% 场景 |
| SQLite 高并发写日志成为瓶颈 | WAL + 批量异步落库（内存 ring buffer 定期 flush），日志表按月分表/滚动清理 |
| 与 gpt-load 等成熟项目同质化 | 坚守差异：更轻（无企业调度）、CLI 一键接入、三协议原生；不做大而全 |

---

## 7. 参考资料

- 各项目 README 与 GitHub API 元数据（2026-09-04 抓取）
- 2026-09-07 同类项目二次调研：gpt-load、new-api、one-api/one-hub/done-hub、CLIProxyAPI、axonhub、
  LiteLLM、OpenRouter、Portkey/Kong/Envoy AI Gateway/Higress/Cloudflare AI Gateway（结论见 M3-M6）
- 协议规范：OpenAI Chat Completions API、Anthropic Messages API、Google Gemini API
- 相关实现可参考：`one-api`（渠道调度模型）、`gpt-load`（凭证加密与订阅账号调度）、`cc-switch`（CLI 配置模板）
