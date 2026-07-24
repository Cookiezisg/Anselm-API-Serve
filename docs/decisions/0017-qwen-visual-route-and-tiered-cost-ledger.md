---
id: DOC-034
type: decision
status: active
owner: @weilin
created: 2026-07-24
reviewed: 2026-07-24
review-due: 2099-12-31
audience: [human, ai]
---

# 0017 — Qwen 视觉 route 与分档费率账本

## 背景 / Context

`anselm-auto` 的图片、视频 route 使用新加坡 Model Studio 的 `qwen3.7-plus`，以发布真实
1M input / 64K output 的视觉主 Agent。该模型的价格不是
一个固定 token 单价：一次请求的**全部**输入按该请求最终 input-token 所在档位计价；256K
以内与 256K–1M 的单价不同，且启用 thinking 时的输入和输出费率也不同。

旧账本只支持一组 input/output 单价。更重要的是，旧 inline media 协议只能可靠限制 decoded
bytes，不能从图片像素、视频帧率/时长推导官方视觉 token。因此用文本 byte estimate 选低档
Qwen 价格会少预留；把 Omni 的 64K 窗口直接当主会话 route 又会让音频附件把 1M Agent 降级。

## 决策 / Decision

1. 视觉主 route 的唯一上游为 `qwen3.7-plus`，固定显式 `enable_thinking=true`；不保留
   fallback、双写或兼容代码。普通用户仍只看 `anselm-auto`，不能选择内部模型或 thinking 档位。
2. rate card 由“一个固定单价”升级为可冻结的 request-wide tier：reserve 用输入上界所在的最贵
   适用档；settle 用上游权威 `prompt_tokens` 重新选择该请求实际适用档，对 input 与 output 一起
   结算。rate-card id、档位边界、每档所有单价都必须是代码中的精确、可验证事实。
3. C3 的旧 inline 图片/视频 route 因无法证明媒体 token 上界，必须按 Qwen 1M 最高档输入和 64K
   output 做悲观 reserve；成功后若 usage 完整，再按实际档位多退少补。它不以本地 estimate 拒绝
   context，也不把预算预留伪装成 tokenizer admission。
4. 原始音频继续在 `Reserve`/`Open` 前稳定返回 `AUDIO_UNAVAILABLE`，直到 M3 的一次性媒体、音频
   感知双阶段及 Omni 的 text/vision/audio 分项 usage 已完整落地。C3 不发布 Omni 64K 为 conversation
   budget，避免“加音频就把主 Agent 缩到 64K”的产品退化。
5. Qwen provider 使用独立 endpoint/key pool/breaker/readiness/metrics 身份；配置从
   `DASHSCOPE_API_KEY` 与 `DASHSCOPE_WORKSPACE_ID` 推导新加坡 compatible-mode base URL，或接受同等
   严格校验的显式 base URL。workspace id 非 secret；API key 仅 env，绝不进入 settings、dump、日志或
   客户端。

## 后果 / Consequences

- 视觉主 Agent 能诚实发布 1M，而 context 上限仍由 provider 作为最终权威；
- 旧内联媒体的大请求会临时占用较高预算，这是“绝不少扣”的必要代价；M1–M3 用可计量 proxy、lease
  和 evidence capsule 后才可缩小 quote，不能以猜测替代；
- 账本可以处理当前两档 Qwen，也为未来 Omni 的按模态价格留下同一“冻结报价、权威 usage 结算”边界；
- 音频产品能力不是被删除，而是被明确后置到不会降级主 Agent、不会少计费的实现阶段。
