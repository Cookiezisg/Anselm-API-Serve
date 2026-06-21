---
id: DOC-018
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0009 — React 管理面板 Clean Architecture + go:embed

## 背景 / Context

管理面板需随单二进制分发（无独立前端部署）、在 loopback dashboard 口后用 session+CSRF 守护、且其 TS 类型须与后端裸实体契约对得上。前端若散乱写 inline fetch、错误分支各处不一，会与后端 wire code 契约漂移。

## 决策 / Decision

**SPA 分层（api-client / types-mirror / auth-context / pages），`go:embed` 进 `infra/webassets`，session+CSRF 后置。**

1. **分层（依赖单向下流）**：`pages/`（每路由一屏、无 inline fetch）→ `auth/AuthContext`（session + csrfToken + `/api/session` F5 重水合）→ `lib/api.ts`（**唯一传输层**：fetch 封装、注 `X-CSRF-Token`、401→跳登录、`ApiError(status,code,message,details)`、客户端独有 `NETWORK_ERROR(0)`）+ `lib/types.ts`（**契约镜像**）。
2. **契约镜像纪律**：`lib/types.ts` 手镜像每个后端裸实体 struct（`loginResponse{csrfToken,user}`、`overviewResponse`、`installRow`、`AuditEvent`、`DumpItem{key,value,editable,restartRequired,min?,max?}`、error `{error:{code,message,details?}}`）；错误只认 UPPER_SNAKE `code`，绝不认 OpenAI 式 `type/param`（见 [ADR 0003](0003-bare-success-error-envelope.md)）。
3. **go:embed 分发**：`npm run build` → `ui/dist/` → `infra/webassets` 内 `//go:embed all:ui/dist`；`webassets` 服 `/static/`（显式 `contentTypeFor`）+ 未知路径回退 `index.html`（SPA shell），全 dashboard 响应带 `securityHeaders`（CSP `script-src 'self'`、nosniff、no-store、frame-ancestors 'none'）。
4. **鉴权**（GW-INV-19）：dashboard 仅当 `DASHBOARD_USER`+`DASHBOARD_PASSWORD` 同时设才启（半配 fail-fast）；bcrypt + 常时用户名比对 + `crypto/rand` session token（`rand.Read` 失败 panic、绝不弱值）；`HttpOnly; Secure; SameSite=Strict` cookie；CSRF 经 `X-CSRF-Token`；per-IP 登录退避；除 `/healthz` 外每路过 `requireSession`。

## 理由 / Rationale

- 单传输层 + 契约镜像把「与后端 wire code 同步」收敛到一处，错误分支统一、漂移面最小。
- `go:embed` 让面板随单二进制走、零额外部署，契合 local-first 单运营者形态。

## 取舍与后果 / Consequences

**为何不选：**

- **inline fetch / 各页各写错误处理**：与后端契约多点漂移，错误分支不一致。
- **前端独立部署 / 反代静态资源**：违背单二进制分发，徒增运维件。
- **自动生成 TS 类型**：薄网关手镜像成本极低且更可控；引代码生成链不值。

**B0 状态说明（构造性 + 回归守门，非 live 缺陷）：**

旧 spec 把 B0（clean checkout 不编译：`//go:embed all:ui/dist` 但 `internal/dashboard/ui/` 零跟踪文件）记为 open。**经核已解决**——`ui/dist` embed target 已提交、`go build ./...` clean。本决策因此把 B0 当**已修复**对待：保留 CI **fresh-clone build gate** + npm 构建漂移守门（`npm ci && npm run build && git diff --exit-code` + 全新检出 `go build ./...`）作为**回归守卫**，而非描述为活缺陷。embed target 必须始终提交（或在 Go job 前于 CI 构建）。

**后果：**

- 前端居 `internal/dashboard/ui/`，构建产物经 `infra/webassets` 嵌入。
- `overviewResponse.recent` 的 `rates{QPS,ErrorRate,WindowSec}` 字段名须在 references/api 钉死，供 types-mirror 对镜（见 [ADR 0011](0011-fault-classification-excludes-cancel-429.md) 关联的 B16 服务端化）。

## 相关 / Links

- [ADR 0003 裸成功信封](0003-bare-success-error-envelope.md) · [ADR 0004 三监听口](0004-three-physically-isolated-listeners.md) · [ADR 0011 故障分类](0011-fault-classification-excludes-cancel-429.md) · [架构](../concepts/architecture.md)
- 不变量：GW-INV-19（dashboard 鉴权）
