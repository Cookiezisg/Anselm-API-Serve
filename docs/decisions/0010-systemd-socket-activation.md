---
id: DOC-019
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0010 — systemd socket-activation 用于业务监听口

## 背景 / Context

业务口 `:8080` 是对外（Caddy 后）唯一入口。进程重启（升级/配置 startup-hard 变更）若用自绑 listener，重启窗口内内核 backlog 被丢弃、在途连接被拒。运营者跑在 systemd 下时，可让内核持有 socket、跨进程重启保 backlog，实现零丢连。admin/dashboard 是 loopback 内省/管理口，短重启窗口可接受、不值得同等复杂度。

## 决策 / Decision

**业务 `:8080` 优先用 socket-activation fd，非 systemd 时自绑回退；admin/dashboard 自绑。**

1. **业务口偏好 activation fd**：`bootstrap/listeners.go` 先取 `activation.Listeners`（systemd 传入的 fd），命中即复用——重启保内核 backlog、零丢连。
2. **自绑回退**：未在 systemd 下（无 activation fd）时退回自绑 `LISTEN_ADDR`，行为不变。
3. **admin/dashboard 自绑**：两者 loopback、读者有限，短重启间隙可接受（**已记录**）；不引 activation 复杂度。
4. **绑定校验串接**：admin 自绑仍过 `requireLoopback`（见 [ADR 0004](0004-three-physically-isolated-listeners.md)）。

## 理由 / Rationale

- socket-activation 是 systemd 下零丢连重启的标准手段：内核持 socket，进程换而连接队列不断。
- 仅业务口享此待遇是成本/收益对齐：对外口零丢连价值高；loopback 内省/管理口的短窗口无实际损失。

## 取舍与后果 / Consequences

**为何不选：**

- **全部口都自绑**：业务口重启丢内核 backlog、拒在途连接。
- **三口都上 socket-activation**：admin/dashboard 收益近零、徒增装配复杂度。
- **依赖外部 LB 做零丢连**：local-first 单机形态无 LB；Caddy 前置但重启仍需后端保 backlog。

**后果：**

- `bootstrap/listeners.go` 持 activation fd 偏好 + 自绑回退 + `requireLoopback`。
- 部署文档（how-to）须说明 systemd unit 的 `.socket` 配置；非 systemd 环境自动降级、无需配置。
- admin/dashboard 重启短窗口在运维文档明记为可接受。

## 相关 / Links

- [ADR 0004 三监听口](0004-three-physically-isolated-listeners.md) · [架构](../concepts/architecture.md)
- 不变量：GW-INV-18（监听口）
