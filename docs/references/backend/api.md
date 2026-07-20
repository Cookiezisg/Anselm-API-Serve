---
id: DOC-003
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
---

# HTTP API 契约（business / admin / dashboard 三 mux）

> Go 1.22 method+path `ServeMux`。成功默认是裸实体，失败统一 `{"error":{"code","message"[,"details"]}}`；`/v1/models` 刻意保 OpenAI list wrapper。错误逐字值见 [error-codes.md](error-codes.md)，配置边界见 [config.md](config.md)。bearer 只认 `Authorization: Bearer <token>`。

## 1. business mux（`LISTEN_ADDR`，默认 `127.0.0.1:8080`）

外→内中间件：`Recover → DenyCORS → MaxBody(MAX_BODY_BYTES) → mux`。除 `/healthz` 外，固定低基数 RED handler label。生产 Caddy 的 5MiB `request_body` edge cap 与部署值对齐，并将 edge-native 413 映射为同一 `400 BAD_REQUEST` JSON envelope。

| 方法 + 路径 | label | auth | 成功 / 说明 |
|---|---|---|---|
| `POST /v1/install` | `install` | 无 | 每次新建 install + 一次性 token；handler 独立 8KiB body cap；Sybil/PoW 默认 dormant |
| `GET /v1/install/challenge` | `install_challenge` | 无 | 120s 无状态 HMAC PoW challenge |
| `POST /v1/chat/completions` | `chat_completions` | bearer | OpenAI-compatible chat；按 content capability 确定性路由；stream/non-stream 透传安全 body |
| `GET /v1/quota` | `quota` | bearer | 裸 `{limit,used,remaining,resetAt,available}`；前三项是月请求次数，available 也折入 install/global 日成本钱包 |
| `GET /v1/models` | `models` | bearer | `{"object":"list","data":[{"id":PUBLIC_MODEL_ID,"object":"model","owned_by":"anselm-gateway"}]}`，恰一个逻辑模型 |
| `GET /healthz` | 不包 RED | 无 | liveness；不碰 DB/provider |

共享 auth 决策：空/未知 bearer→401，banned→403，lookup fault→500。带 `Origin` 的 `OPTIONS` 一律 403，且不发任何 `Access-Control-*`。

## 2. `POST /v1/chat/completions`

### 2.1 top-level 与 message 白名单

只 decode：

```json
{
  "model": "anselm-auto",
  "messages": [],
  "stream": true,
  "temperature": 0.7,
  "max_tokens": 1024,
  "n": 1,
  "tools": [],
  "tool_choice": "auto"
}
```

未声明 top-level 字段构造性丢弃；`n>1` 拒绝。`tools` / `tool_choice` 与 message 的 `name` / `tool_calls` / `tool_call_id` 作为 opaque JSON 保留，支持完整 tool loop；其中 Kimi 3 返回在 `tool_calls[].extra_content.google.thought_signature` 的签名也原样回传，保证下一步 function call 不被 provider 以 400 拒绝。DeepSeek 历史中的 `reasoning_content` 在文本路由保留，在 Kimi adapter 内剥离。

`messages[].content` 是关闭联合类型：

| wire 形状 | 合法性 / canonical 结果 |
|---|---|
| JSON string | 文本 |
| parts array | 只允许下表三种 part；纯 text parts 按顺序拼接成 string |
| missing / `null` | 仅当 `role="assistant"` 且 `tool_calls` 是非空数组 |
| object/number/bool/未知 part | `400 BAD_REQUEST` |

message 数、每条文本 rune、整个 JSON body 分别受 `MAX_MESSAGES`、`MAX_MESSAGE_CHARS`、`MAX_BODY_BYTES` 约束。`INPUT_TOKEN_CAP` 约束 messages 文本及 `tools` / `tool_choice` / message tool-call JSON 的 UTF-8 byte-fallback estimate（另含 provider framing 余量）；它不声称能估算 media token。

### 2.2 part 逐字形状

文本：

```json
{"type":"text","text":"describe this"}
```

图像：

```json
{
  "type":"image_url",
  "image_url": {
    "url":"data:image/png;base64,<strict-base64>",
    "detail":"auto"
  }
}
```

- URL 必须是 inline data URI；MIME 仅 `image/jpeg`、`image/png`、`image/webp`；base64 必须 canonical/strict，MIME 必须匹配 decoded magic bytes。
- `detail` 可省略；存在时仅 `auto|low|high`。

视频：

```json
{
  "type":"video_url",
  "video_url":{"url":"data:video/mp4;base64,<strict-base64>"}
}
```

- 仅接受 inline MP4 data URI；base64 必须 canonical/strict，声明 MIME 必须匹配 MP4 容器标识。

message `role` 是闭集 `system|user|assistant|tool`，且 tool message 必须带非空 `tool_call_id`。media 只允许在 `role="user"`；整请求 image+video part 数≤`MAX_MEDIA_PARTS`，累计 decoded bytes≤`MAX_MEDIA_DECODED_BYTES`。远程 `http(s)` URL、PDF、file/file_id、未知 MIME/format/part、跨 variant 多余字段，以及 `input_audio` 全部 400；gateway **不 fetch 客户端 URL**。

### 2.3 唯一路由表

路由扫描**完整 history**，与 client `model` 值无关：

| 完整 content | provider | 实际模型 |
|---|---|---|
| 只有 string / text parts / 合法 tool-call 空内容 | DeepSeek | `TEXT_UPSTREAM_MODEL`=`deepseek-v4-flash` |
| 任一 accepted image/video part | Kimi | `MULTIMODAL_UPSTREAM_MODEL`=`kimi-k2.6` |

`PUBLIC_MODEL_ID` 是完整 client-facing alias：空、未知或任意 client `model` 都不能选 provider/价格；stream chunk 与 non-stream completion 的单一、大小写精确顶层 `model` 统一改写为该 alias，duplicate 或 case-fold 等价 key fail closed，真实 provider model 不出 wire（嵌套业务字段不误改）。non-stream 2xx 在写 200 前必须是单一完整 UTF-8 JSON object；SSE 只接受 data-only object、精确 `[DONE]`、空分隔行与被归一成裸 `:` 的 comment heartbeat，任何其它 control/畸形 data 都不透传 provider bytes 并保守结算。选定 provider 后无 fallback。Kimi adapter 剥离跨 provider 的 `reasoning_content`，并使用 K2.6 的默认 thinking 行为；成本仍按完整模型 hard limits 预留。

`KIMI_API_KEY` 未配置时不构造 Kimi backend：纯文本照常；合法多模态在 reserve/Open 前返回 `503 MULTIMODAL_UNAVAILABLE`（`multimodal input is unavailable on this deployment`），不会转 DeepSeek。Kimi 故障同样只返回自身归一错误，不跨 provider。

### 2.4 clamp、stream 与账务可见行为

- `max_tokens` 取正 client 值与 `MAX_TOKENS_CAP`/实际模型 output limit 的较小值；否则取 cap。
- stream request 强加 `stream_options.include_usage=true`；stream/non-stream 都须在 2xx 后等到首个 body byte 才 handoff，`UPSTREAM_HEADER_TIMEOUT_SEC` 覆盖这段等待。response SSE 逐行校验后 relay（仅 data JSON object 可携带业务 payload，并结构化改写唯一的精确顶层 `model`）、写 deadline 每帧滚动；non-stream body 最大读取 8MiB，须为单一 JSON object 后作同一改写。畸形/超限 provider success body 不原样透传。流式 usage 只有在网关完整读到合法终止帧 `[DONE]` 后才可用于退款；此前 EOF、读错或断连即使已见 usage 也保留 full quote。若 `[DONE]` 已读到，随后 client 写失败不抹掉这份终局证据。
- response 只写 gateway 白名单 header：Content-Type/Cache-Control 与 `X-Quota-Limit`、`X-Quota-Reset`；上游 header/auth 不透传。
- upstream failure 同时携带 client-facing `APIError` 与独立 `ChargeExposure`；client code 不决定退款：

| provider call 结果 | Exposure | retry / accounting |
|---|---|---|
| Open 前本地拒绝；明确 3xx/4xx（含 400/401/402/403/429） | `DefinitelyUnbilled` | rollback；只有 401/403 key cooldown 或 key-breaker-open 可换 sibling key，总 attempt≤3 |
| connect/TLS/read error、timeout、5xx、call 中 client-cancel、未知 exposure | `ChargePossible` | 不 retry；full reservation settle |
| 2xx 且首个 body byte 已到达（stream/non-stream） | charge 已可能发生 | 不 retry；non-stream 完整 object 或 stream 完整读到合法 `[DONE]` 后才按累计 usage settle；任一负数、duplicate/case-fold key 或畸形证据 sticky，提前 EOF、usage 缺失/矛盾/不可计价均 full quote |

`ChargePossible` 是 fail-safe 零值。同一个 `UPSTREAM_ERROR` 可对应明确 401/402 refusal（rollback）或 connect/5xx 歧义（full settle），调用方不得从 code/status/message 推导账务。429 与其它明确 3xx/4xx 虽可退款，也不自动 retry。账本状态见 [database.md](database.md)。

## 3. admin mux（`ADMIN_ADDR`，默认 `127.0.0.1:9090`）

无应用鉴权，完全依赖 loopback 物理隔离；绝不反代到公网。

| 方法 + 路径 | 说明 |
|---|---|
| `GET /metrics` | Prometheus；provider label 固定 `deepseek|kimi` |
| `GET /readyz` | DB + disk + cached authenticated `/models` probe：DeepSeek key/固定模型永远必需；配置了 Kimi key 时 Kimi key/固定模型也必须通过；未配 Kimi 不使文本 deployment unready |
| `/debug/pprof/`、命名 pprof 路由 | CPU/heap/trace/cmdline/symbol |
| `GET /debug/vars` | expvar runtime gauges |

## 4. dashboard mux（`DASHBOARD_ADDR`，默认 `127.0.0.1:8081`）

`SecurityHeaders` 覆盖全部路由；`/api/*` 要 session，状态变更 POST 还要 `X-CSRF-Token`。`/healthz`、login/logout 公开但监听仍 loopback-only。

| 方法 + 路径 | session / CSRF | 说明 |
|---|---|---|
| `GET /healthz` | 否 | dashboard liveness |
| `POST /login` / `POST /logout` | 否 | 建立/销毁 session；login 有 per-IP backoff |
| `GET /api/session` | session | session + CSRF token |
| `GET /api/overview` | session | global budget 为 `{day,usedMicroUsd,limitMicroUsd,remainingMicroUsd,unit:"micro_usd"}`；固定带 `providers.deepseek` / `providers.kimi`，各为 `{configured,breakerOpen}`；`upstreamBreakerOpen` 仅保留为两路已配置 provider breaker 的兼容聚合；另有 inflight/open ledger/disk/rate/install 指标 |
| `GET /api/config` | session | secret-free Dump |
| `POST /api/config` | session + CSRF | runtime-hot batch，全有或全无 |
| `GET /api/installs` | session | safe 行；`todaySpendMicroUsd`，无 token/fp/ip |
| `POST /api/installs/ban` / `unban` | session + CSRF | install 状态变更 |
| `GET /api/audit` / `GET /api/export` | session | 审计 / 一致 DB snapshot |
| `GET /` | 否 | embedded SPA/static fallback |

## 5. 错误头

`APIError.Details.retryAfterSec` 存在时，renderer 同步写 `Retry-After` delta-seconds；当前用于 `LOGIN_LOCKED`。状态行前先写 Content-Type/Retry-After，message 永远是静态 client-safe 文本。
