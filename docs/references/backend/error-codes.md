---
id: DOC-004
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
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
| `INVALID_TOKEN` | 401 | missing or invalid install token | bearer 缺失或非法 |
| `ACCOUNT_BANNED` | 403 | this install has been disabled | install 被封禁 |
| `RATE_LIMITED` | 429 | rate or daily sub-limit exceeded | per-minute 速率或日次数子限额 |
| `QUOTA_EXHAUSTED` | 429 | monthly free-tier quota exhausted | 月度免费额度耗尽 |
| `UPSTREAM_BUSY` | 429 | upstream capacity is busy, retry shortly | 队列满/超时、breaker open、上游 429；**独立类，永不 retry / breaker**（GW-INV-23） |
| `BUDGET_EXHAUSTED` | **402** | daily free-tier budget reached, try again tomorrow or use your own key | 每日全局预算到顶；注意是 402 非 429 |
| `BAD_REQUEST` | 400 | invalid request body | 请求体非法（含 `n>1`、消息形状越界、严格白名单失败、body 超 `MAX_BODY_BYTES`、`est` 超单 install 日容量的运行时预检） |
| `UPSTREAM_ERROR` | 502 | upstream model provider error | 上游 provider 错误（5xx/connect/重试耗尽；**不含** 4xx 请求拒绝） |
| `UPSTREAM_REJECTED` | 400 | upstream rejected the request: reduce input size or max_tokens, or fix request parameters | 上游 400/413/422 请求拒绝（如超上下文）；`details.reason ∈ {context_length, max_tokens, invalid_request}`；不 retry、不计 breaker、预留回滚（GW-INV-41） |
| `UPSTREAM_TIMEOUT` | 504 | upstream model provider timeout | 上游超时 |

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
