---
id: DOC-002
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-06-21
review-due: 2026-09-19
audience: [human, ai]
---

# 后端总览（backend overview）

> 模块 `github.com/sunweilin/anselm/gateway`，二进制 `cmd/gateway`。本篇是 `references/backend/` 的**导航 + 物理事实索引**：三监听器、四层依赖方向、六域、运行期形态。深入查同目录五篇硬契约：[api.md](api.md) · [config.md](config.md) · [database.md](database.md) · [error-codes.md](error-codes.md) · [invariants.md](invariants.md)。架构心智模型见 [../../concepts/architecture.md](../../concepts/architecture.md)。

## 1. 三个物理隔离监听器（ADR-004 / GW-INV-13/18）

启动期 `LoadBase` 校验三个地址**两两互异**（相等即 fail-fast，指名冲突），各自一个 `http.Server`：

| 监听器 | env 键 | 默认 | 装配函数 | 鉴权 | 暴露面 |
|---|---|---|---|---|---|
| business | `LISTEN_ADDR` | `127.0.0.1:8080` | `router.BuildHandler` | bearer token（install） | 公网经 Caddy；TLS 由 Caddy 终结 |
| admin | `ADMIN_ADDR` | `127.0.0.1:9090` | `router.BuildAdminHandler` | 无（物理 loopback-only） | `/metrics` `/readyz` `/debug/pprof/*` `/debug/vars`，**绝不反代** |
| dashboard | `DASHBOARD_ADDR` | `127.0.0.1:8081` | `router.BuildDashboardHandler` | session cookie + CSRF | 管理后台 SPA + `/api/*` |

admin 的免鉴权靠**物理回环**而非中间件：`/debug/pprof/*` `/debug/vars` 暴露 goroutine/heap/cpu 画像与运行期 gauge，只能在隔离 admin 监听器上服务，必须 loopback-only 绑定（GW-INV-13）。

## 2. 四层 Clean Architecture，依赖单向内指

```
domain  ← app  ← transport
                ↖ infra（实现 app/domain 声明的 port）
bootstrap 在最外层装配全部
```

| 层 | 包根 | 纪律 |
|---|---|---|
| domain | `internal/domain/*` | 纯类型 + 规则，零 I/O（无 `os` / `net/http` / `database/sql`）。`config`、`apierr` 在此 |
| app | `internal/app/*` | 用例服务（`chat` / `install` / `quota` / `model` …），依赖 domain + port 接口 |
| transport | `internal/transport/httpapi/*` | HTTP 装配、中间件、handler；可依赖 app，**绝不 import infra** |
| infra | `internal/infra/*` | port 的具体实现（`sqlite`、`configprovider`、`metrics`、`webassets`） |

transport 保持 infra-free 的手法：把 infra 能力声明成结构化接口注入。例：`router.Wrapper`（RED 度量）由 `*infra/metrics.Metrics` 结构化满足；admin mux 的 `Metrics http.Handler` 由 bootstrap 传入。

## 3. 六域关注点

| 域 | app 包 | 入口端点 | 权威契约 |
|---|---|---|---|
| quota | `app/quota` | `GET /v1/quota` | [invariants.md](invariants.md) A 组（悲观三闸门记账） |
| install | `app/install` | `POST /v1/install` · `GET /v1/install/challenge` | GW-INV-12/16/20、防 Sybil + PoW 三态 |
| chat | `app/chat` | `POST /v1/chat/completions` | GW-INV-31..37 输入护栏、GW-INV-01..09 记账 |
| model | `app/model` | `GET /v1/models` | GW-INV-35 白名单强改写 |
| health | — | `GET /healthz`（三监听器各一）· admin `/readyz` | GW-INV-13（liveness 不碰 DB） |
| dashboard | `app/dashboard` | dashboard `/api/*` | GW-INV-19 session/CSRF/backoff |

## 4. 错误线缆契约（ADR-002/003）

单一结构化错误类型 `domain/apierr.APIError`（status + UPPER_SNAKE wire code + message [+ details]）。transport `response` 渲染：**成功裸实体、失败 `{"error":{"code","message"[,"details"]}}` 信封**；非 `*APIError` 归一 `500 INTERNAL`，绝不泄露上游 body / key。全码表见 [error-codes.md](error-codes.md)。

## 5. 持久化

单文件 SQLite，WAL；写池 `MaxOpenConns=1`（单写者，悲观原子 reserve 的地基）+ 有界只读池。schema 前向 only、版本化迁移（`schema_migrations`）。表与列见 [database.md](database.md)。

## 6. 配置三层级

runtime-hot（后台可改、存 `settings` 表、原子热生效）/ secret-env-only（机密：env only、绝不入库、dump 掩码）/ startup-hard（监听 / tz / DB 路径 / 内存预算项：env only，改需重启）。全表与边界见 [config.md](config.md)。
