---
id: DOC-022
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
landed-into: ../references/backend/config.md
---

# 配置全表（已落地）

本抽取稿已由 [canonical config 契约](../references/backend/config.md) 取代；此处不再复制 env/default/bounds。

当前公开模型键是 `PUBLIC_MODEL_ID`；真实 provider 模型为 startup-hard `TEXT_UPSTREAM_MODEL` / `MULTIMODAL_UPSTREAM_MODEL`。raw-token budget/allowlist 已退出配置面，钱包使用 integer microUSD 配置并以 pUSD 入账。`DEEPSEEK_API_KEY` 必填，`GEMINI_API_KEY` 可选且 secret-env-only。
