---
id: DOC-013
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0004 — 三物理隔离监听口（业务 / admin / dashboard）

## 背景 / Context

网关同时暴露三类性质迥异的面：对外业务代理、运维内省（pprof/expvar/metrics/readyz）、管理面板。把它们挤在一个监听口上靠路由前缀 + 中间件区分，意味着「内省口是否暴露」退化成一个**运行时鉴权判断**——判断错一次，pprof/expvar 就成了公网 DoS 面 + 内部结构泄漏。物理隔离把「能不能访问」从软件判断升级为**网络层事实**。

## 决策 / Decision

**三个独立监听口、地址必须互异、admin 必须 loopback。**

| 口 | 默认地址 | 暴露 | 控制手段 |
|---|---|---|---|
| 业务 | `0.0.0.0:8080`（Caddy 前置，`LISTEN_ADDR` 默认 `127.0.0.1:8080`） | `/v1/*`、`/install`、`/healthz` | Caddy 终止 TLS；token/quota 鉴权 |
| admin | `127.0.0.1:9090`（`ADMIN_ADDR`） | `/metrics`、`/readyz`、`/debug/pprof/*`、`/debug/vars` | **loopback-only、无鉴权 = 物理控制**（绑定即 fail-fast） |
| dashboard | `127.0.0.1:8081`（`DASHBOARD_ADDR`） | 管理面板 `/api/*` + SPA | loopback + session/CSRF（见 [ADR 0009](0009-react-dashboard-clean-architecture.md)） |

1. **三地址必须物理互异**：`config.Load` 发现 `LISTEN_ADDR`/`ADMIN_ADDR`/`DASHBOARD_ADDR` 任两者相等 → 具名 fail-fast（避免运行时不透明的 "address already in use"，GW-INV-18）。
2. **admin 必须 loopback**：`bootstrap` 绑定前 `requireLoopback` 校验——拒空 host；IP 字面量经 `IsLoopback` 判；hostname 则要求 `net.LookupIP` 每个结果都是 loopback（不盲信 "localhost"，hosts/NSS 可被改写）。非 loopback / `0.0.0.0` / 裸 `:port` 一律拒（GW-INV-13）。
3. **admin 无鉴权是刻意的**：loopback 本身即访问控制；在已物理隔离的口上再加鉴权是冗余，且会让运维抓取（Prometheus 本机拉）凭空复杂。

## 理由 / Rationale

- pprof/expvar 暴露运行时内部 + DoS 面，readyz 泄漏依赖状态——这些**绝不能上公网**。物理 loopback 比「中间件判断 + 别配错」可靠一个数量级。
- 启动期 fail-fast 优于运行时撞口：错误在装配阶段被具名捕获，而非服务半启动后不透明崩。

## 取舍与后果 / Consequences

**为何不选：**

- **单口 + 路由前缀 + 鉴权中间件区分内省**：内省暴露退化成软件判断，错一次即灾难；本决策用网络事实取代判断。
- **admin 也加鉴权**：在 loopback 口上是冗余，徒增运维抓取复杂度。
- **dashboard 与 admin 合一**：两者读者/认证模型不同（运维抓取 vs 人类登录），合一会逼 admin 口也背 session/CSRF。

**后果：**

- `bootstrap` 装配三个 `http.Server` + 三个 mux builder（`router/`）。
- admin/dashboard 自绑（重启有短暂窗口、已记录可接受）；业务口偏好 socket-activation 保 backlog（见 [ADR 0010](0010-systemd-socket-activation.md)）。
- TLS 仅由 Caddy 终止，Go 进程绑 loopback（GW-INV-18）。

## 相关 / Links

- [ADR 0009 React dashboard](0009-react-dashboard-clean-architecture.md) · [ADR 0010 socket-activation](0010-systemd-socket-activation.md) · [架构](../concepts/architecture.md)
- 不变量：GW-INV-13（admin loopback）、GW-INV-18（三口互异 + Caddy TLS）、GW-INV-19（dashboard 鉴权）
