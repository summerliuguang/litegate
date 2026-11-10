# 本轮实施与验证记录

> 交付更新（2026-11-10）：用户已授权将本轮代码、测试和文档提交至 `summerliuguang/litegate` 的独立分支 `codex/litegate-review-20261110`。下文“未提交/未推送”为审查当时状态；远端交付以该分支是否存在及提交为准。仍不创建 PR、不合并、不部署。CI 仅监听 main 推送和 PR，新分支推送不会自动触发；推送前重跑相同本地检查。2026-11-09 导出的补丁/归档为当时源码快照，不包含本交付更新。

日期：2026-11-09（UTC）。基线 `b0d687579afdd585f1c85c48194338510d8c6cbe`，工作分支 `work`。
开始时工作区干净；无适用仓库 AGENTS.md。全部改动保留工作区，未提交、推送、创建 PR、合并或部署。

## 实施内容

1. **计费正确性**：非流式响应逐块采集 usage，保留正文原样透传，支持首尾/跨块/转义字段及 message.usage；新日志落计费币种，旧日志回退现价；趋势使用 `cost_by_currency`，混合币种不相加；日志摘要按模型+币种拆行；预算始终累计日/月窗口。
2. **可靠性**：音频 multipart→JSON 桥接修正 Content-Type；SSE 半行最多保留 64 KiB，超长行跳过后恢复采集；用量对象也有界，保持轻量。
3. **页面/易用性**：默认密钥不再隐藏无渠道引导，改为四步说明；统计、趋势与图表坐标统一 UTC；费用分币种序列；修正输出速度选项原先显示 Token 总量；移动抽屉补 aria、焦点进入/限制/返回、Escape 和桌面尺寸切换关闭。
4. **性能/资源**：模拟上游基准复现统计与数据面单连接排队。保留一个数据面连接，新增一个只读统计连接，利用 WAL 并行读写；不增加外部服务/运行依赖。包含回归测试证明占用数据面连接时统计仍能完成、只读拒绝写入、读事务不阻塞写入且保持快照。
5. **文档/验证**：README 对齐 Go 1.27、软预算和资源证据；新增五阶段计划、协议边界、压测与浏览器脚本、原始结果。运行完整检查及旧库迁移/备份恢复测试。

## 先复现再修复

新增测试在修复前明确失败：
- `TestLargeResponseAccounting`：256 KiB 响应首/尾 usage 均漏记，tokens/cost 为 0。
- `TestBudgetEnableAndSwitch`：启用 daily 预算遗漏已完成消费。
- `TestAudioTranscriptionJSONHeader`：严格模拟上游返回 415。
- `TestSSEOverlongLineRecovers`：缓冲增长至 69632 字节并继续增长。
- `TestTrendSeparatesCurrency`：1 USD + 7 CNY 被合成 cost=8，标 CNY。
- `TestLogCurrencySnapshot`：插入 CNY 后读取 currency 为空。
- `TestDashboardDoesNotQueueOnPrimaryConnection`：持有数据面连接时统计阻塞。

修复后上述测试和新增加的 Top8/其他币种合并、旧库迁移幂等、历史币种不被价格变更重标、只读隔离和备份恢复测试全部通过。

## 检查命令及结果

开发 shell 使用 `/workspace/.setup/env.sh`，Go **1.27.3 linux/amd64**，云机器 CPU **Intel Xeon Platinum 8573C**；Go 基准显示 4 个逻辑执行线程。

```sh
source /workspace/.setup/env.sh
test -z "$(gofmt -l internal cmd)"
git diff --check
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go build -trimpath -ldflags='-s -w -X litegate/internal/api.Version=review' -o /tmp/litegate-review ./cmd/litegate
```

全部通过。最终 race：api 15.145s、store 3.579s；其余包无测试。二进制 **12,558,599 字节（约 11.98 MiB）**。

浏览器脚本使用已有 Playwright 与 Chromium（仅测试依赖，不进入产品运行环境）：

```sh
PLAYWRIGHT_MODULE=/workspace/.setup/browser/node_modules/playwright/index.mjs \
  node scripts/browser-smoke.mjs /tmp/litegate-review /workspace/scratch/litegate-review
```

实际启动临时数据库和本地服务，随机管理密码只在进程内使用。深度健康检查、真实登录和默认密钥引导通过。
320/390/1280px 均无横向溢出或 JS 错误；手机菜单焦点/Shift-Tab/Escape/返回焦点通过。
浏览器设 Asia/Shanghai，图表 fixture 验证 UTC 时间戳、USD/CNY 分线、token 不重复和 tps 数值正确。
截图保存于 `/workspace/scratch/litegate-review/`，`dashboard-*.png` 为真实空库页面，`currency-fixture-*.png` 为明确注入的图表测试数据，非生产用量。

## 性能基准

原始数据：[基线](benchmarks/baseline.jsonl)、[修复版](benchmarks/review.jsonl)、[usage 微基准](benchmarks/usage-scanner.txt)。
基线由 `git archive b0d6875` 导出至临时目录并以同样裁剪参数构建（无 git 元数据，使用 `-buildvcs=false`）；未改动原仓库或创建 worktree。

```sh
python3 scripts/benchmark.py /tmp/litegate-baseline-bin --requests 200 --logs 10000,100000
python3 scripts/benchmark.py /tmp/litegate-review --requests 200 --logs 10000,100000
go test ./internal/api -run '^$' -bench BenchmarkJSONUsageScanner -benchmem -count=3
```

两版顺序运行；每版 16 个场景，每场景 200 次请求，共 3200 次，全部成功，无仪表盘错误。
模拟上游为本机 Python HTTP 服务，固定等待 5ms、返回小 JSON；启用 TCP_NODELAY、监听队列128以减少模拟器额外阻塞。负载发生器为 Python urllib，每个请求建立客户端连接。
数据库预填 1万/10万条当日日志；同一规模下后续场景累计新增日志。仪表盘开启时开始即请求 dashboard+trend，之后每10秒刷新；大多数场景较短，只有一次刷新。RSS 每50ms采样，只覆盖请求窗口，CPU秒也只计请求窗口（不是完整仪表盘计算或模拟器 CPU）。原始 DB 大小包含 WAL/SHM。

| 预填日志 | 并发 | 仪表盘 | RPS 基线→修复 | P95 ms 基线→修复 | 修复版采样峰值 RSS MiB |
|---|---|---|---|---|---|
| 10000 | 1 | 关 | 149.52 → 147.38 | 7.45 → 7.09 | 17.34 |
| 10000 | 1 | 开 | 143.77 → 151.74 | 7.07 → 7.57 | 22.48 |
| 10000 | 10 | 关 | 960.68 → 987.18 | 16.48 → 13.85 | 22.84 |
| 10000 | 10 | 开 | 726.52 → 973.37 | 42.47 → 13.49 | 23.59 |
| 10000 | 50 | 关 | 1069.94 → 1029.91 | 51.42 → 55.31 | 24.07 |
| 10000 | 50 | 开 | 780.75 → 1035.91 | 105.79 → 44.3 | 24.45 |
| 10000 | 100 | 关 | 1095.83 → 1015.49 | 29.34 → 31.49 | 24.56 |
| 10000 | 100 | 开 | 778.76 → 958.67 | 159.16 → 36.89 | 24.75 |
| 100000 | 1 | 关 | 151.09 → 151.07 | 7.1 → 6.95 | 17.72 |
| 100000 | 1 | 开 | 84.27 → 150.99 | 7.41 → 7.33 | 26.83 |
| 100000 | 10 | 关 | 1033.85 → 1017.88 | 12.69 → 12.07 | 26.18 |
| 100000 | 10 | 开 | 157.54 → 943.62 | 422.95 → 14.76 | 26.61 |
| 100000 | 50 | 关 | 1062.81 → 989.33 | 40.13 → 42.46 | 27.7 |
| 100000 | 50 | 开 | 164.15 → 993.47 | 806.89 → 48.44 | 27.14 |
| 100000 | 100 | 关 | 1010.25 → 1004.99 | 32.56 → 30.19 | 27.13 |
| 100000 | 100 | 开 | 161.07 → 978.32 | 712.09 → 31.78 | 27.27 |

修复版请求前 RSS 约13.21–13.59 MiB；本轮请求窗口采样峰值不超过27.70 MiB。不能把这个范围当作所有模型、长流、多用户或长期运行的内存保证。
usage 微基准以1 MiB正文、32 KiB写入块运行，约533–549 MB/s，256–272 B/op、11 allocs/op；它不含HTTP、日志尾部缓冲和SQLite，不能等同网关吞吐。

结论限于本次短测：统计只读连接显著降低仪表盘对转发请求的排队影响。未开仪表盘的部分场景存在正常波动，不能宣称所有场景提速。没有与竞品做同机公平压测。

## 剩余工作和风险

- 阶段1本轮缺陷已修复；历史无币种快照不能补回过去真实性；旧 API 消费方应切换 `cost_by_currency`。预算仍为软限制，不承诺并发硬额度。
- 阶段2保持既有超时默认值。真实供应商、音频质量及转换后的音频用量独立审计待做；超长/缺失 usage 不虚构计费。协议边界见 [COMPATIBILITY.md](COMPATIBILITY.md)。
- 阶段3已验证仪表盘和菜单；全部管理表单错误态、读屏器、各页真实业务流程以及完整引导状态机尚待逐项验收。
- 阶段4已完成可复现短测和一次有证据的优化；还需持续负载、多次重复、慢流/长响应/异常上游、百万日志及真实硬件测量。仍有多个扫描/全量延迟样本查询；读连接解决排队，不消除其CPU/内存成本，后续再据实测考虑聚合缓存/索引。
- 阶段5本地CI对应检查、迁移和备份恢复已通过；未运行远程GitHub CI、生产恢复演练、真实供应商测试或发布。没有操作被拒绝或需要等待的审批。

## 第二轮：继续修复与发布前验证（当前状态）

本节更新前文第一轮的剩余事项，不代表已部署或完成真实供应商认证。

### 新增修复

- **音频用量丢失**：新增 `TestAudioConversionPreservesUsage`，修复前 JSON/text/speech 三种转换均记0 tokens/费用。现在转换前采集原始usage，使用内部响应体元数据传递给计费逻辑，不信任上游自报的计费请求头；三种转换均保留用量和费用。原始响应超过32 MiB时关闭响应体并返回502，避免无限读取。新增上限/关闭回归通过。
- **发送/停止按钮无响应**：真实浏览器流程发现对话页只有Enter监听，没有发送/停止按钮的点击监听。已补齐，浏览器验证点击发送、接收流式内容、点击停止后恢复可发送状态。
- **统计内存**：今日基础汇总从5个日志扫描合并为一次；P50/P95改为按延迟毫秒值聚合的精确直方图。重复值、最近秩分位数、错误请求排除和加权输出速度都有回归断言。移除不再使用的全样本排序函数。

### 回归与管理页面

- `go vet ./...` 和 `go test -race ./... -count=1` 再次通过：api17.357s，store3.788s。删除不再使用的函数后，相关store回归和最终构建再次通过。
- `FuzzJSONUsageScannerEnvelope` 以任意文本、嵌套伪usage、转义及随机分块生成合法响应：**30,572次执行通过**，见 [原始结果](benchmarks/fuzz-envelope.txt)。不是任意畸形JSON的完整语法验证。
- 扩展 `scripts/admin-flows.mjs`，使用真实页面、临时数据库、本地模拟上游：登录错误态→登录；渠道新增/校验/测试/编辑/删除；模型价格新增/删除；密钥新增/预算编辑/趋势/删除；非法与有效汇率；只读令牌拒绝写入；对话流式发送/取消和请求日志计量。全部通过。
- 8个管理页（仪表盘、渠道、密钥、日志、价格、余额、设置、对话）×320/390/1280px，无横向溢出，无JS错误。24张实际页面截图在 `/workspace/scratch/litegate-review/*-flow-*.png`，另有截图说明文件。余额页只验证渲染，没有调用真实账户余额服务。
- 运行命令：`ADMIN_FLOWS=1 PLAYWRIGHT_MODULE=/workspace/.setup/browser/node_modules/playwright/index.mjs node scripts/browser-smoke.mjs /tmp/litegate-review /workspace/scratch/litegate-review`。
- 原始 [浏览器记录](benchmarks/browser-flows.txt)、[race记录](benchmarks/race.txt) 已保存。

### 持续负载与百万日志

每个场景使用独立临时数据库和本地模拟上游，未调用真实供应商。非流式模拟固定等待5ms；流式模拟为5个间隔10ms的数据块，随后usage和DONE。前后数据都是共享云机器上的单次运行，不能当作稳定SLA或严格性能置信区间。

| 版本/场景 | 初始日志 | 并发 | 时长 | 成功请求 | P95 ms | 采样峰值 RSS MiB | 仪表盘+趋势刷新耗时 |
|---|---:|---:|---:|---:|---:|---:|---|
| 直方图优化前，非流式 | 100000 | 100 | 30s | 34702 | 294.44 | 38.10 | 2.26–3.14s |
| 直方图优化前，非流式 | 1000000 | 100 | 30s | 33305 | 170.42 | 55.01 | 25.11s |
| 直方图优化前，流式 | 100000 | 50 | 30s | 18646 | 103.84 | 35.74 | 2.21–2.68s |
| 直方图优化后，非流式 | 100000 | 100 | 30s | 34454 | 301.62 | 36.88 | 1.99–3.08s |
| 直方图优化后，非流式 | 1000000 | 100 | 30s | 32480 | 189.64 | 39.04 | 24.47s |
| 直方图优化后，非流式 | 100000 | 100 | 120s | 134789 | 283.45 | 45.15 | 2.22–4.86s |

这些持续测试共 **288,376 次成功请求**，无请求或仪表盘刷新失败。优化主要体现为百万日志统计内存降低，不能宣称这一改动降低了所有请求延迟；30秒P95在这两次抽样中略有波动甚至上升。

120秒场景约1122.4 RPS，P99 571.03ms，Go进程CPU201.85秒（约1.68核平均占用，不含模拟器/负载发生器）。RSS从请求前12.81 MiB升至结束41.45 MiB、峰值45.15 MiB；期间数据库不断增加日志，未证明内存已稳定或不存在长期泄漏。RSS序列每200个采样点抽取一次，目标采样间隔50ms，实际间隔受调度影响，不能当作精确每10秒时序。

百万日志场景结束时 DB/WAL/SHM 合计约760.97 MiB；120秒10万初始日志场景结束约145.06 MiB。这是结束时文件总量而非磁盘峰值或纯数据文件大小。长统计读事务和持续写入可能使WAL增长，必须继续验证检查点和长读事务策略。

原始结果：[30秒原版](benchmarks/sustained.jsonl)、[流式](benchmarks/sustained-stream.jsonl)、[30秒统计优化后](benchmarks/sustained-optimized.jsonl)、[120秒](benchmarks/sustained-120s.jsonl)。

```sh
python3 scripts/benchmark.py /tmp/litegate-review --duration 30 --logs 100000,1000000 --concurrency 100 --dashboard on
python3 scripts/benchmark.py /tmp/litegate-review --duration 30 --logs 100000 --concurrency 50 --dashboard on --stream
python3 scripts/benchmark.py /tmp/litegate-review --duration 120 --logs 100000 --concurrency 100 --dashboard on
```

### 发布前状态和剩余缺口

- Linux AMD64默认构建及本地运行通过；另以`CGO_ENABLED=0`生成AMD64静态二进制，启动、深度健康检查和真实管理登录通过。Linux ARM64交叉构建通过（确认是aarch64 ELF），**没有ARM设备运行验证**。
- 仍未推送、创建PR、合并、部署或触发远程CI。所有检查和临时模拟在本地云工作区完成。
- 真实供应商、真实余额/音频质量和音频专有价格规则需要授权凭据/可能费用，因此未启用。没有进行外部告警通知发送。
- 百万日志统计仍需约24秒，仍有多次全窗口扫描；下一步应单独设计可验证的预聚合/缓存失效与WAL控制方案。这是已实测的性能缺口，不是已经解决。
- 尚缺数小时持续运行、多面板同时刷新、固定数据量的内存稳定性和真实硬件容量验证。120秒测试不能替代这些检查。
- 页面已覆盖主要流程，不代表穷尽每个配置组合、读屏器行为、真实上传附件/音频或所有错误分支。

## 第三轮：百万日志聚合与 WAL（最新结果）

本节取代前两轮尚未解决的百万日志性能结论；历史测量保留供审查。没有增加外部服务或内存 TTL 缓存。

### 原因、实现及正确性

基线的仪表盘/趋势重复扫描原始日志，长读事务延迟检查点；SQLite 默认还会保留 WAL 高水位供复用。现改为事务内触发器维护小时用量汇总、每日精确延迟直方图，查询读取一致快照、固定 UTC 时间窗口，支持请求取消和10秒总超时。写连接自动检查点1000页，WAL重置保留目标16 MiB；活动事务期间可超过，未在请求路径强制 TRUNCATE。

新增回归以原始日志 GROUP BY 独立比对所有维度/指标，覆盖插入、更新、删除、回滚、清理、迁移重启、币种快照/旧价格回退、元数据变化、UTC午夜、并发读写的同响应一致性、查询取消及取消后连接复用。WAL回归用较低测试阈值复现长读阻止回收、读者释放后自动回收。实现和运维约束见 [ANALYTICS.md](ANALYTICS.md)。

### 同条件单次对照

百万日志冷查询基准（低维度fixture，`-benchtime=1x`）：

| 指标 | 本轮优化前 | 最终实现 |
|---|---:|---:|
| 仪表盘 | 11.273 s | 0.274 s |
| 趋势 | 2.325 s | 0.000364 s |
| 百万日志装载 | 2.758 s | 6.501 s |
| 一次性历史回填 | 不适用 | 4.183 s |

高维度fixture（20模型、50密钥、5应用、24小时，实际12万聚合行）最终仪表盘1.047 s、趋势0.265 s、装载14.241 s。维度组合越多，收益越低；写放大是明确代价，不将低维度结果推广到所有数据库。

百万初始日志、本地5ms模拟上游、100并发、30秒、仪表盘开启：

| 指标 | 本轮优化前 | 最终实现 |
|---|---:|---:|
| 成功请求 | 33186 | 32443 |
| 请求/秒 | 1103.31 | 1078.94 |
| P95 / P99 | 217.13 / 464.20 ms | 161.87 / 237.66 ms |
| 仪表盘+趋势刷新 | 24.621 s | 0.476–0.499 s |
| Go进程CPU | 56.69 s | 39.63 s |
| 采样峰值RSS | 39.35 MiB | 33.02 MiB |
| 结束DB/WAL/SHM总量 | 772.74 MiB | 123.81 MiB |
| WAL开始 / 峰值 / 结束 | 104.70 / 663.76 / 663.76 MiB | 104.74 / 104.74 / 16.00 MiB |

两次均无请求/统计错误。吞吐略降，不能宣称所有指标提升。WAL开始包含一次性数据装载产生的文件；不能将16 MiB结束值解释为全程峰值。结束后的PASSIVE检查点分别为`[0,7,7]`与`[0,813,813]`，未强制截断。这些都是共享云机器单次运行，无统计置信区间或生产SLA。

原始记录：[冷查询基线](benchmarks/analytics-million-before.txt)、[最终冷查询及高维度](benchmarks/analytics-final.txt)、[负载基线](benchmarks/rollups-load-before.jsonl)、[最终百万日志负载](benchmarks/rollups-final-million.jsonl)。名称含after/wal的其他记录属于中间实现，不能替代final结果。

```sh
go test ./internal/store -run '^$' -bench 'BenchmarkAnalytics(Million|DiverseMillion)' -benchtime=1x -count=1 -v
python3 scripts/benchmark.py /tmp/litegate-review --duration 30 --logs 1000000 --concurrency 100 --dashboard on
python3 scripts/benchmark.py /tmp/litegate-review --duration 120 --logs 100000 --concurrency 100 --dashboard on
```

### 最终持续负载与发布前检查

- 最终120秒测试：初始10万日志、100并发，**130,766次成功请求**、1088.87 RPS、P95 176.59ms、P99 329.61ms；12次统计刷新42.1–105.0ms，无请求/统计错误。CPU186.70秒，RSS峰值42.32 MiB、结束37.14 MiB，DB/WAL/SHM总量36.31 MiB，WAL开始/峰值/结束均10.73 MiB。RSS随数据增加仍上升，不能据此证明不存在长期泄漏。[原始记录](benchmarks/rollups-final-120s.jsonl)。
- 最终 `go vet ./...`、`go test -race ./... -count=1` 通过（api21.566s，store8.854s），[race记录](benchmarks/race-rollups.txt)。格式及差异空白检查、浏览器脚本语法和Python解析通过。
- 最终浏览器完整管理流程再次通过，8页×3尺寸、0 JavaScript错误，含登录错误、渠道CRUD/测试、价格增删、密钥/预算操作、只读写入拒绝、发送和取消。使用本地模拟上游，无真实费用。[浏览器结果](benchmarks/browser-rollups.txt)。
- 仍未推送、合并、部署或创建PR。真实供应商/余额/音频质量、数小时压力、多面板并发、读屏器、远程CI及真实硬件容量尚未验证。没有操作被拒绝或等待审批。
- 最新代码的 Linux AMD64/ARM64 `CGO_ENABLED=0` 静态交叉构建均通过；AMD64二进制深度健康检查和管理登录通过，ARM64仅确认ELF架构、未上机执行。[构建记录](benchmarks/build-rollups.txt)。

## 第四轮：最终代码审查

重新审查所有未提交变更后，修复非法用量负值/单笔溢出、预算历史回填非原子及忽略币种快照、特殊名称导致图表丢系列三个问题，均先复现再修复。详情和五阶段剩余范围见 [FINAL_REVIEW.md](FINAL_REVIEW.md)。

最终 `go vet ./...`、`go test -race ./... -count=1` 全部通过（api21.647s、store9.282s），包括100个并发完成请求的预算累计与迁移失败回滚/重试。最终浏览器8页×3尺寸完整管理流程通过，新增 `__proto__`/`constructor`/HTML事件属性名称的系列完整性和无DOM注入断言通过，0 JS错误。Linux AMD64/ARM64静态构建再次通过；此次正常AMD64构建已在浏览器回归中实际运行，ARM64仍仅交叉构建。

原始结果：[最终race](benchmarks/race-final-review.txt)、[最终浏览器](benchmarks/browser-final-review.txt)、[计费修复前失败](benchmarks/final-review-before-usage.txt)、[迁移修复前失败](benchmarks/final-review-before-migration.txt)、[图表修复前失败](benchmarks/final-review-before-ui.txt)。本轮未改统计查询/触发器/WAL配置，未重复百万日志压测；上一轮性能数据对应上一轮实现，不宣称是这次最终二进制的重新测量。

## 独立分支交付前复核（2026-11-10）

在用户明确授权 `summerliuguang/litegate` 新分支交付后，重新通过 gofmt、`go vet ./...`、`go test -race ./... -count=1` 和带版本标记的构建；[本次race结果](benchmarks/race-pre-push.txt)。待交付文件检查未发现常见凭据模式、数据库、压缩包或无关文件；测试中的密码/令牌为运行时临时生成或本地模拟值。未修改CI触发范围，新分支不创建PR，因此没有适用的自动远程CI运行。
