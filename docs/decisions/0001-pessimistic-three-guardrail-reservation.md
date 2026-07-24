---
id: DOC-010
type: decision
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2099-12-31
audience: [human, ai]
superseded-by: DOC-034 (0017-qwen-visual-route-and-tiered-cost-ledger.md)
---

# 0001 — 悲观三闸预占记账（一事务预占、按实结算、崩溃永远多扣）

## 背景 / Context

网关替运营者花真金白银调用 DeepSeek。计费必须在**并发尖峰**下绝不超卖、绝不漏扣，且单进程崩溃后可收敛。三道护栏同时约束一次请求：

| 护栏 | 条件 | wire code |
|---|---|---|
| 月度计数 | `usage.count < MonthlyQuota` | `QUOTA_EXHAUSTED`(429) |
| 装机日 token | `usage.tokens + est <= InstallDailyTokenCap` | `QUOTA_EXHAUSTED`(429) |
| 全局日预算 | `budget.tokens_used + est <= GlobalDailyBudget` | `BUDGET_EXHAUSTED`(402) |

后验累加（先放行、事后求和扣减）在并发下天然超卖：两个 goroutine 同读旧余额、各自放行、再各自扣减 → 钱包被透支过 `GlobalDailyBudget`。预占的 `est` 必须是保守上界 `estimatePromptTokens(messages) + clampMaxTokens(client, MaxTokensCap)`（保证 `est ≥ count×8` 且 `est ≥ 真实分词`，每消息 +8 抵御 OWASP-API4 碎消息放大）。

## 决策 / Decision

**三道护栏在写池（`MaxOpenConns=1` + `_txlock=immediate`）的单个 `BEGIN IMMEDIATE` 事务里悲观预占，按实结算，单点回滚，孤儿对账。**

1. **一事务三条件 UPDATE**（`app/quota.Reserve`）：每道护栏一条 `UPDATE ... WHERE <条件>`；任一 `RowsAffected()==0` 立刻返回对应 `APIError` 并整事务回滚（`defer tx.Rollback` 直到 `committed=true`）。串行化写池是原子三合一预占成立的物理前提（GW-INV-01）。
2. **Period 入口快照一次**：`SnapshotPeriod(h.now())` 在 `cfg.Load().Location` 取一次 `Period{Month,Day}`，同一个 `p` 贯穿 Reserve/Settle/Rollback，**结算/回滚绝不重算**——否则跨午夜并发会作用到不同的 `period_day`/`period_month` 行，破坏守恒（GW-INV-05）。
3. **单点 REL-5 回滚**：`forward` 里唯一防御点 `outputStarted bool` + `defer{ if !outputStarted { rollback } }`。任何**产出前**失败（重试耗尽 / breaker open / 全 key down / marshal 失败 / 队列超时 / 排队中被取消）回滚全部三项预占 + 当日 `requests-1` +（若启用）日子限 `count-1`（GW-INV-02）。
4. **首字节算一次**：流式仅在 `br.Peek(1)` 成功后置 `outputStarted=true`，非流式在 2xx header 后；产出一旦开始，计数**保留**（即便中途断连）且**永不重试**（GW-INV-03）。
5. **`settled IS NULL` 幂等三态**：Settle(`SET settled=actual`) / Rollback(`SET settled=0`) / Reconcile(`SET settled=reserved`) 互斥单赢，`RowsAffected()==0` ⇒ 提交空操作不动余额（GW-INV-04）。分离的 settle goroutine 与周期孤儿扫描会竞争同一 ledger 行，单赢 CAS 是唯一防双调整的手段。
6. **孤儿对账保守释放**：崩溃留下的 `settled IS NULL` 行由 `ReconcileOrphans(older=10m)` 全额退 `reserved` 给 `budget` + 装机日 `usage`、置 `settled=reserved`；+1 月度计数**刻意保留**。崩溃只会**多扣**（GW-INV-06）。

## 理由 / Rationale

- 单写池串行化是「原子预占」最廉价正确的实现：无需分布式锁、无需后验对账，`BEGIN IMMEDIATE` 直接拿写锁。
- 保守预占（高估 `est`） + 按实回退（`delta = Reserved - actual` 双向调 budget 与 usage，缺 usage 时全额 `est` 兜底，GW-INV-09）使钱包既精确又**绝不欠扣**。
- 「崩溃只多扣」是安全方向：欠扣会让滥用免费溜走；虚占必须最终释放，否则日预算被长期钉死。

## 取舍与后果 / Consequences

**为何不选：**

- **后验累加**：并发超卖、钱包可透支——本决策要消灭的核心风险。
- **每护栏独立事务**：跨护栏不再原子，部分提交破坏守恒；正是旧 install 多闸 bug（B11）的同类毛病。
- **把 Period 在 settle 重算**：跨午夜结算落错日行（GW-INV-05 反例）。

**make-immune-by-construction（本决策构造性免疫的已评审 bug）：**

| bug | 旧病灶 | 本决策的构造规则 |
|---|---|---|
| **B1** 子限热加载在 Reserve↔Rollback 间漂移 | Reserve 用快照 cfg 闸 `count+1`，Rollback 用 fresh `cfg.Load()` 闸 `count-1` | `Reservation` 携带显式 `SublimitApplied bool`，Rollback 读它而非重读 cfg；每请求单 `cfg.Load()` 快照贯穿全部护栏 |
| **B2** Settle/Rollback 错误被吞、孤儿全额退致欠扣 | `_ = h.q.Settle(...)`，失败的 top-up（actual>est）被孤儿扫描全额退 | Settle/Rollback 错误必须被记账层捕获、非 nil ⇒ 低基数 `SettleFailures` 计数器；失败结算可观测、不被静默对账成全额退 |

**后果：**

- `quota` 的事务逻辑（三 UPDATE）刻意保留在**单包**内（见 [架构 §4 over-engineering 备注](../concepts/architecture.md)）——把 use-case 与 DML 强行拆进 app/infra 两层会令 `*sql.Tx` 跨端口泄漏或把 app 层掏空成透传，与本原子性决策冲突。
- 守恒由 `TestConcurrent*NoOversell` / `TestBudgetExhausted` / `TestRollbackConservation` / `TestPeriodSnapshotReuse` 等回归。
- 孤儿扫描在启动 + 每 5 分钟 tick（`lifecycle` reconciler 循环）跑一次。

## 相关 / Links

- [架构](../concepts/architecture.md) · [ADR 0002 统一错误类型](0002-unified-structured-error-type.md) · [ADR 0005 W/R 池 + 迁移](0005-sqlite-rw-pool-versioned-migrations.md) · [ADR 0011 故障分类](0011-fault-classification-excludes-cancel-429.md)
- 不变量：GW-INV-01..10（财务正确性）
