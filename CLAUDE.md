# CLAUDE.md — anselm-gateway 工程纪律（最高法）

> 每次会话自动加载。工程纪律的**唯一事实源**，与文档规范 [`docs/GOVERNANCE.md`](docs/GOVERNANCE.md) 同级、互为引用。冲突时本文 > references > concepts > working > archive。
> 按 GOVERNANCE §1.7 **整体重述**维护（改一处状态就重写相关节、不留旧痕、不旁追加）。

## 0. 项目一句话

`github.com/sunweilin/anselm/gateway`：纯 Go + SQLite(WAL) 单二进制**薄网关**，给 Anselm(Flutter)客户端一个**免费 DeepSeek 档**(key 只在服务端)。Clean Arch 重写已落 `main` 并取代旧部署、线上运行(海外 VPS，Caddy 终结 TLS)。心智模型见 [`docs/concepts/architecture.md`](docs/concepts/architecture.md)，硬契约见 [`docs/references/backend/`](docs/references/backend/)。

## 1. 三条工程铁律(违反 = 严重 Bug，与编译失败同级)

1. **正确性 > 架构纯度 > 速度**。这是花 operator 真钱的网关:记账永不超卖、崩溃只多扣不少扣、key 永不出端、admin 面绝不公网。不变式登记册 [`docs/references/backend/invariants.md`](docs/references/backend/invariants.md)(GW-INV-NN)是一切改动的**验收准绳**——动代码前先看它要守哪几条。
2. **文档与代码物理同步**(doc-code parity):碰了 GOVERNANCE §7 触发表里的东西(API/config/DB/error-code/不变量/行为/ADR/前端契约)却没在**同一提交**同步对应文档 → 改动**未完成**。
3. **每片绿了再下一片**:`go build ./... && go vet ./... && gofmt -l(空) && go test -race ./...` 全绿 + 相关 GW-INV 验收过，才算完成。

## 2. 依赖方向铁律(import-lint 强制，见 architecture §3 + .golangci.yml depguard)

```
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg
```
- `domain` 只依赖 stdlib + pkg;`app` 在本包声明 infra 端口(interface)，**禁** import infra/`database/sql`/HTTP server;`infra` 结构化满足端口、唯一碰 OS/DB/网络;`bootstrap` 唯一可跨全层、**无人 import 它**;`pkg` 叶内核谁都不依赖。
- **事务聚合**(quota/install):事务边界 owned 在 `infra/store/*`(`orm.DB.Transaction`/`BEGIN IMMEDIATE`)，app 端口只暴露**一个原子聚合操作**，`*sql.Tx` 永不漏出 infra。
- **密钥边界**:secret(`DEEPSEEK_API_KEY`/`DASHBOARD_*`/`INSTALL_POW_SECRET`)env-only，不入 `config.Specs()`、绝不入 settings 表/Dump/Snapshot/日志。

## 3. 不可破的红线(摘要，全集见 invariants.md)

- **记账**:三闸(月度 count/install 日 token/全局日预算)在单 `BEGIN IMMEDIATE` 预占;产出前失败回滚全三项;计费一次边界=上游首字节;`settled IS NULL` 幂等;period 入口快照一次贯穿;崩溃只多扣。
- **安全**:DeepSeek key 注入在 clone 上、归一化上游错误不透传;token 只存 SHA-256;admin/metrics/pprof 仅 loopback(绑定 fail-fast);日志/指标无 key/prompt/token/ip、label 低基数;XFF 仅信回环直连对端。
- **可靠**:在飞 ≤ N_global(多 key/排队不放大);**故障分类排除 client-cancel 与 429**(ADR-011，不触进程 breaker);关停 DB 最后关(bgWG 排空)。
- **输入**:严格白名单(model 强改写/messages/stream/temperature/max_tokens clamp **+ tools/tool_choice 透传**[免费档 agentic，messages 保 tool_calls/tool_call_id/name/reasoning_content]);拒 n>1;SEC-1 形状;256KB body;其余危险字段(logit_bias/function_call/response_format)剥离。

## 4. 工作流(切片纪律)

地基优先的 Clean Arch 重写**已完成**并落 `main`;后续每处改动仍按同一纪律走:`domain → app → infra → transport → 测试 → ref 文档`，GW-INV 当验收。
- **唯一事实源是代码 + `docs/references/backend/*`**(逐字契约);`docs/working/*` 是重写期的抽取契约、现已 landed/superseded，作历史参考非现行准绳;旧 `_legacy/` 树已删除。
- 14 个历史审查 bug 按构造免疫(B5/B3/B12/B16→ADR-011、B1→Period 入口快照 + Reservation.SublimitApplied、B0→前端漂移门、迁移债→ADR-005)。

## 5. 门禁命令

```sh
make verify   # vet + build + test -race + docs(本地门禁)
make docs     # cmd/docs:frontmatter/类型·目录/INDEX≤50/孤儿链接/working 90 天
make lint     # golangci-lint v2.6.1(errcheck/staticcheck/gosec/govet/depguard/...)
```
提交前:`gofmt -l`(空)+ build/vet/test-race 绿 + GOVERNANCE §12 收尾清单逐条勾。CI(`.github/workflows/ci.yml`)另跑 govulncheck / fuzz smoke / 前端漂移门 / 覆盖率地板(quota ≥70%、chat ≥65%)。

## 6. 速查

- 仓库:`<repo>` · 分支 `main`(线上 lineage)
- 入口:`cmd/gateway`(瘦壳)→ `internal/bootstrap`(组合根) · 三监听器:业务 8080(公网/socket-activated)· admin 9090(loopback)· dashboard 8081(loopback)
- Go:`mise which go`(1.25) · 文档体系:[`docs/INDEX.md`](docs/INDEX.md)(会话入口)
