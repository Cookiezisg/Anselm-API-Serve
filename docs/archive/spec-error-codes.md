---
id: DOC-024
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/error-codes.md
---

# 错误码表（已落地）

本抽取稿已由 [canonical wire-code 表](../references/backend/error-codes.md) 取代；status/code/message 只以该 reference 与 `domain/apierr` 为准。

`MULTIMODAL_UNAVAILABLE` 保留为防御性稳定码，但正式部署在启动期要求 Qwen credential 完整，因此正常配置不会进入该状态。所有 wire code 与 `ChargeExposure` 正交；尤其 `UPSTREAM_ERROR` 既可是明确 401/402 refusal（rollback），也可是 connect/5xx 歧义（no-retry + full settle）。
