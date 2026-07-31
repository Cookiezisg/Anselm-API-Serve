---
id: DOC-035
type: working
status: superseded
owner: @weilin
created: 2026-07-27
reviewed: 2026-07-31
review-due: 2026-10-29
audience: [human, ai]
landed-into: ../references/backend/api.md
---

# 生成能力（图 / 语音 / 视频 / 音色）：已落地

四条生成能力已全部施工完成并上线，本抽取稿的每一项都已进入 canonical 契约，不再维护重复表：

| 曾在本稿的东西 | 现在住哪 |
|---|---|
| 六条端点的请求/响应形状与拒绝码 | [api.md](../references/backend/api.md) §1 表 + §1.3（异步视频）/ §1.5（图像）/ §1.6（语音）/ §1.7（音色） |
| 「URL 直通」原则与它的带宽理由 | [api.md](../references/backend/api.md) §1.4 |
| 受管媒体输入只收 `data:` 的 SSRF 闸 | [api.md](../references/backend/api.md) §1.4；决策见 ADR 0011（桌面仓）/ [ADR 0012](../decisions/0012-media-lease-inline-content.md) |
| 配置键（`IMAGE_*` / `SPEECH_*` / `TTS_*` / `VIDEO_*` / `VOICE_*` / `DASHSCOPE_NATIVE_BASE` / `MEDIA_DOMAIN`） | [config.md](../references/backend/config.md) |
| 品类日账本与 pUSD 卡 | [database.md](../references/backend/database.md)、[invariants.md](../references/backend/invariants.md) GW-INV-52/57 |
| 新增 wire code（`IMAGE_*` / `TTS_*` / `VIDEO_*` / `VOICE_*`） | [error-codes.md](../references/backend/error-codes.md) |
| 不变量草案 | [invariants.md](../references/backend/invariants.md) GW-INV-50…57 |
| 语音为何返裸字节 | [ADR 0018](../decisions/0018-speech-raw-bytes-over-websocket.md) |

**本稿与实到实现有已知出入，故不作为参考**：它写的上游是 `qwen3-tts-instruct-flash`（实到 `qwen-audio-3.0-tts-flash`，且只有双工 WebSocket），列了一个从未存在的 `IMAGE_UPSTREAM_TIMEOUT_SEC`，价格全是待对账的工作假设。一份施工中的草案在完工后继续被当成契约读，是比没有它更贵的东西——这也正是它归档而不是留在 `working/` 的理由。

施工期的批次编号（`批B`/`批C`/`H1`/`H9`/`P8`/`P13` 等）只在本稿定义过，已从全仓代码与文档中清除；需要「当时为什么这么定」时读 git 历史，需要「现在是什么」时读上表。
