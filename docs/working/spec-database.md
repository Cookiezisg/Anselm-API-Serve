---
id: DOC-023
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/database.md
---

# DB schema(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

# Anselm-API-Serve — Exact SQLite Schema Extraction (Spec for Rewrite)

Authoritative source files:
- `<repo>/internal/store/store.go` — the single `schema` const + connection-pool / DSN / PRAGMA logic + migration.
- `<repo>/internal/install/install.go` — install/Sybil row DML (no DDL here; tables defined in store.go).
- `<repo>/internal/quota/quota.go` — quota/budget/ledger row DML.
- `<repo>/internal/config/runtime.go` + `provider.go` — `settings` overlay DML.
- `<repo>/internal/config/config.go` + `cmd/server/main.go` + `cmd/server/admin.go` — Tuning → DSN binding.

**All DDL lives in exactly one place**: the `schema` string constant in `store.go` (lines 159–231), applied by `func migrate(db *sql.DB)` via a single `db.Exec(schema)` (lines 233–236). The only other `CREATE TABLE` in `*.go` is a test fixture (`internal/config/provider_test.go:24`, an inline `settings` table for unit tests) — not production DDL. There is no SQL `.sql` file, no embedded migration directory, no version/migrations table anywhere.

Driver: `github.com/glebarez/go-sqlite` (pure-Go, no CGO), registered as driver name `"sqlite"` (store.go:13, 86, 107).

---

## 1. Tables

### 1.1 `installs` — issued install identities (auth + dashboard)
Defined store.go:160–168. Each `POST /v1/install` inserts a brand-new row (no get-or-create, no fingerprint merge — install.go:206–213).

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `TEXT` | `PRIMARY KEY` | `ins_` + 8 random bytes hex (install.go:88–92) |
| `token_sha256` | `TEXT` | `NOT NULL UNIQUE` | hex SHA-256 of the opaque token (`gwk_` + 32B base64url); token itself never stored (install.go:71–100). Auth lookup key (install.go:605). |
| `fingerprint` | `TEXT` | nullable (NO UNIQUE) | Plaintext fp, risk-observation only. Comment is explicit: never a merge/dedup key; dedup keys go through hash (`install_fp_rate`). Truncated to 256 chars before insert (install.go:152–154). `NULL` when empty (`nullStr`, install.go:228–233). |
| `client` | `TEXT` | nullable | Truncated to 128 chars (install.go:155–157); `NULL` when empty. |
| `status` | `TEXT` | `NOT NULL DEFAULT 'active'` | Dashboard can flip status (queried at dashboard_test.go:714, installs.go:34). |
| `created_at` | `DATETIME` | `NOT NULL` | UTC at issuance (install.go:203, 210). |
| `last_seen_at` | `DATETIME` | nullable | Set = created_at at issuance; opportunistically refreshed on auth lookup, DB-throttled (`WHERE last_seen_at < now - lastSeenRefreshInterval`, interval = 10 min, install.go:589–631). |

**Indexes:** PK on `id` + implicit UNIQUE index on `token_sha256`. No explicit secondary index. (Dashboard list query orders by `created_at DESC, id DESC` with OFFSET pagination — installs.go:38 — using no dedicated index.)

### 1.2 `usage` — per-install monthly count + daily token/request usage
Defined store.go:170–176. Dual-purpose by `period` shape: month row (`YYYY-MM`) holds monthly request `count`; day row (`YYYY-MM-DD`) holds daily `tokens` (sub-cap) and optional daily request `count` (sublimit). Period strings computed in `cfg.Location` tz (quota.go:54–60).

| Column | Type | Constraints |
|---|---|---|
| `install_id` | `TEXT` | `NOT NULL` |
| `period` | `TEXT` | `NOT NULL` — `'YYYY-MM'` (monthly) or `'YYYY-MM-DD'` (daily) |
| `count` | `INTEGER` | `NOT NULL DEFAULT 0` — monthly request count, or daily sublimit count |
| `tokens` | `INTEGER` | `NOT NULL DEFAULT 0` — daily reserved tokens (install daily token sub-cap) |

**Indexes:** `PRIMARY KEY (install_id, period)` (composite). No secondary index.
**Usage:** `INSERT OR IGNORE` then conditional `UPDATE ... WHERE count < quota` / `WHERE tokens + est <= cap` (quota.go:94–140). Settle/rollback decrement (quota.go:218–222, 263–271).

### 1.3 `budget` — global daily token budget
Defined store.go:178–182. One row per day; the global daily budget guardrail.

| Column | Type | Constraints |
|---|---|---|
| `period` | `TEXT` | `PRIMARY KEY` — the day `'YYYY-MM-DD'` |
| `tokens_used` | `INTEGER` | `NOT NULL DEFAULT 0` |
| `requests` | `INTEGER` | `NOT NULL DEFAULT 0` |

**Indexes:** PK on `period`. No secondary index.
**Usage:** reserve `UPDATE ... WHERE tokens_used + est <= cap` (quota.go:148–157); settle/rollback/reconcile decrement (quota.go:213–217, 257–259, 419–424).

### 1.4 `ledger` — pessimistic reservation ledger (crash reconciliation)
Defined store.go:184–192. One row per request; `settled IS NULL` = in-flight reservation.

| Column | Type | Constraints |
|---|---|---|
| `request_id` | `TEXT` | `PRIMARY KEY` — `req_` + 8 random bytes hex (quota.go:62–66) |
| `install_id` | `TEXT` | `NOT NULL` |
| `period_day` | `TEXT` | `NOT NULL` — the reservation's day bucket |
| `reserved` | `INTEGER` | `NOT NULL` — est tokens reserved |
| `settled` | `INTEGER` | nullable — `NULL`=open; `=actual` on settle; `0` on rollback; `=reserved` on orphan reconcile |
| `created_at` | `DATETIME` | `NOT NULL` — UTC |

**Indexes:**
- PK on `request_id`.
- `CREATE INDEX IF NOT EXISTS idx_ledger_open ON ledger(settled, created_at);` (store.go:192) — serves the open-reservation count (`WHERE settled IS NULL`, quota.go:349) and orphan scan (`WHERE settled IS NULL AND created_at < ?`, quota.go:374–375).

**State machine:** insert with `settled = NULL` (quota.go:161–164). Settle: `UPDATE ledger SET settled = ? WHERE request_id = ? AND settled IS NULL` (quota.go:200–202). Rollback: `settled = 0` (quota.go:245–247). Orphan reconcile: `settled = reserved` for rows older than cutoff (quota.go:403–405). The `settled IS NULL` guard makes settle/rollback/reconcile mutually-exclusive-once (no double refund).

### 1.5 `install_ip_rate` — persisted per-IP hourly install rate bucket
Defined store.go:194–199. Survives restarts (not in-memory).

| Column | Type | Constraints |
|---|---|---|
| `ip_key` | `TEXT` | `NOT NULL` — `/64`-collapsed IPv6 or v4 string (install.go:104–119) |
| `window_hour` | `TEXT` | `NOT NULL` — `'2006-01-02T15'` in `cfg.Location` tz (install.go:398) |
| `count` | `INTEGER` | `NOT NULL DEFAULT 0` |

**Indexes:** `PRIMARY KEY (ip_key, window_hour)`. No secondary index.
**Usage:** `INSERT OR IGNORE` then `UPDATE ... SET count = count + 1 WHERE ... AND count < INSTALL_PER_IP_HOUR` (install.go:411–419). Shared by both `POST /v1/install` and `GET /v1/install/challenge`.

### 1.6 `install_global_rate` — M2 global daily install coarse valve
Defined store.go:207–210. Sybil gate; **disabled by default** (`INSTALL_GLOBAL_DAILY_CAP=0` short-circuits before any DB work, install.go:453–455). Comment marks it log-natured (no `deleted_at` — buckets are execution logs, never soft-deleted; single-account gateway so no `workspace_id`).

| Column | Type | Constraints |
|---|---|---|
| `window_day` | `TEXT` | `PRIMARY KEY` — `'YYYY-MM-DD'` in `cfg.Location` tz |
| `count` | `INTEGER` | `NOT NULL DEFAULT 0` |

**Indexes:** PK on `window_day`. No secondary index.
**Usage:** `INSERT OR IGNORE` + `UPDATE ... WHERE window_day = ? AND count < cap` (install.go:468–476). Read by dashboard `InstallsToday` (install.go:573–574); missing row reads as 0.

### 1.7 `install_fp_rate` — M2 per-fingerprint daily cap + cooldown
Defined store.go:214–220. Sybil gate; **disabled by default** (`INSTALL_PER_FP_DAILY=0` AND `INSTALL_PER_FP_COOLDOWN_SEC=0` short-circuit, install.go:504–506). Privacy red-line: stores ONLY SHA-256(fp), never plaintext.

| Column | Type | Constraints |
|---|---|---|
| `fp_sha256` | `TEXT` | `NOT NULL` — hex SHA-256 of fingerprint (`hashFP`, install.go:83–86) |
| `window_day` | `TEXT` | `NOT NULL` — `'YYYY-MM-DD'` in `cfg.Location` tz |
| `count` | `INTEGER` | `NOT NULL DEFAULT 0` |
| `last_at` | `DATETIME` | nullable — last issuance time, drives cooldown predicate |

**Indexes:** `PRIMARY KEY (fp_sha256, window_day)`. No secondary index.
**Usage:** single conditional UPSERT expressing BOTH daily cap and cooldown atomically (install.go:545–551):
```
INSERT INTO install_fp_rate(fp_sha256, window_day, count, last_at) VALUES (?, ?, 1, ?)
ON CONFLICT(fp_sha256, window_day) DO UPDATE
  SET count = count + 1, last_at = excluded.last_at
  WHERE install_fp_rate.count < ? AND install_fp_rate.last_at < ?
```
Disabled sub-gates pass sentinels: cap → `fpDailyCapDisabledSentinel = 1 << 62` (install.go:35); cooldown-off → cutoff `now.AddDate(1000,0,0)` (install.go:526). Empty fp is never keyed (always allowed, install.go:507–509).

### 1.8 `settings` — runtime-config DB overlay (hot-reload)
Defined store.go:226–230. Key-value store of ONLY dashboard-overridden runtime-tunable items. Secrets / startup hard-constraints (restart-only keys) never land here (`config.ApplyOverrides` rejects them; `validatePowSecretPresent` etc.).

| Column | Type | Constraints |
|---|---|---|
| `key` | `TEXT` | `PRIMARY KEY` |
| `value` | `TEXT` | `NOT NULL` |
| `updated_at` | `DATETIME` | `NOT NULL` — UTC |

**Indexes:** PK on `key`. No secondary index.
**Usage:** read-all overlay `SELECT key, value FROM settings` (provider.go:457); multi-key persist in one tx via UPSERT (runtime.go:140–145):
```
INSERT INTO settings(key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
```
Loaded at startup by `LoadWithOverlay`: env → settings overlay → re-validate semantics; a corrupt overlay fails fast (provider.go:400–419).

---

## 2. PRAGMAs / DSN parameters

Built by `func dsn(path string, immediate bool, t Tuning) string` (store.go:59–71). Every PRAGMA is injected **per-connection via the DSN** (`_pragma=...`), so both pools apply the identical set. The DSN template (store.go:60–66):

```
file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)
  &_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)
  &_pragma=cache_size(-<CacheKiB>)&_pragma=mmap_size(<MmapBytes>)
  &_pragma=wal_autocheckpoint(<WalAutocheckpoint>)
```
…and, **write pool only** (`immediate==true`), append `&_txlock=immediate` (store.go:67–69).

| PRAGMA / DSN param | Value | Source | Purpose |
|---|---|---|---|
| `journal_mode` | `WAL` | hardcoded | Concurrent readers during a write; durability mode (蓝图 §7.3). |
| `busy_timeout` | `5000` (ms) | hardcoded | Wait up to 5s on lock before SQLITE_BUSY. |
| `foreign_keys` | `ON` | hardcoded | FK enforcement (no FKs declared today, but enforced if added). Tested as reporting `1` on both pools (store_test.go:133). |
| `synchronous` | `NORMAL` | hardcoded | Standard WAL durability/throughput tradeoff. |
| `cache_size` | `-CacheKiB` (negative = KiB) | `Tuning.CacheKiB` ← `SQLITE_CACHE_KIB`, default **32768** (32 MiB/conn) | Per-connection page cache ceiling. Negative value forces KiB semantics (page-count would be unstable across page sizes). Reported back as `-32768` (store_test.go:47). |
| `mmap_size` | `MmapBytes` (bytes) | `Tuning.MmapBytes` ← `SQLITE_MMAP_MB`×1024×1024, default **256 MiB** (0=disable) | Memory-mapped read path saves a copy. |
| `wal_autocheckpoint` | `WalAutocheckpoint` (pages) | `Tuning.WalAutocheckpoint` ← `SQLITE_WAL_AUTOCHECKPOINT`, default **4000** | Bounds WAL growth (auto checkpoint trigger page count). |
| `_txlock=immediate` | write pool only | `dsn(path, true, t)` at store.go:86 | Every `BeginTx` grabs the write lock up front (BEGIN IMMEDIATE), so the read-modify-write reservation can't interleave (蓝图 §7.3). |

DefaultTuning (store.go:41–49, used by tests / `cmd/server/admin.go:146`): `CacheKiB:32768, MmapBytes:256MiB, WalAutocheckpoint:4000, ReadPoolMaxConns:4, ConnMaxLifetime:30m`.

Note for rewrite: **`busy_timeout`, `journal_mode`, `foreign_keys`, `synchronous` are hardcoded constants** (not config-driven); only `cache_size`/`mmap_size`/`wal_autocheckpoint`/read-pool-size/lifetime are tunable via env. `_txlock=immediate` is keyed on the `immediate bool` param, not a config flag.

---

## 3. Write pool (MaxOpenConns=1) vs Read pool (READ_POOL_MAX_CONNS) split

The `Store` struct holds two distinct `*sql.DB` pools over the same file (store.go:16–22): `W` (write) and `R` (read). Accessors `WriteDB()`/`ReadDB()` (store.go:144–145). Opened in `OpenWithTuning` (store.go:85–136).

**Write pool `W`** (store.go:86–105):
- DSN with `_txlock=immediate`.
- `w.SetMaxOpenConns(1)` — **single serialized writer**; serializes all mutations to avoid SQLITE_BUSY churn (蓝图 §6). Comment: "Single writer serializes all mutations." Tested `MaxOpenConnections == 1` (store_test.go:68, 146).
- `w.SetConnMaxLifetime(ConnMaxLifetime)` if > 0 (default 30 min) — periodic recycle to avoid pinning mmap/fd long-term.
- Pinged, then `migrate(w)` runs the schema (DDL only on the write pool).
- All BEGIN-IMMEDIATE transactions (reserve/settle/rollback/reconcile, all Sybil gates, settings persist) run on `st.W` exclusively.

**Read pool `R`** (store.go:107–133):
- DSN **without** `_txlock=immediate` (plain transactions).
- Bounded by `READ_POOL_MAX_CONNS` (env, default **4**, must be > 0 — config.go:468–472):
  - `r.SetMaxOpenConns(ReadPoolMaxConns)` (store.go:119). Tested = configured value (store_test.go:64); must NOT be 1 — WAL allows concurrent readers (store_test.go:151).
  - `r.SetMaxIdleConns(ReadPoolMaxConns / 2)`, floored at 1 (store.go:120–124).
  - `r.SetConnMaxLifetime(ConnMaxLifetime)` if > 0 (store.go:126–128).
- Rationale (store.go:112–117): a few concurrent readers saturate a 1-vCPU box; an unbounded pool would let a burst spawn hundreds of sqlite conns, each carrying its own per-connection `cache_size`+mmap budget → memory blowup.
- All read queries (`LookupInstall`, `View`, `RawBudget`, `OpenReservations`, `ReconcileOrphans` scan, dashboard lists, `InstallsToday`, settings overlay read) use `st.R`. **Exception:** `refreshLastSeen` (an opportunistic best-effort `UPDATE`) runs on `st.W` even though triggered during the read-path auth lookup (install.go:626–631).

**Memory-budget coupling** (config.go:569–608, `WorstCaseMemoryMiB`): `cache_size` is per-connection, so worst-case RSS = `GoMemLimitMiB + cacheMiB*(1 + ReadPoolMaxConns) + mmapMiB`. The `(1 + ReadPoolMaxConns)` multiplier = write pool's 1 conn + read pool's N conns. Default = `768 + 32*(1+4) + 256 = 1184 MiB`. Startup self-check (`validateMemoryBudget`) fails fast if this exceeds `MEM_BUDGET_MIB - MEM_SAFETY_MARGIN_MIB` (defaults 2048 − 400). This is exactly why the read pool MUST be bounded (`config_test.go` asserts `READ_POOL_MAX_CONNS=20` is rejected — config_test.go:512).

---

## 4. Migration model — and why the rewrite must replace it

**Current model (store.go:159–236):**
- A single Go string constant `schema` containing all `CREATE TABLE IF NOT EXISTS` + the one `CREATE INDEX IF NOT EXISTS idx_ledger_open`.
- `migrate(db)` is one `db.Exec(schema)` against the write pool, run once at every startup inside `OpenWithTuning`.
- **`IF NOT EXISTS` on every statement** = idempotent re-run; safe to execute on every boot.
- **Forward-only / backward-compatible by policy** (AGENTS.md:301–302): "DB migrate 必须 forward-only / 向后兼容 (只加列·加表;绝不删列·改类型·破坏性 NOT NULL)" — rollback swaps only the binary, never the schema, so the old binary must still run on the new schema.
- **NO version/migrations table.** There is no `schema_version`, no `migrations` ledger, no applied-migration tracking anywhere in the tree (only the schema TABLEs above exist). Migration "state" is implicit: whatever the union of `IF NOT EXISTS` statements produces.

**Known limitations this design carries (the rewrite should fix):**
1. **No version tracking** — the engine cannot know which migration a DB is at; it can only re-apply the whole idempotent blob. No detection of drift, no "pending migrations" concept, no fail-on-unknown-future-schema.
2. **`IF NOT EXISTS` hides additive changes silently** — adding a column to an existing table is *impossible* via this mechanism (`CREATE TABLE IF NOT EXISTS` no-ops on an existing table; the new column never appears). The current schema has never needed an `ALTER`, but the moment one is required there is no path — you'd need ad-hoc `ALTER TABLE` guarded by introspection. The AGENTS policy ("只加列·加表") is unenforceable with this tool.
3. **No down/rollback migrations, no transactional multi-step migration framework** — correctness depends entirely on hand-discipline and the `IF NOT EXISTS` clauses.
4. **No ordering / no checksums** — cannot detect a tampered or out-of-order migration history.

**Rewrite recommendation:** adopt a real migration framework (e.g. embedded numbered/`.sql` migrations applied via `golang-migrate`, `goose`, or `pressly/goose`-style with an explicit `schema_migrations` version table), preserving:
- The exact 8 tables + columns + constraints + the `idx_ledger_open(settled, created_at)` index above (byte-for-byte behavior).
- Forward-only / additive-only policy, but now *enforced and tracked* via a version table + checksum so old binaries running on new schema is a verified invariant rather than a hope.
- Migrations run only on the single-writer pool, before serving traffic — same as today's `migrate(w)` placement.

**Behavioral invariants the new migration output MUST satisfy (regression guards in `store_test.go`):**
- After open, tables `installs, usage, budget, ledger, install_ip_rate` (and per AGENTS.md:90–92 also `install_global_rate, install_fp_rate, settings`) all exist and are queryable on both pools (`TestOpenCreatesSchema`).
- Both pools report `journal_mode=wal` and `foreign_keys=1` (`TestPragmasWALAndForeignKeys`).
- Both pools report `mmap_size`, `wal_autocheckpoint`, and `cache_size=-CacheKiB` matching tuning (`TestTuningPragmasApplied`).
- Write pool `MaxOpenConnections==1`; read pool `==ReadPoolMaxConns` and never 1 (`TestReadPoolBounded`, `TestWritePoolSingleConn`).
- `Close()` is idempotent (`TestCloseIdempotent`); a bad path fails fast on open (`TestOpenBadPathFails`).

