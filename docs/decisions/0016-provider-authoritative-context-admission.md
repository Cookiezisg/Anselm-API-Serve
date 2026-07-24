---
id: DOC-033
type: decision
status: active
owner: @weilin
created: 2026-07-23
reviewed: 2026-07-23
review-due: 2099-12-31
audience: [human, ai]
---

# 0016 — Provider 权威上下文准入与 route-aware 能力发布

## 背景 / Context

Gateway 的 `PromptEstimate` 是“一个 UTF-8 byte 至少预留成一个 token”的悲观成本上界。它适合防止 DeepSeek 账本少预留，却不是 tokenizer：中文、JSON schema、tool history 等输入会被系统性高估。把这个数字同时拿来与 `INPUT_TOKEN_CAP` 或模型 input limit 比较，会在实际 prompt 远未用满时本地返回 `input too large`，客户端也无法通过压缩恢复，因为请求根本没有到达掌握真实 tokenizer/context 规则的 provider。

同一个公开模型有两条确定性 content route：纯文本 DeepSeek 与图片/视频 Qwen3.7 Plus 都有 1,000,000 input。向客户端只报一个保守 256K 数字会浪费两条 route 的真实能力；route-aware 发布仍保留，为未来音频 profile 预留明确边界。

## 决策 / Decision

1. Gateway 不再按 token estimate 拒绝上下文请求，也不把 estimate 与模型 input hard limit 比较。`INPUT_TOKEN_CAP` 仅为旧 env/settings 兼容保留，值没有执行效果。
2. 内存与协议安全仍由可证明的 structural limits 负责：HTTP body bytes、message 数、单 message rune、media part 数和 decoded media bytes。body 超限统一为 `413 REQUEST_BODY_TOO_LARGE`。
3. DeepSeek 成本预留继续使用保守 byte estimate，但 quote 在 DeepSeek 1,000,000 input rate-card 边界 clamp；启动/热改必须证明完整模型 input + bounded output 的最坏请求能装入 operator 月钱包。Qwen 视觉 route 因媒体 token 无法本地证明，按完整 1M/64K highest-tier hard limits 预留。
4. 只有选中 provider 的真实拒绝才是 context hard-limit 事实。400/413/422 归一为 `UPSTREAM_REJECTED` 与闭集 reason，原文不透传、不 retry、不计 breaker并 rollback；客户端可据 `context_length` 做压缩后重试。
5. `GET /v1/models` 保持一个 OpenAI-compatible public model，同时用可忽略的 namespaced `anselm_capabilities` 发布 text/multimodal 各自的 input/output limit 与 availability。客户端按每个具体 prompt 是否含 native media 选预算。
6. absent/non-positive `max_tokens` 归一为显式 `min(MAX_TOKENS_CAP, selected output hard limit)`，与 positive caller 值的 clamp 规则、DeepSeek quote 和客户端 output headroom 使用同一个数字。

## 后果 / Consequences

- 保守记账仍 fail-safe，但不会再把计费上界冒充 tokenizer 造成假超限。
- 纯文本与图片/视频 Agent 均可完整利用 1M；未来音频 profile 只能以独立 capability 发布，不能把主会话 route 静默缩短。
- Gateway 不承担对话压缩策略；它诚实转发 provider 的结构化拒绝。Agent 负责在每次 sampling 前整理上下文，并对权威 `context_length` 透明恢复。
- 超大但结构合法的请求可能到 provider 后才被拒，这是有意的：只有 provider 掌握真实 tokenization。请求在 output 前被明确拒绝时预留回滚，不造成 breaker 污染。
