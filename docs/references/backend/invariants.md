---
id: DOC-007
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-21
review-due: 2026-10-19
audience: [human, ai]
---

# 不变量登记册（GW-INV-NN）

> 验收准绳；编号永不复用。金额一律为整数 pico-US dollar（`1 USD=10^12 pUSD`），请求在冻结 rate card 下换算后才进入共享钱包。逐字 schema 见 [database.md](database.md)，wire code 见 [error-codes.md](error-codes.md)，输入/路由见 [api.md](api.md)，选型见 [ADR-0012](../../decisions/0012-deterministic-capability-routing-and-cost-ledger.md)。

## A. 财务正确性

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-01 | 两道请求拒绝闸门（per-install 月请求 `quota_monthly.requests<MonthlyQuota`、operator 月 pUSD `global_spend_monthly.spend_pusd+reserved≤GlobalMonthlySpendPUSD`）与 `spend_ledger(state='open')` 在写池单个 `BEGIN IMMEDIATE` 内原子悲观预留；install/provider/global 日表只做统计与审计，不因日花费拒绝请求 | 并发尖峰超卖用户月额度或运营者月钱包；把统计表误当闸门导致误杀 |
| GW-INV-02 | 只有 `CallFailure.Exposure==DefinitelyUnbilled` 可 rollback：Open 前本地拒绝或 provider 明确 pre-generation 3xx/4xx（400/401/402/403/429 等）；rollback 原子反转 per-install 月额度、operator 月钱包、日统计余额/requests 及实际启用的日子限次数 | 从 wire code 猜退款形成可刷欠扣通道；部分退款破坏守恒 |
| GW-INV-03 | `ChargeExposure` 与 `APIError`/breaker/retry 正交：零值及未知值均 `ChargePossible`，connect/TLS/read/timeout/5xx/call 中 cancel/nil stream 立即 full settle；同一 `UPSTREAM_ERROR` 可对应 rollback 或 full settle，app 只能调用 `MayHaveCharged()` | provider 已收费却因错误码退款，或 adapter 漏字段静默欠扣 |
| GW-INV-04 | `spend_ledger` 终态转换都以 `WHERE state='open'` 做单赢家 CAS：`open→settled|rolled_back|orphaned`；`RowsAffected()==0` 为幂等 no-op | 双退款、双补记或状态/余额分裂 |
| GW-INV-05 | `Period{Month,Day}` 在请求入口按 `RESET_TZ` 快照一次并贯穿 Reserve/Settle/Rollback；任何终态操作都绝不重算日期 | 跨午夜结算落错月/日行 |
| GW-INV-06 | aged `open` 孤儿原子转为 `orphaned(charged_pusd=reserved_pusd)`，余额和请求计数均不退款；崩溃与未知外部结果只能多扣、不能少扣。0002 对 legacy ledger 同样保守：open 取 reserved、positive settled 取 settled、zero rollback 排除，日统计 floor=`max(v1 balance, chargeable ledger aggregate×rate)`；0004 从 global 日统计聚合出 operator 月钱包，聚合溢出则整迁移失败 | 自动退款或迁移漏掉一个 provider 可能已收费的请求，形成永久欠账 |
| GW-INV-07 | 唯一 spend 拒绝钱包是 `global_spend_monthly(period_month='YYYY-MM')`，cap 必须为正；`install_spend_daily`、`provider_spend_daily`、`global_spend_daily` 只记录花费与请求数统计 | 日花费误杀请求、provider/installation 分钱包与 operator 月预算语义分裂 |
| GW-INV-08 | Reserve 只接受由精确 provider/model rate card 构造且可自校验的冻结 `billing.Plan`；跨 provider raw token 绝不相加，只有 pUSD 可进入余额 | 伪造零价 plan 或把不同价格 token 当同一金额导致超卖 |
| GW-INV-09 | Settle 从累计 usage 逐字段取最大快照，按冻结 rate card refund/top-up operator 月钱包与日统计表；任何 negative/malformed/duplicate 或 case-fold 等价 billing key 证据 sticky，后续正常帧不可洗白；missing/negative/矛盾/不可计价 usage 保留全 reservation。Kimi 退款须有正 `total_tokens≥prompt+completion` 且 reasoning≤`total-prompt`；DeepSeek cache hit+miss 不得超过 prompt，未报告部分按 miss；actual 超 quote 仍如实 top-up 并发 `billing_drift` | 多帧 usage 重复累加、last-key-wins 洗掉负数、畸形帧被后续值掩盖、未知用量被退款或已发生支出被 cap 隐藏 |
| GW-INV-10 | DeepSeek quote=`UTF-8 byte-fallback prompt 上界（含 tools/tool_choice/message tool continuation 与 64 token/message framing）+ clamped output`；Kimi K2.6 compatibility 因不能证明 thinking 上界而 reserve 完整 `262,144` input + `32,768` output hard limits；任一已启用 route 的单请求 quote 必须能装入 operator 月预算，否则配置 fail-fast | 请求注定无法 reserve、byte-split tokenizer/tool 字段造成欠预留，或 Kimi hidden thinking 造成欠预留 |

## B. 安全

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-11 | DeepSeek/Kimi key 永不离服务器：provider-local client 只在 cloned request 注入 auth；remote endpoint 强制 HTTPS（HTTP 仅 canonical loopback IP literal dev/test，拒 `localhost` 与 `127.0.0.1.` 等 DNS/NSS/proxy 绕过拼写），redirect 永不跟随；上游 header/body/error 原文绝不透传；≤4KiB 错误体只归一到闭集 reason；key 事件只记固定 provider + `key_index` | key 被明文/代理/redirect 带往错误主机，或上游账户细节与敏感错误体外泄 |
| GW-INV-12 | 身份只由 Ed25519 possession proof 建立：DB 仅存 public key + UNIQUE thumbprint，`/install` 同 key 幂等；每个 protected request 签名绑定 install/kid、±90s iat、5min server nonce、一次性 jti、method、authority+target 与 exact body hash；replay cache 满载 fail closed；无 bearer 兼容 | 复制 install id/请求即可滥用、篡改 body/path、跨域复用或重放，或同设备重复领取额度池 |
| GW-INV-13 | `/metrics` `/readyz` `/debug/pprof/*` `/debug/vars` 与 dashboard 均 loopback-only；`requireLoopback` 拒空 host/IP 非回环/解析到任一非回环地址；`/healthz` 不碰 DB | 管理/画像面公网暴露，远程 DoS 或凭证攻击 |
| GW-INV-14 | `DEEPSEEK_API_KEY`、`KIMI_API_KEY`、`DASHBOARD_*`、`INSTALL_POW_SECRET` 均 secret-env-only，不进 `Specs`/settings/Dump；Snapshot 只给安全状态/数量，日志 redaction 覆盖 auth、media data 与 provider key 字段 | secret 或 inline media 经配置/日志/导出泄露 |
| GW-INV-15 | metric/audit 标签严格低基数：provider 仅 `deepseek|kimi`，另仅固定 `outcome`/`handler`/`result`；禁止 model、install_id、token、prompt、media、ip 入 label | Prometheus 基数爆炸或 PII/内容入时序 |
| GW-INV-16 | business XFF 仅当直连对端为 loopback Caddy 时可信并取最右段，否则回退 `RemoteAddr`；dashboard 直连/SSH tunnel 的 login bucket **永远忽略 XFF**，只取 direct peer | 攻击者伪造 XFF 绕过 install 或 dashboard per-IP 限速 |
| GW-INV-17 | redaction 对 auth/cookie/provider-key/password/secret 精确名及 `*_api_key|*_password|*_secret` 后缀（大小写不敏感）直接 `[REDACTED]`，非 allowlist key 下 struct/map/slice 整体抹除；`image_url`、`input_audio`、inline/file data 在红线内 | 新 provider key、`slog.Any` 或 base64 media 被静默序列化 |
| GW-INV-18 | TLS 仅由 Caddy 终结；Go 三监听默认 loopback、地址必须两两不同，否则 `LoadBase` fail-fast | 误绑公网或运行时端口冲突 |
| GW-INV-19 | `DASHBOARD_AUTH_MODE` 是 closed enum：`disabled` 不起 dashboard；`builtin` 必须同设 user/password，并启用 bcrypt、常时用户名比较、CSPRNG session、Secure/HttpOnly/SameSite=Strict cookie、CSRF、per-IP 登录退避；`external` 不构造 Go credential/session/CSRF，且 dashboard 仍只能 loopback 绑定，前置 IAP 必须覆盖所有路径 | 登录墙被配置遗漏、session 劫持、CSRF、暴破，或把无 Go 鉴权的 dashboard 直接暴露公网 |
| GW-INV-20 | `/install` 各拒绝路径用 DISTINCT wire code + 不采样 `install_audit`；只记 `/64 ip_key`/gate/error_code，fp 仅以 SHA-256 进入 `install_fp_rate` | Sybil 洪流不可区分，raw fp/ip/secret 入日志或 DB |

## C. 可靠性

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-21 | 两个 provider 共享一个固定容量 `N_GLOBAL_CONCURRENCY` 信号量，gateway→全部上游总在飞 ≤ N；provider 内换 key/retry 绝不新增 slot | 双上游把总并发翻倍、带宽/账户被打满 |
| GW-INV-22 | 每个 provider 有独立 process breaker；fault health 与 charge/retry 分开判：client-cancel、429、401/403 key signal、key-open、400/413/422 不计 process fault；5xx/timeout/connect 与其它被 typed health class 归为 upstream fault 的明确 refusal 可计一次。`DefinitelyUnbilled` 不推出“不计 breaker”，“计 breaker”也不推出“可 retry” | 一个 provider/客户端错误误熔断另一 provider，或把 health policy 当财务证明 |
| GW-INV-23 | provider 429 独立归一 `UPSTREAM_BUSY`、`DefinitelyUnbilled`：不 retry、不计 process/per-key breaker；`Retry-After>5s` 只给当前 key cooldown，当前请求仍 rollback | 429 retry 风暴、误熔断或已知拒绝仍收费 |
| GW-INV-24 | 关停严格 ①取消扫描 loop ②关闭三 HTTP server ③等待 bgWG≤30s ④最后关 DB；所有 detached settle/rollback/reconcile 均挂 bgWG | 结算中关 DB 留 open/半写账本 |
| GW-INV-25 | 流式每帧滚动 `SetWriteDeadline(now+30s)`；server 不设全局 `WriteTimeout` | 长回答被固定墙钟截断 |
| GW-INV-26 | 自动 retry 总 attempt≤3，且只重试可证明未收费并能绕过当前 key 的 401/403 cooldown 或 key-breaker-open；connect/TLS/read/timeout/5xx/client-cancel 全部 ChargePossible、终止并 full settle，429/其它 3xx/4xx虽可退款也不 retry；始终同 provider/plan/slot | 一份 reservation 隐藏多次可能 provider charge，账本无法守恒 |
| GW-INV-27 | `UPSTREAM_HEADER_TIMEOUT_SEC` per attempt 界定 connect→header→first body byte；stream/non-stream 都必须成功 `Peek(1)` 才 handoff，2xx header 不提前停表；handoff 后 timer 已幂等 disarm，且不覆盖其余 body/stream。timeout 是 ChargePossible，不 retry、保留 full quote | non-stream headers-only stall 钉死 slot；timer race 截断已返回 stream；timeout retry 造成多次潜在收费 |
| GW-INV-28 | `QUEUE_WAIT_MS` 是共享信号量的有界等待：0=立即拒绝，超时→429，排队取消→499 审计；三者都在 Open 前 rollback 且不占/放大 slot | 无界排队、并发放大或取消请求虚占钱包 |
| GW-INV-29 | diskguard 每 30s 探测数据盘，低于 MB 或百分比阈值时在任何 reserve 前返回 `503 DISK_LOW`；启动同步预热、探测失败 fail-open、恢复自动清 | 磁盘满时中途写坏账本或探测抖动永久只读 |
| GW-INV-30 | DeepSeek 与 Kimi 各自固定 endpoint、API key pool、per-key breaker/cooldown、process breaker；选定 provider 后禁止 fallback、禁止跨池取 key；单 key 配置行为不变 | 单 provider 故障污染另一 provider，或实际费用与冻结 plan 不一致 |
| GW-INV-41 | provider 400/413/422 归一为 `400 UPSTREAM_REJECTED`，只暴露 `details.reason∈{context_length,max_tokens,invalid_request}`；Exposure=DefinitelyUnbilled，不 retry/不计 breaker并 rollback；其它明确 3xx/4xx也 DefinitelyUnbilled，但可归一成不同 APIError/fault class | 恶意超长输入触发全站 breaker、上游原文泄露或用 code 代替 exposure |

## D. 输入、能力与模型路由

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-31 | 入站 JSON transport 必须是合法 UTF-8；top-level chat 白名单只有 `model/messages/stream/temperature/max_tokens/n/tools/tool_choice`；client `thinking`/`reasoning_effort` 不入 canonical request；canonical request 只有 messages/stream/temperature/bounded optional max_tokens/tools/tool_choice + streaming usage；真实 model 与 provider knobs 仅由选中 provider adapter 附加 | invalid byte 经 RawMessage/字符串产生不同解释或膨胀报价；client 走私 provider 参数、key、价格或 reasoning 规格 |
| GW-INV-32 | `n>1` 经 raw probe 与 typed decode 双重拒绝为 `400 BAD_REQUEST` | 单请求扇出多个 completion、破坏报价上界 |
| GW-INV-33 | pre-reserve shape guard：messages 非空且 `len≤MAX_MESSAGES`；每 message 文本 rune 数≤`MAX_MESSAGE_CHARS`；body≤`MAX_BODY_BYTES` | 多小消息或单大文本造成内存/估算放大 |
| GW-INV-34 | business middleware 与 chat handler 共享 `MAX_BODY_BYTES`（默认 256KiB，范围 4KiB..8MiB，重启生效）；`/v1/install` 独立 8KiB | 未鉴权或 chat body 无界缓冲 |
| GW-INV-35 | 路由只由完整 history 的已验证 content 决定：全 string/文本 parts→DeepSeek V4 Flash；任一受支持 image/video→Kimi K2.6；任一合法 audio→reserve/Open 前 `AUDIO_UNAVAILABLE`。client `model` 永不选择 provider/实际模型；`/v1/models` 及 stream/non-stream 成功响应顶层 `model` 都只给 `PUBLIC_MODEL_ID`，且 duplicate/case-fold 等价 model key fail closed。non-stream 只放行单一 UTF-8 object；SSE 只放行 object data、精确 DONE/blank 与归一 bare-comment；只有完整读到合法 DONE 才允许按 stream usage 退款，提前 EOF/读错即 full quote | 客户端绕过成本/能力策略、只看最后一条消息而误路由，公开 alias 被真实 provider/version 漂移击穿，duplicate key 放大输出，或未完成 stream 用中途 usage 错误退款 |
| GW-INV-36 | message role 是闭集 `system|user|assistant|tool`（tool 必须有非空 `tool_call_id`）；`content` 是关闭联合类型：missing/null/string/parts；missing/null 仅合法 assistant tool-call；part 仅 `text`、`image_url`、`video_url`、`input_audio` 且未知/交叉字段拒绝；文本-only parts canonicalize 为 string；opaque `tool_calls` 原样保留 | 未知 role/多部分走私、provider 对同一 body 产生不同解释，或 Kimi function loop 第二步被 400 拒绝 |
| GW-INV-37 | reasoning 档位由服务端唯一确定：DeepSeek adapter 固定注入 `thinking.enabled` + `reasoning_effort=high`；Kimi adapter 固定注入 `thinking.enabled` 且不传 `reasoning_effort`。caller `max_tokens` 是受保护调用参数：正数 wire 值为 `min(client,MAX_TOKENS_CAP,selected model output limit)`，缺省/非正不写 wire；DeepSeek quote 用同一 bounded output，缺省按 cap 保守预留；Kimi quote 始终按完整 hard limits | payload 与账本上界漂移，输出成本失控，或客户端绕过统一 thinking/effort 产品档 |
| GW-INV-42 | media 只允许 user role：image 必须 inline strict-base64 `data:image/{jpeg|png|webp};base64,...` 且 MIME=magic；video 必须 inline strict-base64 `data:video/mp4;base64,...` 且 MIME=MP4 容器标识；audio 必须 raw strict-base64 `input_audio{data,format}`，`format∈{wav,mp3}` 且 format=magic；remote URL/PDF/file/未知 part 一律 400，网关永不 fetch URL。合法 audio 固定在 reserve/Open 前 `503 AUDIO_UNAVAILABLE`，直到显式接入音频 provider | SSRF/下载放大/MIME 欺骗，或 compatibility 未定义输入进入上游；更不能把尚未计价的音频静默送给 Kimi |
| GW-INV-43 | 整请求 media part 数≤`MAX_MEDIA_PARTS`、累计 decoded bytes≤`MAX_MEDIA_DECODED_BYTES≤MAX_BODY_BYTES`；`INPUT_TOKEN_CAP` 约束文本及所有透传 tool JSON（含 `tools`/`tool_choice`/message tool calls）的 estimate，图片/视频由 parts/decoded-byte 闸限制并交 Kimi 判定实际 media token，但账本仍按 Kimi full hard limits 预留；audio 也受同一形状/bytes 闸，随后在本地拒绝 | base64 解码 OOM、tool JSON 绕过输入/报价护栏，或把文本估算误称为可证明的媒体 token 上限 |
| GW-INV-44 | `KIMI_API_KEY` 可选：缺失时 Kimi backend 不构造，文本 readiness/DeepSeek 路由正常，合法图片/视频在任何 reserve/Open 前固定返回 `503 MULTIMODAL_UNAVAILABLE`；合法音频无论 key 是否存在均先返回 `503 AUDIO_UNAVAILABLE`。配置 key 后 readiness 的 cached authenticated `/models` probe 必须同时确认 Kimi 固定模型，DeepSeek 固定模型始终必需；绝无静默 fallback | 未配 key 导致全站不 ready、已配坏 key 却错误放行部署，或媒体被错误送到文本模型 |
| GW-INV-45 | production deploy 只消费本仓成功 push-main CI 的精确且仍为 main tip 的 `head_sha`；远端 transition 先持久化 root-only marker，永久 Caddy condition 在 marker 存在时禁止 ingress 跨 reboot 自启；checksummed bundle 含 recovery program，commit/rollback 都须在兼容单元 durable 后清 marker，人工新发版遇 marker 必先 `--recover-incomplete` | CI 失败/过期代码上线，崩溃后半迁移 DB/二进制对公网开放，或恢复依赖已被下一版覆盖的脚本 |

## E. 跨切配置 / 可观测

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-38 | `RESET_TZ` 内嵌 tzdata 且 `LoadLocation` 失败 panic，period 统一在 `cfg.Location` 算，绝无 UTC fallback | 月/日额度边界静默偏移 |
| GW-INV-39 | 每个 runtime 数值 knob 在 env 与 overlay 共用 floor/ceiling；multi-key hot apply 先 clone→全量语义校验→单 tx persist→atomic swap；startup-hard/secret 指名拒绝 | 热改绕过 rate card、钱包、媒体或 OOM 护栏 |
| GW-INV-40 | SQLite 写池单连接 + `_txlock=immediate`，读池有界；两池 WAL/foreign_keys/synchronous 与 PERF-2 pragma 一致；最坏 RSS 超预算时按 GOMEMLIMIT 规则 warn/fail-fast | 丢写序列化破坏 atomic reserve，或 per-conn cache 使进程 OOM |

## 备注

- wire error 只描述客户端结果；是否 rollback 由 `CallFailure.ChargeExposure` 决定。尤其 `UPSTREAM_ERROR` 既可 DefinitelyUnbilled（明确 3xx/4xx）也可 ChargePossible（connect/5xx）。
- readiness 以 DeepSeek + DB + disk 为必需基线；可选 Kimi 缺 key 不使文本服务失活，配置 key 后则必须和 DeepSeek 一样通过 cached authenticated exact-model probe。
- 中间件链：`Recover → DenyCORS → MaxBody → mux`；带 `Origin` 的 `OPTIONS` 为 403 且不发 `Access-Control-*`。
- Sybil/PoW 旋钮默认 dormant：`INSTALL_POW_MODE=off`，其余 M2 cap 为 0 时在 DB 工作前短路。
