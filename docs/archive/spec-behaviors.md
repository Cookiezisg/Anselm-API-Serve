---
id: DOC-026
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/overview.md
---

# 动态行为 / 状态机（已落地）

本抽取稿不再承载运行期事实。当前心智模型见 [architecture.md](../concepts/architecture.md)，硬验收见 [invariants.md](../references/backend/invariants.md)，状态列见 [database.md](../references/backend/database.md)。

当前 upstream failure 显式携带独立 `ChargeExposure`，不能从 `APIError` 推导：Open 前本地拒绝和明确 3xx/4xx 为 DefinitelyUnbilled→rollback；connect/TLS/read/timeout/5xx/call 中 cancel 为 ChargePossible→不 retry、full settle；aged open 变 `orphaned` 且不退款。stream/non-stream 都以首个 body byte 为 handoff，usage 的 negative/malformed 证据 sticky；stream 只有完整读到合法 `[DONE]` 后才允许按 usage 退款，提前 EOF/读错即 full quote。自动 retry 总 attempt≤3，只用于 401/403 cooldown 或 key-breaker-open 的 sibling-key failover。
