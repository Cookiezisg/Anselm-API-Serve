---
id: DOC-003
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
audience: [human, ai]
---

# HTTP API 契约（business / admin / dashboard 三 mux）

> 与代码逐字对齐：business 来自 `internal/transport/httpapi/router/router.go`，admin 来自 `router/admin.go`，dashboard 来自 `router/dashboard.go`。路由用 Go 1.22 `method+path` `ServeMux` 模式。错误信封与码见 [error-codes.md](error-codes.md)。
> 信封规约（`transport/httpapi/response`）：**成功裸实体（无 wrapper）**；失败 `{"error":{"code","message"[,"details"]}}`。bearer 提取统一走 `response.Bearer`（仅认 `Authorization: Bearer <token>`）。

## 1. business mux（`LISTEN_ADDR`，默认 `127.0.0.1:8080`）

中间件链外→内：`Recover`（X-Request-ID + scoped logger + panic→counter）→ `DenyCORS` → `MaxBody(MAX_BODY_BYTES)` → mux。每条业务路由（**非 `/healthz`**）经 `Mx.Wrap(label,…)` 包 HTTP RED，label 低基数固定。body 上限来自配置 `MAX_BODY_BYTES`（默认 256KiB，重启生效，GW-INV-34）；`/v1/install` 在 handler 内另有独立 8KiB 小上限（未鉴权面）。

| 方法 + 路径 | RED label | handler | 鉴权 | 说明 |
|---|---|---|---|---|
| `POST /v1/install` | `install` | `business/install` | 无 | 领号：每次全新 install 行 + 全新配额池，返回**一次性**新 token；防 Sybil 闸 + PoW（默认 dormant） |
| `GET /v1/install/challenge` | `install_challenge` | `business/challenge` | 无 | 取 PoW challenge（无状态 `base64(rand‖ts‖HMAC[:16])`，120s TTL） |
| `POST /v1/chat/completions` | `chat_completions` | `business/chat` | bearer | 代理 DeepSeek；悲观三闸门 reserve→settle/rollback；流式透传 |
| `GET /v1/quota` | `quota` | `business/quota` | bearer | 查当前 install 配额用量 |
| `GET /v1/models` | `models` | `business/models` | bearer | 返回 `MODEL_ALLOWLIST` 模型目录 |
| `GET /healthz` | （**不包**） | `business/healthz` | 无 | liveness；**绝不碰 DB**（GW-INV-13），故意不挂 RED |

鉴权单点：install service 同时充当 quota/models 的 `Authenticator`（一次 bearer→install 查找，绝无第二校验器）。404/405/CORS 由链 + mux 默认行为产生；`DenyCORS` 对带 `Origin` 的 `OPTIONS` 返回 403 且**绝不发** `Access-Control-*` 头。

## 2. admin mux（`ADMIN_ADDR`，默认 `127.0.0.1:9090`，loopback-only）

无中间件鉴权——靠**物理回环**隔离（bootstrap 的 `requireLoopback`）。`Metrics` 为 nil 时不挂 `/metrics`。

| 方法 + 路径 | 来源 | 说明 |
|---|---|---|
| `GET /metrics` | 注入 `deps.Metrics`（promhttp） | Prometheus 暴露 |
| `GET /readyz` | `admin.Ready(deps.Ready)` | readiness 检查 |
| `/debug/pprof/` | `pprof.Index`（+ 命名 profile） | 运行期画像 |
| `/debug/pprof/cmdline` | `pprof.Cmdline` | |
| `/debug/pprof/profile` | `pprof.Profile` | CPU profile |
| `/debug/pprof/symbol` | `pprof.Symbol` | |
| `/debug/pprof/trace` | `pprof.Trace` | |
| `GET /debug/vars` | `expvar.Handler()` | 免抓取的运行期 gauge（goroutines / heap_alloc_bytes） |

🔴 pprof / expvar / readyz 暴露运行期内部 + DoS 面 + 依赖状态，**必须 loopback-only、绝不反代**（GW-INV-13）。

## 3. dashboard mux（`DASHBOARD_ADDR`，默认 `127.0.0.1:8081`，loopback-only）

`SecurityHeaders` 包裹**所有**路由。公开：`/healthz` `/login` `/logout`；`/api/*` 全部在 `RequireSession` 之后。状态变更 POST 在 `RequireSession` 之后**再**校验 `X-CSRF-Token`（handler 内）。`/api/*`、`/login`、`/logout` 模式按 ServeMux 优先级压过 `"/"` SPA 兜底，故 SPA 永不能遮蔽鉴权路由。

| 方法 + 路径 | handler | session | 说明 |
|---|---|---|---|
| `GET /healthz` | `Handler.Healthz` | 公开 | liveness |
| `POST /login` | `Handler.Login` | 公开 | 建立 session（bcrypt + 常时用户名比对 + per-IP 退避，`LOGIN_LOCKED`） |
| `POST /logout` | `Handler.Logout` | 公开 | 销毁 session |
| `GET /api/session` | `Handler.Session` | 需 | 当前 session + CSRF token |
| `GET /api/overview` | `Handler.Overview` | 需 | 概览指标 |
| `GET /api/config` | `Handler.GetConfig` | 需 | 配置只读面（脱敏，机密不出现，见 [config.md](config.md)） |
| `POST /api/config` | `Handler.PostConfig` | 需 + CSRF | 热改 runtime-hot 项（全有或全无） |
| `GET /api/installs` | `Handler.Installs` | 需 | install 列表 |
| `POST /api/installs/ban` | `Handler.Ban` | 需 + CSRF | 封禁 install |
| `POST /api/installs/unban` | `Handler.Unban` | 需 + CSRF | 解封 install |
| `GET /api/audit` | `Handler.Audit` | 需 | 审计 |
| `GET /api/export` | `Handler.Export` | 需 | 导出 |
| `GET /` | `Static`（nil ⇒ 最小 index shell） | 公开 | SPA + 静态资源（`infra/webassets`） |

## 4. 响应头与 Retry-After

`response.WriteError` 渲染信封；当 `APIError.Details` 含 `retryAfterSec`（目前仅 `LOGIN_LOCKED`）时同步置 `Retry-After` 头，使头与 body lockstep。成功体 `Content-Type: application/json`，状态行前先写头。
