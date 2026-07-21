# anselm-gateway 文档索引（会话入口）

> 模块：`github.com/sunweilin/anselm/gateway`。一个**薄网关**（文本→DeepSeek、多模态→Kimi 的确定性 capability 路由 + 悲观成本记账 + install/PoW 身份 + 管理后台），Clean Arch 重写已落 `main`、线上运行。
> 本文件是会话**起点**，只给指针、不放内容（≤50 行，`make docs` 强制）。任何深入都点链接到权威源。

## 先读

| 你要做什么 | 去哪 |
|---|---|
| 懂规矩（文档怎么写 / 同步 / 淘汰，三铁律 + 收尾清单） | [GOVERNANCE.md](GOVERNANCE.md)（DOC-000，强制规范） |
| 懂系统（6 域 + 4 层 Clean Arch + 双 provider 隔离 + pUSD 悲观记账） | [concepts/architecture.md](concepts/architecture.md) |
| 查硬契约（与代码逐字一致，**现行事实源**） | [references/backend/](references/backend/)（见下表） |
| 查选型与取舍（为何这样设计） | [decisions/](decisions/)（ADR-001..015，不可变；0012 成本账本，0013 音频边界，0014 dashboard 认证，0015 设备证明） |
| 看重写期的抽取契约（**已 landed/superseded，作历史参考**） | [working/slice-plan.md](working/slice-plan.md) |

## references/backend 契约（reference，每次代码改动即同步）

| 文档 | 管什么 |
|---|---|
| [overview.md](references/backend/overview.md) | 后端总览：3 监听器 · 4 层 · 6 域 · 导航 |
| [api.md](references/backend/api.md) | 全 HTTP 端点（business / admin / dashboard 三 mux） |
| [config.md](references/backend/config.md) | 全配置面 + 三层级 + 边界 |
| [database.md](references/backend/database.md) | SQLite schema（identity/config + v2 pUSD 账本 + v1 只读审计表） |
| [error-codes.md](references/backend/error-codes.md) | 全 wire code → status / message |
| [invariants.md](references/backend/invariants.md) | GW-INV-NN 不变量登记册 |

## 工程纪律

最高法是项目根 [`CLAUDE.md`](../CLAUDE.md)（权威层级 §10）；本索引与之冲突时以 `CLAUDE.md` 为准。
