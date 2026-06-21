---
id: DOC-011
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0002 — 统一结构化错误类型 domain/apierr（一种 sentinel 造法）

## 背景 / Context

旧树有**两套重复信封**：业务侧 `internal/httpx/errors.go`（带 wire code 的 sentinel + writer）和 dashboard 侧 `internal/dashboard/http.go`（自己一套 envelope 写法）。两套各写一遍 status→JSON 的映射，必然 drift；且错误类型与 HTTP writer 混在同一包，分层后 `domain`/`app` 无法 import 一个绑定 `net/http` 的错误类型。

wire code 是**对外契约**（前端、Caddy、运营者都依赖它的稳定值），必须逐字保留、单一事实源。

## 决策 / Decision

**一个 `APIError{Status, Code, Message, Details}`，全部 wire code 作为 `domain/apierr` 包内的稳定 UPPER_SNAKE sentinel；两套旧信封塌缩成一套。**

1. **类型落在 domain**：`apierr` 是纯类型 + sentinel 列表（零 `net/http`、零 DB），属最内层；`domain`/`app`/`infra`/`transport` 全层可 import、无反向依赖。
2. **唯一信封写法在 transport**：`transport/httpapi/response` 提供 `WriteJSON`（裸成功）+ `WriteError`/`WriteErrorWith`/`WithDetails`（把 `APIError` 序列化成 `{"error":{...}}`）。`response` 只 import `domain/apierr`。
3. **wire code 全集（逐字保留）**：`INVALID_TOKEN`(401)、`ACCOUNT_BANNED`(403)、`RATE_LIMITED`(429)、`QUOTA_EXHAUSTED`(429)、`UPSTREAM_BUSY`(429)、`BUDGET_EXHAUSTED`(402)、`BAD_REQUEST`(400)、`UPSTREAM_ERROR`(502)、`UPSTREAM_TIMEOUT`(504)、`INSTALL_RATE_LIMITED`(429)、`INSTALL_CAP_REACHED`(429)、`INSTALL_FP_LIMITED`(429)、`INSTALL_POW_REQUIRED`(403)、`INSTALL_POW_INVALID`(403)、`DISK_LOW`(503) + 内部审计码 `CLIENT_CANCELED`(499)。
4. **`Details` 仅 omitempty 的少数场景**：当前唯一用例是 `LOGIN_LOCKED`（携带 `retryAfterSec`）。其余错误不带 `Details`。
5. **非 `APIError` 归一**：response 层遇到任何非 `APIError` 的 error 一律归一成 `500 INTERNAL`——绝不把内部错误原文透传（防泄漏）。

## 理由 / Rationale

- 一种造法 = 一处事实源 = 不可能 drift。dashboard 与业务口共享同一信封，前端只需面对一套 `{error:{code,message,details?}}`。
- 类型下沉 domain 解开「pkg/domain 无法 import 绑 HTTP 的错误」死结，且 wire code 作为业务契约本就属 domain。

## 取舍与后果 / Consequences

**为何不选：**

- **保留两套信封**：双倍映射、必然 drift、dashboard 与业务口对前端呈现不一致——正是要消灭的冗余。
- **错误类型放 pkg**：违反依赖规则 6（`pkg` 不得 import `domain`），且 wire code 是业务契约非纯机制，错置。spec 的 `internal/pkg/apierr` 残留行已删，唯一家在 `domain/apierr`。
- **学 OpenAI 的 `type/param` 错误结构**：前端只需 UPPER_SNAKE `code` 分支，`type/param` 是无用复杂度。

**后果：**

- 全库错误经 `domain/apierr` sentinel 构造；`/metrics`、`/healthz` 等非信封形态由各自 handler 直写、不走 response 信封（见 [ADR 0003](0003-bare-success-error-envelope.md)）。
- 秘密绝不进 `Message`（key/token/prompt 全经 logx 红线脱敏，见 GW-INV-11/17）。

## 相关 / Links

- [ADR 0003 裸成功/error 信封契约](0003-bare-success-error-envelope.md) · [架构](../concepts/architecture.md)
- 不变量：GW-INV-11（key 不泄漏）、wire-code 契约（invariants 附注）
