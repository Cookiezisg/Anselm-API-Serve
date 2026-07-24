---
id: DOC-005
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-24
review-due: 2026-10-22
audience: [human, ai]
---

# SQLite schema（database）

> 与 `internal/infra/sqlite/migrations/*.sql` 对齐。schema forward-only；`schema_migrations` 记录 `version`、`applied_at`、`checksum`。v2 以后 quota store 只写 provider-aware pUSD 表；v1 accounting 表保留为只读审计/迁移来源，不再参与当前记账。

## 1. 当前仍活跃的 identity / abuse / config 表（0001）

### `installs`

| 列 | 类型 | 约束 / 语义 |
|---|---|---|
| `id` | TEXT | PRIMARY KEY |
| `public_key` | BLOB | NOT NULL；32-byte Ed25519 public key |
| `key_thumbprint` | TEXT | NOT NULL UNIQUE；`base64url(SHA-256(public_key))`，registration 幂等键 |
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

余额单位均为非负整数 pUSD（`1 USD=10^12 pUSD`）。`provider` 是 DB 级闭集 `deepseek|gemini|qwen`；Gemini 仅为 v1 数据迁移身份，运行时仅 DeepSeek/Qwen。

### `quota_monthly` — 月请求额度

| 列 | 类型 | 约束 |
|---|---|---|
| `install_id` | TEXT | NOT NULL |
| `period_month` | TEXT | NOT NULL，`YYYY-MM` |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| | | PRIMARY KEY `(install_id,period_month)` |

### `install_spend_daily` — per-install 日花费统计 + 可选日次数

| 列 | 类型 | 约束 |
|---|---|---|
| `install_id` | TEXT | NOT NULL |
| `period_day` | TEXT | NOT NULL，`YYYY-MM-DD` |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0；只在 `DAILY_SUBLIMIT>0` 的 reserve 上 +1 |
| | | PRIMARY KEY `(install_id,period_day)` |

### `provider_spend_daily` — provider 日花费统计

| 列 | 类型 | 约束 |
|---|---|---|
| `provider` | TEXT | NOT NULL CHECK IN (`deepseek`,`gemini`,`qwen`) |
| `period_day` | TEXT | NOT NULL |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| | | PRIMARY KEY `(provider,period_day)` |

### `global_spend_daily` — shared 日花费统计

| 列 | 类型 | 约束 |
|---|---|---|
| `period_day` | TEXT | PRIMARY KEY |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |

### `global_spend_monthly` — operator 全局月钱包

| 列 | 类型 | 约束 |
|---|---|---|
| `period_month` | TEXT | PRIMARY KEY，`YYYY-MM` |
| `spend_pusd` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 CHECK ≥0 |

### `spend_ledger` — provider/rate-card 冻结预留

| 列 | 类型 | 约束 / 语义 |
|---|---|---|
| `request_id` | TEXT | PRIMARY KEY |
| `install_id` | TEXT | NOT NULL |
| `provider` | TEXT | NOT NULL CHECK IN (`deepseek`,`gemini`,`qwen`) |
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
| `open` | NULL | NULL | reservation 已在 install 月额度 + global 月钱包 + 日统计中 |
| `settled` | 非 NULL | 非 NULL | global 月钱包与日统计按 `reserved−charged` refund 或 top-up；月/request count 保留 |
| `rolled_back` | 0 | 非 NULL | 仅 `ChargeExposure=DefinitelyUnbilled`；精确反转 install 月额度、global 月钱包、日统计 requests 及实际应用的 sublimit count |
| `orphaned` | `reserved_pusd` | 非 NULL | 不动余额/requests；未知 provider 结果按 full quote 收口 |

索引：

- `idx_spend_ledger_open(state,created_at)`：open count + aged orphan scan；
- `idx_spend_ledger_period_provider(period_day,provider)`：按日/provider 审计。

## 3. 原子 Reserve / Settle / Rollback

`quotastore` 在单写池的一个 `BEGIN IMMEDIATE` 内按顺序：

1. lazy upsert + conditional increment `quota_monthly`；
2. lazy upsert + guarded add `install_spend_daily.spend_pusd`，需要时 conditional `requests+1`（`DAILY_SUBLIMIT=0` 时不拦）；
3. lazy upsert + guarded add provider daily spend、`requests+1`（统计，不按 provider cap 拦）；
4. lazy upsert + guarded add global daily spend、`requests+1`（统计，不按 day cap 拦）；
5. lazy upsert + conditional add `global_spend_monthly`、`requests+1`，要求 `spend_pusd+reserved≤GLOBAL_MONTHLY_SPEND_MICRO_USD`；
6. insert 与冻结 `billing.Plan` 一致的 `spend_ledger(open)`。

任一条件未命中或 insert 失败，整个事务回滚。Settle/Rollback 先 CAS `state='open'`，CAS 胜者才调整余额；调整用 exact-one row + underflow/overflow guard，禁止 `MAX(0,…)` 隐藏守恒错误。Settle top-up 不受现有 cap 限制，因为费用已经发生；超 cap 余额会阻止后续 reserve。

`CallFailure.APIError` 不参与状态选择：只有独立 `ChargeExposure=DefinitelyUnbilled` 调 Rollback；`ChargePossible` 以 full reservation 调 Settle。`ReconcileOrphans(cutoff)` 先从读池枚举 aged open id，再逐行在写 tx 里 CAS 为 `orphaned`。它不退款：网络/进程崩溃无法证明 provider 未收费。

dashboard 的全员月请求额度重置也在同一写池事务：先检查全库 `spend_ledger(open)` 为零，才将当前月所有正的 `quota_monthly.requests` 置零。它不写 `global_spend_monthly`、三张日统计表或 `spend_ledger`；因此是权益周期操作而非成本账务修订。

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

v1 orphan reconciler 曾用 `settled=reserved` 表示未知外部结果，却同时从 `usage`/`budget` 退掉预留；该编码与真实 full-cost settlement 无法区分。为避免迁移后低估 provider 可能已经收取的费用，0002 还会从 legacy `ledger` 构造 chargeable token：`settled IS NULL` 取 `reserved`、`settled>0` 取 `settled`、`settled=0` 排除。然后按 install/day 与 day 聚合，并把 install、DeepSeek provider、global 三张日统计表分别设为 `max(v1 copied balance, ledger aggregate×280000)`；缺失行会补建。这里用 `max` 而不是相加，因此正常情况下已包含在 v1 余额里的请求不会双计。provider/global `requests` 同样取 copied count 与 chargeable ledger count 的较大值；install `requests` 仍只保留 v1 日子限次数，无法从 legacy ledger 臆造。

0002 中旧 runtime settings 曾以 `ceil(tokens×280000/10^6)=ceil(tokens×28/100)` 转成整数 microUSD：

- `GLOBAL_DAILY_BUDGET_TOKENS` → `GLOBAL_DAILY_SPEND_MICRO_USD`；
- `INSTALL_DAILY_TOKEN_CAP` → `INSTALL_DAILY_SPEND_MICRO_USD`。

随后删除旧 token 键与 `MODEL_ALLOWLIST`；若新键已存在，`INSERT OR IGNORE` 保留新值。0004 再把 `global_spend_daily` 按 `period_month` 聚合进 `global_spend_monthly`，并删除 retired `GLOBAL_DAILY_SPEND_MICRO_USD` / `INSTALL_DAILY_SPEND_MICRO_USD` / `DEEPSEEK_DAILY_SPEND_MICRO_USD` / `QWEN_DAILY_SPEND_MICRO_USD` settings。

## 5. 媒体 staging 与 lease（0005）

媒体原件/代理字节不进入 SQLite。`media_uploads` 只记录 install 绑定的分块上传状态、预声明 SHA-256、
MIME、总/已收字节和到期时间；`media_leases` 是完成后的唯一 completion capability，拥有独立高熵 id 与
HMAC 从 lease/install/expiry 确定性派生、只存哈希的 provider-fetch token。二者均以状态/到期索引支持启动和定期回收；`upload_id UNIQUE` 禁止一个
staging 对象被签发为多个 lease，lease id、文件路径、SHA 均不能互相推导。

| 表 | 状态闭集 | 关键唯一性 / 索引 |
|---|---|---|
| `media_uploads` | `open`,`completed`,`aborted`,`expired` | `idx_media_uploads_expiry(state,expires_at)`；`idx_media_uploads_install(install_id,created_at)` |
| `media_leases` | `active`,`expired`,`deleted` | `upload_id UNIQUE`、`fetch_token_hash UNIQUE`；expiry/install 索引 |

`received_bytes` 是进度，不是完成证明；完成时必须从暂存文件重算 SHA-256 后才能转 `completed` 并创建 lease。
过期/删除必须先撤销 DB capability，再删除文件、最后 acknowledge；崩溃恢复会截去 fsync 后但 cursor 未推进的尾部，绝不因无法证明成功而把媒体跨 install 复用。

## 6. 迁移框架与连接纪律

`schema_migrations`：`version INTEGER PRIMARY KEY`、`applied_at DATETIME NOT NULL`、`checksum TEXT NOT NULL`。runner 拒绝未知未来 version 与已应用 checksum drift；每份 SQL 和记录行在同一个 tx。

`0001_init` 使用 `IF NOT EXISTS` 只为 baseline 既有 unversioned DB；后续迁移是有版本、checksum 的 forward-only DDL。写池 `MaxOpenConns=1` + `_txlock=immediate`，读池有界；两 DSN 均启 WAL、foreign keys、synchronous NORMAL 与统一 PERF-2 pragmas。连接/内存不变量见 [invariants.md](invariants.md) GW-INV-40。
