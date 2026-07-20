---
id: DOC-029
type: decision
status: active
owner: @weilin
created: 2026-07-21
reviewed: 2026-07-21
review-due: 2099-12-31
audience: [human, ai]
---

# 0013 — 音频协议先就绪，路由与计价后置

## 背景 / Context

Anselm 客户端的附件域已经需要统一表达图像、视频与音频；未来上游会改为具备音频能力的模型。但当前网关只有 DeepSeek 文本 route 与 Kimi 图像/视频 route，Kimi 的固定 rate card 和 compatibility payload 都不能作为音频计价、路由依据。

把音频继续当作未知输入会迫使客户端另外分叉协议；把它伪装成 Kimi 多模态又会在没有可靠能力、上游 wire 和 rate card 的情况下错误扣费。ADR-0012 中“拒绝 `input_audio`”这一输入边界因此不再适合当前公共协议；其余确定性分流、无 fallback 与 pUSD 冻结账本决策不变。

## 决策 / Decision

`input_audio{data,format}` 加入关闭 content-part 联合：仅 `user` role，`data` 是 canonical strict base64，`format` 仅 `wav|mp3` 且必须与文件魔数吻合。它与图片、视频共享请求级 part 数与累计 decoded-byte 闸，先完成形状校验，避免把未来能力变成无界二进制入口。

当前路由表显式增加第三个终点：任一合法 audio part（即使同一历史也有图片/视频）在任何 `Reserve`、provider `Open`、wallet 变更之前返回 `503 AUDIO_UNAVAILABLE`。这不是 `MULTIMODAL_UNAVAILABLE`：后者只表示已实现的 Kimi 图像/视频 route 缺少可选密钥；前者表示当前无任何可计价、可兼容的音频上游。两者均绝不 fallback。

未来启用音频必须另行决策并同时落齐：provider payload adapter、精确 model/rate card、报价与 usage 结算、readiness probe、route、错误语义、GW-INV 与端到端验收。只把 `Audio=true` 写进客户端目录不构成能力上线。

## 理由 / Rationale

- 公共协议现在稳定，客户端附件与历史/压缩链不必在接 Qwen 等全模态 provider 时重塑；
- 显式 503 保留用户附件并给客户端可恢复信号，区别于不可恢复的 `400 BAD_REQUEST`；
- 在 `Reserve` 前停下，使尚未有 rate card 的输入不可能污染悲观账本；
- 固定失败而非降级，继续守住“内容形状决定 provider、选定后无 fallback”的可审计边界。

## 后果 / Consequences

当前产品只对用户宣告图像与视频原生可用；音频可被安全传到网关边界并得到稳定不可用响应。适配未来音频 provider 时，最小改动点是既有内容联合与 `ModalityAudio` 分支，而不是新增客户端私有协议。
