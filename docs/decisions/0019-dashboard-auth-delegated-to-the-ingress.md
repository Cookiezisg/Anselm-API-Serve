---
id: DOC-037
type: decision
status: active
owner: @weilin
created: 2026-07-31
reviewed: 2026-07-31
review-due: 2099-12-31
audience: [human, ai]
---

# 0019 — 管理后台的鉴权完全交给入口，进程内不再有登录墙

> **取代 [ADR-0014](0014-configurable-dashboard-auth-boundary.md) 的可配置部分。**
> ADR-0014 不可变、原文保留:它记录的是「为什么当时需要一个可配置的边界」,那个推理在它自己的
> 时点上是对的。本篇只推翻**结论**:那三种模式里,只有一种被用过。

## 背景 / Context

ADR-0014 把管理后台的信任边界做成了 `DASHBOARD_AUTH_MODE` 三态枚举:

- `disabled` —— 根本不挂载后台
- `builtin` —— Go 自己的 bcrypt 登录 → session → CSRF → per-IP 登录退避
- `external` —— 委托给前置 IAP(Cloudflare Access / Tailscale / SSH 隧道)

生产从始至终只用 `external`。另外两态从未被启用过一次,却一直在被维护:一整套 session 存储与
登录限流(两个文件 ~300 行)、handler 里的 `BuiltinAuth` 结构与 5 处 nil 分支、bcrypt 依赖、
前端的 AuthContext / 登录页 / api 客户端里的 CSRF 与 authMode 分支、部署脚本的三分支派发与
三组模式测试、以及三个 GitHub secret。

## 决策 / Decision

**只保留 `external` 那一种,并且不再把它叫做一种「模式」。** 管理后台:

1. **恒定挂载**,且**恒定只绑 loopback**——绑定 fail-fast。
2. **进程内不构造任何** credential / session / cookie / CSRF 状态。
3. 登录墙归**前置 IAP**,而前置 IAP 必须覆盖**所有**路径。
4. `DASHBOARD_AUTH_MODE` / `DASHBOARD_USER` / `DASHBOARD_PASSWORD` /
   `DASHBOARD_DEV_INSECURE_COOKIE` 四个配置项与三个同名 secret 全部移除。

## 理由 / Rationale

**一道从未被启用的墙不是纵深防御,是待同步的第二份真相。** `builtin` 若真被启用,它与 IAP 会
成为两条各自演进的鉴权路径;若不被启用(实际情况),它就是一段没人验证过、却仍要随每次改动一起
被读懂和维护的代码。两种结局都不比「没有它」更安全。

**loopback 绑定才是那条真边界,而它是进程的性质、不是配置值。** 绑定失败即启动失败,故「可达但
未鉴权」不是本网关到得了的状态。ADR-0014 当时把安全性寄托在「运营者选对了模式」上;现在它不
依赖任何人选对什么。

**这条收敛不是白拿的,代价写在这里:**

① **本地 plain-HTTP 直连不再有登录页可用。** 从前 `builtin` 允许你在没有任何 IAP 的机器上直接
   开 8081 登录。现在必须先有隧道(SSH `-L` 最省事)。这不是退步——那条路径本来就要求你信任
   本机网络。

② **审计里没有人名了。** `actor` 恒为固定标记 `external-iap`。身份的事实源是 IAP 自己的审计
   日志;把它的转发头拷进本地审计环,等于凭空立起第二个没人要的 PII 存储。

③ **配错 IAP 的后果不再被本进程接住。** 从前 `builtin` 至少在 IAP 失效时还有一道墙。现在没有
   了——但也正因如此,「IAP 必须覆盖所有路径」从一句建议变成了一条必须被当真的部署前提
   (GW-INV-19)。

## 后果 / Consequences

- 删除:`middleware/dashboard/{session,loginlimit}.go`、handler 的 builtin 分支、bcrypt 用法、
  前端 `auth/AuthContext.tsx` 与 `pages/Login.tsx`、api 客户端的 CSRF/authMode/401 重定向。
- 端点消失:`POST /login`、`POST /logout`、`GET /api/session`、`GET /api/bootstrap`。
- GitHub secret 从 12 个降到 9 个。
- GW-INV-19 整体重述;`api.md` / `config.md` / `overview.md` / `architecture.md` / 两个 README
  / `CLAUDE.md` 同步。
