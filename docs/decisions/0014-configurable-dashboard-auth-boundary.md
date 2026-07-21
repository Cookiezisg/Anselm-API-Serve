---
id: DOC-030
type: decision
status: active
owner: @weilin
created: 2026-07-21
reviewed: 2026-07-21
review-due: 2099-12-31
audience: [human, ai]
---

# 0014 — 可配置的 Dashboard 认证边界

## 背景 / Context

Dashboard 是单二进制内嵌的 operator capability 面。原实现把 Go 自身的账号密码、session、CSRF 与登录退避固定为唯一入口，适合经 SSH tunnel 的独立部署；但同一二进制也需要能部署在 Cloudflare Access、Tailscale 或公司 IAP 后面。若前置 IAP 已完整认证，第二套 Go 登录既增加操作摩擦，也让 SPA 与服务端持有不必要的 credential/session 状态。

反过来，直接删除 Go 登录会使未部署 IAP 的复用环境失去安全边界。因此模式必须是显式、fail-closed 的启动配置，而不能通过“是否恰好设置密码”推断。

## 决策 / Decision

增加 env-only、restart-required closed enum `DASHBOARD_AUTH_MODE`：

| mode | Dashboard 行为 |
|---|---|
| `disabled`（默认） | 不启动 `:8081` dashboard listener |
| `builtin` | 要求 `DASHBOARD_USER` 与 `DASHBOARD_PASSWORD` 同时非空；保留 bcrypt、常时用户名比较、CSPRNG server-side session、Secure/HttpOnly/SameSite=Strict cookie、CSRF 与 per-IP 登录退避 |
| `external` | Go 不创建 credential、session、cookie 或 CSRF 状态；前置 IAP 是唯一认证边界 |

不论模式，启用的 dashboard 都必须继续通过 `requireLoopback` 绑定至 loopback 地址。`external` 只能在 IAP/Tunnel 的 origin 指向该 loopback listener、且 IAP policy 覆盖整个 hostname 的情况下使用；它不是“无认证公开 dashboard”。部署脚本在 `external` / `disabled` 时不把遗留的 builtin 凭证下发到服务器，允许 repository secrets 在迁移窗口内暂留而不扩大运行时 secret 面。

SPA 通过无敏感的 `GET /api/bootstrap → {authMode}` 选择 UX：builtin 维持 `/login`、`/logout` 与 `/api/session` 的 F5 重水合；external 不注册这些 login/session routes，直接调用 API。外部 IAP 的个人身份仍留在 IAP audit log；Gateway audit 只记固定 actor `external-iap`，避免另造一份 email/identity PII 数据库。

## 理由 / Rationale

- 一套 binary 可安全复用于 SSH、本机、Cloudflare Access、Tailscale 和其他 IAP 部署；
- `disabled` 默认不产生管理面，避免漏配时新开 listener；
- `builtin` 不降级已有防护；
- loopback 强制 + 前置 IAP 全路径覆盖，把 external 模式的信任前提变成清晰可审计的网络架构，而非隐含约定；
- bootstrap endpoint 只暴露模式，不暴露身份或 secret，前端可保持同一编译产物。

## 后果 / Consequences

运维必须有意选择模式：没有 IAP 选 `builtin` 并配置 credentials；已配置完整 IAP 选 `external`；不需要后台选 `disabled`。切换模式需重启/重新部署，不能由 dashboard 热改。`external` 模式的访问控制与用户级审计责任转移到 IAP；Go 仍保留输入校验、security headers、loopback binding、配置/导出等 capability 的完整业务防护。

本 ADR 取代 ADR-0009 中“builtin 登录是唯一 dashboard 认证模型”的部分；SPA 分层、`go:embed` 分发与 types-mirror 纪律继续有效。
