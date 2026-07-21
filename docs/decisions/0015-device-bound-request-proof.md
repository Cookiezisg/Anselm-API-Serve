---
id: DOC-031
type: decision
status: active
owner: @weilin
created: 2026-07-21
reviewed: 2026-07-21
review-due: 2099-12-31
audience: [human, ai]
---

# 0015 — 设备绑定请求证明取代 bearer credential

## 背景 / Context

桌面端是开源软件，因此任何只存在于客户端的算法都不能证明调用者运行的是未经修改的官方二进制。可复用 gateway token 仍使最便宜的滥用路径过于容易：复制一个数据库值或截获一个 header，就能从另一台机器调用免费额度；捕获的完整 HTTP 请求也能直接重放。

有效的安全边界应当是持有不可导出的安装私钥，而不是依赖公开客户端实现的保密性。

## 决策 / Decision

每个 Anselm 安装创建一对 Ed25519 密钥。加密后的 32-byte seed 以 `device-proof.key`（`0600`）保存于桌面 sidecar 数据目录；私钥永不进入 Flutter、Gateway 数据库、HTTP header 或受管 API-key 行。

`POST /v1/install` 携带公钥与签名注册证明。Gateway 保存 `public_key` 及其 SHA-256 base64url thumbprint，返回公开的 `installId`。注册按 thumbprint 幂等：同一设备始终得到同一 install，不会重复占用 quota pool 或 issuance bucket。

签名调用前，客户端请求 `GET /v1/proof/challenge`。返回的 HMAC 认证 nonce 有效五分钟并可缓存。每个受保护请求携带：

- `X-Anselm-Install-ID` (omitted only during registration)
- `X-Anselm-Public-Key` (registration only)
- `X-Anselm-Proof = base64url(payload-json) + "." + base64url(ed25519-signature)`

固定版本 payload 为 `{v,kid,iat,jti,nonce,htm,htu,bh}`，绑定公钥或 install id、签发时间、随机 request id、服务端 nonce、HTTP method、小写 authority 与 request target，以及精确 body 的 base64url SHA-256。Gateway 接受 ±90 秒时钟偏差，校验五分钟 nonce，并在有界的两分钟 replay cache 中仅消费每个 `(kid,jti)` 一次。cache 满载时 fail closed 拒绝新 proof，绝不驱逐未过期条目后重新放行旧 proof。进程重启会更换 nonce HMAC key，旧证明自动失效。nonce 过期时，客户端在业务执行前刷新 challenge 并最多重试一次。

不提供 bearer 兼容模式，也不保留 token 列；产品未上线，直接执行 clean break。

该 profile 采用 [RFC 9449 DPoP](https://www.rfc-editor.org/rfc/rfc9449.html) 的 `jti`、`iat`、`htm`、`htu` 与服务端 nonce 思路，并按 [RFC 9421 HTTP Message Signatures](https://www.rfc-editor.org/rfc/rfc9421.html) 的消息完整性原则额外绑定精确 body digest；它不是 OAuth token flow，也不宣称 wire-level RFC 兼容。

## 后果 / Consequences

- 复制 `installId`、受管 API-key 行、日志或一次 HTTP 请求不再获得持续访问权；修改 request/body/path 或重放均会失败。
- 控制一台机器的攻击者仍可修改开源客户端、生成新密钥并实现公开注册协议。现有 PoW/IP/fingerprint issuance gate 继续承担 Sybil 成本层；未来可加入 platform attestation，而无需改变逐请求证明结构。
- 加密私钥丢失会产生新的安装身份；服务端按设计不提供私钥恢复。
