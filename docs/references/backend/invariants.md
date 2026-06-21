---
id: DOC-007
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
audience: [human, ai]
---

# 不变量登记册（GW-INV-NN）

> 验收准绳。每条 GW-INV 是一条硬契约；编号**永不复用**。本篇与代码对齐，详尽语句沉淀自 `docs/working/spec-invariants.md`（已 landed-into 本篇）。配套契约：记账→[database.md](database.md)，码→[error-codes.md](error-codes.md)，配置→[config.md](config.md)，端点→[api.md](api.md)。

## A. 财务正确性

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-01 | 三闸门（月次数 `usage.count<MonthlyQuota`、install 日 token `usage.tokens+est≤InstallDailyTokenCap`、全局日预算 `budget.tokens_used+est≤GlobalDailyBudget`）在写池单 `BeginTx` 内悲观预留，各为条件 `UPDATE…WHERE…`；任一 `RowsAffected()==0` 返对应 `APIError` 并整 tx 回滚 | 并发尖峰超卖配额 / 抽干钱包 |
| GW-INV-02 | 输出前失败（retry 耗尽 / breaker open / 全 key down / marshal 错 / 队列超时 / 排队中 client cancel）经单一 REL-5 防御点 `outputStarted` + `defer` 回滚全部三预留 + 日请求计数 `requests-1`（+ 启用时日子限额 `count-1`） | 失败请求永久虚占当日钱包；部分回滚破坏守恒 |
| GW-INV-03 | 计费恰一次于首字节：流式 `br.Peek(1)` 成功后才 arm `outputStarted`，非流式 2xx header 后；输出已起 → 中途断连仍保计、永不 retry | 重试流双计费 / 故意断连逃费 |
| GW-INV-04 | Settle/Rollback/Reconcile 经 `ledger.settled IS NULL` 守卫互斥幂等（CAS：`settled=?` / `=0` / `=reserved`），`RowsAffected()==0` ⇒ no-op commit | 双退款 / 双补记，守恒破坏 |
| GW-INV-05 | `Period{Month,Day}` 请求入口快照一次并贯穿 Reserve/Settle/Rollback，settle/rollback **绝不重算** | 跨午夜并发结算落到错误 period_day 行 |
| GW-INV-06 | 崩溃只会 OVER-count（保守）：`settled IS NULL` 行由 `ReconcileOrphans(older=10m)` 退全额 + 标 `settled=reserved`；+1 月计数有意保留 | 无 reconcile 则崩溃预留长期钉死当日预算 |
| GW-INV-07 | 全局预算桶**按日** `budget.period='YYYY-MM-DD'`，日预算是唯一钱包护栏（`GLOBAL_DAILY_BUDGET_TOKENS>0`） | 月初尖峰烧光整月钱包 |
| GW-INV-08 | `est = estimatePromptTokens(messages) + clampMaxTokens(client, MaxTokensCap)`，保证 `est≥count×8` 且 `est≥真实 tokenizer`；per-message +8 防 OWASP-API4 多小消息放大 | 欠预留 → 静默超支 |
| GW-INV-09 | Settle 对齐真实用量（流式读 `total_tokens` final frame，非流式读 body；不可得则全额 `est` 兜底）；`delta=Reserved-actual` 调 `budget` + install 日 `usage` | 长期过收（永不退）/ 欠收（actual>est 不补） |
| GW-INV-10 | SEC-2 跨字段保证子配额有意义：`InstallDailyTokenCap>GlobalDailyBudget` 或 `InputTokenCap+MaxTokensCap>InstallDailyTokenCap` fail-fast | 单 install 抽干钱包 / 每日首请求恒被拒 |

## B. 安全

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-11 | DeepSeek key 绝不离开服务器：`Authorization` 注入在 `req.Clone()`、上游 body/header 绝不透传、`Recover` 绝不记 panic 值、key 事件仅按 `key_index` 审计 | key 经日志/panic dump/透传外泄 = 上游账户全失陷 |
| GW-INV-12 | install token 仅存 `SHA-256(token)`：发放返一次性 `gwk_`+32B token，存 `installs.token_sha256`（UNIQUE），`/v1/install` 永不重显旧 token（每次全新行 + 全新配额池，无 get-or-create、无 fp merge） | DB 泄露出可用 token / token 回显给错 client |
| GW-INV-13 | `/metrics` `/readyz` `/debug/pprof/*` `/debug/vars` LOOPBACK-only 绑 `ADMIN_ADDR`：`requireLoopback` 拒空 host、IP 字面判 `IsLoopback`、hostname 要求每个 `LookupIP` 均回环；`/healthz` liveness 绝不碰 DB | 公网 pprof = 远程 DoS + 内部结构泄露 |
| GW-INV-14 | 机密 env-only、绝不持久化/dump：三件套只读 env、绝不写 `settings`、`Snapshot()`/`Dump()` 掩码；`INSTALL_POW_SECRET` 绝不自动生成 | 机密经 config snapshot / 导出 / settings 行泄露 |
| GW-INV-15 | metric/audit 标签严格低基数：仅 `outcome` / `handler`（`install`/`install_challenge`/`chat_completions`/`quota`/`models`）/ `result`；绝无 `install_id`/`token`/`prompt`/`ip` 入标签 | Prometheus 基数爆炸 OOM / PII 入时序 |
| GW-INV-16 | XFF 仅当直连对端为回环（Caddy hop）时才信：取最右 XFF 段且 `isLoopback(RemoteAddr)`，否则回退 `RemoteAddr`（绝不信左/可伪造段） | 攻击者伪造左 XFF 绕过 per-IP Sybil 限速 |
| GW-INV-17 | redaction 底线抹掉每条日志每个属性的机密/PII：`redactKeys` 命中即 `[REDACTED]`，非 allowlist key 下的复合值（struct/map/slice）整体抹除 | `slog.Any` 嵌套机密静默序列化外泄 |
| GW-INV-18 | TLS 仅 Caddy 终结；Go 进程绑 `127.0.0.1`；三监听 `LISTEN_ADDR`/`ADMIN_ADDR`/`DASHBOARD_ADDR` 必须物理互异，否则 `LoadBase` 指名 fail-fast | 误绑公网端口 / 运行期端口冲突 |
| GW-INV-19 | dashboard 独立 loopback server，仅当 `DASHBOARD_USER`+`DASHBOARD_PASSWORD` 同设才启（半配 fail-fast）；bcrypt + 常时用户名比对 + `crypto/rand` session（rand 错则 panic 绝不弱值）+ `HttpOnly;Secure;SameSite=Strict` cookie（Secure 恒 true，除 dev-only flag）+ `X-CSRF-Token` + per-IP 登录退避（`LOGIN_LOCKED`+`Retry-After`）+ 除 `/healthz` 外全过 `requireSession` | session 劫持 / CSRF / 暴破；ban/export 被滥用 |
| GW-INV-20 | `/install`(+challenge) reject 用 DISTINCT wire code 审计分离 + 不采样 WARN `install_audit` 仅携 `ip_key`(/64) + `gate` + `error_code`，绝不携 fp 明文/机密；fp 仅存 `SHA-256` 于 `install_fp_rate.fp_sha256` | Sybil 洪流无法区分；raw fp 入日志/DB |

## C. 可靠性

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-21 | gateway→上游在飞 `≤ N_GLOBAL_CONCURRENCY` 且永不放大：固定容量 `sem`（热改不 resize、重启生效），多 key failover 只换用哪个 key、绝不加在飞槽位 | 超上游账户并发上限 / 带宽饱和 / 被上游封 |
| GW-INV-22 | 进程级 breaker **排除** client-cancel 与 429：`ConsecutiveFailures≥5` 或（`Requests≥10` 且失败率 `>0.5`）才跳，仅计真上游故障（5xx/timeout/connect），每 attempt-set 恰记一次失败；排队后 client cancel → `499 CLIENT_CANCELED` 全回滚、不计 busy/不计 breaker | breaker 因 client/上游 busy 误开，对健康上游断流 |
| GW-INV-23 | 上游 429 独立类 `UPSTREAM_BUSY`(429)：不 retry、不计 breaker、不误判为输出前错误；per-key transport 平 429 不跳 key（仅 `Retry-After>5s` 设 per-key cooldown） | 上游 429 retry 风暴 / breaker 误开 |
| GW-INV-24 | 关机最后关 DB（REL-4）：所有 detached settle/rollback + reconciler + prober + diskguard + metrics loop 由 `bgWG` 跟踪，顺序严格 ① `scanCancel()` ② `srv.Shutdown`+`adminSrv.Shutdown` ③ `waitWithTimeout(&bgWG,30s)` ④ `st.Close()`；`st.Close` 故意不 `defer` | 结算中关 DB → 账本损坏 / 预留永不结算 |
| GW-INV-25 | 流式用滚动 per-frame 写 deadline，绝无全局 `WriteTimeout`：每帧 `SetWriteDeadline(now+30s)`，`http.Server` 不设 `WriteTimeout`（仅 ReadHeader/Read/Idle） | 全局写超时截断长 LLM 流 |
| GW-INV-26 | retry 仅限 connect→首字节窗口（REL-1）：`{maxAttempts:3, base:200ms, cap:3s}` 指数退避 + full jitter，仅重试瞬态故障（connect/TLS reset/502·503·504，**非** 429），复用同一预留（est 不变），`outputStarted` 后绝不 retry | 已产出输出被重试 → 重复计费 + 重复字节 |
| GW-INV-27 | connect→header 阶段由 `UPSTREAM_HEADER_TIMEOUT_SEC`(默认 60) per-attempt 首字节计时（`time.AfterFunc`→`cancelUp`，每 attempt 从 `cfg.Load()` 读 LIVE），输出一起即停表（不盖流式 body） | 卡住上游钉死并发槽 + 永久虚占预算 |
| GW-INV-28 | REL-7 过载背压是有界等待非二元拒绝且永不放大：`acquireSlot` 快路径空槽否则等 `QUEUE_WAIT_MS`(默认 1500，0=二元拒绝)，超时→`429 UPSTREAM_BUSY`+回滚，client-cancel→不占槽放弃；sem 容量恒 `=N_global` | 尖峰硬拒 / 队列诱发并发放大 |
| GW-INV-29 | REL-6 低磁盘只读降级在任何预留**之前** shed：diskguard 每 30s 探数据盘，free `<DISK_MIN_MB`(默认 500) 或 `<DISK_MIN_PERCENT`(默认 5) 原子翻 `degraded`，proxy 预留前查 `h.degraded()` 返 `503 DISK_LOW`，启动同步预热，探测失败 FAIL-OPEN，恢复自动清；WAL 由 `wal_autocheckpoint`(4000) 限增长 | 磁盘满中途写损坏账本 / 探测抖动卡死只读 |
| GW-INV-30 | 多 key failover（REL-3）隔离 per-key 健康而不放大并发：`[]*keyState` 各带 gobreaker+cooldown，401/403→10min cooldown+换 key、持续 5xx→per-key breaker、平 429→非 key 故障，`pickKey` round-robin 健康 key，仅按 `key_index` 审计，单 key 配置行为不变 | 单坏 key 拖垮整上游 / key 轮换放大在飞并发 |

## D. 输入校验

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-31 | chat body 严格白名单：`decodeInbound` 仅解 `model`/`messages`/`stream`/`temperature`/`max_tokens`/`n`，`sanitizeUpstream` 仅转发 `model`(改写)/`messages`/`stream`/`temperature`/clamp 后 `max_tokens` + 网关强加 `stream_options.include_usage`，其余字段构造性丢弃 | client 注入上游字段（tools/logprobs/raw key 走私 / 成本放大） |
| GW-INV-32 | `n>1` 经 raw `json.Unmarshal` 探针 **和** 类型检查 `*in.N>1` 双重 → `400 BAD_REQUEST` | 单请求扇出 N 补全，成本翻倍 + 计费假设破坏 |
| GW-INV-33 | SEC-1 深度护栏（预留前、估算前）：`len(messages)>MAX_MESSAGES`(256) 或单条 `RuneCount>MAX_MESSAGE_CHARS`(131072) 或空 messages → `400`（`checkMessageShape`） | 消息数/单消息体积放大 DoS |
| GW-INV-34 | body 上限 256 KiB：中间件 `MaxBody(256*1024)` + proxy 防御性 `MaxBytesReader`，超限读错 → `400 BAD_REQUEST` | 无界 body 耗内存 / 放大成本 |
| GW-INV-35 | client `model` 强改写到白名单：成员则保留，否则 `DefaultModel`(=`ModelAllowlist[0]`)；用 `ServeHTTP` 内取一次的同一 `cfg.Load()` 快照（白名单热改不会单请求内半旧半新） | 调用未批准/昂贵模型 / 热改半旧半新 |
| GW-INV-36 | message `content` 仅 string：`{Role string; Content string}`，非 string（数组/对象）严格解码失败 → `400` | 多部分 content 绕过估算 / 走私 payload |
| GW-INV-37 | `max_tokens` clamp 到 `MaxTokensCap`(4096)：`client!=nil && *client>0 && *client<cap` 时取 client，否则 cap；clamp 值既转发又入 `est` | 无界输出 → 成本爆 + 欠预留 |

## E. 跨切配置 / 可观测

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-38 | `RESET_TZ` 内嵌（`import _ "time/tzdata"`）且 fail-fast：`LoadLocation` 失败 **PANIC**（绝无静默 UTC 回退），period 边界在 `cfg.Location` 算 | 所有 period 边界静默偏移（如 8h），误重置配额/预算 |
| GW-INV-39 | 每个 runtime 数值旋钮都有 floor + per-spec ceiling，env `Load` 与 overlay `ApplyOverrides` 路径**同一套**边界，多 key 单 tx 全或无持久化 + 越界指名 fail-fast；`N_GLOBAL_CONCURRENCY` 可改但容量重启才换 | 一次后台/settings 编辑使护栏失效或 OOM |
| GW-INV-40 | store 分池（`W` MaxOpenConns=1 + `_txlock=immediate`；有界读池 `READ_POOL_MAX_CONNS=4`），两 DSN 均 `journal_mode=WAL`/`foreign_keys=ON`/`synchronous=NORMAL` + PERF-2 pragma（`cache_size=-32768`KiB/conn、`mmap_size=256MiB`、`wal_autocheckpoint=4000`）；`Close` 幂等；PERF-2 自检：最坏 RSS = `GOMEMLIMIT + cache×(1+READ_POOL) + mmap` 超 `MEM_BUDGET_MIB − MEM_SAFETY_MARGIN_MIB` 则 fail-fast | 丢失写序列化破坏原子 reserve / 误算 cache×readpool OOM |

## 备注

- wire-code 契约逐字保留见 [error-codes.md](error-codes.md)（外加内部审计码 `CLIENT_CANCELED`(499)）。
- schema 前向 only、表清单与 `ledger.settled` 幂等支点见 [database.md](database.md)。
- 中间件链单一事实源（main + e2e 共用）：`Recover → DenyCORS → MaxBody(256KiB) → mux`；`DenyCORS` 对带 `Origin` 的 `OPTIONS` 返 403 且绝不发 `Access-Control-*`。
- Sybil/PoW dormancy（GW-INV-20 族）：M2 全闸默认 0=禁用、DB 工作前短路；`INSTALL_POW_MODE` 三态 `off`/`shadow`/`enforce`（零值≡off，逐字不变 `/install`），challenge 无状态 + 120s TTL + nonce-once；生效 shadow/enforce 须非空 env `INSTALL_POW_SECRET`（`CONFIG_POW_SECRET_REQUIRED` fail-fast）。
