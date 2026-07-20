---
id: DOC-001
type: concept
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
---

# Anselm Gateway 架构（当前心智模型）

> Go 模块 `github.com/sunweilin/anselm/gateway`。本文解释系统为何这样分层、一次请求怎样跨 capability/provider/账本；逐字 HTTP/config/DB/error/invariant 契约在 [`references/backend/`](../references/backend/)，决策在 [`decisions/`](../decisions/)。本文按 state 文档规则整体重述，只描述当前事实。

## 1. 系统边界

Anselm Gateway 是单二进制、单 SQLite 的**确定性 capability 网关**：它不生成内容、不存会话；它为 install bearer 用户代理两种能力，并在调用前用运营者真实价格悲观占款。

```text
纯文本完整历史 ──▶ DeepSeek V4 Flash
任一受支持媒体 ──▶ Gemini 3.1 Flash-Lite
                       │
                       ▼
         provider rate card ──▶ pUSD 成本账本
```

“模型选择”不是用户等级：client 只看到一个 `PUBLIC_MODEL_ID`（`/v1/models`、stream chunk 与 non-stream completion 顶层 `model` 一致）。服务端扫描完整 message history；内容形状确定 provider。没有语义分类、没有高级/普通档、没有 fallback。

系统不是多租户 SaaS、账号平台、对话库或任意文件处理器。隔离单元是 install；只接收本文 API 契约内的 inline jpeg/png/webp 与 wav/mp3，不下载 URL、不接 PDF/video/file。

### 三个物理监听器

| listener | 默认 | 面 | 控制 |
|---|---|---|---|
| business | `127.0.0.1:8080` | `/v1/*`、health | Caddy 终止 TLS；install bearer |
| admin | `127.0.0.1:9090` | metrics/readiness/pprof/expvar | 物理 loopback，无应用 auth，绝不反代 |
| dashboard | `127.0.0.1:8081` | operator SPA/API | loopback + session + CSRF |

地址两两不同；admin/dashboard 绑定前 fail-fast 验 loopback。business 优先使用 systemd socket activation，缺失时自绑。

## 2. Clean Architecture 与依赖方向

把纯路由/计价/记账规则放内层，把 HTTP、SQLite、provider SDK 变化隔离在外层：

```text
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg
```

| 层 | 职责 | 关键边界 |
|---|---|---|
| `domain` | strict chat union、capability classification、billing rate card/Plan、quota types、config rules、wire errors | stdlib + pkg；无 DB/HTTP/OS infra |
| `app` | auth/rate/disk/input/routing/reserve/forward/settle 用例编排；在本包声明 infra port | 不 import infra/transport/`database/sql`；`app/chat` 是唯一可组合 install+quota 的协调者 |
| `infra` | SQLite/migrations/stores、provider-local upstream engine、chatprovider adapters、metrics/config/disk/webassets | 唯一网络/DB/OS 实现；结构化满足 app port |
| `transport` | 三 mux、中间件、HTTP↔app 薄 handler、统一 envelope | 不 import infra/bootstrap |
| `bootstrap` | 构造 DeepSeek/Gemini backend registry、stores、services、routers、loops、lifecycle | 唯一跨全层组合根；无人 import 它 |
| `pkg` | logging/redaction、request id、client IP、PoW、sampling/alerts 等叶内核 | 不依赖其它 internal 层 |

`cmd/gateway` 只调用 bootstrap。依赖方向由 depguard 强制；事务只暴露聚合 port，`*sql.Tx` 不穿过 infra 边界。

## 3. 双 provider 是两个故障域

bootstrap 固定构造：

```text
chatprovider.Registry
├── DeepSeek Backend
│   ├── endpoint + DEEPSEEK_API_KEY pool
│   ├── per-key breaker/cooldown
│   └── process breaker
└── Gemini Backend（GEMINI_API_KEY 非空才存在）
    ├── endpoint + Gemini key pool
    ├── per-key breaker/cooldown
    └── process breaker
```

两边共享唯一 `N_GLOBAL_CONCURRENCY` 总信号量，但不共享 endpoint、key、breaker、provider 扩展或账单身份。这样 Gemini 故障不会熔断 DeepSeek，换 key/retry 也不会放大总在飞。

Registry 没有 fallback API。路由已冻结 billing Plan 后切 provider 会同时破坏能力、认证、价格和审计，因此从类型/装配上禁止。Gemini adapter 去掉 DeepSeek 专有 `reasoning_content`，强制最低可用 thinking effort；DeepSeek adapter 保留其 tool continuation 语义。

DeepSeek key 是启动必需；Gemini key 可稍后配置。缺 Gemini 只关闭多模态请求并返回明确 503，不影响文本 route 或 readiness。readiness 每 30s 缓存一次无推理费用的认证 `/models` 结果：DeepSeek key + 固定模型始终必须通过；若配置 Gemini key，则 Gemini key + 固定模型也必须通过。它不为每个请求现场探测 provider。

## 4. Chat 请求生命周期

顺序遵循“廉价验证先行、外部支出最后”：

| # | 动作 | 失败结果 / 账务 |
|---:|---|---|
| 1 | bearer lookup、banned 判定、rate limit、anomaly observe | 401/403/429；无 reserve |
| 2 | diskguard、body cap、strict decode、messages/文本 shape | 400/503；无 reserve |
| 3 | 完整 history 验证 content union；base64/MIME/magic/role/media totals | 400；无 reserve |
| 4 | capability 二分；检查选中 backend 是否 configured | Gemini 缺 key→503；无 reserve |
| 5 | 文本/tools estimate cap；模型 input hard limit；输出 clamp | 400；无 reserve |
| 6 | 冻结 provider/model/rate-card Plan；Gemini 按 full hard limits quote | 无精确 card/quote 不可装入 install cap→fail closed/400 |
| 7 | 单事务 reserve 月额度 + install/provider/global 三 pUSD 钱包 | 429 quota/rate 或 402 budget |
| 8 | provider-local breaker fast path + shared N_global 有界排队 | Open 前失败，完整 rollback |
| 9 | 对**唯一选中 provider** Open；stream/non-stream 均等到首个 body byte 才返回 stream，否则返回 `CallFailure{APIError,ChargeExposure}` | DefinitelyUnbilled→rollback；ChargePossible→不 retry、full settle |
| 10 | success body fail-closed 校验/alias；终局 usage→冻结 rate card→settle | 非单一 object、duplicate/case-fold model key 或非法 SSE control 不透传；stream 仅完整读到合法 DONE 后可退款；缺/坏/duplicate usage（含 sticky malformed）→full quote；真实费用更高→truthful top-up |

### client error、health、retry、charge 是四条独立轴

```text
CallFailure
├── APIError ─────────────▶ client status/code/message
└── ChargeExposure
    ├── DefinitelyUnbilled ▶ rollback
    └── ChargePossible      ▶ no retry + full settle
```

- `ChargePossible` 是零值；unknown/malformed adapter 因此只能保留 reservation。app 调 `MayHaveCharged()`，不从 error code 反推。
- Open 前本地拒绝与明确 provider 3xx/4xx（400/401/402/403/429 等）是 `DefinitelyUnbilled`；connect/TLS/read error、timeout、5xx、call 中 client-cancel 是 `ChargePossible`。
- 同一个 `UPSTREAM_ERROR` 可同时覆盖 401/402 明确 refusal（rollback）与 connect/5xx 歧义（full settle）；wire code 只为客户端归一，不是财务凭证。
- stream 与 non-stream 都以“2xx 且首个 body byte 已到达”为成功 handoff 边界；2xx header 本身不算进展，避免 non-stream 在 `ReadAll` 前永久钉住共享 slot。成功 handoff 后月请求计数必保留。non-stream 完整校验 object 后可按 usage settle；stream 只有网关完整读到合法 `[DONE]` 才把累计 usage 当作退款证据，提前 EOF/读错/断连保留 full quote，而 DONE 已读后的 client 写失败不推翻终局证据。

自动 retry 不是“任何首字节前瞬态错误都重试”。总 attempt 最多 3，且只对可证明未收费、能绕开单 key 的两类信号：401/403 cooldown 后换 sibling key，或 key breaker 已 open 时换 sibling key。429/其它 3xx/4xx 虽可退款但不 retry；connect/TLS/timeout/5xx/client-cancel 可能已收费，立即终止并 full settle。这个限制保证一份 reservation 不会隐藏多个可能的 provider charge。

process breaker 是另一维度：client-cancel、429、401/403 key signal、key-open、400/413/422 不计 fault；connect/timeout/5xx 与其它被 typed health class 归为 upstream fault 的明确 refusal 可计所选 provider breaker。明确 refusal 即使 `DefinitelyUnbilled` 也不自动等于“不计 breaker”；反过来，“计 breaker”也不意味着“可以 retry”。

## 5. 为什么账本用 pUSD

raw token 不是钱：DeepSeek cache hit/miss/output 与 Gemini text/audio/thinking output 各有价格。跨 provider 累加 token 会把不同金额误当相同额度。billing domain 因此先冻结：

```text
Plan = provider + actual model + rate-card version
     + input class + input/output quote + reserved pUSD
```

内部金额用整数 pUSD，精确表达最小单 token 价格且无浮点误差。DeepSeek quote 使用 UTF-8 byte-fallback 上界（所有透传 message/tool 字段 + 每消息 64 token framing 余量）和 output clamp；Gemini compatibility 不能证明 hidden thinking 的较小上界，所以 reserve 模型完整 input/output hard limits，usage 可证明后再 refund。流式累计 usage 逐字段取最大值而不求和；任一帧出现负数/畸形证据会 sticky 到终局，不能被后续较大正常值洗白。Gemini 只有正且不小于 `prompt+completion` 的 `total_tokens` 才可能退款；DeepSeek 的 cache hit/miss 合计不得超过 prompt，未报告部分按 miss。`INPUT_TOKEN_CAP` 只挡文本/tools estimate；media 用 part/decoded-byte 上限控制内存与形状，实际 media token 由 Gemini 判定，但绝不会削弱 full-limit 成本预留。

## 6. Unit of Work 与状态机

单写 SQLite pool + `BEGIN IMMEDIATE` 串行化以下不可分操作：

```text
monthly entitlement
  + install-day pUSD
  + selected-provider-day pUSD
  + global-day pUSD
  + spend_ledger(open)
```

任何 gate 未命中，整 tx 回滚。Reservation 携带入口 `Period`、冻结 Plan 与 `SublimitApplied`，后续不重读 hot config 来猜“当时扣了什么”。

```text
open ── usage / conservative full quote ──▶ settled
  ├── definitely unbilled ───────────────▶ rolled_back
  └── aged unknown outcome ──────────────▶ orphaned(full quote)
```

每条边都是 `WHERE state='open'` CAS，只有单赢家调整余额。orphan 不退款，因为 crash 不能证明 provider 没收费；这是保守记账的方向性。Settle 的实际 cost 若超过 reservation，会 top-up 到真实余额，即使越 cap；cap 只阻止未来支出，不能改写已经发生的账。

## 7. 安全与可观测

- provider auth 只注入 cloned request；上游 header/body/error 原文不反射。
- inline media、prompt、tool JSON、token、key、IP 不进入日志/指标；redactor 对相关字段和复合值 fail closed。
- metrics provider label 只有 `deepseek|gemini`，outcome 是固定闭集；model id 不作 label。
- dashboard 金额在展示边界从 pUSD 转成整数 microUSD；DB 仍保留精确 pUSD。
- low disk 在 reserve 前 shed；所有 detached accounting goroutine 由 bgWG 跟踪，DB 在 shutdown 最后关闭。

## 8. 六个业务关注点

| 域 | app | 核心职责 |
|---|---|---|
| chat | `app/chat` | capability route + provider open/relay + accounting saga |
| quota | `app/quota` | Plan reserve/settle/rollback/reconcile + client quota view |
| install | `app/install` | bearer identity、Sybil/PoW issuance |
| model | `app/model` | 恰一个 provider-neutral public model |
| health | `app/health` | liveness/readiness 聚合；active providers 的认证 exact-model cached probe |
| dashboard | `app/dashboard` | spend/ledger/health/config/install 运维 read/write model |

## 9. 决策索引

| ADR | 当前作用 |
|---|---|
| [0012](../decisions/0012-deterministic-capability-routing-and-cost-ledger.md) | capability 路由、provider 隔离、pUSD Plan/ledger、Open 账务边界（取代 001 raw-token 账本） |
| [0002](../decisions/0002-unified-structured-error-type.md) / [0003](../decisions/0003-bare-success-error-envelope.md) | 统一 APIError 与 wire envelope |
| [0004](../decisions/0004-three-physically-isolated-listeners.md) / [0010](../decisions/0010-systemd-socket-activation.md) | 三监听器与 business socket lifecycle |
| [0005](../decisions/0005-sqlite-rw-pool-versioned-migrations.md) | SQLite 单写/有界读 + forward migrations |
| [0006](../decisions/0006-config-tiers-atomic-hot-reload.md) | config tiers 与原子 overlay |
| [0007](../decisions/0007-sybil-pow-dormant-by-default.md) | dormant Sybil/PoW |
| [0008](../decisions/0008-doc-governance-adoption.md) | doc-code parity |
| [0009](../decisions/0009-react-dashboard-clean-architecture.md) | embedded operator dashboard |
| [0011](../decisions/0011-fault-classification-excludes-cancel-429.md) | provider fault classification 与低基数指标 |

## 10. 生命周期

bootstrap 顺序：env/base config→SQLite+migrations→overlay→stores→DeepSeek backend→optional Gemini backend→Registry→app services→三 routers→background loops。diskguard 在 serve 前同步 prime；orphan reconciler 启动即扫、随后周期运行。

shutdown：①取消 scanners/loops；② Shutdown business/admin/dashboard；③等待 bgWG（detached settle/rollback、reconcile、probe、disk/metrics）；④最后 Close SQLite。关 DB 提前于 accounting drain 是财务数据损坏，不是普通 shutdown error。
