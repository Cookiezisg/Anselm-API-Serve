---
id: DOC-023
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/database.md
---

# DB schema（已落地）

本抽取稿已由 [canonical SQLite schema](../references/backend/database.md) 取代；此处不再维护表/列副本。

当前记账写入 `quota_monthly`、`install_spend_daily`、`provider_spend_daily`、`global_spend_daily`、`spend_ledger`。金额为 pUSD；账本状态是 `open→settled|rolled_back|orphaned`：只有显式 `ChargeExposure=DefinitelyUnbilled` 可 rollback，`ChargePossible` 与 aged orphan 都保留 full reservation。v1 `usage/budget/ledger` 只读保留供迁移审计。
