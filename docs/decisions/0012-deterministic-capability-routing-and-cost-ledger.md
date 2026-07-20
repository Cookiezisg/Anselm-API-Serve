---
id: DOC-028
type: decision
status: active
owner: @weilin
created: 2026-07-20
reviewed: 2026-07-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0012 — 确定性 capability 路由、provider 隔离与 pUSD 成本账本

## 背景 / Context

网关同时提供纯文本与多模态能力，但两个上游的能力、计价和故障域不同：DeepSeek V4 Flash 是文本模型；Kimi K2.6 接受本文限定的图像/视频输入。不同 provider 的 token 价格不可直接相加，Kimi OpenAI compatibility 的 thinking token 与 usage 字段也不能证明一个比模型硬上限更小的账单上界。

客户端只需要知道“Anselm 可以自动处理请求内容”，不应通过 `model` 字符串选择运营者账户、价格层或 fallback。记账则必须在调用前冻结 provider/model/rate card。client-facing error、provider health、retry 与是否可能收费是四个正交事实：任何网络歧义都只能多扣，不能因某个 wire code 看似失败而猜测“也许没生成”。

## 决策 / Decision

### 1. 一个逻辑模型，按完整历史确定性分流

`/v1/models` 只暴露 `PUBLIC_MODEL_ID`（默认 `anselm-auto`）。`POST /v1/chat/completions` 扫描**完整 messages 历史**后只走以下二分：

| 规范化内容 | provider / 实际模型 |
|---|---|
| 全部是 string、文本 part，或合法 assistant tool-call 空内容 | DeepSeek / `deepseek-v4-flash` |
| 任一 user message 含受支持的图像或视频 part | Kimi / `kimi-k2.6` |

客户端 `model` 不参与 provider、实际模型或价格选择；服务端配置的实际模型必须存在精确 rate card。路由没有“高级/普通”判断，也不按用户、文本语义、顺序或健康状态改选 provider。

### 2. 多模态输入是关闭的联合类型

只接受 OpenAI-compatible inline 形状：

- `image_url.url`：严格 base64 data URI，MIME 仅 `image/jpeg`、`image/png`、`image/webp`，声明 MIME 必须匹配 magic bytes；
- `video_url.url`：严格 base64 `data:video/mp4;base64,...`，声明 MIME 必须匹配 MP4 容器标识；
- media 只允许出现在 `user` message，并受整请求 `MAX_MEDIA_PARTS` 与累计 `MAX_MEDIA_DECODED_BYTES` 约束。

远程 URL、PDF、file、未知 part、跨 variant 字段与 `input_audio` 全部 `400 BAD_REQUEST`；网关永不抓取客户端 URL。纯文本 part 数组在 canonical request 中折叠成 string。

### 3. provider 物理隔离，禁止 fallback

DeepSeek 与 Kimi 各自拥有固定 endpoint、API key pool、per-key health/cooldown、process breaker 和低基数指标标签。选定 provider 后**绝不**切另一个 provider，也不跨 provider 复用 key、breaker 或请求扩展：Kimi adapter 会剥离 DeepSeek 专有 `reasoning_content`，并使用 K2.6 的默认 thinking 行为。opaque `tool_calls` JSON 原样保留。

`DEEPSEEK_API_KEY` 是文本基线的必填 secret；`KIMI_API_KEY` 可选。未配置 Kimi 时文本与 readiness 正常，多模态请求在 reserve/Open 前返回 `503 MULTIMODAL_UNAVAILABLE`，不会暗降级为 DeepSeek。

### 4. token 先按冻结 rate card 换算为 pUSD

内部唯一金额单位是 pico-US dollar：`1 USD = 10^12 pUSD`，`1 microUSD = 10^6 pUSD`。精确模型的版本化 rate card 是编译期闭集：

| provider/model | input cache miss | cache hit | output |
|---|---:|---:|---:|
| DeepSeek V4 Flash | 140,000 pUSD/token | 2,800 pUSD/token | 280,000 pUSD/token |
| Kimi K2.6 | 950,000 pUSD/token | 160,000 pUSD/token | 4,000,000 pUSD/token |

DeepSeek quote 使用 byte-fallback prompt 上界（所有透传 message/tool 字段逐 UTF-8 byte 计数，另加固定 request framing 与每消息 64 token framing 余量）+ 已 clamp 的输出上界，避免多字节 tokenizer 或 tool continuation 先少占后追账。Kimi compatibility 无法证明 thinking 上界，故图片/视频 quote 使用模型完整输入/输出硬上限；settle 只在 usage 结构自洽时，以 cache hit 明细按较低费率退款，其余 prompt 都按 cache miss 计价。Kimi 缺 `total_tokens`、`total<prompt+completion` 或 reasoning 明细超过 `total-prompt` 时不可计价；DeepSeek cache hit/miss 合计超过 prompt 同样不可计价，均保留 full quote。完整 Kimi quote 为 `380,108,800,000 pUSD = 380,108.8 microUSD`。未知 provider/model、越模型 limit 或无法精确计价一律 fail closed。

### 5. 单事务预留四道余额

`billing.Plan{Provider, Model, RateCardID, InputClass, PromptQuote, OutputQuote, ReservedPUSD}` 在 reserve 前冻结并贯穿请求。写池的单个 `BEGIN IMMEDIATE` 原子占用：

1. install 月度请求额度；
2. install 当日 pUSD 钱包；
3. 所选 provider 当日 pUSD 钱包；
4. 全局当日 pUSD 钱包；

并插入 `spend_ledger(state='open')`。任一条件更新失败，整个事务回滚。不同 provider 的 raw token 从不进入共享余额，只有 rate card 换算后的整数 pUSD 可以相加。

### 6. `CallFailure` 显式携带独立账务暴露

provider engine 的失败值是 `CallFailure{APIError, Exposure}`；`Exposure` 是 domain `billing.ChargeExposure`：

| Exposure | 证明 | accounting |
|---|---|---|
| `DefinitelyUnbilled` | 本地在到达 provider 前拒绝，或 provider 返回明确 pre-generation 3xx/4xx（含 400/401/402/403/429） | 可 rollback |
| `ChargePossible` | connect/TLS/read 不确定、timeout、5xx、call 中 client-cancel、未知/缺失 exposure | full reservation settle |

`ChargePossible` 刻意是 enum 零值；只有显式 `DefinitelyUnbilled` 才能退款。`APIError.Code` 不能替代 exposure：例如 `UPSTREAM_ERROR` 既可能来自明确 401/402（DefinitelyUnbilled），也可能来自 connect/5xx（ChargePossible）。app 只调用 `Exposure.MayHaveCharged()`，不从 status/code/message 二次推导。

### 7. 自动 retry 只做可证明未收费的 key failover

同一 reservation 最多覆盖**一个 charge-ambiguous provider attempt**。自动 retry 总 attempt 上限为 3，但仅以下 `DefinitelyUnbilled` per-key signal 可进入下一 attempt：

- 当前 key 明确 401/403，先 cooldown 再选 sibling key；
- 当前 key breaker 已 open，请求尚未发出，改选 sibling key。

connect/TLS/timeout/first-byte read failure/5xx/client-cancel 全部 `ChargePossible`，当前请求立即终止并 full settle，**不 retry**；429 与其它明确 3xx/4xx 虽 `DefinitelyUnbilled`，但也不自动 retry。这样一份 ledger reservation 不会隐藏两次可能的 provider 收费。

### 8. 显式账本状态

状态机只有：

```text
open ── authoritative/保守结算 ──▶ settled
  ├── 明确未计费 ───────────────▶ rolled_back
  └── 超时孤儿扫描 ─────────────▶ orphaned
```

- breaker open、队列失败、排队取消、provider 未配置等 Open 前本地拒绝 → `DefinitelyUnbilled`，转 `rolled_back` 并反转月度额度与三个 pUSD 钱包；
- provider 明确 3xx/4xx refusal（含归一后的 `UPSTREAM_REJECTED`/`UPSTREAM_BUSY`，也含呈现为 `UPSTREAM_ERROR` 的 401/402 等）→ `DefinitelyUnbilled`，转 `rolled_back`；
- timeout、connect/TLS/read error、5xx、call 中 client-cancel、nil stream 或未知 exposure → `ChargePossible`，转 `settled(charged_pusd=reserved_pusd)`，即使客户端尚未看到首字节；
- stream 与 non-stream 都须在 connect→header→**首个 body byte** 后才 handoff；成功响应按累计 usage 经冻结 rate card 结算。各字段取累计最大值而不跨帧求和，但任何负数/畸形证据 sticky：后续正常帧不能洗白；缺失、矛盾或不可计价 usage 均保留全额 reservation；实际费用超过 quote 时如实 top-up，不用 cap 隐藏已发生支出；
- aged `open` → `orphaned(charged_pusd=reserved_pusd)`，**不退款、不改钱包**。

所有终态转换都是 `WHERE state='open'` 的单赢家 CAS。`Period{Month,Day}` 在请求入口快照一次；settle/rollback 永不重算日期。

## 理由 / Rationale

- 内容形状是可测试、不可含糊的 capability 事实；语义分类或客户端 model 都会把价格/安全策略变成隐式分支。
- no fallback 保证请求 body、密钥、账单计划与实际 provider 始终一致；一个 provider 故障不会污染另一个 provider 的 breaker。
- pUSD 可精确表示最低价格维度（DeepSeek cache hit 2,800 pUSD/token），避免浮点舍入和跨 provider raw-token 假等价。
- `ChargeExposure` 是外部计费证明边界；Open 调用本身不足以区分本地拒绝、明确 HTTP refusal 与网络歧义。显式类型令 app 无法从 client error 猜账，把歧义按全额结算是“崩溃只多扣不少扣”的唯一安全方向。
- charge-ambiguous failure 不 retry 保证“一份 reservation 最多对应一次可能收费”；否则两个 timeout/5xx attempt 可能各自产生费用，任何单份 quote/usage 都无法正确结算。

## 取舍与后果 / Consequences

**不选择：**

- 客户端显式选择 DeepSeek/Kimi：暴露运营者价格层并允许绕过 capability 路由；
- Kimi 故障后回退 DeepSeek：多模态内容不受支持，且会撕裂已冻结账单与 provider 身份；
- 共享 raw-token 钱包：把不同价格 token 当成同一金额，必然超卖或过度拒绝；
- 按 `UPSTREAM_ERROR`/HTTP status 猜退款：同一 client code 可代表明确 refusal 或网络歧义，必然在一侧记错；
- 对 connect/timeout/5xx 自动 retry：多次可能收费被藏在一份 reservation 后，账本无法守恒；
- 自动抓取 remote URL/PDF/video：扩大 SSRF、下载放大、MIME 欺骗与不确定 compatibility 面。

**后果：**

- operator 必须为每个可路由 provider 配正数日 cap；共享 global cap 仍是最终钱包上限。
- Kimi key 可在代码部署后再配置；此前只有多模态能力返回明确 503。
- `/v1/quota` 继续以月请求次数对客户端呈现；成本余额是 operator 内部护栏，dashboard 在展示边界转成整数 microUSD。

## 相关 / Links

- 本 ADR 整体取代 [ADR-001](0001-pessimistic-three-guardrail-reservation.md) 的 raw-token ledger；定向取代 ADR-005 的“schema 恒为 8 表”、ADR-006 的 `MODEL_ALLOWLIST`，以及 [ADR-011](0011-fault-classification-excludes-cancel-429.md) 中“connect→首字节 transient fault 可 retry / 产出前一律 rollback”的结论。ADR-011 的 typed fault/breaker 分类仍有效，但财务 exposure 与 retry 以本 ADR §6–7 为准。
- [架构](../concepts/architecture.md) · [API](../references/backend/api.md) · [配置](../references/backend/config.md) · [数据库](../references/backend/database.md) · [不变量](../references/backend/invariants.md)
- [ADR-005 SQLite 读写分池与迁移](0005-sqlite-rw-pool-versioned-migrations.md) · [ADR-011 故障分类](0011-fault-classification-excludes-cancel-429.md)
- 不变量：GW-INV-01..10、11、14、15、21..23、30、31..37、41..44
