---
id: DOC-012
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0003 — 裸成功 / error 信封 API 契约（刻意背离 Foryx 的 `{data}`）

## 背景 / Context

Foryx 后端成功响应统一裹 `{"data": ...}`。本网关的客户端是**两类既定消费者**：① OpenAI 兼容的 chat/models 客户端，期望裸实体或 OpenAI list 形态；② 自家 React dashboard，对接裸实体 + 统一 error。强加 `{data}` 包裹会破坏 OpenAI 兼容、并给 dashboard 平添一层无意义解包。

## 决策 / Decision

**成功 = 裸实体 JSON（无 wrapper）；失败 = `{"error":{"code","message"[,"details"]}}`；刻意不学 Foryx 的 `{data}`。**

| 端点族 | 成功形态 | 理由 |
|---|---|---|
| 业务 / dashboard 实体口 | **裸实体**（如 `loginResponse{csrfToken,user}`、`installRow`、`overviewResponse`） | 无包裹，前端直读 |
| `/v1/models` | OpenAI `{"object":"list","data":[...]}` | 保 OpenAI 兼容（此处 `data` 是 OpenAI 契约，非 Foryx 包裹） |
| `/healthz` `/readyz` `/metrics` | 各自非信封形态（纯文本 / Prometheus exposition） | 探针/抓取契约固定 |
| 任何失败 | `{"error":{"code","message","details?"}}`，非 `APIError` 归一 `500 INTERNAL` | 单一错误信封（见 [ADR 0002](0002-unified-structured-error-type.md)） |

- `response.WriteJSON` 直写裸实体；`response.WriteError*` 写 error 信封。
- 前端 `lib/api.ts` 据此：成功体**直接返回解析后的 JSON、不解 `{data}`**；仅错误体从 `.error` 取 `ApiError(status,code,message,details)`；客户端独有 `NETWORK_ERROR(0)` 永不由服务端发出。

## 理由 / Rationale

- OpenAI 兼容是硬约束：chat 客户端不认 `{data}` 包裹。统一包裹会逼网关在 `/v1/chat/completions` 破例，破例即不统一。
- 裸成功 + 单 error 信封已是「成功/失败」的最小可判别契约：成功体无 `error` 键、失败体有——前端 `if (error)` 一行分流。`{data}` 包裹不增加任何可判别性。

## 取舍与后果 / Consequences

**为何不选：**

- **Foryx `{data}` 统一包裹**：破坏 OpenAI 兼容、给 dashboard 加无谓解包层、对错误分流零增益。本网关消费者形态与 Foryx 不同，不盲从其约定。
- **成功也用信封 `{ok:true,data}`**：同上，纯仪式。

**后果：**

- React types-mirror（`lib/types.ts`）逐一手镜像后端裸实体 struct；错误分支只认 UPPER_SNAKE `code`，绝不认 OpenAI 式 `type/param`（不存在）。
- 探针/抓取口（healthz/readyz/metrics）与信封口的差异在 [架构](../concepts/architecture.md) 与 references/api 明列，handler 直写、不经 response 信封。
- `/v1/quota` 的 `available` 布尔由 `app/quota.View` 拥有计算（`remaining>0 && budgetUsed < GlobalDailyBudget`），handler 仅拷贝 `v.Available`（守分层）。

## 相关 / Links

- [ADR 0002 统一错误类型](0002-unified-structured-error-type.md) · [ADR 0009 React dashboard](0009-react-dashboard-clean-architecture.md) · [架构](../concepts/architecture.md)
