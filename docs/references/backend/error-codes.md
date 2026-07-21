---
id: DOC-004
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-21
review-due: 2026-10-19
audience: [human, ai]
---

# 错误线缆码（wire codes）

> 与 `internal/domain/apierr/apierr.go` 逐字对齐。wire code 是**外部契约**（SPA / Caddy / 运维依赖确切值），status/code/message 即 spec，非任意。client 永远分支于 `code`（UPPER_SNAKE），绝不依赖 OpenAI 式 type/param。信封形态见 [api.md](api.md)。

## 1. 信封与归一

- 成功：**裸实体**（无 wrapper）。
- 失败：`{"error":{"code","message","details"?}}`（`details` omitempty）。
- 任何非 `*APIError` 到达 transport 渲染器 → 归一为 `500 INTERNAL`（`apierr.Internal()`），绝不泄露内部 detail / 上游 body / key（§4.3 / GW-INV-11）。
- `message` 是 client-safe 静态串：无上游 body、无 key（GW-INV-11/17）。

## 2. 业务 sentinel 码（`§5.2` 表，verbatim）

| wire code | HTTP status | message | 触发 / 备注 |
|---|---|---|---|
| `INVALID_INSTALL` | 401 | missing or invalid install id | install id 缺失或未知 |
| `DEVICE_PROOF_REQUIRED` | 401 | device proof is required | proof/header 缺失 |
| `DEVICE_PROOF_INVALID` | 401 | device proof is invalid | 公钥/签名/iat/method/authority/target/body hash 非法 |
| `DEVICE_PROOF_NONCE_INVALID` | 401 | device proof challenge expired; fetch a fresh challenge | nonce 过期、伪造或来自上个进程；client 刷新后重试一次 |
| `DEVICE_PROOF_REPLAYED` | 409 | device proof was already used | `(kid,jti)` 已消费 |
| `ACCOUNT_BANNED` | 403 | this install has been disabled | install 被封禁 |
| `RATE_LIMITED` | 429 | rate or daily sub-limit exceeded | per-minute 速率、日次数子限额或 per-install 日成本钱包到顶 |
| `QUOTA_EXHAUSTED` | 429 | monthly free-tier quota exhausted | 月度免费额度耗尽 |
| `UPSTREAM_BUSY` | 429 | upstream capacity is busy, retry shortly | shared queue 满/超时、provider/process breaker open、provider 429；当前 CallFailure 为明确未计费，rollback；429 本身不自动 retry |
| `BUDGET_EXHAUSTED` | **402** | daily service budget reached, try again tomorrow | 所选 provider 或 shared global 日 pUSD 钱包到顶；注意是 402 非 429 |
| `BAD_REQUEST` | 400 | invalid request body | body/shape/content 非法，含 n>1、unsupported/remote media、media limits、文本输入 cap、quote 超 install 日容量；生产 Caddy 的 5MiB edge cap 也把原生 413 归一为同一 JSON/status，不暴露第二套 wire 契约 |
| `MULTIMODAL_UNAVAILABLE` | **503** | multimodal input is unavailable on this deployment | 请求含合法 media，但 deployment 未配置 `KIMI_API_KEY`；reserve/Open 前拒绝；文本能力仍正常；无 fallback |
| `AUDIO_UNAVAILABLE` | **503** | audio input is not available on this deployment | 请求含合法 `input_audio`，但当前路由表无音频上游；严格校验后、reserve/Open 前拒绝；不受 `KIMI_API_KEY` 是否配置影响；无 fallback |
| `UPSTREAM_ERROR` | 502 | upstream model provider error | **不能推导账务**：明确 3xx/4xx（如 401/402、key failover 耗尽）可为 DefinitelyUnbilled；connect/TLS/5xx 为 ChargePossible；以后者为 full quote且不 retry |
| `UPSTREAM_REJECTED` | 400 | upstream rejected the request: reduce input size or max_tokens, or fix request parameters | 所选 provider 400/413/422；`details.reason ∈ {context_length,max_tokens,invalid_request}`；明确未生成，故不 retry/不计 breaker并 rollback |
| `UPSTREAM_TIMEOUT` | 504 | upstream model provider timeout | ChargePossible；不 retry，保留 full quote |

### wire code 与 charge exposure 正交

infra 返回 `CallFailure{APIError, Exposure}`；app 只以 `Exposure.MayHaveCharged()` 决定 full settle/rollback。`ChargePossible` 是零值，未知值同样视为可能收费；只有显式 `DefinitelyUnbilled` 可退款。APIError 负责 client 归一，**任何 code/status/message 都不是财务凭证**。

自动 retry 只限 `DefinitelyUnbilled` 的 401/403 key cooldown 或 key-breaker-open failover，总 attempt≤3。connect/TLS/read/timeout/5xx/client-cancel 全部 ChargePossible、立即终止；明确 429/其它 3xx/4xx虽可 rollback，也不 retry。

## 3. /install reject 码（DISTINCT，便于审计分离，GW-INV-20）

| wire code | HTTP status | message |
|---|---|---|
| `INSTALL_RATE_LIMITED` | 429 | too many installs from this address, retry later |
| `INSTALL_CAP_REACHED` | 429 | install issuance is temporarily at capacity, retry later |
| `INSTALL_FP_LIMITED` | 429 | too many installs for this client, retry later |
| `INSTALL_POW_REQUIRED` | 403 | proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW |
| `INSTALL_POW_INVALID` | 403 | invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry |

`INSTALL_POW_REQUIRED` / `INSTALL_POW_INVALID` 是 403（重取 challenge，**不要退避重试**）。三个 install rate 码各自独立，使 Sybil 洪流可与普通限速区分。

## 4. 可靠性 / 仪表盘码

| wire code | HTTP status | message | 备注 |
|---|---|---|---|
| `DISK_LOW` | 503 | service temporarily read-only: low disk space | REL-6 低磁盘只读降级（GW-INV-29），reserve 前即 shed |
| `LOGIN_LOCKED` | 429 | too many attempts, retry later | dashboard per-IP 退避；`details.retryAfterSec`（int）+ 同步 `Retry-After` 头（GW-INV-19） |

## 5. 归一与内部码

| 名称 | status | code | 备注 |
|---|---|---|---|
| `Internal()` | 500 | `INTERNAL` | message `internal error`；所有非 `*APIError` 的归一目标 |

`LOGIN_LOCKED` 经 `apierr.LoginLocked(retryAfterSec)` 构造（携 `details{retryAfterSec}`）；`APIError.RetryAfterSeconds()` 供 transport 取头值，使 `Retry-After` 头与 details lockstep。

## 6. 构造器

`NewError(status, code, message)`（无 details）；`NewErrorWithDetails(status, code, message, details)`（携机器可操作字段，目前仅 `LOGIN_LOCKED.retryAfterSec`）。status 常量在 `apierr` 内为 untyped int（保持 domain 纯净、不 import `net/http`），值即 spec。
