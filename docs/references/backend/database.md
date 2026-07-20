---
id: DOC-005
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
---

# SQLite schema（database）

> 与 `internal/infra/sqlite/migrations/{0001_init,0002_provider_spend_ledger}.sql` 对齐。schema forward-only；`schema_migrations` 记录 `version`、`applied_at`、`checksum`。v2 以后 quota store 只写 provider-aware pUSD 表；v1 accounting 表保留为只读审计/迁移来源，不再参与当前记账。

## 1. 当前仍活跃的 identity / abuse / config 表（0001）

### `installs`

| 列 | 类型 | 约束 / 语义 |
|---|---|---|
| `id` | TEXT | PRIMARY KEY |
| `token_sha256` | TEXT | NOT NULL UNIQUE；只存 token hash |
| `fingerprint` | TEXT | nullable；风险观测，不是 merge/dedup key |
| `client` | TEXT | nullable |
| `status` | TEXT | NOT NULL DEFAULT `active` |
| `created_at` | DATETIME | NOT NULL |
| `last_seen_at` | DATETIME | nullable |

### `/install` 持久限速表

| 表 | 列 / 主键 | 语义 |
|---|---|---|
| `install_ip_rate` | `ip_key TEXT`, `window_hour TEXT`, `count INTEGER DEFAULT 0`; PK `(ip_key,window_hour)` | per-IP 小时领号桶 |
| `install_global_rate` | `window_day TEXT` PK, `count INTEGER DEFAULT 0` | 全局日领号粗阀 |
| `install_fp_rate` | `fp_sha256 TEXT`, `window_day TEXT`, `count INTEGER DEFAULT 0`, `last_at DATETIME`; PK `(fp_sha256,window_day)` | per-fp 日 cap + cooldown；只存 SHA-256(fp) |

### `settings`

| 列 | 类型 | 约束 |
|---|---|---|
| `key` | TEXT | PRIMARY KEY |
| `value` | TEXT | NOT NULL |
| `updated_at` | DATETIME | NOT NULL |

只存 dashboard override 的 runtime-hot K/V；secret/startup-hard 不得入表。multi-key apply 在单 tx 全有或全无。

## 2. v2 provider-aware accounting（0002）

余额单位均为非负整数 pUSD（`1 USD=10^12 pUSD`）。`provider` 是 DB 级闭集 `deepseek|kimi`。

### `quota_monthly` — 月请求额度

| 列 | 类型 | 约束 |
|---|---|---|
| `install_id` | TEXT | NOT NULL |
| `period_month` | TEXT | NOT NULL，`YYYY-MM` |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| | | PRIMARY KEY `(install_id,period_month)` |

### `install_spend_daily` — per-install 日钱包 + 可选日次数

| 列 | 类型 | 约束 |
|---|---|---|
| `install_id` | TEXT | NOT NULL |
| `period_day` | TEXT | NOT NULL，`YYYY-MM-DD` |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0；只在 `DAILY_SUBLIMIT>0` 的 reserve 上 +1 |
| | | PRIMARY KEY `(install_id,period_day)` |

### `provider_spend_daily` — provider 日钱包

| 列 | 类型 | 约束 |
|---|---|---|
| `provider` | TEXT | NOT NULL CHECK IN (`deepseek`,`kimi`) |
| `period_day` | TEXT | NOT NULL |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| | | PRIMARY KEY `(provider,period_day)` |

### `global_spend_daily` — shared 日钱包

| 列 | 类型 | 约束 |
|---|---|---|
| `period_day` | TEXT | PRIMARY KEY |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |

### `spend_ledger` — provider/rate-card 冻结预留

| 列 | 类型 | 约束 / 语义 |
|---|---|---|
| `request_id` | TEXT | PRIMARY KEY |
| `install_id` | TEXT | NOT NULL |
| `provider` | TEXT | NOT NULL CHECK IN (`deepseek`,`kimi`) |
| `model` | TEXT | NOT NULL；实际 provider model，不是 client alias |
| `rate_card_id` | TEXT | NOT NULL；版本化价格快照 id |
| `period_month` | TEXT | NOT NULL |
| `period_day` | TEXT | NOT NULL |
| `reserved_pusd` | INTEGER | NOT NULL CHECK >0 |
| `charged_pusd` | INTEGER | nullable CHECK ≥0；settle 可大于 reserved（truthful top-up） |
| `state` | TEXT | NOT NULL CHECK IN (`open`,`settled`,`rolled_back`,`orphaned`) |
| `sublimit_applied` | INTEGER | NOT NULL DEFAULT 0 CHECK IN (0,1) |
| `created_at` | DATETIME | NOT NULL |
| `terminal_at` | DATETIME | nullable |

行级 CHECK 钉死状态/字段组合：

| state | `charged_pusd` | `terminal_at` | 余额动作 |
|---|---:|---|---|
| `open` | NULL | NULL | reservation 已在三 pUSD 钱包 + 月额度中 |
| `settled` | 非 NULL | 非 NULL | 三钱包按 `reserved−charged` refund 或 top-up；月/request count 保留 |
| `rolled_back` | 0 | 非 NULL | 仅 `ChargeExposure=DefinitelyUnbilled`；精确反转月额度、三钱包、provider/global requests 及实际应用的 sublimit count |
| `orphaned` | `reserved_pusd` | 非 NULL | 不动余额/requests；未知 provider 结果按 full quote 收口 |

索引：

- `idx_spend_ledger_open(state,created_at)`：open count + aged orphan scan；
- `idx_spend_ledger_period_provider(period_day,provider)`：按日/provider 审计。

## 3. 原子 Reserve / Settle / Rollback

`quotastore` 在单写池的一个 `BEGIN IMMEDIATE` 内按顺序：

1. lazy upsert + conditional increment `quota_monthly`；
2. lazy upsert + conditional add `install_spend_daily.spend_pusd`，需要时 conditional `requests+1`；
3. lazy upsert + conditional add provider spend、`requests+1`；
4. lazy upsert + conditional add global spend、`requests+1`；
5. insert 与冻结 `billing.Plan` 一致的 `spend_ledger(open)`。

任一条件未命中或 insert 失败，整个事务回滚。Settle/Rollback 先 CAS `state='open'`，CAS 胜者才调整余额；调整用 exact-one row + underflow/overflow guard，禁止 `MAX(0,…)` 隐藏守恒错误。Settle top-up 不受现有 cap 限制，因为费用已经发生；超 cap 余额会阻止后续 reserve。

`CallFailure.APIError` 不参与状态选择：只有独立 `ChargeExposure=DefinitelyUnbilled` 调 Rollback；`ChargePossible` 以 full reservation 调 Settle。`ReconcileOrphans(cutoff)` 先从读池枚举 aged open id，再逐行在写 tx 里 CAS 为 `orphaned`。它不退款：网络/进程崩溃无法证明 provider 未收费。

## 4. 0002 从 v1 迁移

### 4.1 v1 accounting 表（迁移后只读保留）

| 表 | 列 |
|---|---|
| `usage` | `install_id TEXT`, `period TEXT`, `count INTEGER DEFAULT 0`, `tokens INTEGER DEFAULT 0`; PK `(install_id,period)` |
| `budget` | `period TEXT` PK, `tokens_used INTEGER DEFAULT 0`, `requests INTEGER DEFAULT 0` |
| `ledger` | `request_id TEXT` PK, `install_id TEXT`, `period_day TEXT`, `reserved INTEGER`, `settled INTEGER NULL`, `created_at DATETIME` |

旧索引 `idx_ledger_open(settled,created_at)` 同样保留。当前 quota store 不写这三表；新 open gauge 读取 `spend_ledger.state='open'`。

### 4.2 保守数据换算

迁移先用 guard 拒绝非整数、负数、单值或按日聚合后的乘法溢出，再按历史 DeepSeek 最高价格维度 `280000 pUSD/token` 换算：

- 月 `usage.count` → `quota_monthly.requests`；
- 日 `usage.tokens` → `install_spend_daily.spend_pusd`，日 `count` 原样保留；
- `budget` → `provider_spend_daily(provider='deepseek')` + `global_spend_daily`；
- `ledger` → `spend_ledger`，model/rate card 标成 `legacy-deepseek` / `legacy-v1-max-280000-pusd`；旧 NULL 仍迁为 open，其余迁为 settled/rolled_back。

v1 orphan reconciler 曾用 `settled=reserved` 表示未知外部结果，却同时从 `usage`/`budget` 退掉预留；该编码与真实 full-cost settlement 无法区分。为避免迁移后低估 provider 可能已经收取的费用，0002 还会从 legacy `ledger` 构造 chargeable token：`settled IS NULL` 取 `reserved`、`settled>0` 取 `settled`、`settled=0` 排除。然后按 install/day 与 day 聚合，并把 install、DeepSeek provider、global 三钱包分别设为 `max(v1 copied balance, ledger aggregate×280000)`；缺失的钱包行会补建。这里用 `max` 而不是相加，因此正常情况下已包含在 v1 余额里的请求不会双计。provider/global `requests` 同样取 copied count 与 chargeable ledger count 的较大值；install `requests` 仍只保留 v1 日子限次数，无法从 legacy ledger 臆造。

旧 runtime settings 以 `ceil(tokens×280000/10^6)=ceil(tokens×28/100)` 转成整数 microUSD：

- `GLOBAL_DAILY_BUDGET_TOKENS` → `GLOBAL_DAILY_SPEND_MICRO_USD`；
- `INSTALL_DAILY_TOKEN_CAP` → `INSTALL_DAILY_SPEND_MICRO_USD`。

随后删除旧 token 键与 `MODEL_ALLOWLIST`；若新键已存在，`INSERT OR IGNORE` 保留新值。

## 5. 迁移框架与连接纪律

`schema_migrations`：`version INTEGER PRIMARY KEY`、`applied_at DATETIME NOT NULL`、`checksum TEXT NOT NULL`。runner 拒绝未知未来 version 与已应用 checksum drift；每份 SQL 和记录行在同一个 tx。

`0001_init` 使用 `IF NOT EXISTS` 只为 baseline 既有 unversioned DB；后续迁移是有版本、checksum 的 forward-only DDL。写池 `MaxOpenConns=1` + `_txlock=immediate`，读池有界；两 DSN 均启 WAL、foreign keys、synchronous NORMAL 与统一 PERF-2 pragmas。连接/内存不变量见 [invariants.md](invariants.md) GW-INV-40。
