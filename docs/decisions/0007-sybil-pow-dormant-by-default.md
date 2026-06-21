---
id: DOC-016
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0007 — M2 Sybil / PoW 默认 dormant（零值即关、关时零开销）

## 背景 / Context

M2 引入领号防 Sybil（per-IP/全局/per-fingerprint 速率闸）与 PoW 工作量证明。这些是**防御加固**，不是核心计费路径。默认开启会：① 给零滥用的常态加无谓 DB 开销与延迟；② 把一个本可 byte-for-byte 不变的 `/install` 行为搅动出回归面。本网关单运营者、零真实流量起步，加固应**可开可关、关时彻底无痕**。

## 决策 / Decision

**每道 Sybil 闸默认 `0`=禁用且在任何 DB 操作前短路；`INSTALL_POW_MODE` 三态 off/shadow/enforce，无状态 HMAC challenge，nonce-once 放最后验，live 模式强一致要求 secret。**

1. **Sybil 闸默认关、DB 前短路**：`INSTALL_GLOBAL_DAILY_CAP`、`INSTALL_PER_FP_*`（及 per-IP）默认 `0`=禁用，命中即在**任何 DB 工作之前**短路返回——关闭时 `/install` 路径零额外开销（GW-INV-20 家族）。
2. **PoW 三态枚举**：`off`/`shadow`/`enforce`；零值/空/任何其他值 ≡ `off`，此时 `/install` **byte-for-byte 不变**。
3. **无状态 challenge**：`base64(rand16‖unixTs‖HMAC[:16])`，120s TTL；难度 = `SHA256(challenge"."nonce)` 的前导零比特数。无服务端状态存储。
4. **nonce-once 放最后验**：`httpx.NonceCache`（有界 LRU+TTL）的 `UseOnce(challenge)` 是验证序列的**最后一步**——先验 HMAC/新鲜度/难度，全过才消费 nonce，避免无效请求白耗 nonce 槽。
5. **live 模式强一致要求 secret**：`shadow`/`enforce` 任一为活时**必须**有非空 env `INSTALL_POW_SECRET`，否则 Load **和**热编辑两路都 `CONFIG_POW_SECRET_REQUIRED` fail-fast（不可半启用）。
6. **shadow 标签互斥**：shadow 结果用独立终态标签（`shadow_pass`），与 `verified`/`failed`/`missing` 不重叠。

## 理由 / Rationale

- dormant-by-default 让加固「装好但休眠」：代码与测试都在位、随时可开，常态零成本、零行为漂移。
- 无状态 challenge 免去 challenge 存储与清理；nonce 仅在通过实质校验后消费，抗 nonce 耗尽。
- 三态（含 shadow）让运营者先观测命中率再切 enforce，避免直接 enforce 误伤真实用户。

## 取舍与后果 / Consequences

**为何不选：**

- **默认开启**：常态加无谓延迟/DB 开销、搅动 `/install` 回归面——与「零流量起步」错配。
- **有状态 challenge（存 DB/内存表）**：引入存储 + 过期清理；无状态 HMAC 同等安全且零状态。
- **nonce 先消费再验**：无效请求白耗 nonce 槽，成 nonce 耗尽 DoS——故放最后。
- **secret 可自动生成**：重启即换 secret、跨实例不一致；强制 env 显式提供。

**make-immune-by-construction：**

| bug | 旧病灶 | 本决策构造规则 |
|---|---|---|
| **B8** Sybil 速率表无界增长、`install_ip_rate` 常开泄漏 | `INSTALL_PER_IP_HOUR` 界 `[1,1e6]`（min 1、关不掉）⇒ 每次领号/challenge 都 INSERT，源中零 `DELETE` | 默认 `0`=禁用 + DB 前短路：关闭即零写入；每个持久化速率桶有界保留 + 机会式 prune |
| **B12** shadow PoW 指标双标签 | `powGate` 同时增 outcome（`missing`/`failed`）与 `shadow_pass` ⇒ `sum by(result)` 过计 | 每请求恰一个互斥标签，shadow 走独立终态标签 |

**后果：**

- `app/install` 持闸序（ip/global/fp）+ PoW 闸 + `LookupInstall`；`pkg/pow` 持无状态 mint/verify 加密；`pkg/noncecache` 持有界 nonce；`pkg/clientip` 持 XFF 处理（GW-INV-16）。
- 审计：`/install` 拒路用互异 wire code，发 unsampled WARN `install_audit` 仅带 `ip_key`(/64) + `gate` + `error_code`，绝不带 fingerprint 明文（GW-INV-20）；fingerprint 仅以 `SHA-256` 存 `install_fp_rate.fp_sha256`。

## 相关 / Links

- [ADR 0006 配置三层](0006-config-tiers-atomic-hot-reload.md)（POW_SECRET 强一致 fail-fast）· [ADR 0011 故障分类](0011-fault-classification-excludes-cancel-429.md)（标签互斥同纪律）· [架构](../concepts/architecture.md)
- 不变量：GW-INV-12、16、20
