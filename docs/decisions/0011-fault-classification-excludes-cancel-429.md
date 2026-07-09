---
id: DOC-020
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0011 — 故障分类排除 client-cancel 与 429（B5/B3、B12、B16 构造性免疫）

## 背景 / Context

进程级熔断器（breaker）保护上游账户：连续真实故障应跳闸熔断。但**客户端取消**（`context.Canceled`）与**上游 429**（背压信号）不是上游故障——把它们记进 breaker 会在健康上游上自残：客户端一阵取消或上游一阵忙就跳闸、把全部流量甩掉，DoS 放大。旧代码恰在多处把 client-cancel 硬记 `breakerFault=true`（B5/B3）。指标标签若高基数或重叠，则 Prometheus 内存爆 + `sum by` 过计。

## 决策 / Decision

**单处计算的 typed `faultClass`：client-cancel → 499、429 → UPSTREAM_BUSY，二者皆非故障非重试；仅 {5xx,timeout,connect} 计入 breaker；指标标签严格低基数且互斥。**

1. **单点分类**：故障类别在**一处** `faultClass` 计算，全路径共用——杜绝多处各判一次、判错一次即 bug。
2. **client-cancel → 499 CLIENT_CANCELED**：`context.Canceled` 且 `ctx.Err()!=nil` 且非 timeout = 非故障、非重试、仅审计；排队中被取消则**不占 slot**放弃、全额回滚、不计 busy、不计 breaker（GW-INV-22）。
3. **429 → UPSTREAM_BUSY(429)**：自成一类——不重试、不计 breaker、不误判为产出前错误；per-key transport 里普通 429 不跳该 key（仅 `Retry-After > 5s` 的 429 设 per-key cooldown，GW-INV-23）。
4. **仅真实故障计 breaker**：`breakerFault=true` 仅限 5xx/timeout/connect（经 `errUpstreamFault`），每 attempt-set 恰记一次失败；跳闸条件 `ConsecutiveFailures >= 5` 或（`Requests >= 10` 且失败率 `> 0.5`）（GW-INV-22）。
5. **标签严格低基数互斥**：唯一标签 `outcome`（`gateway_upstream_requests_total`）、`handler`（RED `mx.Wrap`）、`result`（`gateway_install_pow_total`：verified/failed/missing/shadow_pass）；绝无 `install_id`/`token`/`prompt`/`ip` 入标签（GW-INV-15）。

## 修订 / Amendment（2026-07-09）

**排除集扩为 {client-cancel, 429, 上游 4xx 请求拒绝}。** 网关侧输入预检（INPUT_TOKEN_CAP 等）可禁用、由上游模型自身限制判定后，上游 400/413/422（如超上下文）成为**客户端造成的确定性拒绝**——与上游健康无关。若仍按旧默认归入 `classUpstreamFinal`（fault），连发 5 个超长 prompt 即可跳闸全站断流（自残式 DoS 放大，与 B5/B3 同构）。故新增 `classUpstreamRejected`：

- 上游 400/413/422（输出前）→ `400 UPSTREAM_REJECTED`，不重试、**不计 breaker**、不计 per-key 故障，预留经 REL-5 回滚；
- 仅从上游错误 body（≤4KiB）解析**闭集** `details.reason ∈ {context_length, max_tokens, invalid_request}`；上游原文绝不透传（GW-INV-11 不破）；
- metrics 标签新增互斥 `outcome="rejected"`（与 error/busy/timeout 区分，避免把客户端输入错误误读为上游故障）。
- 其余非重试 non-2xx（如 402 余额不足）仍为 `classUpstreamFinal`（fault, 502）。

见 GW-INV-41。

## 理由 / Rationale

- breaker 的语义是「上游是否健康」——client-cancel 与 429 与上游健康无关，记入即语义错配、自残式熔断。
- 单点分类把「一个 error 属于哪类」收敛到唯一真相处，跨重试/流式/排队多入口共用同一判定，免去多点判错。
- 标签互斥低基数是 Prometheus 正确聚合（`sum by` 不过计）与防 OOM/PII 的硬约束。

## 取舍与后果 / Consequences

**为何不选：**

- **把所有 `client.Do` error 一律记 `breakerFault=true`**（旧 B5/B3 病灶）：client-cancel/429 跳闸健康上游、DoS 放大。
- **429 走重试 / 记 breaker**：重试风暴放大上游过载、错把背压当故障、破坏计费分类。
- **多处各自分类**：判定散落、改一处漏一处；故收敛单点。

**make-immune-by-construction：**

| bug | 旧病灶 | 本决策构造规则 |
|---|---|---|
| **B5/B3** client-cancel 跳进程 breaker（DoS 放大器） | `proxy.go:736` 对含 `context.Canceled` 的泛 `client.Do` error 返 `breakerFault:true`；backoff select `:681-682` 与 stream Peek `:787-790` 硬编码 `breakerFault=true` on `<-ctx.Done()` | 故障分类是单处 typed 判定：cancel → 499 非故障非重试仅审计；429 → UPSTREAM_BUSY 非故障非重试；仅 {5xx,timeout,connect} 计 breaker |
| **B12** shadow PoW 指标双标签 | `powGate` 同增 outcome 与 `shadow_pass` ⇒ 标签重叠、`sum by(result)` 过计 | 每请求恰一互斥标签，shadow 走独立终态标签（与 [ADR 0007](0007-sybil-pow-dormant-by-default.md) 同纪律） |
| **B16** 共享 `rateSampler` 被每次 overview 轮询覆写 | `dashboard/rate.go:37-49` 每 Server 一个 `rateSampler`，≥2 并发标签页污染 `dt` ⇒ QPS 减半/尖刺 | QPS 在指标层服务端用固定滑窗算（无 per-poll 可变 sampler，`pkg/ratesample` 服务端化）；`rates{QPS,ErrorRate,WindowSec}` 字段名钉死供前端镜像 |

**后果：**

- `app/chat` 持分类 + breaker 编排；`infra/upstream` 持 per-key breaker+cooldown+pickKey+retry+首字节计时；`pkg/ratesample` 持服务端滑窗 QPS。
- 流式用滚动 per-frame 写截止（`SetWriteDeadline(now+30s)`），`http.Server` 无全局 `WriteTimeout`（GW-INV-25）；重试仅限 connect→首字节窗口、产出后绝不重试（GW-INV-26）。
- 回归：`TestBreakerOpenFires`、`TestClientCancelWhileQueued`、`TestUpstream429MapsBusy`、`TestQueueWaitTimeoutBusy`。

## 相关 / Links

- [ADR 0001 三闸预占](0001-pessimistic-three-guardrail-reservation.md)（REL-5 单点回滚 + 首字节算一次）· [ADR 0007 PoW dormant](0007-sybil-pow-dormant-by-default.md)（标签互斥）· [ADR 0009 React dashboard](0009-react-dashboard-clean-architecture.md)（rates 镜像）· [架构](../concepts/architecture.md)
- 不变量：GW-INV-15、22、23、25、26、27、28、30
