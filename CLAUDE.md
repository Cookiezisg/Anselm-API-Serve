# CLAUDE.md — anselm-gateway 工程纪律（最高法）

> 每次会话自动加载。工程纪律的**唯一事实源**，与文档规范 [`docs/GOVERNANCE.md`](docs/GOVERNANCE.md) 同级、互为引用。冲突时本文 > references > concepts > working > archive。
> 按 GOVERNANCE §1.7 **整体重述**维护（改一处状态就重写相关节、不留旧痕、不旁追加）。

## 0. 项目一句话

`github.com/sunweilin/anselm/gateway`：纯 Go + SQLite(WAL) 单二进制**确定性 capability 薄网关**，给 Anselm(Flutter)客户端一个 provider-neutral 逻辑模型：**文本与多模态同走 Qwen3.7 Plus**，由内容形状决定的是**能力**（媒体额外需要上传/lease 通道）而非 provider；无语义分级、无 fallback。key 只在服务端。项目尚未上线。心智模型见 [`docs/concepts/architecture.md`](docs/concepts/architecture.md)，硬契约见 [`docs/references/backend/`](docs/references/backend/)。

## 1. 三条工程铁律(违反 = 严重 Bug，与编译失败同级)

1. **正确性 > 架构纯度 > 速度**。这是花 operator 真钱的网关:记账永不超卖、崩溃只多扣不少扣、key 永不出端、admin 面绝不公网。不变式登记册 [`docs/references/backend/invariants.md`](docs/references/backend/invariants.md)(GW-INV-NN)是一切改动的**验收准绳**——动代码前先看它要守哪几条。
2. **文档与代码物理同步**(doc-code parity):碰了 GOVERNANCE §7 触发表里的东西(API/config/DB/error-code/不变量/行为/provider·rate-card·pUSD 账务/ADR/前端 wire)却没在**同一提交**同步对应文档 → 改动**未完成**。
3. **每片绿了再下一片**:`go build ./... && go vet ./... && gofmt -l(空) && go test -race ./...` 全绿 + 相关 GW-INV 验收过，才算完成。

## 2. 依赖方向铁律(import-lint 强制，见 architecture §3 + .golangci.yml depguard)

```
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg
```
- `domain` 只依赖 stdlib + pkg;`app` 在本包声明 infra 端口(interface)，**禁** import infra/`database/sql`/HTTP server;`infra` 结构化满足端口、唯一碰 OS/DB/网络;`bootstrap` 唯一可跨全层、**无人 import 它**;`pkg` 叶内核谁都不依赖。
- **事务聚合**(quota/install):事务边界 owned 在 `infra/store/*`(`orm.DB.Transaction`/`BEGIN IMMEDIATE`)，app 端口只暴露**一个原子聚合操作**，`*sql.Tx` 永不漏出 infra。
- **密钥边界**:secret(`DASHSCOPE_API_KEY`/`INSTALL_POW_SECRET`/`MEDIA_SIGNING_SECRET`)env-only，不入 `config.Specs()`、绝不入 settings 表/Dump/Snapshot/日志；`DASHSCOPE_WORKSPACE_ID` 是非 secret 的启动期 endpoint 标识；管理后台**不持有任何凭证**——它恒定只绑 loopback，登录墙归前置 IAP；Snapshot 只能暴露安全状态/数量。

## 3. 不可破的红线(摘要，全集见 invariants.md)

- **记账**:请求先冻结 provider/model/rate card 并换算为整数 pUSD；per-install 月请求额度 + operator 全局月 pUSD 钱包在单 `BEGIN IMMEDIATE` **双闸原子预留**，日表只做统计/审计。仅明确未计费可 rollback；provider `Open` 尝试后的 timeout/connect/error/client-cancel 等歧义结果按 full quote settle。`spend_ledger.state='open'` 终态 CAS 单赢家，period 入口快照一次贯穿，orphan 不退钱；崩溃只多扣不少扣。
- **安全**:key 只在 cloned request 上注入、且端点与 key 池在构造期一起冻结（一家的凭证走不到另一家的地址），redirect/上游 header/body/error 原文不得带 key 离端或透传；客户端无 bearer，install 以 Ed25519 公钥登记、逐请求 proof 绑定 authority/target/body 并防重放；admin/metrics/pprof + 管理后台仅 loopback(绑定 fail-fast、不上公网)；日志/指标无 key/prompt/media/proof/ip，label 仅低基数闭集；XFF 仅信回环直连对端。
- **可靠**:唯一 N_global 界住全部在飞;每个上游账号的 endpoint/key pool/per-key health/process breaker 物理隔离(两个账号不共享任何一样)；无 fallback、无跨池 key。**故障分类排除 client-cancel、429 与 400/413/422 请求拒绝**(不触进程 breaker)；其他显式 3xx/4xx 拒绝可证明未计费，但仍与 provider health 分类正交；多 key/排队/retry 不放大总在飞；关停 DB 最后关(bgWG 排空)。
- **输入/路由**:严格 top-level/content-part 关闭联合类，拒 `n>1`；客户端 `model` 只是逻辑 alias、绝不选 provider。文本与媒体同走 `MULTIMODAL_UPSTREAM_MODEL`；合法 user inline jpeg/png/webp 图片或 mp4 视频额外需要媒体通道；合法 wav/mp3 `input_audio` 先完成协议/形状/bytes 校验，再在 reserve/Open 前固定 `503 AUDIO_UNAVAILABLE`（当前无音频上游）。远程 URL/PDF/file 一律拒绝。Qwen 凭证是**部署必需**，缺失即启动 fail-fast（没有它什么也服务不了）。body/message/media 形状有界；`INPUT_TOKEN_CAP` 仅兼容保留，UTF-8 estimate 只做账务报价、**绝不准入拒绝**，真实 context hard limit 由 provider 判定。provider 400/413/422 归一为 `400 UPSTREAM_REJECTED`(闭集 reason、原文不透传、非故障非重试)；本地/边缘 body cap 统一 `413 REQUEST_BODY_TOO_LARGE`。

## 4. 工作流(切片纪律)

地基优先的 Clean Arch 重写**已完成**并落 `main`;后续每处改动仍按同一纪律走:`domain → app → infra → transport → 测试 → ref 文档`，GW-INV 当验收。
- **唯一事实源是代码 + `docs/references/backend/*`**(逐字契约);`docs/working/*` 中重写期抽取契约已 landed/superseded 作历史参考,**现行进行中工单**:[`working/repo-governance.md`](docs/working/repo-governance.md)(仓库治理战役)与 [`working/multimodal-generation.md`](docs/working/multimodal-generation.md)(生成能力,主仓 WRK-082)。capability/provider/pUSD 主决策见 ADR-0017，上下文准入与 route profile 由 ADR-0016 定向补充；ADR-001 raw-token 账本及 ADR-005/006 被取代部分只是历史。

## 5. 门禁命令

```sh
make verify   # vet + build + test -race + e2e(-tags=integration)+ lint + docs(本地门禁,与 CI 同一套规则)
make docs     # cmd/docs:frontmatter/类型·状态·目录/review-due/INDEX≤50/孤儿链接/working 90 天
make lint     # golangci-lint v2.6.1(errcheck/staticcheck/gosec/govet/depguard/...)
```
提交前:`gofmt -l`(空)+ build/vet/test-race 绿 + GOVERNANCE §12 收尾清单逐条勾。CI(`.github/workflows/ci.yml`)另跑 go mod verify / govulncheck / integration e2e / fuzz smoke / SBOM / 前端漂移门 / 覆盖率地板(quota ≥70%、chat ≥65%)。

## 6. 速查

- 仓库:`<repo>` · 分支 `main`(线上 lineage)
- 入口:`cmd/gateway`(瘦壳)→ `internal/bootstrap`(组合根) · 三监听器:业务 8080(公网/socket-activated)· admin 9090(loopback)· dashboard 8081(loopback)
- 路由:`PUBLIC_MODEL_ID`(默认 `anselm-auto`)· text 与 image/video 同为 `qwen3.7-plus`（1M input / 64K output、thinking-on）· audio=协议已知但当前 `AUDIO_UNAVAILABLE`· no fallback
- SQLite:0001..0007 七个迁移;应用表含 identity/config、pUSD 账本、媒体 staging/lease、品类日账本与音色库存,外加 `schema_migrations`。v1 accounting 三表仅读保留(阶段 4 压平时移除)。
- Go:`mise which go`(1.25) · 文档体系:[`docs/INDEX.md`](docs/INDEX.md)(会话入口)
