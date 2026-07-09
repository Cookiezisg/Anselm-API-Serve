---
id: DOC-001
type: concept
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-20
audience: [human, ai]
---

# Anselm Gateway 架构（目标心智模型）

> 本文件是 gateway（Go 模块 `github.com/sunweilin/anselm/gateway`）的**稳定心智模型**：它是什么、怎么分层、依赖往哪流、一个请求怎么走完。代码契约（api/config/db/errors）见 [`references/`](../references/)；决策动机见 [`decisions/`](../decisions/)；文档规范见 [`GOVERNANCE.md`](../GOVERNANCE.md)。本文按 §1.7 整体重述维护——只描述当前事实，零历史叙述。

---

## 1. gateway 是什么

一个**薄的免费层 DeepSeek 代理**。它在公网与上游 DeepSeek 之间挡一道，对匿名领号用户做**保守额度记账 + Sybil/PoW 防滥用 + 可靠性兜底**，本身不产 token、不存对话。核心价值不在功能多，而在**记账永不超卖、滥用可降速、崩溃只多扣不少扣**——所有复杂度都服务于这三条财务/安全红线。

它**不是**：多租户 SaaS、带真实账号鉴权的平台、对话存储后端。隔离单元是 install（领号实体），鉴权 = bearer token → install 查找。

**三个物理隔离的监听器**（[ADR-004](../decisions/0004-three-physically-isolated-listeners.md)）——隔离是物理的、不是中间件判断：

| 监听器 | 默认地址 | 暴露面 | 鉴权模型 | 理由 |
|---|---|---|---|---|
| business | `0.0.0.0:8080` | `/v1/*`、领号、challenge、healthz | bearer token | Caddy 前置终止 TLS；唯一公网面 |
| admin | `127.0.0.1:9090` | `/metrics`、`/readyz`、pprof、expvar | **无**（loopback = 物理控制） | 绑定即 `requireLoopback` fail-fast，无需鉴权代码（GW-INV-13） |
| dashboard | `127.0.0.1:8081` | 运维 SPA + 其 API | session + CSRF | 管理面与公网面物理分桶 |

三地址**必须互异**，否则 `config.Load` fail-fast（GW-INV-18）；admin **必须 loopback**，绑定时校验。business 优先取 systemd socket-activation 的 fd（重启不丢内核 backlog），无 systemd 时自绑（[ADR-010](../decisions/0010-systemd-socket-activation.md)）。

---

## 2. 为什么 Clean Architecture（六层）

依赖**单向**，箭头只能指向**更稳定的内层**。目的：把"会变的"（HTTP 框架、SQLite、gobreaker、Prometheus）挡在外层，让"财务/安全不变式"住在不依赖任何外部包的纯内核里——这样换驱动、换 HTTP 库、加端点都动不到记账逻辑。这是把 14 个已确认 bug 中的多数变为**构造性免疫**的结构前提（见 §6）。

| 层 | 包 | 职责 | 关键约束（为什么） |
|---|---|---|---|
| **domain** | `internal/domain/*` | 纯类型 + wire-code sentinel + period/estimate 纯算 | **零 infra import**（仅 stdlib + pkg）。记账/PoW 难度/clamp 等不变式住这里，与 DB/HTTP 解耦才能被任意层复用、被单测直测 |
| **app** | `internal/app/*` | 用例编排：把 domain 跑在 infra 之上；**在本包内声明 infra 端口（interface）** | 不 import infra/transport/`database/sql`/`net/http` server。业务流程靠 DIP 端口注入，换实现不改用例 |
| **infra** | `internal/infra/*` | 适配器：SQLite 池/迁移、上游 HTTP 客户端、限流、磁盘哨兵、配置 provider、metrics、内嵌资产 | **唯一碰 OS/DB/网络**的层。结构化满足 app 端口（Go duck-typing，不反向 import app 用例） |
| **transport** | `internal/transport/httpapi/*` | HTTP 边界：`response`（统一信封）+ `middleware` + `handlers`（按端点薄壳）+ `router`（3 个 mux） | 只翻译 HTTP ↔ app，不含业务。不 import infra/bootstrap |
| **bootstrap** | `internal/bootstrap` | 组装根：env→provider→stores→upstream→app→3 routers→生命周期 | **唯一可同时 import 全层**的包。**无人 import 它**（无环） |
| **pkg** | `internal/pkg/*` | 跨层叶内核：logx、reqid、idgen、clientip、noncecache、pow、ratesample、alert | 只 import stdlib + 其它 pkg + 受信三方。**绝不** import domain/app/infra/transport/bootstrap |

`cmd/gateway/main.go` 是薄壳：解析 env → 调 `bootstrap.Build` → 跑生命周期 + 信号处理；**只 import bootstrap**。

---

## 3. 依赖规则（import-lint 强制）

一层只能 import **严格在其右侧**的层：

```
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg   (pkg 只依赖 stdlib + pkg + 三方)
```

机械可判的红线（用 `depguard` / `go-arch-lint` 守）：

| 规则 | 内容 |
|---|---|
| domain | 只 import stdlib + pkg；**禁** app/infra/transport/bootstrap 及任何 DB/HTTP/OS 三方 |
| app | import domain + pkg；infra 需求一律声明为**本包内 interface 端口**；禁 import infra/transport/`database/sql`/HTTP server 类型/任何具体 store·client |
| infra | import domain + pkg + 三方；**结构化**满足 app 端口（不 import app 用例）；禁 import transport/bootstrap |
| transport | import app + domain + pkg；`response` 只 import `domain/apierr`；禁 import infra/bootstrap |
| bootstrap | 唯一可跨 import 全层；**无人可 import bootstrap** |
| pkg | 叶内核；禁 import 任何 internal 层（除其它 pkg） |

**层内唯一允许的兄弟耦合**：`app/chat` 可组合 `app/quota` + `app/install` 用例——chat 是唯一合法编排 quota+install+upstream 的协调者。**这是唯一例外**，其余 app 包保持兄弟独立。

**密钥边界**（测试守，非 import-lint）：`domain/config` 与 `infra/configprovider` 绝不序列化 secret 字段；`Dump`/`Snapshot` 返回掩码值（[ADR-006](../decisions/0006-config-tiers-atomic-hot-reload.md)）。

---

## 4. 请求生命周期 — `/v1/chat/completions`

核心设计是**最便宜优先（cheapest-first）的闸门顺序** + **reserve→forward→settle 的 saga**。理由：最贵的工作（写 DB、调上游）只在所有便宜闸门通过后才到达；每一步在进入下一步前就拒绝，把无效请求挡在 DB 写入之前。`cfg = h.cfg.Load()` **整个请求只快照一次**（step 3b），复用于所有 guardrail + 模型解析——热更新永不在单请求内出现半旧半新边界（GW-INV-08，这是 B1 修复的泛化形式）。

| # | 闸门 | 拒绝（status / wire code） |
|---|---|---|
| 0 | Method = POST | 400 BAD_REQUEST |
| 1 | bearer 非空 | 401 INVALID_TOKEN |
| 1b | token 查找：err→500；未找到→401；status=banned→拒 | 401 / 403 ACCOUNT_BANNED / 500 |
| 2 | anomaly observe（置 `rec.throttled`）→ 每 install 分钟桶 `rl.Allow` | 429 RATE_LIMITED |
| 2b | 磁盘降级 `!degraded()`——**在任何 reserve 之前**（REL-6） | 503 DISK_LOW |
| 3 | body 读（`MAX_BODY_BYTES` 上限，默认 256KiB）+ `decodeInbound`（n>1 探测 + 白名单严解）+ messages 非空 | 400 BAD_REQUEST |
| 3b | SEC-1 形状：`len ≤ MaxMessages`、每条 runes ≤ MaxMessageChars | 400 BAD_REQUEST |
| 4 | 输入 token 上限：`estimatePromptTokens ≤ InputTokenCap`（保守：max(bytes/3,runes)+8/msg ×1.2 ceil）；`INPUT_TOKEN_CAP=0` ⇒ 跳过（上游模型判定） | 400 BAD_REQUEST |
| 5 | 构上游体：`resolveModel`（强制白名单，默认 `DefaultModel`）+ `clampMaxTokens` + `sanitizeUpstream`（注入 `stream_options.include_usage`）；`est = promptEst + maxTok`；预检 `est ≤ InstallDailyTokenCap`（恒不可能成功的请求直接 400，GW-INV-10） | 400 BAD_REQUEST |
| 6 | **额度 reserve**：3-guardrail 单事务（§5） | 429 QUOTA_EXHAUSTED / 429 RATE_LIMITED / 402 BUDGET_EXHAUSTED / 500 |
| 6b | 进程 breaker 快路：State≠Open；Open⇒回滚 resv，**不占 N_global slot** | 429 UPSTREAM_BUSY |
| 7 | N_global 信号量（REL-7）：取 slot 或等 `QueueWait`；失败回滚 resv；ctx 取消⇒499，否则429。cap 恒 `N_global`（队列不放大并发） | 429 UPSTREAM_BUSY / 499 CLIENT_CANCELED |
| 8 | forward：connect→首字节，有界重试（maxAttempts=3、base 200ms、cap 3s、full jitter、**仅 pre-output、仅 502/503/504/connect、非 429**）；首字节首字节计时器=`UpstreamHeaderTimeout`；首字节(Peek)/2xx⇒`outputStarted=true` | 归一化 429/502/504；上游 400/413/422 ⇒ 400 UPSTREAM_REJECTED（非故障非重试，ADR-011 修订）；REL-5 deferred rollback if `!outputStarted` |
| 9 | relay + settle：流式按 SSE 到 `[DONE]` 解析 `usage.total_tokens`；非流读 `LimitReader 8MiB` 解析 usage；按实际 settle（无 usage/断连则按全 est）。**出字节后 count 永保留** | — |

**saga 三段**（reserve→forward→settle）：

1. **reserve** = 悲观预扣：按 `promptEst + clampedMaxTokens` 高估，先占住三道闸门（§5）。
2. **forward** = 预输出窗口：重试 + 两个 breaker 都**只活在出第一字节之前**；REL-5 单一防御点 `outputStarted bool` + `defer{ if !outputStarted { rollback } }`——出字节前任何失败回滚全部三项，出字节后产物绝不重试/双扣（GW-INV-02/03）。
3. **settle** = 按真实 usage 对账：refund/top-up 差额。**月度 count 一旦出字节即保留**（反流式滥用，settle 不动 count）。

Settle/Rollback 跑在 `context.WithoutCancel(parent)` 的 goroutine、由共享 `bgWG` 追踪——优雅关停**等记账完成才关 DB**（REL-4 红线，§7）。

---

## 5. Unit-of-Work：事务模式

**问题**：三道 guardrail（月度 count、install 日 token、全局日预算）的 read-modify-write 必须**原子**，否则并发尖峰超卖、钱包被刷穿。**解法**：单写池 + 单事务，事务边界**owned 在 infra/store**，app 端口只暴露**一个原子聚合操作**，`*sql.Tx` 永不泄漏出 infra。

| 关注点 | 归属 | 内容 |
|---|---|---|
| 事务编排 | `infra/store/quotastore`（owns `orm.DB.Transaction` / `BEGIN IMMEDIATE`） | 在**一个** tx 内顺序跑三道条件 `UPDATE ... WHERE ...` + INSERT ledger；任一 `RowsAffected()==0` ⇒ 整 tx ROLLBACK（deferred `tx.Rollback()` until `committed`）+ 对应 `APIError` |
| 原子聚合 op | `app/quota` 端口 | `Reserve / Settle / Rollback / ReconcileOrphans`——每个是一次**不可分**的聚合操作，调用方拿到的是结果（`Reservation` 或 error），**看不到 tx** |
| 纯类型/规则 | `domain/quota` | `Period{Month,Day}`、`Reservation{RequestID,InstallID,Period,Reserved,SublimitApplied}`、guardrail 值类型 |

**单写池**（[ADR-005](../decisions/0005-sqlite-rw-pool-versioned-migrations.md)）：写池 `MaxOpenConns=1` + DSN `_txlock=immediate` ⇒ **所有写串行**，read-modify-write 无法交织——这是单进程下防超卖最省力的正确答案，无需分布式锁。读走有界读池。

**为什么 `*sql.Tx` 不能泄漏到 app**：若 app 持有 tx，事务边界就散落到用例里，infra 换实现（如换 ORM）会撕裂 app；且 app 无法被无 DB 单测。端口签名 = 聚合 op，让"一次记账 = 一个原子调用"成为类型级事实。

**保守偏向 + 幂等**（GW-INV-01/04/05/06）：reserve 高估、settle 按实际下修、崩溃只多扣不少扣；Settle/Rollback/Reconcile 经 `ledger.settled IS NULL` 单赢家 CAS 互斥幂等（detached settle goroutine 与周期 orphan 扫描可竞争同一行，CAS 防双调整）；`Period` **请求入口快照一次**、贯穿三段、settle/rollback **永不重算**（防跨午夜对错日行）。会计核心动机详见 [ADR-001](../decisions/0001-pessimistic-three-guardrail-reservation.md)。

---

## 6. 关键设计 → ADR

每个非平凡取舍住一篇不可变 ADR（`decisions/`，仅新建 supersede）。本表是索引、不复制结论：

| ADR | 决策一句话 |
|---|---|
| [ADR-001](../decisions/0001-pessimistic-three-guardrail-reservation.md) | 悲观三-guardrail 预扣记账：单 `BEGIN IMMEDIATE` 预扣 `est=promptEst+clampedMaxTokens`，按实结算、出字节前回滚一次、孤儿对账；崩溃恒多扣 |
| [ADR-002](../decisions/0002-unified-structured-error-type.md) | 统一结构化错误 `APIError{Status,Code,Message,Details}`，稳定 UPPER_SNAKE wire code 作 `domain/apierr` sentinel，合并两套旧信封 |
| [ADR-003](../decisions/0003-bare-success-error-envelope.md) | 裸成功 / 错误信封契约（**刻意背离** Foryx `{data}`）：成功=裸实体 JSON，失败=`{"error":{...}}`；`/v1/models` 保 OpenAI list 形状；healthz/readyz/metrics 保非信封 |
| [ADR-004](../decisions/0004-three-physically-isolated-listeners.md) | 三个物理隔离监听器（business 公网 / admin loopback 无鉴权 / dashboard loopback session）；三地址互异 fail-fast |
| [ADR-005](../decisions/0005-sqlite-rw-pool-versioned-migrations.md) | SQLite 读写分池（单写 `MaxOpenConns=1,_txlock=immediate` + 有界读池）+ 内嵌编号 `.sql` 迁移 + `schema_migrations`（forward-only、checksum） |
| [ADR-006](../decisions/0006-config-tiers-atomic-hot-reload.md) | 配置三档（runtime-hot / secret-env-only / startup-hard）+ `atomic.Pointer[Config]` 无锁读 + clone→validate→全或无持久→swap 热更新；secret 永不持久/dump |
| [ADR-007](../decisions/0007-sybil-pow-dormant-by-default.md) | M2 Sybil/PoW **默认 dormant**：每道 Sybil 闸门默认 0=禁用并在任何 DB 工作前短路；`INSTALL_POW_MODE` 三态 off/shadow/enforce，无状态 HMAC challenge，nonce-once 最后验，secret 强一致 |
| [ADR-008](../decisions/0008-doc-governance-adoption.md) | 采用 Foryx `docs/GOVERNANCE.md` 模型（6 类型、frontmatter、doc-code parity、`make docs` 门禁、ADR 不可变），本地化到 gateway |
| [ADR-009](../decisions/0009-react-dashboard-clean-architecture.md) | React/Vite/AntD dashboard 分层（api-client / types-mirror / auth-context / pages），`go:embed` 进 `infra/webassets`，session+CSRF 后服务，fresh-clone 构建门禁 |
| [ADR-010](../decisions/0010-systemd-socket-activation.md) | business `:8080` 优先 systemd socket-activation fd（重启不丢 backlog），无 systemd 自绑回退；admin/dashboard 自绑 |
| [ADR-011](../decisions/0011-fault-classification-excludes-cancel-429.md) | 故障分类**排除** client-cancel、429 与上游 4xx 请求拒绝：单一 `faultClass` 一处计算，client-cancel→499（非故障非重试），429→UPSTREAM_BUSY（非故障非重试），400/413/422→UPSTREAM_REJECTED(400)（非故障非重试，2026-07 修订）；仅 {5xx,timeout,connect} 计入进程 breaker；metrics 标签严格低基数互斥 |

**结构如何换来 bug 免疫**：故障分类单点排除 client-cancel ⇒ 客户端断连不再触发进程 breaker（B5/B3 DoS 放大消失）；`Reservation.SublimitApplied` 显式记录 ⇒ rollback 反转恰好所占、不重读热配置（B1）；Settle/Rollback 错误捕获进低基数计数器 + 非采样 WARN ⇒ 失败 settle 可见、不被孤儿扫描静默全额退（B2）；16B install id + regenerate-on-conflict ⇒ 碰撞可恢复非 500（B13）。完整 14 bug→规则映射见 [`references/`](../references/) 行为契约。

---

## 7. 组装与生命周期（bootstrap）

`bootstrap` 是唯一组装根，按依赖序装配，分三文件：

| 文件 | 职责 |
|---|---|
| `build.go` | `Build(*App)`：env load → configprovider → sqlite 池 + 迁移 → stores → upstream client → app services → 3 routers，返回 servers + `bgWG` |
| `listeners.go` | socket-activation fd 优先 + 自绑回退 + `requireLoopback`（admin 绑定时 fail-fast） |
| `lifecycle.go` | READY notify；reconciler / prober / diskguard 等后台 loop（均挂 `bgWG`）；有序关停 |

**关停顺序严格**（REL-4，GW-INV-24，DB **最后**关）：① `scanCancel()` 停所有 loop → ② `srv.Shutdown` + `adminSrv` + `dashboardSrv` → ③ `bgWG.Wait(30s)`（等所有 detached settle/rollback + reconciler + prober + diskguard + metrics loop）→ ④ `st.Close()`。理由：任何 detached 记账跑在 `WithoutCancel` 上，DB 必须等它们落库才关，否则 settle 中途撞 DB 关闭、保守记账被腐蚀。

diskguard 必须在服务前**同步 `Check()` 一次**（乐观起始 `false`，否则满盘时首请求漏过）；orphan reconciler 启动 + 5 分钟 ticker 扫 `settled IS NULL AND created_at < now-10m`。

---

## 8. 包树（canonical）

```
github.com/sunweilin/anselm/gateway
├── cmd/gateway/main.go              # 薄壳：env→bootstrap.Build→生命周期→信号
├── internal/
│   ├── domain/                      # 纯类型 + wire-code sentinel + period/estimate 纯算；零 infra import
│   │   ├── apierr/                  # APIError + 全部 wire-code sentinel（ADR-002）
│   │   ├── quota/                   # Period / Reservation / guardrail 值类型
│   │   ├── install/                 # Install 实体 + token/fp 哈希契约 + id 形状 + PoW challenge 值类型 + 难度算
│   │   ├── chat/                    # inbound/upstream/chatMessage + estimate + clampMaxTokens + resolveModel 规则
│   │   ├── model/                   # model-catalog 实体 + OpenAI list 信封形状
│   │   └── config/                  # Config struct + 全部边界常量 + tier 枚举 + validateSemantics + WorstCaseMemoryMiB（纯）
│   ├── app/                         # 用例：在 domain 之上编排 infra；端口（interface）在本包声明
│   │   ├── quota/                   # Reserve/Settle/Rollback/ReconcileOrphans；声明 Ledger/Usage/Budget Store 端口
│   │   ├── install/                 # 领号用例 + Sybil 闸门(ip/global/fp) + PoW 闸门 + LookupInstall 鉴权
│   │   ├── chat/                    # 代理用例：闸门序 + reserve→forward→settle + REL-5 回滚 + anomaly throttle + breaker 编排
│   │   ├── model/                   # live-allowlist catalog 读
│   │   ├── health/                  # liveness + readiness 聚合（db/upstream/disk）
│   │   └── dashboard/               # 运维用例：overview / config-apply / install 列表·ban·unban / audit / export
│   ├── infra/                       # 实现 app 端口；唯一碰 OS/DB/网络
│   │   ├── sqlite/                  # 读写池 open + DSN/PRAGMA + 版本化迁移 runner
│   │   │   └── migrations/          # 内嵌编号 .sql + schema_migrations
│   │   ├── store/{installstore,quotastore,settingsstore}/   # 各实体 DML
│   │   ├── upstream/                # DeepSeek HTTP 客户端：redactingTransport / per-key breaker+cooldown / pickKey / retry / 首字节计时
│   │   ├── ratelimit/               # 内存 per-install 分钟令牌桶 + SetKeyLimit
│   │   ├── diskguard/               # statfs 探测 + 原子降级 flag + SetFloors
│   │   ├── configprovider/          # atomic.Pointer[Config]：Load/ApplyOverrides/LoadWithOverlay/Dump
│   │   ├── metrics/                 # Prometheus registry + RED Wrap + gauges + expvar
│   │   └── webassets/               # go:embed SPA dist + contentTypeFor
│   ├── transport/httpapi/
│   │   ├── response/                # 单一信封：WriteJSON(裸成功) + WriteError/WithDetails；APIError→wire
│   │   ├── middleware/              # Recover / DenyCORS / MaxBody / securityHeaders / requireSession / requireCSRF / loginlimit
│   │   ├── handlers/{business,admin,dashboard}/   # 每端点薄壳，调 app 用例
│   │   └── router/                  # 3 个 mux builder（business/admin/dashboard）+ chain 组装
│   ├── bootstrap/{build,listeners,lifecycle}.go   # 组装根 + 监听器 + 生命周期
│   └── pkg/                         # 跨层叶内核
│       ├── logx/        # slog JSON + redactAttr floor + slog.Any ban
│       ├── reqid/       # X-Request-ID mint + sanitizeRID
│       ├── idgen/       # ins_/req_/gwk_ id（16B install id，conflict 重生）
│       ├── clientip/    # XFF rightmost-only-if-loopback + /64 collapse
│       ├── noncecache/  # 有界 LRU+TTL UseOnce
│       ├── pow/         # 无状态 challenge mint/verify（HMAC/freshness/difficulty）
│       ├── ratesample/  # 服务端滑窗 QPS（无 per-poll 可变 sampler）
│       └── alert/       # AlertState 聚合（低基数）
└── docs/                            # 本规范体系
```
