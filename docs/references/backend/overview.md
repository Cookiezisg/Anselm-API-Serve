---
id: DOC-002
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-24
review-due: 2026-10-22
audience: [human, ai]
---

# 后端总览（backend overview）

> 模块 `github.com/sunweilin/anselm/gateway`，二进制 `cmd/gateway`。一个 client-facing 逻辑模型，文本与受支持图片/视频同走 Qwen3.7 Plus；内容形状决定的是**能力与准入**，不是 provider。provider token 先换成 pUSD 再进入共享成本账本。本篇是三监听器、依赖方向、七域与运行期形态的导航。深入契约：[api.md](api.md) · [config.md](config.md) · [database.md](database.md) · [error-codes.md](error-codes.md) · [invariants.md](invariants.md)。

## 1. 三个物理隔离监听器（ADR-004 / GW-INV-13/18）

启动期 `LoadBase` 校验三个地址**两两互异**（相等即 fail-fast，指名冲突），各自一个 `http.Server`：

| 监听器 | env 键 | 默认 | 装配函数 | 鉴权 | 暴露面 |
|---|---|---|---|---|---|
| business | `LISTEN_ADDR` | `127.0.0.1:8080` | `router.BuildHandler` | Ed25519 device proof | 公网经 Caddy；TLS 由 Caddy 终结 |
| admin | `ADMIN_ADDR` | `127.0.0.1:9090` | `router.BuildAdminHandler` | 无（物理 loopback-only） | `/metrics` `/readyz` `/debug/pprof/*` `/debug/vars`，**绝不反代** |
| dashboard | `DASHBOARD_ADDR` | `127.0.0.1:8081` | `router.BuildDashboardHandler` | 恒定挂载;鉴权归前置 IAP,进程内无 credential/session/CSRF | 管理后台 SPA + `/api/*` |

admin 的免鉴权靠**物理回环**而非中间件：`/debug/pprof/*` `/debug/vars` 暴露 goroutine/heap/cpu 画像与运行期 gauge，只能在隔离 admin 监听器上服务，必须 loopback-only 绑定（GW-INV-13）。

## 2. 四层 Clean Architecture，依赖单向内指

```
domain  ← app  ← transport
                ↖ infra（实现 app/domain 声明的 port）
bootstrap 在最外层装配全部
```

| 层 | 包根 | 纪律 |
|---|---|---|
| domain | `internal/domain/*` | 纯类型 + 规则，零 I/O；含 strict chat union、billing rate cards/Plan、quota/config/apierr |
| app | `internal/app/*` | 用例服务（`chat` / `install` / `quota` / `model` …），依赖 domain + port 接口 |
| transport | `internal/transport/httpapi/*` | HTTP 装配、中间件、handler；可依赖 app，**绝不 import infra** |
| infra | `internal/infra/*` | port 实现：SQLite、provider-local upstream、chatprovider registry、config/metrics/disk/webassets |

transport 保持 infra-free 的手法：把 infra 能力声明成结构化接口注入。例：`router.Wrapper`（RED 度量）由 `*infra/metrics.Metrics` 结构化满足；admin mux 的 `Metrics http.Handler` 由 bootstrap 传入。

## 3. 七域关注点

| 域 | app 包 | 入口端点 | 权威契约 |
|---|---|---|---|
| quota | `app/quota` | `GET /v1/quota` | provider-aware pUSD 双闸预留（install 月次数 + operator 月预算）与显式 ledger 状态（A 组） |
| install | `app/install` + `app/deviceproof` | `POST /v1/install` · `GET /v1/install/challenge` · `GET /v1/proof/challenge` | GW-INV-12/16/20、逐请求 proof、防 Sybil + PoW 三态 |
| chat | `app/chat` | `POST /v1/chat/completions` | GW-INV-31..44 输入/capability/provider，GW-INV-01..10 记账 |
| media | `app/media` | `POST /v1/media/uploads` · `GET/DELETE/PUT /v1/media/uploads/{id}` · `POST /complete` | install-bound resumable staging（GET 是 ambiguous PUT 后权威 cursor；DELETE 先 durable abort 后删暂存；complete 以 magic 重验 MIME）、opaque lease、HMAC-derived private fetch credential、startup/periodic TTL recovery；字节不进 SQLite |
| model | `app/model` | `GET /v1/models` | 恰一个 `PUBLIC_MODEL_ID`；client model 不选 provider |
| health | — | `GET /healthz`（三监听器各一）· admin `/readyz` | GW-INV-13（liveness 不碰 DB）；cached authenticated provider/model probe |
| dashboard | `app/dashboard` | dashboard `/api/*` | GW-INV-19 auth-mode trust boundary |

## 4. 错误线缆契约（ADR-002/003）

单一结构化错误类型 `domain/apierr.APIError`（status + UPPER_SNAKE wire code + message [+ details]）。transport `response` 渲染：**成功裸实体、失败 `{"error":{"code","message"[,"details"]}}` 信封**；非 `*APIError` 归一 `500 INTERNAL`，绝不泄露上游 body / key。全码表见 [error-codes.md](error-codes.md)。

## 5. 持久化

单文件 SQLite，WAL；写池 `MaxOpenConns=1` + 有界只读池。v2 在单 `BEGIN IMMEDIATE` 原子预留月额度与 install/provider/global 三个 pUSD 钱包；`spend_ledger` 显式 `open→settled|rolled_back|orphaned`。v1 token accounting 表只读保留供审计。schema 见 [database.md](database.md)。

## 6. 配置三层级

runtime-hot / secret-env-only / startup-hard 三层。Qwen key 是启动必需（每一条路由都要它）；`DASHSCOPE_WORKSPACE_ID` 推导 Qwen 新加坡 endpoint。provider model/URL 启动冻结，成本/media caps 可按 registry 热改。全表见 [config.md](config.md)。
