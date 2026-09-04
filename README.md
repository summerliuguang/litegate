# LiteGate

轻量级 AI 网关：把多个大模型供应商/中转站收敛到一个入口，带 Web 管理页面。
对标 LiteLLM（网关能力）与 cc-switch（供应商切换体验），但只做一件事：**单二进制、低占用、开箱即用**。

- **单二进制 ~11MB**，空载内存 ~20MB，无 Python / Node 运行时，**不依赖 Docker**
- 默认 SQLite（WAL），零外部服务；凭证 AES-256-GCM 加密存储
- 下游协议：OpenAI（`/v1/chat/completions`、`/v1/embeddings`）、Anthropic（`/v1/messages`，Claude Code 可直连）
- 上游渠道：OpenAI 兼容 / Anthropic 兼容，多渠道加权轮询、按优先级故障转移
- 内嵌管理页面与管理 API：渠道 CRUD、连通性测试、虚拟密钥、请求日志、用量看板

> 设计文档见 [docs/DESIGN.md](docs/DESIGN.md)。项目处于 M1 阶段：数据面已可用，Web 管理界面与 Gemini 适配在后续里程碑。

## 构建

需要 Go 1.22+：

```bash
go build -trimpath -ldflags="-s -w" -o litegate ./cmd/litegate
```

交叉编译示例（默认无 CGO 依赖）：

```bash
GOOS=linux GOARCH=arm64 go build -o litegate-linux-arm64 ./cmd/litegate
```

## 运行

```bash
LITEGATE_ADMIN_PASSWORD=你的管理密码 ./litegate -addr :8080 -db /var/lib/litegate/litegate.db
```

| 参数 / 环境变量 | 说明 | 默认 |
|---|---|---|
| `-addr` | 监听地址 | `:8080` |
| `-db` | SQLite 文件路径 | `./litegate.db` |
| `LITEGATE_ADMIN_PASSWORD` | 管理密码（必设） | `admin`（仅测试用） |
| `LITEGATE_SECRET` | 32 字节十六进制主密钥，用于加密渠道凭证；不设则首次启动自动生成并存库 | 自动生成 |

生产环境建议用 systemd 托管（`Restart=always`）。

## 快速上手

1. 登录管理 API，创建渠道（首次启动会自动生成一个 `sk-lg-` 开头的虚拟密钥并打印在日志里）：

```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:8080/api/admin/login \
  -d '{"password":"你的管理密码"}' | jq -r .token)

curl -X POST http://127.0.0.1:8080/api/admin/channels \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "openai-main",
    "type": "openai",
    "base_url": "https://api.openai.com/v1",
    "api_key": "sk-xxx",
    "models": ["gpt-4o", "gpt-4o-mini"]
  }'
```

`base_url` 需包含版本前缀（如 `https://api.openai.com/v1`、Anthropic 用 `https://api.anthropic.com/v1`）；
`models` 留空表示该渠道可服务任意模型。

2. 下游客户端把 Base URL 指向网关、API Key 换成虚拟密钥即可：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-lg-xxxx" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

Claude Code 接入（Anthropic 协议直连网关）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=sk-lg-xxxx   # 网关虚拟密钥
```

## 管理 API

```
POST   /api/admin/login                 {password} → {token}
GET    /api/admin/dashboard             今日请求/错误、渠道与密钥统计
GET    /api/admin/channels              渠道列表（api_key 打码）
POST   /api/admin/channels              新建渠道
PUT    /api/admin/channels/{id}         更新（api_key 留空表示沿用）
DELETE /api/admin/channels/{id}
POST   /api/admin/channels/{id}/test    连通性测试（拉取上游模型列表）
GET    /api/admin/keys                  虚拟密钥列表
POST   /api/admin/keys                  签发密钥 {name}
DELETE /api/admin/keys/{id}
GET    /api/admin/logs?limit=100        请求日志
```

数据面：

```
POST /v1/chat/completions     OpenAI 协议（支持 stream）
POST /v1/embeddings
POST /v1/messages             Anthropic 协议（鉴权可用 x-api-key 头）
GET  /v1/models               聚合各渠道模型列表（缓存 60s）
GET  /healthz
```

## 测试

```bash
go test ./...
```

## 路线图

- [x] M1 数据面：OpenAI/Anthropic 双协议、多渠道加权路由、故障转移、SSE 流式、虚拟密钥、日志
- [ ] M2 用量统计：Token 计量、成本核算、仪表盘完善、日志分页
- [ ] M3 Agent 接入：Claude Code / Codex / Gemini CLI 一键配置生成；健康巡检与自动冷却
- [ ] M4 扩展：Gemini 上游适配、订阅账号（OAuth）渠道、完整 Vue3 管理界面、i18n

详见 [docs/DESIGN.md](docs/DESIGN.md)。
