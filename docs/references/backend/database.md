---
id: DOC-005
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
audience: [human, ai]
---

# SQLite schema（database）

> 与 `internal/infra/sqlite/migrations/0001_init.sql` 逐字对齐：8 表 + `idx_ledger_open` + 迁移框架表 `schema_migrations`。schema **前向 only**（OPS-1），迁移器在 `schema_migrations` 记版本、每个迁移恰应用一次，故 DDL 无需 `IF NOT EXISTS` 守卫。记账不变量见 [invariants.md](invariants.md) A 组。

## 1. 业务表（`0001_init`，8 张）

### `installs` — 已发放的 install 身份
| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| `id` | TEXT | PRIMARY KEY | install id |
| `token_sha256` | TEXT | NOT NULL UNIQUE | 仅存 SHA-256(token)，绝不存明文（GW-INV-12） |
| `fingerprint` | TEXT | | 明文，**仅风险观测**，绝非 merge/dedup key（dedup 走 hash，见 `install_fp_rate`） |
| `client` | TEXT | | |
| `status` | TEXT | NOT NULL DEFAULT 'active' | active / banned |
| `created_at` | DATETIME | NOT NULL | |
| `last_seen_at` | DATETIME | | |

### `usage` — 按 period 形态双用途
| 列 | 类型 | 约束 |
|---|---|---|
| `install_id` | TEXT | NOT NULL |
| `period` | TEXT | NOT NULL |
| `count` | INTEGER | NOT NULL DEFAULT 0 |
| `tokens` | INTEGER | NOT NULL DEFAULT 0 |
| | | PRIMARY KEY (`install_id`, `period`) |

`period` 形态决定语义：`'YYYY-MM'` 存月度请求次数；`'YYYY-MM-DD'` 存当日已 reserve 的 token（+ 可选日子限额次数）。

### `budget` — 每日全局预算护栏（一天一行）
| 列 | 类型 | 约束 |
|---|---|---|
| `period` | TEXT | PRIMARY KEY（`'YYYY-MM-DD'`，**按日**非按月，GW-INV-07） |
| `tokens_used` | INTEGER | NOT NULL DEFAULT 0 |
| `requests` | INTEGER | NOT NULL DEFAULT 0 |

### `ledger` — 悲观预留账（`settled IS NULL` = 在飞）
| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| `request_id` | TEXT | PRIMARY KEY | |
| `install_id` | TEXT | NOT NULL | |
| `period_day` | TEXT | NOT NULL | |
| `reserved` | INTEGER | NOT NULL | 预留 token |
| `settled` | INTEGER | （nullable） | NULL=在飞；Settle/Rollback/Reconcile 的幂等支点（GW-INV-04/06） |
| `created_at` | DATETIME | NOT NULL | |

`CREATE INDEX idx_ledger_open ON ledger(settled, created_at);` — 服务 open-reservation 计数（`settled IS NULL`）+ 孤儿扫描（`… AND created_at < ?`）。

### `install_ip_rate` — per-IP 每小时领号桶（持久化、跨重启）
| 列 | 类型 | 约束 |
|---|---|---|
| `ip_key` | TEXT | NOT NULL |
| `window_hour` | TEXT | NOT NULL |
| `count` | INTEGER | NOT NULL DEFAULT 0 |
| | | PRIMARY KEY (`ip_key`, `window_hour`) |

### `install_global_rate` — 全局每日领号粗阀（Sybil 闸，默认禁用）
| 列 | 类型 | 约束 |
|---|---|---|
| `window_day` | TEXT | PRIMARY KEY（reset-tz 日历日） |
| `count` | INTEGER | NOT NULL DEFAULT 0 |

### `install_fp_rate` — per-fingerprint 日上限 + 冷却（Sybil 闸，默认禁用）
| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| `fp_sha256` | TEXT | NOT NULL | 隐私红线：**仅** SHA-256(fp)，绝不存明文（GW-INV-20） |
| `window_day` | TEXT | NOT NULL | |
| `count` | INTEGER | NOT NULL DEFAULT 0 | |
| `last_at` | DATETIME | | 冷却判定 |
| | | PRIMARY KEY (`fp_sha256`, `window_day`) | |

### `settings` — runtime-config DB overlay（热改）
| 列 | 类型 | 约束 |
|---|---|---|
| `key` | TEXT | PRIMARY KEY |
| `value` | TEXT | NOT NULL |
| `updated_at` | DATETIME | NOT NULL |

仅存被仪表盘覆盖的 runtime-hot 项 K-V；**机密绝不入此表**（env-only，见 [config.md](config.md)）。

## 2. 迁移框架

`schema_migrations` 记已应用版本；`0001_init` 为初始 schema。前向 only、无回滚 DDL。连接形态（写池 `MaxOpenConns=1` 单写者 + 有界只读池、WAL、PERF-2 pragma）见 [invariants.md](invariants.md) GW-INV-40。

## 3. 跨表记账关系（速查）

| 关注 | 表/列 |
|---|---|
| 月度次数闸 | `usage.count`（`period='YYYY-MM'`） < `MONTHLY_QUOTA` |
| install 日 token 子配额 | `usage.tokens`（`period='YYYY-MM-DD'`） + est ≤ `INSTALL_DAILY_TOKEN_CAP` |
| 全局日预算 | `budget.tokens_used` + est ≤ `GLOBAL_DAILY_BUDGET_TOKENS` |
| 在飞预留 / 孤儿回收 | `ledger.settled IS NULL` + `idx_ledger_open` |
| 持久 Sybil key | `install_fp_rate.fp_sha256`（绝非 `installs.fingerprint`） |
