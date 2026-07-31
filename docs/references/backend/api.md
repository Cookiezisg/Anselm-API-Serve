---
id: DOC-003
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-24
review-due: 2026-10-19
audience: [human, ai]
---

# HTTP API 契约（business / admin / dashboard 三 mux）

> Go 1.22 method+path `ServeMux`。成功默认是裸实体，失败统一 `{"error":{"code","message"[,"details"]}}`；`/v1/models` 刻意保 OpenAI list wrapper。错误逐字值见 [error-codes.md](error-codes.md)，配置边界见 [config.md](config.md)。业务身份统一采用 [ADR-0015](../../decisions/0015-device-bound-request-proof.md) 的 Ed25519 逐请求设备证明，无 bearer token。

## 1. business mux（`LISTEN_ADDR`，默认 `127.0.0.1:8080`）

外→内中间件：`Recover → DenyCORS → MaxBody(MAX_BODY_BYTES) → mux`。除 `/healthz` 外，固定低基数 RED handler label。生产 Caddy 的 8MiB `request_body` edge cap 与部署值对齐；edge 与 Go `http.MaxBytesError` 都返回同一 `413 REQUEST_BODY_TOO_LARGE` JSON envelope。

| 方法 + 路径 | label | auth | 成功 / 说明 |
|---|---|---|---|
| `POST /v1/install` | `install` | 注册 proof + public key | 公钥登记；同 key 幂等返回 `{installId,monthlyQuota,resetAt}`；handler 独立 8KiB body cap；Sybil/PoW 默认 dormant |
| `GET /v1/install/challenge` | `install_challenge` | 无 | 120s 无状态 HMAC PoW challenge |
| `GET /v1/proof/challenge` | `proof_challenge` | 无 | 5 分钟 HMAC-authenticated request nonce，client 可缓存 |
| `POST /v1/chat/completions` | `chat_completions` | device proof | OpenAI-compatible chat；proof 绑定 exact body；按 content capability 确定性路由 |
| `GET /v1/speech/asr` | `speech_asr` | device proof | Realtime speech-to-text WebSocket；binary PCM 输入，Qwen ASR 事件输出；proof 绑定空 GET body |
| `GET /v1/quota` | `quota` | device proof | 裸 `{limit,used,remaining,resetAt,available}`；前三项是月请求次数，available 也折入 operator 月钱包 |
| `POST /v1/images/generations` | `images_generate` | device proof | 同步图像生成。请求 `{model?,prompt,size?,n?}`——`model` 是逻辑别名不选上游、`prompt` 非空 ≤2000 字符、`size` 为 `WxH`(边 256–4096 且总像素 512²–2048²,默认 `1024x1024`)、`n` 缺席或恒 1,未知字段拒;响应 `{created,data:[{url}]}`。详见 §1.5 |
| `POST /v1/images/edits` | `images_edit` | device proof | **改图** = generations 的形状 + **`image`**(必填,**base64 data URL**)。每一道闸与 generations 共用,按同一张图像卡计价、走同一条图像日额度。详见 §1.5 |
| `POST /v1/audio/speech` | `audio_speech` | device proof | 同步语音合成。请求 `{model?,input,voice?}`——`input` 非空 ≤500 **字符**(按 rune 计)、`voice` 缺席取 `TTS_DEFAULT_VOICE`,未知字段拒;**响应是裸 `audio/wav` 字节,不是 JSON 信封**。详见 §1.6 |
| `POST /v1/videos/generations` | `videos_generate` | device proof | **异步**视频生成提交。请求 `{model?,prompt,seconds?,aspect?,resolution?}`——`seconds` 缺席=5 且必须落在 2–15、`aspect` ∈ `landscape`(默认)/`portrait`/`square`、`resolution` ∈ `720p`(默认)/`1080p`,未知字段拒;响应 **202** `{id,object:"video.generation",status:"pending",created}`,`id` 是**签名句柄**。详见 §1.3 |
| `POST /v1/videos/animations` | `videos_animate` | device proof | **图生视频** = generations 的形状 + **`image`**(必填,**base64 data URL**,首帧);`aspect`/`resolution` 照常解析但**转发时整个丢弃**。详见 §1.3 |
| `POST /v1/voices` | `voice_enroll` | device proof | **参考音色登记**。请求 `{name,leaseId}`——`name` 非空 ≤120 字符;**`leaseId` 指名一个本 install 已上传好的媒体 lease,绝不是地址**。详见 §1.7 |
| `GET /v1/voices` | `voice_list` | device proof | 列出本 install 的音色:`{voices:[{voiceId,name,createdAt}],capacity,remaining}`。返全集、无游标(**N4 豁免①**:有界可枚举资源)。详见 §1.7 |
| `POST /v1/voices:delete` | `voice_delete` | device proof | 删除一个音色,请求 `{voiceId}`,成功 **204**。是 `:action`(**N5**)而非 `DELETE /v1/voices/{id}`。详见 §1.7 |
| `GET /v1/videos/{videoId}` | `video_status` | device proof | 轮询一个签名句柄:响应 `{id,object:"video.generation",status,url?}`——`status` ∈ `pending`/`running`/`succeeded`/`failed`(网关**自己的**封闭词表、非上游六态),`url` 仅 `succeeded` 时出现且是裸预签名 OSS 链接(取它时**不带**任何 Authorization 头);**完全不动钱**——不碰钱包、不碰品类账本,唯一的闸是限流器;签名验不过与上游已忘掉该任务**同答** `VIDEO_TASK_NOT_FOUND`(区分两者等于确认「有别的 install 拥有它」) |
| `GET /v1/models` | `models` | device proof | OpenAI list 中恰一个逻辑模型；model object 另带 namespaced `anselm_capabilities`，见 §2.3 |
| `POST /v1/media/uploads` | `media_create` | device proof | 创建 proof-bound resumable upload，返回 opaque `uploadId`、`offset=0`、过期时间与 chunk 上限 |
| `GET /v1/media/uploads/{uploadId}` | `media_status` | device proof | 返回 install-owned、仍 open 的权威 `offset`；仅用于 ambiguous chunk result 后安全续传 |
| `DELETE /v1/media/uploads/{uploadId}` | `media_cancel` | device proof | 先持久化为 `aborted` 再删除私有暂存字节；同 install 的重试安全，周期 recovery 会确认清理 |
| `PUT /v1/media/uploads/{uploadId}` | `media_append` | device proof | raw bytes；必须带精确 `Upload-Offset`，成功返回新的 offset；不接受 multipart |
| `POST /v1/media/uploads/{uploadId}/complete` | `media_complete` | device proof | 空 body；重算 staged file 的 SHA-256 且以 bytes magic 重验声明 MIME 后，才返回 opaque `leaseId` 与短期 `fetchPath` |
| `GET /v1/media/leases/{leaseId}/content?token=…` | `media_fetch` | HMAC capability | 短期内容拉取口（运维/诊断可用）；不要求/不接受 device proof，失败统一 404。**模型侧已不经它取内容**——chat 把已校验 lease 内容内联上游（ADR 0012） |
| `GET /healthz` | 不包 RED | 无 | liveness；不碰 DB/provider |

设备证明头：protected call 带 `X-Anselm-Install-ID` + `X-Anselm-Proof`；registration 改带 `X-Anselm-Public-Key` + `X-Anselm-Proof`。proof payload 固定 `{v,kid,iat,jti,nonce,htm,htu,bh}`，Ed25519 签名覆盖 base64url payload；`htu` 是 lowercase authority + path/query，`bh` 是 exact body SHA-256。空/未知 install→401，坏签名/过期→401，重复 jti→409，banned→403。带 `Origin` 的 `OPTIONS` 一律 403，且不发任何 `Access-Control-*`。

### 1.1 Durable media upload

该接口只在 `MEDIA_ENABLED=true` 时可用；关闭时仍要求 device proof，返回 `503 MEDIA_UNAVAILABLE`。创建 JSON 必须严格为 `{"sha256":"<64 lowercase hex>","mimeType":"image/jpeg|image/png|image/webp|video/mp4|audio/wav|audio/mpeg","totalBytes":N}`，未知字段拒绝。`GET upload` 只返回仍 open 的 install-owned cursor，客户端在 raw `PUT` 连接中断、无法确认服务端是否写入时必须先读它，不能盲重发。每个 `PUT` 的 body 是原始 chunk，`Upload-Offset` 必须是非负十进制且等于服务端已确认 offset；chunk 长度不得超过 `MEDIA_CHUNK_MAX_BYTES` 和全局 `MAX_BODY_BYTES`。上传 id 对其他 install 不可枚举（统一 `MEDIA_UPLOAD_NOT_FOUND`）。

完成必须在 `receivedBytes==totalBytes` 后从私有 staging 文件复算字节数与 SHA-256；仅这一步原子地将 upload seal 为 completed 并创建一次 lease。响应的 `fetchPath` 是短期 HMAC capability：含 token query，不含 install、原始 SHA 或文件路径。**客户端把它以相对形原样嵌进 chat 请求的 `image_url`/`video_url`**（ADR 0011——凡带 scheme/host 一律拒）；chat 校验归属与时效后**读出内容、以 data URI 内联转发上游**（ADR 0012——上游 provider 的拉取器拒绝从本网关公开主机下载，线上实测；内联同时消灭对第三方拉取策略的依赖）。读取端点不走 device proof（provider 无法携带），只校验 token、lease active 状态与 expiry，任一失败统一 404；成功 `Cache-Control: private, no-store`、`nosniff`。文件先 fsync、再 CAS 推进 cursor；崩溃后启动/定期恢复会截去未持久化 cursor 的文件尾，过期 capability 先持久化撤销、后删除文件。

### 1.2 Realtime speech transcription

`GET /v1/speech/asr` 是设备证明保护的 WebSocket 窄代理，用于“麦克风输入转可编辑文本”，不属于 chat `input_audio` 模型理解路由。proof 的 exact body 为空，`htu` 仍覆盖 path/query。可选 query：

| query | 说明 |
|---|---|
| `language` | 透传给 Qwen `input_audio_transcription.language`；空值表示上游自动处理 |

客户端只允许发送两类帧：

| client frame | 说明 |
|---|---|
| binary | 16kHz PCM bytes；单帧≤256KiB；空帧拒绝 |
| text JSON | 仅 `{"type":"commit"}` 或 `{"type":"finish"}`；其它字段/类型不构成能力 |

网关 owns upstream `session.update`：`input_audio_format="pcm"`、`sample_rate=16000`、`turn_detection.type="server_vad"`、`silence_duration_ms=400`。随后 binary frame 被编码成 Qwen `input_audio_buffer.append`，`commit`/`finish` 分别转成 `input_audio_buffer.commit`/`session.finish`，`cancel` 只关闭本次会话、不转发为上游 JSON。服务端把 Qwen ASR realtime 事件原样作为 text WebSocket message 返给客户端；典型事件包括 partial delta、completed transcription 与 `session.finished`。服务端对 client leg 与 upstream leg 均发送 WebSocket ping，收到 message/pong 后滚动 30s 读 deadline，单次会话仍有 2 分钟绝对上限。ASR 会话在上游拨号前进入同一 quota/ledger：按 `qwen3-asr-flash-realtime` 预留 120 秒，按成功转发 PCM 字节向上取整为秒结算；累计音频超过预留上限会返回帧内 `SPEECH_AUDIO_TOO_LONG` 并结束会话。部署未配置 Qwen key/endpoint 时返回 `503 SPEECH_UNAVAILABLE`；上游 key 永不进入 client wire/log。

真实上游验收入口为 `make qwen-asr-evals`。它只在显式 `EVALS_QWEN_ASR=1` 下运行，要求
`DASHSCOPE_API_KEY`/`EVALS_KEY` 加 `DASHSCOPE_WORKSPACE_ID`/`EVALS_BASE_URL`；可选
`EVALS_ASR_WAV=/path/to/pcm16-mono-16k.wav` 时会要求真实转写文本，否则只做 live protocol/connect smoke。

### 1.3 Asynchronous video generation

视频是本网关**唯一的两次请求**能力。chat / image / speech 都在一次 HTTP 请求里开始并结束,故「谁有权读这个结果」从来不必问;视频先提交、桌面端几分钟后才轮询,而这两次请求之间那个答案必须由**某个东西**带着。

**签名句柄**。提交返回的 `id` 不是上游 task id,而是 `base64url(taskId).base64url(HMAC-SHA256(key, installId ‖ 0x00 ‖ body)[:16])`。install id 在 MAC **之内**却不在句柄之内——桌面端本来就知道自己是哪个 install,把它放上线缆只是白白公开它。验证用常数时间比较(这是一次鉴权检查,tag 上的时序侧信道会把伪造一个字节一个字节地送出去)。

**句柄密钥不是 env**。它由 `MEDIA_SIGNING_SECRET` **域分离**派生(`HMAC(secret, "anselm-gateway-video-handle-v1")`),每次 config load 重算、不落任何地方。于是开视频不必让运营者再放一个要轮换、要防泄的 secret,而其中一把被攻破仍伪造不出另一把。`VIDEO_ENABLED=true` 而 secret 缺席时**启动即失败**(`CONFIG_VIDEO_HANDLE_KEY_REQUIRED`)——一次「提交得了却永远轮询不到」的生成,会花掉用户当天的一条额度去换一条他拿不到的片子,比根本不提供更糟。

**钱的形状(与其余能力都不同,故在此明说)**:**费用落在提交,轮询绝不动钱**。一次在上游失败的生成**照样付费**。这是刻意的——另一条路是「只有客户端肯回来轮询时才跑」的退款路径,那意味着**走开的人白拿视频、等着的人付钱**。免费档的钱绝不能取决于有没有人回来看。唯一会退的路径仍是可证明未计费的显式上游拒绝(GW-INV-50);超时、5xx、200 却没有 task id 一律保留全额。

**两个账本数不同的单位**。钱按**秒**报价(`InputVideoSeconds`,wan 720P 每秒一张卡);`install_category_daily` 的 `video` 品类按**条**配给(`VIDEO_DAILY_LIMIT`,默认 **10**)。人心里配给的是整条片子,而不是秒数——按秒配给只会让用户写更短的提示词、而不是少生成视频。

**不可能的时长在预留之前被拒**。`seconds` 越界返 400 且分文未付,而不是把当天的一条额度花在一个上游 400 上。

**图生视频(`/v1/videos/animations`)共用以上每一条**。它与 generations 只差一个必填的 `image` 首帧,其余闸(prompt 上限、预留之前就拒的时长窗、未知字段拒)、计价单位、日额度、轮询端点与签名句柄全部相同——一段动起来的片子**花的是视频的秒数、产出的也是视频**。`image` 必须是 `data:` 且不含 `://`(§1.4 的形状闸),自己的码 `VIDEO_FRAME_INVALID` 而非复用 `IMAGE_SOURCE_INVALID`:一个在让图动起来的客户端被告知「**改图**的源无效」会去查错的请求。`aspect`/`resolution` 照常解析、**转发时整个丢弃**——片子继承首帧几何,转发我们的等于静默要求上游对用户刚递来的那张图做信箱边或裁切;仍然解析,故词表外的值依旧 400、不被静默忽略。上游是与出视频**同一对**提交/轮询端点、input 多一个 `img_url`。

**拒绝码**:`VIDEO_UNAVAILABLE` / `VIDEO_QUOTA_EXHAUSTED` / `VIDEO_FRAME_INVALID`(仅 animations) / `BAD_REQUEST` / `UPSTREAM_*`;轮询另有 `VIDEO_TASK_NOT_FOUND`。

### 1.4 两条贯穿生成能力的规则

**产物 URL 直通(图像与视频)**。这两条路由的响应主形是**上游 OSS 签名 URL 原样直通**,不是把字节内联回来。理由是物理的:本部署公网出方向约 1Mbps,一张 1–3MiB 的图 base64 内联要占 11–30 秒的上行,而 DashScope 的生成结果本就是 24 小时有效、不含任何网关凭证的签名链接——直通让客户端**直连上游**下载,网关的水管零占用。

这条规则带三个后果,都必须写下来:①计费点在**生成成功**,不在客户端下载——URL 给出去没人取也照扣(GW-INV-50);②直通的 URL 必须可证明不含任何**网关配置的** key(GW-INV-51;OSS 签名参数 `OSSAccessKeyId` 是上游临时访问标识、不在此列);③它**对语音不适用**——那条路根本没有产物 URL 可直通(§1.6),所以「响应形状不统一」是上游形态的事实、不是疏漏。

**受管媒体输入只收 `data:`,绝不收地址**。`/v1/images/edits` 的 `image`、`/v1/videos/animations` 的首帧都必须是 base64 data URL 且不含 `://`。这**不是校验客套,而是全部的 SSRF 缓解**:一个本网关会去取的地址是一枚指向我们自己网络的原语(包括 `169.254.169.254` 这种看起来最人畜无害的),而 data URL **取不了**——它就是字节。闸在 **service 层**、不在 handler:一条「只在某个调用方守规矩时才成立」的边界不是边界。ADR 0011 的**入站**那半由此原样成立;唯一的窄缝例外是音色登记的**出站**一跳(§1.7)。

### 1.5 图像生成与改图

上游走**原生** DashScope multimodal-generation 同步形,成功即按张 settle(reserve == settle,确定性成本)。

**出图与改图是同一个模型、同一条端点**。`IMAGE_UPSTREAM_MODEL` 一个配置项服务两条路由:`qwen-image-2.0` 官方描述即「生成与编辑的融合」,两次真钱验证成立(2026-07-28,同模型先生成后改图、皆 200 出图);单独的 `qwen-image-edit*` 那几个 id **正在退役**,留第二个配置项等于为模型原生就会做的事维护一个排期消失的 model id。改图按**图像**卡计价、走图像日额度、不开新品类。

**拒绝码**:`IMAGE_UNAVAILABLE` / `IMAGE_QUOTA_EXHAUSTED` / `IMAGE_SOURCE_INVALID`(仅 edits) / `BAD_REQUEST` / `UPSTREAM_*`。

### 1.6 语音合成

**响应是裸 `audio/wav` 字节,不是 `{data:[{url}]}` 信封**——这是上游逼的,不是偏好。`qwen-audio-3.0-tts-flash` **只**在 `api-ws/v1/inference` 双工 WebSocket 上提供服务(两种 HTTP 形状都答 `url error`,真机实测),而双工流回来的是**帧、没有产物 URL 可直通**。见 [ADR 0018](../../decisions/0018-speech-raw-bytes-over-websocket.md)。

**为什么是这个模型**:它是**唯一**既能用预置音色、又能用 `voice-enrollment` 铸出的克隆音色合成的模型。故「克隆音色与普通语音走同一配额」是**同一模型同一次调用**这个物理事实,不是产品硬凑。

**默认音色属于模型、不属于产品**。`TTS_DEFAULT_VOICE` 默认 `longanhuan_v3.6`:qwen-audio-3.0 的音色带 `_v3.6` 后缀且**直接拒绝** qwen3-tts 那套名字,故留一个过期默认值会让**每一次**省略 `voice` 的合成都在上游失败。线缆上**没有** format 字段。

**计费判别按有没有字节流出来**:`task-failed` 出现在任何音频之前 = 可证明未计费、可回滚;一旦有字节即保留计费;空的 `task-finished` 按歧义保留(GW-INV-50)。

**拒绝码**:`TTS_UNAVAILABLE` / `TTS_QUOTA_EXHAUSTED` / `BAD_REQUEST` / `UPSTREAM_*`。

### 1.7 克隆音色

样本由 **lease id 指名,绝不由地址指名**——ADR 0011 的入站那半原样不动。样本走普通断点上传,故 30 秒的音频不必塞进 JSON body。

**上游契约是真机实测出来的,与最初照猜的那份完全不同**:`model=voice-enrollment`、`action=create_voice/query_voice/delete_voice`、**只收公网可取 URL**(`data:` 会被拿去跑 ASR 然后 500)、**登记异步**(返回时状态 DEPLOYING,须轮询到 OK 才可用)、`target_model=qwen-audio-3.0-tts-flash`(必须与 TTS 路由真调的模型一致,否则不匹配**只在合成时**暴露、那时钱早花完)。用户起的名字**永不上游**——`create_voice` 收的是 `prefix` 命名空间,名字只住在我们表里。

**出站那一跳是唯一的窄缝例外**。网关把自己已经拥有的 lease 解析成**它自己的** `MEDIA_DOMAIN` 公开地址(ADR 0012 第 4 条为此留的门),上游取**一次**之后**立刻吊销该 lease**——那个 URL 是持有型凭据,撤销比缩短 TTL 紧且不依赖时钟。

**三道闸,顺序恒为「本地前置 → 钱包 → 上游」**:逐 install 库存 2(`VOICE_INVENTORY_FULL`,补救话术是「删一个」而非「过会儿再试」)、账号级 `VOICE_ACCOUNT_CEILING`(满则拒 + WARN,**绝不驱逐**别人的音色)、品类日闸 `VOICE_DAILY_LIMIT`(默认 2)。钱走与图像/语音/视频**同一个** pUSD 钱包(卡 `qwen-tts-clone-2026-07-28`,**非** assumed),reserve 在付费调用之前、settle 按全额;可证明的创建前拒绝才 rollback,一切歧义保留计费(GW-INV-50)。

**库存上限不是花钱上限**:它界的是**同时**持有几个,而删除会腾位,故没有日闸与钱包时 enroll→delete 循环可以无界烧掉真钱。

**部署慢不丢音色**:等待超时只记 WARN,登记照样落库——它已付费、上游做完就能用。

**列表返全集、无游标**(N4 豁免①:有界可枚举资源,而库存上限**就是**那个界)。`capacity`/`remaining` 随响应走,因为上限正是调用方来读它的理由:一个列出两行却不说「就这些了」的列表,会让下一次登记的失败无从解释。空库存序列化为 `[]` 而非 `null`。

**删除是 `:action`(N5)而不是 `DELETE /v1/voices/{id}`**:本网关受管面每条路由都是「header 带 device proof + POST」,一条带路径参数的 DELETE 会是整个面上唯一的异形;顺带让音色 id 不进 URL、因而不进代理日志与 referrer。**先删上游、再删记录**——记录是唯一持有上游 id 的东西,上游失败即中止且记录留着(可重试、库存计数继续说真话);在这里「成功」会留下一份还活着、已付费、永久不可见的登记。删除收回的是**库存位、不是费用**。别的 install 的 id 读作不存在(`VOICE_NOT_FOUND`),故音色 id **不是存在性预言机**。

**拒绝码**:`VOICE_UNAVAILABLE` / `VOICE_SAMPLE_INVALID` / `VOICE_INVENTORY_FULL` / `VOICE_NAME_TAKEN` / `VOICE_CAPACITY_REACHED` / `VOICE_QUOTA_EXHAUSTED` / `BAD_REQUEST` / `UPSTREAM_*`;列表与删除另有 `VOICE_NOT_FOUND`。

## 2. `POST /v1/chat/completions`

### 2.1 top-level 与 message 白名单

只 decode：

```json
{
  "model": "anselm-auto",
  "messages": [],
  "stream": true,
  "temperature": 0.7,
  "max_tokens": 1024,
  "n": 1,
  "tools": [],
  "tool_choice": "auto"
}
```

未声明 top-level 字段构造性丢弃；`n>1` 拒绝。client-supplied `thinking` / `reasoning_effort` 不在白名单内，也不能改变 provider knob。`max_tokens` 是 OpenAI 兼容调用参数：正数值在选定模型 output hard limit 与 `MAX_TOKENS_CAP` 内 clamp；缺省或非正值归一成同一个显式 cap 并写上游，使 wire、账务 quote 与客户端预留的 output headroom 一致。`tools` / `tool_choice` 与 message 的 `name` / `tool_calls` / `tool_call_id` 作为 opaque JSON 保留，支持完整 tool loop。历史 `reasoning_content` 一律在 adapter 内剥离,不回传上游。

`messages[].content` 是关闭联合类型：

| wire 形状 | 合法性 / canonical 结果 |
|---|---|
| JSON string | 文本 |
| parts array | 只允许下表三种 part；纯 text parts 按顺序拼接成 string |
| missing / `null` | 仅当 `role="assistant"` 且 `tool_calls` 是非空数组 |
| object/number/bool/未知 part | `400 BAD_REQUEST` |

message 数、每条文本 rune、整个 JSON body 分别受 `MAX_MESSAGES`、`MAX_MESSAGE_CHARS`、`MAX_BODY_BYTES` 约束。messages/tool JSON 的 UTF-8 byte-fallback estimate（含 framing 余量）仅作 悲观成本 quote：它可能显著高估 tokenizer 结果，**不参与准入、不与模型 input hard limit 比较**。`INPUT_TOKEN_CAP` 只为旧 env/settings 兼容保留，值不改变执行。

### 2.2 part 逐字形状

文本：

```json
{"type":"text","text":"describe this"}
```

图像：

```json
{
  "type":"image_url",
  "image_url": {
    "url":"data:image/png;base64,<strict-base64>",
    "detail":"auto"
  }
}
```

- URL 必须是 inline data URI；MIME 仅 `image/jpeg`、`image/png`、`image/webp`；base64 必须 canonical/strict，MIME 必须匹配 decoded magic bytes。
- `detail` 可省略；存在时仅 `auto|low|high`。

视频：

```json
{
  "type":"video_url",
  "video_url":{"url":"data:video/mp4;base64,<strict-base64>"}
}
```

- 仅接受 inline MP4 data URI；base64 必须 canonical/strict，声明 MIME 必须匹配 MP4 容器标识。

```json
{
  "type":"input_audio",
  "input_audio":{"data":"<strict-base64>","format":"wav|mp3"}
}
```

- 音频 `data` 是 raw strict base64（不是 data URI）；`format` 仅 `wav|mp3`，且必须匹配文件魔数。当前部署在任一 reserve/Open 前返回 `503 AUDIO_UNAVAILABLE`，因此它是**协议已知但尚未路由**的能力。

message `role` 是闭集 `system|user|assistant|tool`，且 tool message 必须带非空 `tool_call_id`。media 只允许在 `role="user"`；整请求 image+video+audio part 数≤`MAX_MEDIA_PARTS`，累计 decoded bytes≤`MAX_MEDIA_DECODED_BYTES`（lease 引用入口计零 decoded 字节，其预算在**内联时**按 lease 记录大小执行——同一口径，见下）。**唯一被接受的非 data 引用**是本网关自己的相对 lease fetch 路径 `/v1/media/leases/{id}/content?token=…`（ADR 0011）：chat 逐个校验「本网关签发给当前 install 且仍 active」（失败统一 404 `MEDIA_LEASE_NOT_FOUND`，绝不做存在性预言机；`MEDIA_ENABLED=false` 时 503 `MEDIA_UNAVAILABLE`），随后**读出内容以 data URI 内联进上游请求**（ADR 0012），累计超 `MAX_MEDIA_DECODED_BYTES` 则 400。其余远程 `http(s)` URL、PDF、file/file_id、未知 MIME/format/part、跨 variant 多余字段全部 400；gateway **不 fetch 客户端 URL**，也**不再要求 provider 回拉本网关**。

### 2.3 唯一路由表

准入扫描**完整 history**，与 client `model` 值无关。它决定的是**能力与受理**，不是 provider——两种模态都到达同一个上游模型：

| 完整 content | 受理 | 实际模型 |
|---|---|---|
| 只有 string / text parts / 合法 tool-call 空内容 | 文本请求 | `MULTIMODAL_UPSTREAM_MODEL`=`qwen3.7-plus` |
| 任一 accepted image/video part | 需媒体通道(`MEDIA_ENABLED`) | `MULTIMODAL_UPSTREAM_MODEL`=`qwen3.7-plus` |
| 任一 accepted audio part | 拒 | reserve/Open 前 `503 AUDIO_UNAVAILABLE` |

`PUBLIC_MODEL_ID` 是完整 client-facing alias：空、未知或任意 client `model` 都不能选 provider/价格；stream chunk 与 non-stream completion 的单一、大小写精确顶层 `model` 统一改写为该 alias，duplicate 或 case-fold 等价 key fail closed，真实 provider model 不出 wire（嵌套业务字段不误改）。non-stream 2xx 在写 200 前必须是单一完整 UTF-8 JSON object；SSE 只接受 data-only object、精确 `[DONE]`、空分隔行与被归一成裸 `:` 的 comment heartbeat，任何其它 control/畸形 data 都不透传 provider bytes 并保守结算。无 fallback。

网关统一产品档位由服务端注入，client 不能改：

| profile | provider knobs | context / output hard limit | `max_tokens` wire/accounting behavior |
|---|---|---:|---:|
| text 与 media 同为 Qwen3.7 Plus | top-level `enable_thinking=true`；历史 `reasoning_content` 不回传 | 1,000,000 input / 65,536 output | wire value is explicit `min(positive client or MAX_TOKENS_CAP,MAX_TOKENS_CAP,65536)`；cost reservation 恒按完整 hard-limit 的最高价格分档 |

adapter 剥离历史 `reasoning_content`(provider 私有,回传即再计费),但保留 opaque `tool_calls`。`GET /v1/models` 在标准 model object 上追加：

```json
{"anselm_capabilities":{"version":1,"routing":"content","text":{"input_limit":1000000,"output_limit":16384,"available":true},"multimodal":{"input_limit":1000000,"output_limit":16384,"available":true},"image_generation":{"available":true,"daily_limit":10},"speech_generation":{"available":true,"daily_limit":50000},"video_generation":{"available":true,"daily_limit":10}}}
```

`output_limit` 取各 route 模型硬上限与 live `MAX_TOKENS_CAP` 的较小值。

`available` 一律描述**调用方将要走完的整条路**、而非某一半:`multimodal` 要 Qwen key **且** `MEDIA_ENABLED`(没有上传通道时,信了这个标志的客户端会在第一次发媒体时**晚**失败、失败在对话中途);`image_generation`/`speech_generation` 要各自能力开关 **且** Qwen key;`video_generation` 还要**第三半**——句柄签名密钥,因为一个「提交得了却永远不让调用方轮询」的网关,宣告的是一个吃掉一条日额度、什么也不给的功能。三个 `*_generation` 都是增量字段(`version` 仍 1):旧桌面忽略之,旧网关缺席之而新桌面读 `nil`=不可用。`daily_limit` 的**单位逐能力不同**——图像是**张**、语音是**字符**、视频是**条**。通用 OpenAI client 可忽略该扩展，Anselm 按实际 prompt 是否含 native media 动态选 route budget。成本仍按冻结 rate card 预留：byte estimate 只作账务证据、在模型 input limit 处 clamp;reserve 恒用完整模型 input/output hard limits 后按自洽 usage 退款。

`DASHSCOPE_API_KEY` 与 Qwen endpoint/workspace 是启动期必需配置；缺失会 fail-fast，绝不以“纯文本照常”静默降级。音频不依赖 Qwen 视觉配置，始终先返回 `503 AUDIO_UNAVAILABLE`；未来音频 route 只能经新 capability 决策启用。Qwen 故障同样只返回自身归一错误，不跨 provider。

### 2.4 输出档位、stream 与账务可见行为

- `max_tokens` 由 caller 控制但受边界保护：正数转发 `min(client,MAX_TOKENS_CAP,selected output limit)`；缺省/非正转发 `min(MAX_TOKENS_CAP,selected output limit)`。quote 始终保守使用完整 1M/64K hard limits。
- stream request 强加 `stream_options.include_usage=true`；stream/non-stream 都须在 2xx 后等到首个 body byte 才 handoff，`UPSTREAM_HEADER_TIMEOUT_SEC` 覆盖这段等待。response SSE 逐行校验后 relay（仅 data JSON object 可携带业务 payload，并结构化改写唯一的精确顶层 `model`）、写 deadline 每帧滚动；non-stream body 最大读取 8MiB，须为单一 JSON object 后作同一改写。畸形/超限 provider success body 不原样透传。流式 usage 只有在网关完整读到合法终止帧 `[DONE]` 后才可用于退款；此前 EOF、读错或断连即使已见 usage 也保留 full quote。若 `[DONE]` 已读到，随后 client 写失败不抹掉这份终局证据。
- response 只写 gateway 白名单 header：Content-Type/Cache-Control 与 `X-Quota-Limit`、`X-Quota-Reset`；上游 header/auth 不透传。
- upstream failure 同时携带 client-facing `APIError` 与独立 `ChargeExposure`；client code 不决定退款：

| provider call 结果 | Exposure | retry / accounting |
|---|---|---|
| Open 前本地拒绝；明确 3xx/4xx（含 400/401/402/403/429） | `DefinitelyUnbilled` | rollback；只有 401/403 key cooldown 或 key-breaker-open 可换 sibling key，总 attempt≤3 |
| connect/TLS/read error、timeout、5xx、call 中 client-cancel、未知 exposure | `ChargePossible` | 不 retry；full reservation settle |
| 2xx 且首个 body byte 已到达（stream/non-stream） | charge 已可能发生 | 不 retry；non-stream 完整 object 或 stream 完整读到合法 `[DONE]` 后才按累计 usage settle；任一负数、duplicate/case-fold key 或畸形证据 sticky，提前 EOF、usage 缺失/矛盾/不可计价均 full quote |

`ChargePossible` 是 fail-safe 零值。同一个 `UPSTREAM_ERROR` 可对应明确 401/402 refusal（rollback）或 connect/5xx 歧义（full settle），调用方不得从 code/status/message 推导账务。429 与其它明确 3xx/4xx 虽可退款，也不自动 retry。账本状态见 [database.md](database.md)。

## 3. admin mux（`ADMIN_ADDR`，默认 `127.0.0.1:9090`）

无应用鉴权，完全依赖 loopback 物理隔离；绝不反代到公网。

| 方法 + 路径 | 说明 |
|---|---|
| `GET /metrics` | Prometheus；provider label 固定 `qwen` |
| `GET /readyz` | DB + disk + cached authenticated `/models` probe：Qwen key 与固定模型为必需 |
| `/debug/pprof/`、命名 pprof 路由 | CPU/heap/trace/cmdline/symbol |
| `GET /debug/vars` | expvar runtime gauges |

## 4. dashboard mux（`DASHBOARD_ADDR`，默认 `127.0.0.1:8081`）

`SecurityHeaders` 覆盖全部路由，dashboard **恒定只监听 loopback**（绑定 fail-fast）。进程内**不构造**任何 credential/session/cookie/CSRF：鉴权边界是前置 IAP，`/api/*` 直连。**没有登录端点**——它的缺席是设计，不是遗漏：在进程里再立一道更弱的墙不会多出边界，只会多出一样要和真边界保持同步的东西。

| 方法 + 路径 | session / CSRF | 说明 |
|---|---|---|
| `GET /healthz` | 否 | dashboard liveness |
| `GET /api/overview` | 前置 IAP | global budget 为 `{day,usedMicroUsd,limitMicroUsd,remainingMicroUsd,unit:"micro_usd"}`，`day` 当前承载 `YYYY-MM` 月预算窗口；固定带 `providers.qwen`，为 `{configured,breakerOpen}`；`upstreamBreakerOpen` 是「有没有哪家上游正在甩负载」的聚合 breaker 的兼容聚合；另有 inflight/open ledger/disk/rate/install 指标 |
| `GET /api/config` | 前置 IAP | secret-free Dump |
| `POST /api/config` | 前置 IAP | runtime-hot batch，全有或全无 |
| `GET /api/installs` | 前置 IAP | safe 行；`todaySpendMicroUsd`，无 token/fp/ip |
| `POST /api/installs/ban` / `unban` | 前置 IAP | install 状态变更 |
| `POST /api/quota/reset` | 前置 IAP | body `{reason}`（非空，审计原因）；原子清零当前 `RESET_TZ` 月所有 `quota_monthly.requests>0`，返回 `{period,resetInstalls}`；任何 `spend_ledger(open)` 存在时 `409 QUOTA_RESET_BUSY`；绝不改 pUSD 钱包、日统计或 ledger 历史 |
| `GET /api/audit` / `GET /api/export` | 前置 IAP | 审计 / 一致 DB snapshot |
| `GET /` | 否 | embedded SPA/static fallback |

## 5. 错误头

`APIError.Details.retryAfterSec` 存在时，renderer 同步写 `Retry-After` delta-seconds；当前用于 `LOGIN_LOCKED`。状态行前先写 Content-Type/Retry-After，message 永远是静态 client-safe 文本。
