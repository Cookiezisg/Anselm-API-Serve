---
id: DOC-014
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0005 — SQLite 读写分离池 + 版本化迁移框架

## 背景 / Context

网关单 SQLite 文件落盘。两个独立需求：

1. **写串行化**是 [ADR 0001](0001-pessimistic-three-guardrail-reservation.md) 原子三闸预占的物理前提——`BEGIN IMMEDIATE` 必须在单写连接上排队，否则 `SQLITE_BUSY` churn 或丢失写序列化即破坏预占原子性。但只读路径（dashboard 查询、readyz）不该被单写连卡死。
2. 旧 schema 是**单个 `schema` const + `db.Exec(schema)`**、用 `IF NOT EXISTS` 幂等、**无版本表**。这是净新增能力的缺口：一旦需要演进列/索引，幂等 blob 无法表达「第 N 版到第 N+1 版做什么」，也无法校验已落库 schema 与代码期望是否一致。

## 决策 / Decision

**读写分离双池 + 编号 `.sql` 迁移框架 + `schema_migrations` 版本表；逐字保留现有 8 表 + `idx_ledger_open`。**

1. **写池**：`MaxOpenConns=1` + DSN `_txlock=immediate`（GW-INV-40）。
2. **读池**：`READ_POOL_MAX_CONNS`（默认 4）有界。
3. **两 DSN 共同 PRAGMA**：`journal_mode=WAL`、`foreign_keys=ON`、`synchronous=NORMAL` + PERF-2（`cache_size=-32768` KiB/conn、`mmap_size=256MiB`、`wal_autocheckpoint=4000`）。`Close` 幂等。
4. **内存预算自检 fail-fast**：启动期 worst-case RSS = `GOMEMLIMIT + cache×(1+READ_POOL) + mmap`，超 `MEM_BUDGET_MIB − MEM_SAFETY_MARGIN_MIB`（默认 1184 < 2048−400）即 fail-fast（GW-INV-40）。
5. **版本化迁移**：内嵌编号 `.sql`（`infra/sqlite/migrations/`，`go:embed`）+ `schema_migrations` 表（forward-only、checksum 跟踪），在**写连接上、对外服务前**跑完（OPS-1）。
6. **保留 8 表**：`installs`、`usage`、`budget`、`ledger`(+`idx_ledger_open`)、`install_ip_rate`、`install_global_rate`、`install_fp_rate`、`settings`。`ledger.settled` 为 nullable INTEGER、是 GW-INV-04/06 的幂等枢轴；`installs.fingerprint` 存明文仅供 MVP 观测（无 UNIQUE、绝非合并键）。

## 理由 / Rationale

- 单写池是预占原子性的地基（[ADR 0001](0001-pessimistic-three-guardrail-reservation.md)）；读池让查询不饿死、又被 `READ_POOL_MAX_CONNS` 上界保护每连 cache 的内存乘数不爆 2G 机。
- forward-only + checksum 的迁移是单文件 SQLite 演进的标准答案：可表达逐版变更、可校验落库一致、可在服务前一次跑完；幂等 blob 三者皆不能。

## 取舍与后果 / Consequences

**为何不选：**

- **保留单 `schema` blob + `IF NOT EXISTS`**：无法表达版本演进、无法校验 schema 一致——净缺口。
- **单池（读写共用）**：读查询与单写连争用，dashboard/readyz 会被预占事务卡死。
- **第三方迁移库（goose/migrate）**：薄网关引依赖不值；内嵌编号 `.sql` + 一张版本表足矣。

**后果（须明确）：**

- 迁移框架是**净新增**（非旧代码抽取）——第一版迁移须把现有 8 表的建表语句迁入编号文件、并落 `schema_migrations` 初始版本。
- `infra/sqlite` 持池开启 + DSN/PRAGMA + 迁移 runner；`infra/store/*` 在两池上写 DML。
- 回归：`TestWritePoolSingleConn`、`TestPragmasWALAndForeignKeys`、`TestReadPoolBounded`、`TestCloseIdempotent`、`TestMemoryBudget*`。

## 相关 / Links

- [ADR 0001 三闸预占](0001-pessimistic-three-guardrail-reservation.md) · [架构](../concepts/architecture.md)
- 不变量：GW-INV-40（双池 + PRAGMA + 内存预算）、GW-INV-01（单写池支撑原子预占）
