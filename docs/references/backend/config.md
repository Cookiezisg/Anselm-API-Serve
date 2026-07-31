---
id: DOC-006
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-24
review-due: 2026-10-19
audience: [human, ai]
---

# 配置全面（config）

> `internal/domain/config/spec.go` 是 dashboard/apply registry，`config.go` 是边界与跨字段语义事实源，`internal/infra/configprovider/load.go` 是 env/default 事实源。金额在 env/dashboard 使用整数 microUSD，进入账本后精确换算为 pUSD：`1 microUSD=10^6 pUSD`、`1 USD=10^12 pUSD`。路由/计价决策见 [ADR-0017](../../decisions/0017-qwen-visual-route-and-tiered-cost-ledger.md)。

## 1. 三层级与 secret 边界

| Tier | dashboard | settings | 生效方式 |
|---|---|---|---|
| `TierRuntimeHot` | 可编辑 | 可持久化 | clone→全量校验→单 tx persist→atomic swap |
| `TierStartupHard` | 只读（在 `Specs` 中的项） | 禁止 | env + restart |
| secret（故意不在 `Specs`） | 不出现 | 禁止 | env + restart |

Secrets：`DASHSCOPE_API_KEY`(**启动必需**——每一条路由都去这一个上游,没有它的部署什么也服务不了,故在启动时失败、不在每个请求上失败)、`INSTALL_POW_SECRET`、`MEDIA_SIGNING_SECRET`。它们不能被 apply、不能进入 `settings`/Dump，Snapshot 只报告掩码状态或已配置 key 数量；raw bytes 永不输出。`DASHSCOPE_WORKSPACE_ID` 虽不是 secret，仍是 env-only startup-hard 边界，不可从 dashboard 修改。管理后台**不再持有任何凭证**：它恒定只绑 loopback，登录墙归前置 IAP。

## 2. runtime-hot registry（`Specs()` 顺序）

`Bounded` 数值在 env-load 与 overlay 路径共用同一闭区间；`MAX_BODY_BYTES` 与 `N_GLOBAL_CONCURRENCY` 虽可持久化，实际装配容量要 restart 才变化。

| key | 默认 | Min | Max | Restart | 语义 |
|---|---:|---:|---:|---|---|
| `GATEWAY_MODE` | **`debug`** | — | — | 否 | 配额总闸 enum `debug\|production`；`debug` 掩开本表所有配给类闸门（见 §2.1）。拼错 fail-fast（两个方向都危险） |
| `PUBLIC_MODEL_ID` | `anselm-auto` | — | — | 否 | 唯一 client-facing 逻辑模型 id；非空；不选择 provider |
| `GLOBAL_MONTHLY_SPEND_MICRO_USD` | 420,000,000 | 1 | 9,000,000,000,000 | 否 | operator 全局月花费钱包（默认 $420/月） |
| `MONTHLY_QUOTA` | 5000 | 1 | 1,000,000,000 | 否 | per-install 月请求次数 |
| `MAX_TOKENS_CAP` | 4096 | 1 | 1,000,000 | 否 | caller output 保险丝；缺省/非正也显式转发此值与模型 output hard limit 的较小值 |
| `INPUT_TOKEN_CAP` | 0 | 0 | 10,000,000 | 否 | **兼容保留、无执行效果**；旧 settings/env 不致启动失败，prompt estimate 永不据此拒绝 |
| `MAX_MESSAGES` | 256 | 1 | 100,000 | 否 | 完整 history 的 message 数上限 |
| `MAX_MESSAGE_CHARS` | 131072 | 1 | 16,777,216 | 否 | 单 message 文本 rune 上限 |
| `MAX_MEDIA_PARTS` | 8 | 1 | 64 | 否 | 整请求 image+video+audio part 数上限 |
| `MAX_MEDIA_DECODED_BYTES` | `min(3MiB, MAX_BODY_BYTES×3/4)` | 1 | 8,388,608 | 否 | 整请求累计 decoded media bytes；同时必须 ≤ body cap |
| `MAX_BODY_BYTES` | 262144 | 4096 | 8,388,608 | **是** | business chat body cap；中间件装配一次 |
| `N_GLOBAL_CONCURRENCY` | 8 | 1 | 100,000 | **是** | 两 provider 共享的总 upstream 在飞 cap |
| `RATE_PER_MIN` | 0 | 0 | 10,000,000 | 否 | per-install 分钟令牌桶；0=禁用 |
| `DAILY_SUBLIMIT` | 0 | 0 | 1,000,000,000 | 否 | per-install 日请求次数子限；0=禁用 |
| `IMAGE_DAILY_LIMIT` | 10 | 0 | 100,000 | 否 | per-install 日图像生成张数上限(品类日闸,WRK-082 批B);0=禁用(仍记账) |
| `SPEECH_DAILY_LIMIT` | 50,000 | 0 | 100,000,000 | 否 | per-install 日语音合成**字符数**上限(品类日闸,WRK-082 批C);0=禁用(仍记账) |
| `VIDEO_DAILY_LIMIT` | 10 | 0 | 10,000 | 否 | per-install 日视频**条数**上限(品类日闸,WRK-082 H1,用户拍板);**不是秒**——钱按秒报价,人配给的是整条片子;0=禁用(仍记账) |
| `MEDIA_DOMAIN` | 空 | — | — | 是(启动硬) | 上游来取 lease 字节的**公开主机**(裸主机名、无 scheme;空=音色登记不可用)。**绝不能是 `api.` 前缀**——ADR 0012 生产判别实验:同一 lease 路径与 token,`api.<域>` 连续三次 400 且 Caddy 日志证明**源站从未收到请求**,普通主机答 200;拉取器在**它自己的边缘**拉黑 API 形主机,不可见不可申诉,故配错**在任何地方都不留诊断**。三处 fail-closed:config 校验、`render-caddy.sh`、`secureurl.PublicFetchURL`。**范围:只给音色登记**(`voice-enrollment` 不收 base64);chat 与图像输入继续内联字节(ADR 0012 不变)。部署侧**必填**(Caddy 渲染不了空主机名),缺省取 `media.<root>` |
| `VOICE_DAILY_LIMIT` | 2 | 0 | 100 | 否 | per-install 日音色**登记**次数上限(品类日闸,WRK-082 H9)。默认 2 = 库存大小,故空库存可在一天内填满、而 delete→重登记 的循环代价是**一天**、不是每圈 $0.2。**这条与另三条性质不同**:它不是「可再生额度的公平」,它是免费档 install 与无界花费之间唯一站着的东西;上限刻意只到 100——这里每个单位都是一笔永不过期的 $0.2 购买,上千的值不是慷慨、是一个附带账单的笔误。0=禁用(仍记账) |
| `VOICE_ACCOUNT_CEILING` | 0 | 0 | 1,000,000 | 否 | **账号级**克隆音色总数上限(WRK-082 H9)。**是库存、不是配额**:没有周期、没有重置,一个音色占着位直到有人删掉。默认 0 = **不强制**,因为供应商没有文档写出真实上限;运营者从一次拒绝里学到之后再设。满时拒 `VOICE_CAPACITY_REACHED` + WARN,绝不驱逐 |
| `INSTALL_PER_IP_HOUR` | 0 | 0 | 1,000,000 | 否 | `/install` per-IP 小时上限；0=禁用 |
| `INSTALL_GLOBAL_DAILY_CAP` | 0 | 0 | 100,000,000 | 否 | 全局日领号 cap；0=禁用 |
| `INSTALL_PER_FP_DAILY` | 0 | 0 | 1,000,000 | 否 | per-fingerprint 日领号；0=禁用 |
| `INSTALL_PER_FP_COOLDOWN_SEC` | 0 | 0 | 86,400 | 否 | fp 相邻领号间隔；0=禁用 |
| `INSTALL_POW_MODE` | `off` | — | — | 否 | enum `off|shadow|enforce` |
| `INSTALL_POW_DIFFICULTY` | 20 | 1 | 32 | 否 | PoW 前导零 bit |
| `TOKEN_ANOMALY_RPM` | 0 | 0 | 10,000,000 | 否 | 自动降速触发点；0=整套禁用 |
| `TOKEN_THROTTLE_FACTOR` | 4 | 1 | 1000 | 否 | 降速倍数 |
| `TOKEN_THROTTLE_COOLDOWN_SEC` | 300 | 1 | 86,400 | 否 | 降速持续秒 |
| `QUEUE_WAIT_MS` | 1500 | 0 | 60,000 | 否 | shared N_global 满后的有界等待；0=立即拒绝 |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | 60 | 1 | 600 | 否 | 每 attempt connect→first byte；不覆盖 stream body |
| `DISK_MIN_MB` | 500 | 0 | 1,073,741,824 | 否 | data volume 剩余 MiB floor；0=禁用该判据 |
| `DISK_MIN_PERCENT` | 5 | 0 | 100 | 否 | data volume 剩余百分比；0=禁用该判据 |

默认 `MAX_BODY_BYTES=256KiB` 时，media decoded 默认是 `196608` bytes（base64 约占原始数据的 4/3）；operator 放大 body cap 后默认公式仍只在启动 env-load 时计算，不会随随后单键 hot edit 自动联动。

## 2.1 `GATEWAY_MODE` 配额掩码（debug / production）

事实源：`config.EffectiveLimits()`（纯函数掩码）+ `configprovider.Provider`（每次 swap 算一次）。

`GATEWAY_MODE=production` 逐字执行下表配置值；`GATEWAY_MODE=debug` 返回一份**掩码副本**，把每一道**配给用户**的闸打开。掩码是**派生且非破坏**的：配置值仍留在 `Provider.Configured()` 上（dashboard 编辑面与 settings overlay 的基准），故 debug 期间任何一次无关的后台编辑都不会把掩码固化，切回 production 逐字节恢复、无需重填。

| | 掩码集（debug 下被打开） | 掩码值 |
|---|---|---:|
| 钱 | `MONTHLY_QUOTA` | `1,000,000,000`（registry 天花板） |
| 钱 | `GLOBAL_MONTHLY_SPEND_MICRO_USD` | `9,000,000,000,000`（registry 天花板） |
| 吞吐 | `RATE_PER_MIN`、`DAILY_SUBLIMIT`、`TOKEN_ANOMALY_RPM` | `0`（各自的「禁用」值） |
| 品类日闸 | `IMAGE_DAILY_LIMIT`、`SPEECH_DAILY_LIMIT`、`VIDEO_DAILY_LIMIT`、`VOICE_DAILY_LIMIT` | `0` |
| 领号 | `INSTALL_PER_IP_HOUR`、`INSTALL_GLOBAL_DAILY_CAP`、`INSTALL_PER_FP_DAILY`、`INSTALL_PER_FP_COOLDOWN_SEC` | `0` |
| 领号 | `INSTALL_POW_MODE` | `off` |

**不在掩码集**（保护**进程**而非配给用户，两种模式一致）：`MAX_TOKENS_CAP`、`MAX_MESSAGES`、`MAX_MESSAGE_CHARS`、`MAX_MEDIA_PARTS`、`MAX_MEDIA_DECODED_BYTES`、`MAX_BODY_BYTES`、`N_GLOBAL_CONCURRENCY`、`QUEUE_WAIT_MS`、`UPSTREAM_HEADER_TIMEOUT_SEC`、`DISK_MIN_*`、PERF-2 内存预算项。debug 不是把机器 OOM 掉的路子。

**记账两模式完全一致**：reserve/settle 照跑、`spend_ledger` 照记每一 pUSD、日统计表照写。debug 是「**永不拒绝**」，不是「永不记账」——GW-INV-01/06 逐字成立，后台花费视图在 debug 下依然是真相。掩码值本身仍落在各自 registry 闭区间内（有测试守卫），故掩码后的 config 自身也是一份合法 config。

读/写面分工：

| 面 | 读哪一份 | 为什么 |
|---|---|---|
| 一切执行点（quota reserve、限速、领号、能力面 `/v1/models`、`X-Quota-Limit`） | `Provider.Load()`（**掩码后**） | 掩码只算一次；分散到十几个执行点各判一次的模式，迟早会在其中一处被忘掉 |
| dashboard 配置表 `Dump()`、override 基准 | `Provider.Configured()`（**未掩码**） | 它是**编辑器**：显示掩码值会诱使运营者把被掩成 0 的值「改回」8，等于把掩码写进自己的配置 |
| 启动 `config_snapshot` 日志 | `Provider.Load()`（**掩码后**），`gateway_mode` 打头 | 该行唯一要回答的是「本进程此刻在执行什么」 |

默认 `debug` 是本仓**唯一**方向上放宽的默认值（`deploy/build-stage.sh` 显式写 `GATEWAY_MODE="debug"`），理由是全新部署首先面对的是运营者自己，而被自家日限挡住的开发者看不出是十几道闸里的哪一道。启动时 debug 会额外打一条 `runtime_mode_debug` WARN 列明所有被打开的闸。**对运营者以外的人开放之前必须切 production**——它是 runtime-hot，后台改一行即可，不必重启、不必重新部署。

## 3. startup-hard / env-only

### 3.1 `Specs()` 中的 dashboard 只读项

| key | 默认 | 约束 / 语义 |
|---|---|---|
| `MULTIMODAL_UPSTREAM_MODEL` | `qwen3.7-plus` | 必须是精确已编译 Qwen rate card；图片/视频路由 |
| `IMAGE_ENABLED` | `false` | 图像生成能力开关(WRK-082 批B);开启时要求 `DASHSCOPE_API_KEY` + 精确已编译图像 rate card + `DASHSCOPE_NATIVE_BASE`,缺一启动 fail-fast |
| `IMAGE_UPSTREAM_MODEL` | `qwen-image-2.0` | 必须是精确已编译 DashScope 图像 rate card;按张计价(reserve==settle) |
| `SPEECH_ENABLED` | `false` | 语音**合成**能力开关(WRK-082 批C),与图像各自独立;开启时要求 `DASHSCOPE_API_KEY` + 精确已编译 TTS rate card + `DASHSCOPE_NATIVE_BASE` + `TTS_DEFAULT_VOICE`,缺一启动 fail-fast |
| `TTS_UPSTREAM_MODEL` | `qwen-audio-3.0-tts-flash` | 必须是精确已编译 DashScope TTS rate card;按**输入字符**计价(reserve==settle——字符数在调用前即精确已知) |
| `VIDEO_ENABLED` | `false` | 视频生成能力开关(WRK-082 H1),与图像、语音各自独立;开启时要求 `DASHSCOPE_API_KEY` + 精确已编译视频 rate card + `DASHSCOPE_NATIVE_BASE` + **`MEDIA_SIGNING_SECRET`**(句柄签名密钥由它域分离派生),缺一启动 fail-fast |
| `VIDEO_UPSTREAM_MODEL` | `wan2.7-t2v` | 必须是精确已编译 DashScope 视频 rate card;按**秒**计价(reserve==settle——请求的时长**就是**计费量) |
| `TTS_DEFAULT_VOICE` | `longanhuan_v3.6` | 请求未带 `voice` 时用的音色(P10:参数留在线缆上,桌面设置页不开) |
| `DASHSCOPE_NATIVE_BASE` | 由 `DASHSCOPE_BASE_URL`/workspace 派生(剥掉 `/compatible-mode/v1`);无凭证时兜底 `https://dashscope-intl.aliyuncs.com` | **原生** DashScope API origin(multimodal-generation、video-synthesis、tasks)。它与凭证**共享 host、只在 path 上不同**,故默认值**从凭证派生**而不是写死一个区域:一把新加坡 workspace key 打到 `dashscope.aliyuncs.com`(北京)会答 401 `Incorrect API key provided`——同一把 key,一个区域 200、另一个 401,而报文里没有一个字暗示这跟地理有关(WRK-082 桌面侧实测到的真 401)。显式配置仍然优先 |
| `DASHSCOPE_BASE_URL` | 由 `DASHSCOPE_WORKSPACE_ID` 推导 | 可选显式 compatible base URL；去尾 `/`；chat 调用 `/chat/completions`，speech ASR 派生 `/api-ws/v1/realtime?model=qwen3-asr-flash-realtime` |
| `GOMEMLIMIT_MIB` | 768 | ≥0；0=禁用 heap soft limit |
| `SQLITE_CACHE_KIB` | 32768 | >0；per connection |
| `READ_POOL_MAX_CONNS` | 4 | >0 |
| `SQLITE_MMAP_MB` | 256 | ≥0；0=禁用 |
| `ADMIN_ADDR` | `127.0.0.1:9090` | 与另两监听互异，必须 loopback |
| `DASHBOARD_ADDR` | `127.0.0.1:8081` | 与另两监听互异，必须 loopback |
| `LISTEN_ADDR` | `127.0.0.1:8080` | 与另两监听互异 |
| `RESET_TZ` | `Asia/Shanghai` | `LoadLocation` 失败 panic，无 UTC fallback |
| `GATEWAY_DB_PATH` | `anselm-gateway.db` | SQLite 文件 |
| `MEDIA_ENABLED` | `false` | durable media upload 显式开关；启用时全部 media 配置与 secret 必须完整 |
| `MEDIA_STAGING_ROOT` | `dirname(GATEWAY_DB_PATH)/anselm-media` | 私有持久目录；不可是 web root/临时目录 |
| `MEDIA_UPLOAD_MAX_BYTES` | 104,857,600 | 1..1,073,741,824；单对象磁盘上限 |
| `MEDIA_CHUNK_MAX_BYTES` | `min(4MiB,MAX_BODY_BYTES)` | 1..`MAX_BODY_BYTES`，每个 raw resumable chunk 的上限 |
| `MEDIA_UPLOAD_TTL_SEC` | 3600 | 1..604800；未完成 staging 到期 |
| `MEDIA_LEASE_TTL_SEC` | 3600 | 1..604800；完成后的 opaque lease 到期 |

### 3.2 其它 startup env（不在 dashboard registry）

| key | 默认 | 约束 / 语义 |
|---|---:|---|
| `SQLITE_WAL_AUTOCHECKPOINT` | 4000 | ≥0 pages |
| `MEM_BUDGET_MIB` | 2048 | >0 |
| `MEM_SAFETY_MARGIN_MIB` | 400 | ≥0 |
| `LOG_LEVEL` | `info` | process log level |

## 4. secret-env-only

| key | 约束 / 缺失行为 |
|---|---|
| `DASHSCOPE_API_KEY` | 必需；支持逗号分隔多 key；与 `DASHSCOPE_WORKSPACE_ID` 一起构造 Qwen 视觉 backend 与 realtime ASR proxy |
| `DASHSCOPE_WORKSPACE_ID` | 必需（除非显式给出 `DASHSCOPE_BASE_URL`）；只允许字母、数字、`_`、`-`，用于构造新加坡 Model Studio endpoint |
| `INSTALL_POW_SECRET` | 不自动生成；`shadow|enforce` 必须非空，`off` 可空 |
| `MEDIA_SIGNING_SECRET` | `MEDIA_ENABLED=true` 时必填、至少 32 bytes；HMAC 派生 provider-only fetch credential，重启后可重建，SQLite 仅保存 hash |

每个 backend 的 URL、key pool 与 breaker 在 construction 时冻结。Qwen 是产品 route 的部署必需能力；缺 key/base/workspace 是启动配置错误，不允许以“只有文本”降级。

## 5. 跨字段语义（每次 env-load / overlay / hot batch 都跑）

1. `GLOBAL_MONTHLY_SPEND_MICRO_USD>0`，并且任一已启用 route 的单请求最坏 quote 必须能装入该月预算；否则配置 fail-fast/热改拒绝。
2. `PUBLIC_MODEL_ID` 非空；client id 与两个实际模型 id 没有映射选择关系。
3. 统一产品档位固定为 thinking-on：注入顶层 `enable_thinking=true`;历史 `reasoning_content` 绝不回传(provider 私有,回传即再计费)。client-supplied thinking/effort 均不改变该档位；client `max_tokens` 是调用参数，只在模型/`MAX_TOKENS_CAP` 边界内透传。
4. `MULTIMODAL_UPSTREAM_MODEL` 必须精确等于一张已编译 rate card(**无条件**校验:再没有哪种部署形态用不到它)；完整 1,000,000 input + 65,536 output 的最坏 quote 必须装入全局月预算。运行时 UTF-8 estimate 只决定较小请求的 reserve 大小，超过 1M 时 quote clamp 到模型硬上限而不拒绝。
5. `MULTIMODAL_UPSTREAM_MODEL` 必须精确等于已知 Qwen3.7 Plus rate card；由于 inline 图片/视频无法由本地字节数证明视觉 token，reserve 使用完整 `1,000,000` input + `65,536` output 的最高分段价格，必须装入全局月预算。媒体形状/bytes 单独受限并由 Qwen 判定实际 token；权威 usage 可退款。chat `input_audio` 虽计入公共媒体形状/bytes 闸，但当前没有 provider 配置可使其作为音频理解 route 可用；麦克风 realtime ASR 是单独的 `qwen3-asr-flash-realtime` 时长费率会话，并进入同一 quota/ledger。
6. `1≤MAX_MEDIA_PARTS≤64`，`1≤MAX_MEDIA_DECODED_BYTES≤MAX_BODY_BYTES`。
7. `INSTALL_POW_MODE∈{shadow,enforce}` 时必须已有 env-only secret。
8. 管理后台的安全前提由两半共同满足:bootstrap 强制的 loopback 绑定(fail-fast),加上部署者在**所有路径**前布好的 IAP policy。进程内没有可配错的鉴权开关。
9. `MEDIA_ENABLED=true` 时 staging root 非空、signing secret 至少 32 bytes、`0<chunk≤MAX_BODY_BYTES≤8MiB` 且 `chunk≤upload max`、两个 TTL 均为正；任一不满足即 fail-fast。

违反任一项 fail-fast，未知模型绝不以旧价格继续运行。金额 rate card 逐字值见 [ADR-0017](../../decisions/0017-qwen-visual-route-and-tiered-cost-ledger.md)。

## 6. PERF-2 与热改原子性

最坏 RSS=`GOMEMLIMIT_MIB + (SQLITE_CACHE_KIB/1024)×(1+READ_POOL_MAX_CONNS) + SQLITE_MMAP_MB`。若超过 `MEM_BUDGET_MIB−MEM_SAFETY_MARGIN_MIB`：`GOMEMLIMIT_MIB=0` 时 WARN 放行，否则 `ErrMemoryBudget` fail-fast。

`ApplyOverrides` 对 base clone 依 key 排序 apply；未知/secret/startup-hard 均指名拒绝；随后重跑全部跨字段语义。Provider 在写锁内先单事务持久化、成功后 atomic swap；任一错误不落半份 settings、不发布半份 Config。每个业务请求只读取一次 Config snapshot。

## 7. v1 overlay 迁移

迁移 `0002_provider_spend_ledger.sql` 曾将旧 `GLOBAL_DAILY_BUDGET_TOKENS` / `INSTALL_DAILY_TOKEN_CAP` 换为 retired daily spend settings；`0004_global_monthly_budget.sql` 将历史 `global_spend_daily` 聚合到 `global_spend_monthly`，并删除旧 daily/provider/install spend settings。新版本不读取这些旧 env/settings 名称；`PUBLIC_MODEL_ID` 是唯一公开模型配置。
