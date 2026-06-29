---
id: DOC-006
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
audience: [human, ai]
---

# 配置全面（config）

> 与代码逐字对齐：层级 / apply / 边界来自 `internal/domain/config/spec.go` + `config.go`（`Max*` 常量、`ValidateSemantics`）；默认值 / env 解析来自 `internal/infra/configprovider/load.go`（`LoadBase`）。three-tier 见 ADR-006。

## 0. 三层级

| Tier | 含义 | 仪表盘 | 持久化 | 改法 |
|---|---|---|---|---|
| `TierRuntimeHot` | 后台可改、原子热生效 | 可编辑 | `settings` 表 overlay | 在线热改（全有或全无） |
| `TierStartupHard` | 启动硬约束（含 PERF-2 内存预算项 + 监听 / tz / DB 路径） | **只读** | env-only | 改需重启 |
| secret（不在 `Specs` registry） | 机密 | **不出现** | env-only | env + 重启 |

机密三件套 `DEEPSEEK_API_KEY` / `DASHBOARD_USER`·`DASHBOARD_PASSWORD` / `INSTALL_POW_SECRET` 故意**不在** `config.Specs()`，故永不被 apply、永不入库、永不在 Dump/Snapshot 出真值（GW-INV-14）。`applyOne` 对未知/机密 key 与 startup-hard key **指名拒绝**。

## 1. runtime-hot 项（`Specs()` 顺序，env 默认 + 边界）

边界列 `[Min, Max]` 是 `apply` 与 env-load **共用**的同一套天花板（防 OOM + 防「天文数字=护栏形同虚设」）。`Max*` 常量在 `config.go`。

| key | 默认 | Min | Max | Bounded | RestartReq | 说明 |
|---|---|---|---|---|---|---|
| `MODEL_ALLOWLIST` | （**必填**） | — | — | 否 | 否 | 逗号分隔；空报错；首项 = `DefaultModel`（GW-INV-35） |
| `GLOBAL_DAILY_BUDGET_TOKENS` | （**必填 >0**） | 1 | 1_000_000_000_000 | 是 | 否 | 唯一钱包护栏（GW-INV-07） |
| `INSTALL_DAILY_TOKEN_CAP` | （**必填 >0**） | 1 | 1_000_000_000_000 | 是 | 否 | 单 install 日 token 子配额 |
| `MONTHLY_QUOTA` | 5000 | 1 | 1_000_000_000 | 是 | 否 | 月度次数 |
| `MAX_TOKENS_CAP` | 4096 | 1 | 1_000_000 | 是 | 否 | 单请求输出 clamp 上限（GW-INV-37） |
| `INPUT_TOKEN_CAP` | 16384 | 1 | 10_000_000 | 是 | 否 | 单请求输入估算上限 |
| `MAX_MESSAGES` | 256 | 1 | 100_000 | 是 | 否 | messages 元素数上限（OWASP API4，GW-INV-33） |
| `MAX_MESSAGE_CHARS` | 131072 | 1 | 16_777_216 | 是 | 否 | 单条 content 字符数上限 |
| `N_GLOBAL_CONCURRENCY` | 8 | 1 | 100_000 | 是 | **是** | 全局在飞并发；信号量容量重启才换（GW-INV-21） |
| `RATE_PER_MIN` | 20 | 0 | 10_000_000 | 是 | 否 | per-install 分钟令牌桶 |
| `DAILY_SUBLIMIT` | 0 | 0 | 1_000_000_000 | 是 | 否 | per-install 日次数子限额；0=禁用 |
| `INSTALL_PER_IP_HOUR` | 10 | 1 | 1_000_000 | 是 | 否 | /install 单 IP 时频控 |
| `INSTALL_GLOBAL_DAILY_CAP` | 0 | 0 | 100_000_000 | 是 | 否 | 全局每日领号粗阀；0=禁用 |
| `INSTALL_PER_FP_DAILY` | 0 | 0 | 1_000_000 | 是 | 否 | 同 fp 当日领号上限；0=禁用 |
| `INSTALL_PER_FP_COOLDOWN_SEC` | 0 | 0 | 86_400 | 是 | 否 | 同 fp 相邻领号最小间隔秒；0=禁用 |
| `INSTALL_POW_MODE` | `off` | — | — | 否（enum） | 否 | `off`\|`shadow`\|`enforce`；非法值 fail-fast；生效≠off 须有 secret |
| `INSTALL_POW_DIFFICULTY` | 20 | 1 | 32 | 是 | 否 | 前导零 bit 数 |
| `TOKEN_ANOMALY_RPM` | 0 | 0 | 10_000_000 | 是 | 否 | per-install 异常 RPM 触发点；0=禁用整套自动降速 |
| `TOKEN_THROTTLE_FACTOR` | 4 | 1 | 1000 | 是 | 否 | 降速倍数=RATE_PER_MIN/此值；1=逃生口 |
| `TOKEN_THROTTLE_COOLDOWN_SEC` | 300 | 1 | 86_400 | 是 | 否 | 单次降速持续秒 |
| `QUEUE_WAIT_MS` | 1500 | 0 | 60_000 | 是 | 否 | N_global 满时有界等待窗口（REL-7，GW-INV-28）；0=binary reject |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | 60 | 1 | 600 | 是 | 否 | connect→header；不盖流式 body（GW-INV-27） |
| `DISK_MIN_MB` | 500 | 0 | 1_073_741_824 | 是 | 否 | 数据盘剩余绝对下限 MiB（REL-6，GW-INV-29） |
| `DISK_MIN_PERCENT` | 5 | 0 | 100 | 是 | 否 | 剩余百分比下限；0=禁用百分比判定 |

> 注：`Specs()` 的 `DISK_MIN_PERCENT` Min/Max 标 0/100；env-load 路径同样校验 0..100。`GLOBAL_DAILY_BUDGET_TOKENS` / `INSTALL_DAILY_TOKEN_CAP` 无 env 默认（缺失或 ≤0 即 fail-fast）。

## 2. startup-hard 项（仪表盘只读，env-only，改需重启）

| key | 默认 | env 约束 | 说明 |
|---|---|---|---|
| `GOMEMLIMIT_MIB` | 768 | ≥0（0=禁用） | `debug.SetMemoryLimit` 软上限；PERF-2 自检输入 |
| `SQLITE_CACHE_KIB` | 32768 | >0 | 每连接 page cache KiB |
| `READ_POOL_MAX_CONNS` | 4 | >0 | 只读池并发上限（每连接一份 cache） |
| `SQLITE_MMAP_MB` | 256 | ≥0（0=禁用） | mmap_size MiB（内部存 `SQLiteMmapBytes` 字节） |
| `SQLITE_WAL_AUTOCHECKPOINT` | 4000 | ≥0 | WAL 自动 checkpoint 触发页数 |
| `MEM_BUDGET_MIB` | 2048 | >0 | 总内存预算 |
| `MEM_SAFETY_MARGIN_MIB` | 400 | ≥0 | 为 OS/runtime 突发预留的余量下限 |
| `ADMIN_ADDR` | `127.0.0.1:9090` | 三监听互异 | /metrics 独立 admin 端口（loopback） |
| `DASHBOARD_ADDR` | `127.0.0.1:8081` | 三监听互异 + 必须 loopback | 管理后台独立 loopback 监听（`requireLoopback` 绑定 fail-fast，不上公网；运维经 SSH 隧道） |
| `LISTEN_ADDR` | `127.0.0.1:8080` | 三监听互异 | business 监听 |
| `RESET_TZ` | `Asia/Shanghai` | `LoadLocation` 失败 **PANIC** | period 边界时区；绝无静默 UTC 回退（GW-INV-38） |
| `GATEWAY_DB_PATH` | `anselm-gateway.db` | — | SQLite 落盘位置 |

非 registry 但 env-only 的 startup 项：`DEEPSEEK_BASE_URL`（默认 `https://api.deepseek.com`，去尾 `/`）、`LOG_LEVEL`（默认 `info`）、`DASHBOARD_DEV_INSECURE_COOKIE`（默认 false，仅 dev）。

## 3. 机密（secret-env-only，绝不入库 / 绝不 dump 真值）

| key | 约束 |
|---|---|
| `DEEPSEEK_API_KEY` | **必填**；逗号分隔多 key，首个为主；缺失 → `ErrDeepSeekKeyRequired` |
| `DASHBOARD_USER` / `DASHBOARD_PASSWORD` | 两者**同设或同空**（半配 fail-fast）；设了即启 dashboard |
| `INSTALL_POW_SECRET` | env-only；present→`configured`/absent→`disabled`，**绝不自动生成**；生效 mode≠off 时必须非空 |
| `WHITELIST_TOKEN_SHA256` | **运维白名单**（凭据派生，非机密但同纪律）：逗号分隔 64-hex `SHA-256(token)`；命中 = **god-mode 全风控绕过**（见 §7）；空=dormant；任一非 64-hex → fail-fast；`Snapshot` 只报 `N configured`，绝不出哈希，**不在** `Specs()`/`Dump`，改需重启 |

## 4. 跨字段语义（SEC-2，`ValidateSemantics`，env-load + overlay + 每次热改都跑）

1. `INSTALL_DAILY_TOKEN_CAP` ≤ `GLOBAL_DAILY_BUDGET_TOKENS`（否则单 install 能抽干全天钱包，子配额无意义）。
2. `INPUT_TOKEN_CAP` + `MAX_TOKENS_CAP` ≤ `INSTALL_DAILY_TOKEN_CAP`（否则单请求最坏预留恒超日子配额，当天首个请求即被拒，无调用能成功）。
3. 生效 `INSTALL_POW_MODE` ∈ {shadow, enforce} 必须有非空 `INSTALL_POW_SECRET`（`CONFIG_POW_SECRET_REQUIRED` fail-fast；env + 热改两路对齐）。

以上违反由 GW-INV-10 / GW-INV-39 守。

## 5. PERF-2 内存预算自检（`ValidateMemoryBudget`）

最坏 RSS = `GOMEMLIMIT_MIB` + cacheMiB × `(1 + READ_POOL_MAX_CONNS)` + mmapMiB（cache 是 per-connection：写池 1 份 + 读池 N 份；mmap 两池共享只记一次，故乘子是 `1+READ_POOL` 而非误算的 ×2）。三态：在 `MEM_BUDGET_MIB − MEM_SAFETY_MARGIN_MIB` 内 → 过；`GOMEMLIMIT_MIB=0`（堆无界）且超 → advisory WARN 放行；`GOMEMLIMIT_MIB>0` 且超 → fail-fast（`ErrMemoryBudget`，指名要调小的旋钮）。GW-INV-40。

## 6. 热改路径（`ApplyOverrides`）

纯函数、全有或全无：克隆 base → 按 key 排序逐项 `applyOne`（未知/机密/startup-hard 指名拒绝）→ 重跑 `ValidateSemantics`；任一失败返 `(Config{}, err)`，**绝不返回半生效配置**。infra `Provider` 在写锁下：domain 校验 → `settings` 表全或无持久化 → 原子 swap（持久化失败不 swap）。读路径 `Load()` 无锁取当前 atomic 快照（每请求快照一次，热更新永不在单请求内半旧半新）。

## 7. 运维白名单（`WHITELIST_TOKEN_SHA256`，god-mode 全风控绕过）

operator 把自己设备 token 的 `SHA-256` 填进 `WHITELIST_TOKEN_SHA256`（`printf %s '<token>' | shasum -a 256`），命中的请求**完全绕过全部风控**——空集时默认配置行为逐字不变（dormant 零成本，不命中不哈希）。

单一判定点 `app/install.Service.IsUnmetered(token)`（token 哈希 + 集合查；chat / quota / install handler 三处共用）。命中后：

- **聊天 `/v1/chat/completions`**：跳过分钟限速 + 异常降速；`Reserve` 收到 `unmetered=true` → `unmeteredLimits()` 把**月度次数 / 日 token / 全局每日钱包预算**三闸抬到 registry 上限（`config.Max*`）→ 永不被拒；**日次数子限额（`DailySublimit`）置 0 → 直接禁用 gate 2b**（不再 +1 日计数，`SublimitApplied` 恒 false，Rollback 一致）。`reserve→settle→rollback` saga + ledger **代码逐字不变**（unmetered 路径只是少跑 gate 2b 这一条 UPDATE），用量照常**记账**（GW-INV-01..06 完好，只是不再 DENY）。`banned` 仍先于 god-mode 生效。
- **配额 `/v1/quota`**：`available` 强制 `true`（client 预检不挡设备），`limit/used/remaining` 仍报真值。
- **领号 `/v1/install`**：可带现有白名单 token 作 bearer 复领；命中 → 跳过 PoW + 单IP/全局/单指纹全部 Sybil 闸（`IssueParams.Unmetered`）。仍是全新行 + 全新配额池（GW-INV-12 不变）。

🔴 代价（operator 显式选定）：

1. **god-mode 设备用量无任何上限**——死循环会真烧钱；务必保护好这个 token 别外泄。
2. **会拖垮他人**：god-mode 设备的花费**仍计入共享 `budget` 日预算表**（gate 3 的自增照跑，只是不再对它设限——saga 对称所需）。故一旦该设备失控把共享 `budget.tokens_used` 顶到 `GLOBAL_DAILY_BUDGET_TOKENS`，**当天所有普通用户都会 `BUDGET_EXHAUSTED` 被挡**（普通请求仍按真实预算闸）。即:god-mode 只让命中设备不被挡,**不等于他人风控不受影响**。

守 GW-INV-41。
