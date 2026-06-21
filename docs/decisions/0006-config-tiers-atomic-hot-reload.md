---
id: DOC-015
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0006 — 配置三层 + atomic 无锁热加载 overlay

## 背景 / Context

配置项性质不一：有的需运营者运行时改（护栏阈值），有的是秘密（绝不可落库/dump），有的改了必须重启才生效（信号容量）。同时热加载必须**无撕裂**——一次请求内绝不能用到「半旧半新」的配置（旧 install 的 B1 子限漂移正是此类）。

## 决策 / Decision

**三层分类 + `atomic.Pointer[Config]` 无锁读 + clone→apply→validate→全有或全无持久化→swap→notify 的写路径；每请求单快照。**

| 层 | 例 | 规则 |
|---|---|---|
| runtime-hot | 护栏阈值、`MODEL_ALLOWLIST`、限流参数 | 可经 dashboard 改、热加载生效；env 与 overlay 共享**同一套上下界** |
| secret-env-only | `DEEPSEEK_API_KEY`、`DASHBOARD_USER/PASSWORD`、`INSTALL_POW_SECRET` | **仅 env 读**，绝不写 `settings` overlay、绝不 dump（`Snapshot/Dump` 掩码：`sk-*** (N configured)` / `configured`/`disabled`）；`INSTALL_POW_SECRET` 绝不自动生成 |
| startup-hard | `N_GLOBAL_CONCURRENCY`（信号容量）、监听地址 | 可编辑但**重启生效**；运行中改不动 in-flight 容量 |

1. **无锁读**：`atomic.Pointer[Config]`，读者 `cfg.Load()` 拿当前快照、零锁。
2. **写路径全有或全无**：clone 当前 → 应用 overrides → `validateSemantics`（含 SEC-2 跨字段：`InstallDailyTokenCap > GlobalDailyBudget` 或 `InputTokenCap + MaxTokensCap > InstallDailyTokenCap` → 具名 fail-fast，GW-INV-10）→ 单事务多键持久化（部分失败整体回滚）→ `swap` 指针 → notify。
3. **env 与 overlay 同界**：每个可调数值旋钮在 env `Load`（`boundInt`/`boundInt64`）与 DB overlay `ApplyOverrides`（`reqInt`/`reqInt64`）两路**用同一上下界**、违规具名 fail-fast；防止 dashboard 改写把护栏置成天文数字（非护栏）或 OOM 向量（GW-INV-39）。
4. **每请求单 `cfg.Load()` 快照**：在 `ServeHTTP` 取一次，贯穿全部护栏 + 模型解析，使热加载绝不在一次请求内半旧半新（GW-INV-35，B1 的泛化修复）。
5. **`RESET_TZ` 内嵌 + fail-fast**：`import _ "time/tzdata"`，`time.LoadLocation` 失败 PANIC、绝不静默退 UTC（否则日/月边界静默偏移 8h，GW-INV-38）。

## 理由 / Rationale

- `atomic.Pointer` 读路径零锁、零争用，契合高频每请求读。
- 「每请求单快照」是无撕裂的最小充分条件：指针在请求间换、请求内不变。
- 秘密 env-only 是最干净的秘密卫生：秘密永不触 DB/dump/banner，泄漏面收敛到环境变量本身。

## 取舍与后果 / Consequences

**为何不选：**

- **`sync.RWMutex` 守 Config**：每请求读取锁、热路径争用；`atomic.Pointer` 无锁更优。
- **秘密也可经 dashboard/overlay 存**：任何秘密落库/dump 即破坏 env-only 卫生（GW-INV-14）。
- **env 与 overlay 各自一套界**：两路漂移，dashboard 能改出 env 拒绝的值——护栏失效。
- **`RESET_TZ` 失败退 UTC**：静默偏移 8h，腐蚀全部 reset 语义。

**后果：**

- `domain/config` 持纯校验（struct + 界常量 + `validateSemantics` + `WorstCaseMemoryMiB`，零 env/db）；`infra/configprovider` 持 `atomic.Pointer` + overlay；`infra/store/settingsstore` 持 overlay DML。
- 回归：`TestSnapshotRedactsSecrets`、`TestPowSecretNeverInSnapshotOrDump`、`TestLoadSemanticValidation`、`Test*Overridable`、`TestLoadInvalidTzPanics`。

## 相关 / Links

- [ADR 0007 Sybil/PoW dormant](0007-sybil-pow-dormant-by-default.md)（POW_SECRET 强一致要求）· [ADR 0001 三闸预占](0001-pessimistic-three-guardrail-reservation.md)（单快照贯穿护栏）· [架构](../concepts/architecture.md)
- 不变量：GW-INV-10、14、35、38、39
