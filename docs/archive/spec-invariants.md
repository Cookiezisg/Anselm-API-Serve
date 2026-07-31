---
id: DOC-025
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/invariants.md
---

# GW-INV 登记册（已落地）

本抽取稿已由 [canonical GW-INV 登记册](../references/backend/invariants.md) 取代；验收只引用后者。

当前红线包括：完整历史 capability 路由、client model 不选 provider、provider key/breaker 隔离且无 fallback、strict inline media 联合类型、pUSD 四闸原子预留、显式 ledger 状态、`CallFailure.ChargeExposure` 独立于 wire error，以及 charge-ambiguous connect/TLS/timeout/5xx 禁止 retry并保留 full reservation。
