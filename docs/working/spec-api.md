---
id: DOC-021
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/api.md
---

# API 契约（已落地）

本抽取稿已由 [canonical HTTP API 契约](../references/backend/api.md) 取代，不再维护重复端点/字段表。当前 chat 契约的关键变化是：完整历史纯文本→DeepSeek V4 Flash，任一受支持图片/视频→Qwen3.7 Plus；client `model` 不选择 provider；Qwen 配置是启动必需能力；media 只接收 inline base64 jpeg/png/webp 与 wav/mp3，无 fallback。provider failure 的 `APIError` 与 `ChargeExposure` 独立，charge-ambiguous 网络/5xx 不自动 retry。

逐字 wire code 见 [error-codes.md](../references/backend/error-codes.md)，路由与账本决策见 [ADR-0017](../decisions/0017-qwen-visual-route-and-tiered-cost-ledger.md)。
