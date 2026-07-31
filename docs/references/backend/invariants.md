---
id: DOC-007
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-24
review-due: 2026-10-19
audience: [human, ai]
---

# 不变量登记册（GW-INV-NN）

> 验收准绳；编号永不复用。金额一律为整数 pico-US dollar（`1 USD=10^12 pUSD`），请求在冻结 rate card 下换算后才进入共享钱包。逐字 schema 见 [database.md](database.md)，wire code 见 [error-codes.md](error-codes.md)，输入/路由见 [api.md](api.md)，选型见 [ADR-0017](../../decisions/0017-qwen-visual-route-and-tiered-cost-ledger.md)。

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
| GW-INV-08 | Reserve 只接受由精确 provider/model rate card 构造且可自校验的冻结 `billing.Plan`；跨 provider raw token 绝不相加，只有 pUSD 可进入余额；ASR 时长 plan 必须显式标记为 audio-seconds，不能通过 token usage 结算 | 伪造零价 plan、把不同价格 token 当同一金额或把音频秒数伪装成文本 token 导致超卖 |
| GW-INV-09 | Chat Settle 从累计 usage 逐字段取最大快照，按冻结 rate card refund/top-up operator 月钱包与日统计表；任何 negative/malformed/duplicate 或 case-fold 等价 billing key 证据 sticky，后续正常帧不可洗白；missing/negative/矛盾/不可计价 usage 保留全 reservation。Qwen 退款须有正 `total_tokens≥prompt+completion` 且 reasoning≤`total-prompt`；已报告的 cached prompt token 不得超过 prompt 总数，未报告部分一律按 cache miss 全价计。Realtime ASR settle 不读取 token usage，只按成功转发 PCM 字节换算 billable seconds；actual 超 quote 仍如实 top-up 并发 `billing_drift` | 多帧 usage 重复累加、last-key-wins 洗掉负数、畸形帧被后续值掩盖、未知用量被退款、ASR 少计时长或已发生支出被 cap 隐藏 |
| GW-INV-10 | 视觉与文本输入都不能由本地字节数证明 token，故 chat reserve 恒取完整 `1,000,000` input + `65,536` output 的最高分档 hard limit；UTF-8 byte-fallback estimate 只作账务证据、绝不参与准入；Qwen realtime ASR reserve 固定 120s 时长上限并拒绝超过该上限的累计 PCM；任一已启用 route 的单请求 quote 必须能装入 operator 月预算，否则配置 fail-fast | 请求注定无法 reserve、byte-split tokenizer/tool 字段造成欠预留、Qwen hidden thinking 造成欠预留，或语音会话可发送超过预留上限的音频 |
| GW-INV-46 | dashboard 手动全员月请求额度重置只可操作当前 `RESET_TZ` 月的 `quota_monthly.requests`；同一写池事务先确认全库无 `spend_ledger(state='open')` 再清零，绝不改 pUSD 钱包、日统计或 ledger 历史；请求排在 reset 后才 reserve，归属新权益窗口 | 未终态 reservation 随后 rollback 扣掉新额度、手动权益操作掩盖真实 provider 成本，或并发请求落入语义不明的周期 |

## B. 安全

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-11 | 上游 key 永不离服务器：provider-local client 只在 cloned request 注入 auth；remote endpoint 强制 HTTPS（HTTP 仅 canonical loopback IP literal dev/test，拒 `localhost` 与 `127.0.0.1.` 等 DNS/NSS/proxy 绕过拼写），redirect 永不跟随；上游 header/body/error 原文绝不透传；≤4KiB 错误体只归一到闭集 reason；key 事件只记固定 provider + `key_index`；speech ASR WebSocket 同样由服务端注入 Qwen auth，client wire 不含 key | key 被明文/代理/redirect 带往错误主机，或上游账户细节与敏感错误体外泄 |
| GW-INV-12 | 身份只由 Ed25519 possession proof 建立：DB 仅存 public key + UNIQUE thumbprint，`/install` 同 key 幂等；每个 protected request 签名绑定 install/kid、±90s iat、5min server nonce、一次性 jti、method、authority+target 与 exact body hash；replay cache 满载 fail closed；无 bearer 兼容 | 复制 install id/请求即可滥用、篡改 body/path、跨域复用或重放，或同设备重复领取额度池 |
| GW-INV-13 | `/metrics` `/readyz` `/debug/pprof/*` `/debug/vars` 与 dashboard 均 loopback-only；`requireLoopback` 拒空 host/IP 非回环/解析到任一非回环地址；`/healthz` 不碰 DB | 管理/画像面公网暴露，远程 DoS 或凭证攻击 |
| GW-INV-14 | `DASHSCOPE_API_KEY`、`DASHBOARD_*`、`INSTALL_POW_SECRET` 均 secret-env-only，不进 `Specs`/settings/Dump；Snapshot 只给安全状态/数量，日志 redaction 覆盖 auth、media data 与 provider key 字段 | secret 或 inline media 经配置/日志/导出泄露 |
| GW-INV-15 | metric/audit 标签严格低基数：provider 仅 `qwen`，另仅固定 `outcome`/`handler`/`result`；禁止 model、install_id、token、prompt、media、ip 入 label | Prometheus 基数爆炸或 PII/内容入时序 |
| GW-INV-16 | business XFF 仅当直连对端为 loopback Caddy 时可信并取最右段，否则回退 `RemoteAddr`；dashboard 直连/SSH tunnel 的 login bucket **永远忽略 XFF**，只取 direct peer | 攻击者伪造 XFF 绕过 install 或 dashboard per-IP 限速 |
| GW-INV-17 | redaction 对 auth/cookie/provider-key/password/secret 精确名及 `*_api_key|*_password|*_secret` 后缀（大小写不敏感）直接 `[REDACTED]`，非 allowlist key 下 struct/map/slice 整体抹除；`image_url`、`input_audio`、inline/file data 在红线内 | 新 provider key、`slog.Any` 或 base64 media 被静默序列化 |
| GW-INV-18 | TLS 仅由 Caddy 终结；Go 三监听默认 loopback、地址必须两两不同，否则 `LoadBase` fail-fast | 误绑公网或运行时端口冲突 |
| GW-INV-19 | 管理后台**恒定挂载且恒定只绑 loopback**(绑定 fail-fast),进程内**不构造任何** credential/session/CSRF 状态:登录墙归前置 IAP,而前置 IAP 必须覆盖**所有**路径。**绝不可**把 8081 直接反代或暴露公网 | 在进程里再立一道更弱、且会与真边界各说各话的墙;或把没有前置 IAP 的后台暴露出去 |
| GW-INV-20 | `/install` 各拒绝路径用 DISTINCT wire code + 不采样 `install_audit`；只记 `/64 ip_key`/gate/error_code，fp 仅以 SHA-256 进入 `install_fp_rate` | Sybil 洪流不可区分，raw fp/ip/secret 入日志或 DB |

## C. 可靠性

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-21 | 两个 provider 共享一个固定容量 `N_GLOBAL_CONCURRENCY` 信号量，gateway→全部上游总在飞 ≤ N；provider 内换 key/retry 绝不新增 slot | 双上游把总并发翻倍、带宽/账户被打满 |
| GW-INV-22 | 每个 provider 有独立 process breaker；fault health 与 charge/retry 分开判：client-cancel、429、401/403 key signal、key-open、400/413/422 不计 process fault；5xx/timeout/connect 与其它被 typed health class 归为 upstream fault 的明确 refusal 可计一次。`DefinitelyUnbilled` 不推出“不计 breaker”，“计 breaker”也不推出“可 retry” | 一个 provider/客户端错误误熔断另一 provider，或把 health policy 当财务证明 |
| GW-INV-23 | provider 429 独立归一 `UPSTREAM_BUSY`、`DefinitelyUnbilled`：不 retry、不计 process/per-key breaker；`Retry-After>5s` 只给当前 key cooldown，当前请求仍 rollback | 429 retry 风暴、误熔断或已知拒绝仍收费 |
| GW-INV-24 | 关停严格 ①取消扫描 loop ②关闭三 HTTP server ③等待 bgWG≤30s ④最后关 DB；所有 detached settle/rollback/reconcile 均挂 bgWG | 结算中关 DB 留 open/半写账本 |
| GW-INV-25 | 流式每帧滚动 `SetWriteDeadline(now+30s)`；server 不设全局 `WriteTimeout` | 长回答被固定墙钟截断 |
| GW-INV-26 | 自动 retry 总 attempt≤3，且只重试可证明未收费并能绕过当前 key 的 401/403 cooldown 或 key-breaker-open；connect/TLS/read/timeout/5xx/client-cancel 全部 ChargePossible、终止并 full settle，429/其它 3xx/4xx 虽可退款也不 retry；始终同 provider/plan/slot | 一份 reservation 隐藏多次可能 provider charge，账本无法守恒 |
| GW-INV-27 | `UPSTREAM_HEADER_TIMEOUT_SEC` per attempt 界定 connect→header→first body byte；stream/non-stream 都必须成功 `Peek(1)` 才 handoff，2xx header 不提前停表；handoff 后 timer 已幂等 disarm，且不覆盖其余 body/stream。timeout 是 ChargePossible，不 retry、保留 full quote | non-stream headers-only stall 钉死 slot；timer race 截断已返回 stream；timeout retry 造成多次潜在收费 |
| GW-INV-28 | `QUEUE_WAIT_MS` 是共享信号量的有界等待：0=立即拒绝，超时→429，排队取消→499 审计；三者都在 Open 前 rollback 且不占/放大 slot | 无界排队、并发放大或取消请求虚占钱包 |
| GW-INV-29 | diskguard 每 30s 探测数据盘，低于 MB 或百分比阈值时在任何 reserve 前返回 `503 DISK_LOW`；启动同步预热、探测失败 fail-open、恢复自动清 | 磁盘满时中途写坏账本或探测抖动永久只读 |
| GW-INV-30 | 每个上游账号自带固定 endpoint、API key pool、per-key breaker/cooldown、process breaker，两个账号不共享其中任何一样；禁止 fallback、禁止跨池取 key；单 key 配置行为不变 | 一个账号的故障污染另一个账号，或实际费用与冻结 plan 不一致 |
| GW-INV-41 | provider 400/413/422 归一为 `400 UPSTREAM_REJECTED`，只暴露 `details.reason∈{context_length,max_tokens,invalid_request}`；Exposure=DefinitelyUnbilled，不 retry/不计 breaker 并 rollback；其它明确 3xx/4xx 也 DefinitelyUnbilled，但可归一成不同 APIError/fault class | 恶意超长输入触发全站 breaker、上游原文泄露或用 code 代替 exposure |

## D. 输入、能力与模型路由

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-31 | 入站 JSON transport 必须是合法 UTF-8；top-level chat 白名单只有 `model/messages/stream/temperature/max_tokens/n/tools/tool_choice`；client `thinking`/`reasoning_effort` 不入 canonical request；canonical request 只有 messages/stream/temperature/bounded optional max_tokens/tools/tool_choice + streaming usage；真实 model 与 provider knobs 仅由选中 provider adapter 附加 | invalid byte 经 RawMessage/字符串产生不同解释或膨胀报价；client 走私 provider 参数、key、价格或 reasoning 规格 |
| GW-INV-32 | `n>1` 经 raw probe 与 typed decode 双重拒绝为 `400 BAD_REQUEST` | 单请求扇出多个 completion、破坏报价上界 |
| GW-INV-33 | pre-reserve shape guard：messages 非空且 `len≤MAX_MESSAGES`；每 message 文本 rune 数≤`MAX_MESSAGE_CHARS`；body≤`MAX_BODY_BYTES` | 多小消息或单大文本造成内存/估算放大 |
| GW-INV-34 | business middleware 与 chat handler 共享 `MAX_BODY_BYTES`（默认 256KiB，范围 4KiB..8MiB，重启生效）；`/v1/install` 独立 8KiB | 未鉴权或 chat body 无界缓冲 |
| GW-INV-35 | 准入只由完整 history 的已验证 content 决定：全 string/文本 parts 按文本受理；任一受支持 image/video 额外要求媒体通道；任一合法 audio→reserve/Open 前 `AUDIO_UNAVAILABLE`。两种模态同走 `MULTIMODAL_UPSTREAM_MODEL`——内容形状决定的是**能力**，不是 provider。client `model` 永不选择 provider/实际模型；`/v1/models` 及 stream/non-stream 成功响应顶层 `model` 都只给 `PUBLIC_MODEL_ID`，且 duplicate/case-fold 等价 model key fail closed。non-stream 只放行单一 UTF-8 object；SSE 只放行 object data、精确 DONE/blank 与归一 bare-comment；只有完整读到合法 DONE 才允许按 stream usage 退款，提前 EOF/读错即 full quote | 客户端绕过成本/能力策略、只看最后一条消息而误路由，公开 alias 被真实 provider/version 漂移击穿，duplicate key 放大输出，或未完成 stream 用中途 usage 错误退款 |
| GW-INV-36 | message role 是闭集 `system|user|assistant|tool`（tool 必须有非空 `tool_call_id`）；`content` 是关闭联合类型：missing/null/string/parts；missing/null 仅合法 assistant tool-call；part 仅 `text`、`image_url`、`video_url`、`input_audio` 且未知/交叉字段拒绝；文本-only parts canonicalize 为 string；opaque `tool_calls` 原样保留 | 未知 role/多部分走私、provider 对同一 body 产生不同解释，或 Qwen function loop 第二步被 400 拒绝 |
| GW-INV-37 | reasoning 档位由服务端唯一确定。caller `max_tokens` 正数 wire 值为 `min(client,MAX_TOKENS_CAP,selected output limit)`，缺省/非正 wire 值为 `min(MAX_TOKENS_CAP,selected output limit)`；quote 始终按完整 1M/64K hard limits 的最高分档 | payload、账本与客户端 headroom 漂移，输出成本失控，或客户端绕过产品档 |
| GW-INV-42 | media 只允许 user role：image 必须 inline strict-base64 `data:image/{jpeg|png|webp};base64,...` 且 MIME=magic；video 必须 inline strict-base64 `data:video/mp4;base64,...` 且 MIME=MP4 容器标识；audio 必须 raw strict-base64 `input_audio{data,format}`，`format∈{wav,mp3}` 且 format=magic；remote URL/PDF/file/未知 part 一律 400，网关永不 fetch URL。合法 audio 固定在 reserve/Open 前 `503 AUDIO_UNAVAILABLE`，直到显式接入音频 provider | SSRF/下载放大/MIME 欺骗，或 compatibility 未定义输入进入上游；更不能把尚未计价的音频静默送给 Qwen |
| GW-INV-43 | 整请求 media part 数≤`MAX_MEDIA_PARTS`、累计 decoded bytes≤`MAX_MEDIA_DECODED_BYTES≤MAX_BODY_BYTES`；文本/tool JSON 的 UTF-8 estimate 仅用于 账务 quote、超过模型 input limit 时 clamp quote 而**绝不准入拒绝**，`INPUT_TOKEN_CAP` 仅兼容保留；真实 context 由选中 provider 判定。图片/视频由 shape/bytes 限内存，账本按 Qwen full hard limits 的最高分档预留 | base64 OOM、误用保守估算制造假超限，或实际 provider 拒绝污染 breaker/泄漏原文 |
| GW-INV-44 | `DASHSCOPE_API_KEY` 与 endpoint/workspace 是启动必需配置：Qwen backend 必须构造、readiness 的 cached authenticated `/models` probe 必须确认 Qwen 固定模型，绝无静默 fallback。合法音频始终先返回 `503 AUDIO_UNAVAILABLE` | 配置不全却错误启动，或媒体被错误送到文本模型 |
| GW-INV-45 | production deploy 只消费本仓成功 push-main CI 的精确且仍为 main tip 的 `head_sha`；远端 transition 先持久化 root-only marker，永久 Caddy condition 在 marker 存在时禁止 ingress 跨 reboot 自启；checksummed bundle 含 recovery program，commit/rollback 都须在兼容单元 durable 后清 marker，人工新发版遇 marker 必先 `--recover-incomplete` | CI 失败/过期代码上线，崩溃后半迁移 DB/二进制对公网开放，或恢复依赖已被下一版覆盖的脚本 |
| GW-INV-46 | durable media 只接受 proof-bound install-owned opaque upload id；chunk 必须以服务端确认的精确 offset 追加并 fsync 后 CAS cursor；完成重算 size+SHA 后才原子签发一个 lease。fetch token 为 HMAC 可重建值，SQLite 仅存 hash；仅作为短期 `fetchPath` query capability 返给已证明的 completion caller，读取路由统一 404 且不暴露 install/path/SHA | 并发/replay 覆盖字节、崩溃产生未确认尾部、跨 install 枚举/复用，或 bearer capability 落库/泄漏 |
| GW-INV-47 | media expiry 先写 DB 状态，后删私有文件，再 acknowledge；启动和每分钟恢复均截断 fsync-before-cursor crash tail、对 cursor 超过物理文件的 staging fail-closed，并重试未完成清理 | restart 后错误续传、过期媒体仍可被 provider 取用，或删除中断留下不可回收私有 bytes |
| GW-INV-48 | realtime speech ASR 是独立于 chat `input_audio` 的 proof-gated WebSocket：proof 绑定空 GET body；gateway owns Qwen `session.update` 与 credentials；client 只能发送≤256KiB 的 binary PCM frame 或 `commit|finish|cancel` control；`cancel` 只关闭本次会话、不转发为上游 JSON；client/upstream 两条 WebSocket leg 都有服务端 ping + 30s pong/message 滚动 deadline，单会话≤2min；每个会话进入同一 quota/ledger，按 120s 预留、按成功转发 PCM 时长 settle、无音频 rollback；关闭/未配置返回 `SPEECH_UNAVAILABLE`，不得落入 chat audio route 或任意上游 JSON 透传 | 用户配置绕过 ASR 产品档、长连接烧资源、key 泄漏，或把尚未计价的音频内容理解误当转写送上游 |
| GW-INV-49 | 品类日闸(0006):图像预留在**同一** BEGIN IMMEDIATE 里按张消耗 `install_category_daily.units`,超 `IMAGE_DAILY_LIMIT` 拒 `IMAGE_QUOTA_EXHAUSTED`;限额=0 仍记账;rollback 恰按 Reservation 快照(`CategoryApplied/CategoryUnits`)反转、绝不重读 live config;ledger 行以 `category/category_units` 与预留逐字段对账 | 日闸旁路 = 免费档图像成本失控;反转读 live 配置 = 热改限额期间账目漂移 |
| GW-INV-50 | 生成能力(图/语音/视频/音色)成功即按冻结卡全额 settle(reserve==settle,确定性单位成本);客户端是否下载产物 URL 与计费无关;**提交后**的 timeout/connect/5xx 等**歧义**结果照 full quote settle,绝不 rollback。**歧义之外照 GW-INV-02/23 退款**:显式 4xx(400/401/402/403/413/422)与 **429** 是「上游拒了请求、根本没开始生成」,一律 `DefinitelyUnbilled` → rollback;「什么也没离开本进程」同理。**不计 breaker 与不可退是两条轴**(GW-INV-22),混为一谈的结果就是用户为被限流付费 | 下载与否影响计费 = 可白嫖;歧义 rollback = 上游已扣我们未记;**把 429 当不可退 = 用户为一次上游拒绝掏钱,而我们一分没被收** |
| GW-INV-51 | 直通给客户端的上游产物 URL 不含任何**网关配置的** upstream API key(OSS 签名参数 `OSSAccessKeyId` 是上游临时访问标识、非网关凭证,不在此列);机械测试:URL 串与全部已配 key 逐一取交为空 | key 随 URL 出端 = 凭证泄漏,免费档全线沦陷 |
| GW-INV-52 | 品类日账本**逐品类独立**:`install_category_daily` 按 `(install, category, day)` 分行,图像按张、语音按**输入字符**各记各账;任一品类耗尽绝不影响另一品类;`planCategory`/`categoryCap` 是封闭 switch,新品类连同自己的 `Limits` 字段与 wire 码一起立法 | 共用计数器 = 图像用尽把语音一起关掉(反之亦然),用户被告知一个自己根本没碰过的能力没了 |
| GW-INV-53 | 音色登记(0007)守**三**道闸,顺序恒为「本地前置 → 钱包 → 上游」:①逐 install 库存 2 与重名(付费调用**之前**查,store 事务内再查一次,竞态与迟到给**同一个** wire 码)②账号级 `VOICE_ACCOUNT_CEILING`(满则拒 + WARN,**绝不驱逐**)③品类日闸 `VOICE_DAILY_LIMIT` 与 pUSD 预留在同一个 BEGIN IMMEDIATE 里。成功按全额 settle;**只有**可证明的创建前拒绝 rollback,歧义保留计费(GW-INV-50)。记录写失败时删掉上游登记但**不退款**——收回的是库存位、不是费用 | **库存上限不是花钱上限**:它界的是同时持有几个,而删除会腾位,故没有日闸与钱包时 enroll→delete 循环可以无界烧掉真钱;先调用后拒绝 = 为一个用户留不住的音色收钱;先删记录后删上游 = 一份永久不可见的已付费登记 |
| GW-INV-54 | chat **单模型**:文本与多模态同走 `MULTIMODAL_UPSTREAM_MODEL`,第二家 provider 已整仓撤除(代码、配置、账本身份、部署与文档)。**三条必须写下来的后果**——①**一次故障带走整个 chat 面**(此前文本在第二家厂商上,只损失多模态);②**每次请求都按卡上限预留**(Qwen 兼容 usage 证明不了 thinking-token 子上限,故报价恒为完整输入/输出上限;客户端 `max_tokens` 仍然约束**线缆**、但不再缩小**报价**);③**漂移告警结构性沉默**(用量不可能超过顶格预留),机制保留是为将来某条报价重新变成估算的路由 | 这三条都不是 bug、是收敛换来的代价;不写下来,下一个人会在一次事故里、或在一份「为什么钱包这么快满」的工单里重新发现它们 |
| GW-INV-53 | 品类日闸的拒绝**自带品类名**(`*quota.CategoryDailyExceededError`,并满足 `errors.Is(ErrCategoryDailyExceeded)`);app 层按品类映射 wire 码(image→`IMAGE_QUOTA_EXHAUSTED` / speech→`TTS_QUOTA_EXHAUSTED`),未知品类**不兜底**、原样上抛 | 光有伞 sentinel 会逼 app 层拿某一个品类的码代表全部(已实际发生过);兜底 default 则会对着用户没碰过的能力说「明天再试」 |
| GW-INV-54 | 语音合成按**输入字符数**(rune,非 byte)预留并 settle——字符数在调用前精确已知,故 reserve==settle 且无需上游 usage 回报;`voice` 缺席时由 `TTS_DEFAULT_VOICE` 填入而非空串上线缆;直通的产物 URL 归一到 `https`(上游可能返 `http` 的 OSS 结果 URL,而本系统两端都拒绝明文取产物;OSS 预签名覆盖 path/query 不覆盖 scheme——真钱冒烟里出 403 即此假设被推翻) | 按 byte 计费把每条中文请求静默乘三;空 voice 上线缆 = 上游 400;明文产物 URL = 桌面下载器按铁律拒收,能力在真机上等于不存在 |
| GW-INV-55 | 视频句柄是**签名**的:`GET /v1/videos/{id}` 只对签发给**同一个 install** 的句柄作答,常数时间比较;签名验不过与上游已忘掉该任务**同答** `VIDEO_TASK_NOT_FOUND`,且被拒句柄**绝不到达上游**。句柄密钥由 `MEDIA_SIGNING_SECRET` **域分离**派生(`HMAC(secret,"anselm-gateway-video-handle-v1")`),`VIDEO_ENABLED` 而 secret 缺席则启动即失败 | 裸转发上游 task id = 任一 install 可枚举并读走**别人**的视频 URL;两种拒绝分开答 = 确认「有别人拥有它」;空 key 会让每个句柄对每个 install 都验得过 |
| GW-INV-56 | 视频的钱**落在提交**:受理成功即按冻结卡全额 settle(reserve==settle,确定性按秒成本),**轮询绝不动钱**——成功不动、失败也不动、重复轮询也不动;上游生成失败**照样付费**,唯一会退的仍是可证明未计费的显式拒绝(GW-INV-50) | 「只在客户端回来轮询时退款」= 走开的人白拿视频、等着的人付钱;轮询动钱 = 一个等三分钟问十几次的客户端会被自己的耐心罚款 |
| GW-INV-57 | 品类账本与钱账本**允许数不同的单位**:视频钱按**秒**(`InputVideoSeconds`)、`video` 品类按**条**(恒 1)。`planCategory` 是这条分歧唯一的立法处,新品类连同 `Limits` 字段、`categoryCap` 分支与 wire 码一起在此立法;**且必须活着穿过 bootstrap 适配器**(`quotaCfgSource.Limits`)——`SPEECH_DAILY_LIMIT` 曾集齐配置、宣告、账本、分支与 wire 码却漏了那一行,于是 store 读到的上限恒为 0、闸一次都没生效过,而没有任何测试是红的 | 按秒配给只会让用户写更短的提示词而不是少生成视频;一个没穿过适配器的上限是一句谎——每层各自都对,中间那根线根本不存在 |

## E. 跨切配置 / 可观测

| id | 一句话 | 失守后果 |
|---|---|---|
| GW-INV-38 | `RESET_TZ` 内嵌 tzdata 且 `LoadLocation` 失败 panic，period 统一在 `cfg.Location` 算，绝无 UTC fallback | 月/日额度边界静默偏移 |
| GW-INV-39 | 每个 runtime 数值 knob 在 env 与 overlay 共用 floor/ceiling；multi-key hot apply 先 clone→全量语义校验→单 tx persist→atomic swap；startup-hard/secret 指名拒绝 | 热改绕过 rate card、钱包、媒体或 OOM 护栏 |
| GW-INV-40 | SQLite 写池单连接 + `_txlock=immediate`，读池有界；两池 WAL/foreign_keys/synchronous 与 PERF-2 pragma 一致；最坏 RSS 超预算时按 GOMEMLIMIT 规则 warn/fail-fast | 丢写序列化破坏 atomic reserve，或 per-conn cache 使进程 OOM |
| GW-INV-58 | `GATEWAY_MODE∈{debug,production}`（拼错 fail-fast，空值=执行）是配给类闸门的唯一总开关：掩码在 `Provider` 每次 swap 由纯函数 `EffectiveLimits()` 算**一次**，`Load()` 恒返掩码后配置、`Configured()` 恒返未掩码配置（override 基准 + dashboard `Dump`）；掩码集仅限**配给用户**的闸（月请求额度、operator 月钱包、`RATE_PER_MIN`/`DAILY_SUBLIMIT`/`TOKEN_ANOMALY_RPM`、图/语音/视频日闸、四道领号闸 + PoW），且掩码值必须仍在各自 registry 闭区间内；body/media/`MAX_TOKENS_CAP`/`N_GLOBAL_CONCURRENCY`/磁盘/内存等**进程保护**项与 reserve/settle/`spend_ledger` 记账两模式**逐字不变** | 掩码若在各执行点分别判断，迟早漏掉一处而使某道闸在 production 下静默失效；若 override/Dump 以掩码为基准，debug 期间任一次无关后台编辑都会把「全开」固化进 settings、永久摧毁生产值；若掩码触及进程保护或记账，debug 就成了 OOM 与「花了钱没有账」的路子 |

## 备注

- wire error 只描述客户端结果；是否 rollback 由 `CallFailure.ChargeExposure` 决定。尤其 `UPSTREAM_ERROR` 既可 DefinitelyUnbilled（明确 3xx/4xx）也可 ChargePossible（connect/5xx）。
- readiness 以 Qwen + DB + disk 为必需基线；那个固定模型必须通过 cached authenticated exact-model probe。
- 中间件链：`Recover → DenyCORS → MaxBody → mux`；带 `Origin` 的 `OPTIONS` 为 403 且不发 `Access-Control-*`。
- Sybil/PoW 旋钮默认 dormant：`INSTALL_POW_MODE=off`，其余 M2 cap 为 0 时在 DB 工作前短路。
