---
id: DOC-003
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-21
review-due: 2026-10-19
audience: [human, ai]
---

# HTTP API 契约（business / admin / dashboard 三 mux）

> Go 1.22 method+path `ServeMux`。成功默认是裸实体，失败统一 `{"error":{"code","message"[,"details"]}}`；`/v1/models` 刻意保 OpenAI list wrapper。错误逐字值见 [error-codes.md](error-codes.md)，配置边界见 [config.md](config.md)。业务身份统一采用 [ADR-0015](../../decisions/0015-device-bound-request-proof.md) 的 Ed25519 逐请求设备证明，无 bearer token。

## 1. business mux（`LISTEN_ADDR`，默认 `127.0.0.1:8080`）

外→内中间件：`Recover → DenyCORS → MaxBody(MAX_BODY_BYTES) → mux`。除 `/healthz` 外，固定低基数 RED handler label。生产 Caddy 的 5MiB `request_body` edge cap 与部署值对齐，并将 edge-native 413 映射为同一 `400 BAD_REQUEST` JSON envelope。

| 方法 + 路径 | label | auth | 成功 / 说明 |
|---|---|---|---|
| `POST /v1/install` | `install` | 注册 proof + public key | 公钥登记；同 key 幂等返回 `{installId,monthlyQuota,resetAt}`；handler 独立 8KiB body cap；Sybil/PoW 默认 dormant |
| `GET /v1/install/challenge` | `install_challenge` | 无 | 120s 无状态 HMAC PoW challenge |
| `GET /v1/proof/challenge` | `proof_challenge` | 无 | 5 分钟 HMAC-authenticated request nonce，client 可缓存 |
| `POST /v1/chat/completions` | `chat_completions` | device proof | OpenAI-compatible chat；proof 绑定 exact body；按 content capability 确定性路由 |
| `GET /v1/quota` | `quota` | device proof | 裸 `{limit,used,remaining,resetAt,available}`；前三项是月请求次数，available 也折入 operator 月钱包 |
| `GET /v1/models` | `models` | device proof | `{"object":"list","data":[{"id":PUBLIC_MODEL_ID,"object":"model","owned_by":"anselm-gateway"}]}`，恰一个逻辑模型 |
| `GET /healthz` | 不包 RED | 无 | liveness；不碰 DB/provider |

设备证明头：protected call 带 `X-Anselm-Install-ID` + `X-Anselm-Proof`；registration 改带 `X-Anselm-Public-Key` + `X-Anselm-Proof`。proof payload 固定 `{v,kid,iat,jti,nonce,htm,htu,bh}`，Ed25519 签名覆盖 base64url payload；`htu` 是 lowercase authority + path/query，`bh` 是 exact body SHA-256。空/未知 install→401，坏签名/过期→401，重复 jti→409，banned→403。带 `Origin` 的 `OPTIONS` 一律 403，且不发任何 `Access-Control-*`。

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

未声明 top-level 字段构造性丢弃；`n>1` 拒绝。client-supplied `thinking` / `reasoning_effort` 不在白名单内，也不能改变 provider knob。`max_tokens` 是 OpenAI 兼容调用参数：正数值会在选定模型 output hard limit 与 `MAX_TOKENS_CAP` 内 clamp 后转发；缺省或非正值不写入上游 body，但 DeepSeek 账本仍按保守输出上限预留。`tools` / `tool_choice` 与 message 的 `name` / `tool_calls` / `tool_call_id` 作为 opaque JSON 保留，支持完整 tool loop；其中 Kimi 3 返回在 `tool_calls[].extra_content.google.thought_signature` 的签名也原样回传，保证下一步 function call 不被 provider 以 400 拒绝。DeepSeek 历史中的 `reasoning_content` 在文本路由保留，在 Kimi adapter 内剥离。

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

```json
{
  "type":"input_audio",
  "input_audio":{"data":"<strict-base64>","format":"wav|mp3"}
}
```

- 音频 `data` 是 raw strict base64（不是 data URI）；`format` 仅 `wav|mp3`，且必须匹配文件魔数。当前部署在任一 reserve/Open 前返回 `503 AUDIO_UNAVAILABLE`，因此它是**协议已知但尚未路由**的能力。

message `role` 是闭集 `system|user|assistant|tool`，且 tool message 必须带非空 `tool_call_id`。media 只允许在 `role="user"`；整请求 image+video+audio part 数≤`MAX_MEDIA_PARTS`，累计 decoded bytes≤`MAX_MEDIA_DECODED_BYTES`。远程 `http(s)` URL、PDF、file/file_id、未知 MIME/format/part、跨 variant 多余字段全部 400；gateway **不 fetch 客户端 URL**。

### 2.3 唯一路由表

路由扫描**完整 history**，与 client `model` 值无关：

| 完整 content | provider | 实际模型 |
|---|---|---|
| 只有 string / text parts / 合法 tool-call 空内容 | DeepSeek | `TEXT_UPSTREAM_MODEL`=`deepseek-v4-flash` |
| 任一 accepted image/video part | Kimi | `MULTIMODAL_UPSTREAM_MODEL`=`kimi-k2.6` |
| 任一 accepted audio part | 无 | reserve/Open 前 `503 AUDIO_UNAVAILABLE` |

`PUBLIC_MODEL_ID` 是完整 client-facing alias：空、未知或任意 client `model` 都不能选 provider/价格；stream chunk 与 non-stream completion 的单一、大小写精确顶层 `model` 统一改写为该 alias，duplicate 或 case-fold 等价 key fail closed，真实 provider model 不出 wire（嵌套业务字段不误改）。non-stream 2xx 在写 200 前必须是单一完整 UTF-8 JSON object；SSE 只接受 data-only object、精确 `[DONE]`、空分隔行与被归一成裸 `:` 的 comment heartbeat，任何其它 control/畸形 data 都不透传 provider bytes 并保守结算。选定 provider 后无 fallback。

网关统一产品档位由服务端注入，client 不能改：

| route | provider knobs | context / output hard limit | `max_tokens` wire/accounting behavior |
|---|---|---:|---:|
| text / DeepSeek V4 Flash | `thinking={"type":"enabled"}` + `reasoning_effort="high"` | 1,000,000 input / 384,000 output | positive client value forwarded after `min(client,MAX_TOKENS_CAP,384000)`; absent value omitted on wire and quoted at `min(MAX_TOKENS_CAP,384000)` |
| media / Kimi K2.6 | `thinking={"type":"enabled"}`；不传 `reasoning_effort` | 262,144 input / 32,768 output | positive client value forwarded after `min(client,MAX_TOKENS_CAP,32768)`; Kimi cost reservation remains full hard-limit because compatibility usage cannot prove hidden-thinking/media upper bounds |

Kimi adapter 剥离跨 provider 的 `reasoning_content`，但保留 opaque `tool_calls`。统一对外上下文若需要单值，应按多模态保守展示 `256K`；纯文本实际走 DeepSeek 的 `1M`。成本仍按冻结 provider/model rate card 预留：DeepSeek 用文本 prompt estimate + bounded output quote；Kimi 用完整模型 input/output hard limits 后按自洽 usage 退款。

`KIMI_API_KEY` 未配置时不构造 Kimi backend：纯文本照常；合法图片/视频在 reserve/Open 前返回 `503 MULTIMODAL_UNAVAILABLE`（`multimodal input is unavailable on this deployment`），不会转 DeepSeek。音频不依赖 Kimi 配置，始终先返回 `503 AUDIO_UNAVAILABLE`；未来音频 route 只能经新 capability 决策启用。Kimi 故障同样只返回自身归一错误，不跨 provider。

### 2.4 输出档位、stream 与账务可见行为

- `max_tokens` 由 caller 控制但受边界保护：正数值转发为 `min(client, MAX_TOKENS_CAP, selected model output limit)`；缺省或非正值不转发。DeepSeek quote 使用同一 bounded output；缺省时使用 `min(MAX_TOKENS_CAP, selected model output limit)`。Kimi quote 始终保守使用完整 K2.6 hard limits。
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

`SecurityHeaders` 覆盖全部路由，dashboard 永远只监听 loopback。`GET /api/bootstrap` 公开但只返回 `{authMode}`：SPA 据此选择 UI。`builtin` 模式下其余 `/api/*` 要 session，状态变更 POST 还要 `X-CSRF-Token`；`external` 模式下前置 IAP 是唯一鉴权，Go 不创建 session/cookie/CSRF，API 直接提供能力。

| 方法 + 路径 | session / CSRF | 说明 |
|---|---|---|
| `GET /healthz` | 否 | dashboard liveness |
| `GET /api/bootstrap` | 否 | `{authMode:"builtin"|"external"}`，无 identity/secret |
| `POST /login` / `POST /logout` | `builtin` only / 否 | 建立/销毁 session；login 有 per-IP backoff |
| `GET /api/session` | `builtin` session | session + CSRF token；external mode 不注册（404） |
| `GET /api/overview` | builtin: session；external: IAP | global budget 为 `{day,usedMicroUsd,limitMicroUsd,remainingMicroUsd,unit:"micro_usd"}`，`day` 当前承载 `YYYY-MM` 月预算窗口；固定带 `providers.deepseek` / `providers.kimi`，各为 `{configured,breakerOpen}`；`upstreamBreakerOpen` 仅保留为两路已配置 provider breaker 的兼容聚合；另有 inflight/open ledger/disk/rate/install 指标 |
| `GET /api/config` | builtin: session；external: IAP | secret-free Dump |
| `POST /api/config` | builtin: session + CSRF；external: IAP | runtime-hot batch，全有或全无 |
| `GET /api/installs` | builtin: session；external: IAP | safe 行；`todaySpendMicroUsd`，无 token/fp/ip |
| `POST /api/installs/ban` / `unban` | builtin: session + CSRF；external: IAP | install 状态变更 |
| `POST /api/quota/reset` | builtin: session + CSRF；external: IAP | body `{reason}`（非空，审计原因）；原子清零当前 `RESET_TZ` 月所有 `quota_monthly.requests>0`，返回 `{period,resetInstalls}`；任何 `spend_ledger(open)` 存在时 `409 QUOTA_RESET_BUSY`；绝不改 pUSD 钱包、日统计或 ledger 历史 |
| `GET /api/audit` / `GET /api/export` | builtin: session；external: IAP | 审计 / 一致 DB snapshot |
| `GET /` | 否 | embedded SPA/static fallback |

## 5. 错误头

`APIError.Details.retryAfterSec` 存在时，renderer 同步写 `Retry-After` delta-seconds；当前用于 `LOGIN_LOCKED`。状态行前先写 Content-Type/Retry-After，message 永远是静态 client-safe 文本。
