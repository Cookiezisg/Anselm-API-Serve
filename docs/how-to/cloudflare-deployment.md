---
id: DOC-032
type: how-to
status: active
owner: @weilin
created: 2026-07-21
reviewed: 2026-07-21
review-due: 2027-01-21
audience: [human, ai]
---

# Cloudflare 生产环境防御与网络配置指南

本指南客观记录了 Anselm-Gateway 部署于生产环境时，依赖 Cloudflare 实施的外围网络架构与安全防线。

后端程序严格遵循 [GW-INV-13](../references/backend/invariants.md)（Dashboard 必须绑定 loopback）和 [ADR-0014](../decisions/0014-configurable-dashboard-auth-boundary.md)（外部 IAP 认证边界）。本配置是上述架构契约在外部网络层的物理实现。

## 1. Zero Trust 内网穿透 (Dashboard)

由于 Gateway 的管理后台（Dashboard，默认端口 `:8081`）强制仅监听 `127.0.0.1`，其对公网完全隔离。我们通过 Cloudflare Zero Trust 建立一条安全的访问通道。

- **基础设施**: 在宿主机运行 `cloudflared` 守护进程。
- **Tunnel 路由**: 将一个子域名（如 `ram.<你的域名>`）映射至本机的 `http://127.0.0.1:8081`。
- **Access 访问控制**: 
  - 拦截所有进入隧道的流量，前端弹出 One-Time PIN (OTP) 邮箱验证页面。
  - 配置 `Allow` 策略，仅允许指定的管理员邮箱白名单通过。
- **结果**: 服务器无需在防火墙（Security Group）开放 8081 端口；未经身份验证的访问者无法与 Dashboard 建立哪怕半个 TCP 连接，将漏洞扫描与暴破攻击终结在 Cloudflare 边缘节点。

## 2. DNS 与基础安全代理

所有对外的业务入口（包括客户端调用的 `api.<你的域名>`，即部署时的 `GATEWAY_DOMAIN`）均需开启 DNS 代理（即点亮“橙色云朵”）。

- **源站隐藏**: 服务器真实公网 IP 被 Cloudflare 掩盖，天然免疫 L3/L4 网络层 DDoS 攻击。
- **Bot Fight Mode**: 全局启用机器人攻击模式，依赖 Cloudflare 的指纹库在边缘节点直接拦截（丢弃或质询）非法的全网自动扫描器与爬虫。

## 3. WAF 速率限制 (API 接口防御)

Anselm-Gateway 作为 AI 大模型调用的薄网关，防范恶意的并发调用（可能导致上游 API 账单失控）是核心外围防御需求。

- **规则位置**: 安全性 (Security) -> WAF -> 速率限制规则 (Rate limiting rules)
- **匹配条件**: `http.host eq "api.<你的域名>"`
- **限制阈值**: 过去 10 秒内请求数超过 30 次。
- **惩罚措施**: 阻止 (Block) 该 IP 长达 1 小时（*注：免费版限制阻断时间与评估时间一致，此时设置为 10 秒阻断，由于恶意脚本通常会持续请求，会触发无限重置阻断时间的效果*）。
- **结果**: 有效防止单 IP 的高频恶意调用消耗大模型 Quota。

## 4. 移动端弱网体验优化

为保障 Anselm Flutter 客户端在请求大模型流式输出（SSE Streaming）时的抗断连能力，开启以下物理层协议优化：

- **HTTP/3 (QUIC)**: 开启。基于 UDP 的协议大幅减少了移动端弱网环境下的队头阻塞，提升流式打字输出的稳定性。
- **0-RTT 连接恢复**: 开启。允许曾经建立过连接的客户端跳过 TLS 握手，直接发送请求数据，实现秒级响应首字节 (TTFB)。
- **始终使用 HTTPS**: 开启。在边缘强制 HTTPS，符合苹果/谷歌移动应用的安全传输要求。
- **Early Hints**: 开启。利用边缘节点提前推送资源，优化响应时序。

---
*本文档仅作架构配置的真实映射记录，不包含具体的个人秘钥与配置账单。*
