# Anselm Gateway

[English](README.md) · 简体中文

一个纯 Go + SQLite 的单二进制网关,对客户暴露一个 OpenAI 兼容模型,内部只有一个固定上游 Qwen3.7 Plus。完整消息历史仍然确定性地决定事情——它决定这条请求**能用哪些能力**、以及能不能被受理——但它从不选 provider,客户端的 `model` 字段也从来不选。chat 音频 part 已有严格公共协议,但当前尚无模型路由；麦克风输入由独立的 Qwen realtime ASR WebSocket 窄代理转写。provider key 只留在服务端;悲观成本记账保证 operator 的美元预算不被超卖。它是为 Anselm 桌面 app 写的,但本身自包含。

它做三件事:

1. **路由** —— OpenAI 兼容的 `/v1/chat/completions`(流式与非流式,支持 `tools`/`tool_choice` 多轮工具调用),外加六条付费生成端点:出图、改图、语音合成、异步视频提交/轮询、克隆音色登记。整段 messages 历史决定的是**这条请求能用哪些能力**——不是 provider,因为只有一家。客户端的 `model` 字符串同样什么都不选。
2. **计量** —— 进上游前,按精确 provider/model 费率卡换算成本,再原子预占每 install 月请求数和 operator 全局月花费预算。成功依上游 usage 结算;无法判定的失败保留保守扣费。
3. **设备绑定与限流** —— 每次调用都证明持有本机 Ed25519 私钥；复制 install id 或抓一条请求无法继续滥用。PoW 与领号闸继续增加批量造号成本。实时语音代理同样受 device proof 保护，且不会把 Qwen 凭据暴露给客户端。

代码采用 Clean Architecture(domain / app / infra / transport,加一个 bootstrap 组合根),依赖方向由 golangci-lint(depguard)强制。四层都有测试,另有一个 loopback 全栈端到端套件;CI 跑 race 测试、lint、govulncheck、gofmt,以及一项"内嵌后台的构建是否与源一致"的检查。

## 成本记账

两道会拒绝请求的护栏 —— 每 install 月请求数、operator 全局月花费 —— 在单写连接池的一个 `BEGIN IMMEDIATE` 事务里预占。各能力的计费单位互不相加(token、张、字符、秒、个):每个请求先按冻结的精确模型费率卡换算,只有整数 picoUSD 成本进入共享钱包。生成能力另有一道**逐品类日闸**,与预留在**同一个**事务里消耗。install/provider/global 日表继续记录统计,不再作为流量闸门。

确定没有达到 provider 前的拒绝会回滚预占。一旦请求已交给 provider,缺 usage、超时、断连或崩溃都保留保守预占;完整 usage 则结算到计算成本。账本转移是 compare-and-swap 且幂等,因此失败模式是多扣,不是花了 operator 的钱却没记账。

## 架构

![架构与依赖方向:cmd 依赖 bootstrap,bootstrap 依赖 transport 与 infra;transport 依赖 app,app 依赖 domain;infra 也依赖 domain;transport、app、infra、domain 都依赖 pkg 叶内核。依赖只指向更稳定的内层,由 depguard 强制。](docs/assets/architecture.svg)

依赖只指向更稳定的内层,违反即构建失败(depguard):`domain` 无 infra import;`app` 在本包以 interface 声明所需 infra 端口;`*sql.Tx` 不漏出 `infra`;`bootstrap` 是唯一跨层 import 的包(它无人 import)。三个独立监听器:公网业务面、loopback-only 的运维/metrics/pprof 面、loopback 管理后台。

## 快速开始

```sh
cp .env.example .env          # 至少填 DASHSCOPE_API_KEY + DASHSCOPE_WORKSPACE_ID
set -a; source .env; set +a   # (bash/zsh;fish 见 .env.example)
make run                      # = go run ./cmd/gateway
curl -s localhost:8080/healthz   # → {"status":"ok"}
```

端到端推理刻意不提供「复制一个 token 就能 curl」的通道。Anselm sidecar 登记 Ed25519 公钥，把
加密私钥种子留在设备上，取得短期 challenge，并对每次请求的 method、authority、target 与 exact body
签名。完整紧凑 proof 契约见 [ADR-0015](docs/decisions/0015-device-bound-request-proof.md)。

## 确定性路由

客户只看到一个逻辑模型 `anselm-auto`;`GET /v1/models` 以及流式/非流式 completion 的顶层 `model` 都只返回这个 ID。客户传入的 `model` 绝不用来选 provider:

- 字符串 content,或只含 `text` part 的 content 数组,按文本请求受理。
- 整段历史任意位置出现一个合法图片或视频 part,**额外**需要持久媒体通道(`MEDIA_ENABLED`)——这正是两者虽然同走 `qwen3.7-plus`、却仍作为两份能力画像发布的原因。
- 合法 `input_audio` 会被公共协议接收，但在路由、记账前固定返回 `503 AUDIO_UNAVAILABLE`，直到部署音频上游。
- 实时麦克风转写是独立的 `GET /v1/speech/asr` WebSocket。它接收 PCM 帧并转发 Qwen ASR 事件，用于桌面 app 填入可编辑文本；它不是 chat 音频内容理解路由。

Inline media 故意采用严格合同,且只允许在 `user` message 中出现。图片用 `image_url` part，URL 必须是 JPEG、PNG 或 WebP 的 base64 data URI；视频用 `video_url` part，URL 必须是 MP4 的 base64 data URI；音频用 `input_audio` object，`data` 是严格 raw base64，`format` 只能是与魔数匹配的 `wav` 或 `mp3`。远程 URL、PDF、文件、未知 part、MIME/魔数不匹配，以及超出 part/解码字节上限的媒体都会直接拒绝，不向上游转发。没有 fallback。`DASHSCOPE_API_KEY` 与 `DASHSCOPE_WORKSPACE_ID` 是 Qwen 视觉 route 的启动必需配置；配置不全时直接启动失败，不会静默失去产品能力。

网关只为模型选择和 reasoning 行为定义一个傻瓜式产品档位。thinking 永远经 Qwen 顶层 `enable_thinking=true` 开启；客户端传入的 `thinking`、`reasoning_effort` 不改变这个档位。历史 `reasoning_content` **绝不回传**:它是 provider 私有的,且回传时会**再计一次费**。正数 `max_tokens` 在 `MAX_TOKENS_CAP` 和实际模型 output hard limit 内 clamp；未传或非正值也归一成同一个显式产品 cap，使 wire、账务与客户端预留的上下文 headroom 一致。两份画像都报该模型的 1M input 与 64K output 上限；`GET /v1/models` 通过 namespaced `anselm_capabilities` 发布它们,桌面 Agent 因此是**读**预算而不是猜,也因此能在发第一条媒体消息**之前**就知道媒体不可用。

## 管理后台

内嵌的 React SPA(总览、配置编辑、install 封禁/审计、DB 导出)由二进制提供，始终只监听 loopback。它**不持有**任何凭证、session 或 CSRF 状态:登录墙属于隧道前面的那个东西——SSH 隧道、Cloudflare Access、Tailscale——而 fail-fast 的 loopback 绑定,正是让这份委托成为**进程的性质**、而不是一句承诺的东西。

```sh
ssh -L 8081:127.0.0.1:8081 <user>@<server>   # 然后浏览器开 http://localhost:8081
```

<!-- 把截图放到 docs/assets/dashboard.png 后取消注释: -->
<!-- ![Anselm Gateway dashboard](docs/assets/dashboard.png) -->

## API

业务面(`127.0.0.1:8080`,经 Caddy 暴露公网):

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| `POST` | `/v1/install` | registration proof | 登记设备公钥；返回 `{installId, monthlyQuota, resetAt}` |
| `GET` | `/v1/proof/challenge` | 无 | 发一个可缓存五分钟的 request nonce |
| `POST` | `/v1/chat/completions` | device proof | OpenAI 兼容推理;按 `stream` 返 SSE 或 JSON |
| `GET` | `/v1/speech/asr` | device proof | 实时麦克风转写代理；binary PCM 输入，Qwen ASR 事件输出 |
| `GET` | `/v1/models` | device proof | 一个公开 ID，并带 route-specific `anselm_capabilities` |
| `GET` | `/v1/quota` | device proof | `{limit, used, remaining, resetAt, available}` |
| `POST` | `/v1/images/generations` | device proof | 同步出图;直通上游产物 URL |
| `POST` | `/v1/images/edits` | device proof | 同一形状 + base64 data URL 源图;同模型、同费率卡 |
| `POST` | `/v1/audio/speech` | device proof | 语音合成;返**裸 `audio/wav` 字节**,不是 URL |
| `POST` | `/v1/videos/generations` | device proof | 异步提交;**202** + 签名句柄。**钱落在提交** |
| `POST` | `/v1/videos/animations` | device proof | 图生视频:同一形状 + 首帧 |
| `GET` | `/v1/videos/{videoId}` | device proof | 轮询一个签名句柄。**完全不动钱** |
| `POST` | `/v1/voices` | device proof | 用一个已上传的媒体 lease 登记克隆音色 |
| `GET` / `POST` | `/v1/voices`、`/v1/voices:delete` | device proof | 列出本 install 的音色;删一个(**先删上游**) |
| `POST` … | `/v1/media/uploads…` | device proof | proof 绑定的断点上传;完成后换成 opaque lease |
| `GET` | `/healthz` | 无 | 进程存活,不碰 DB / 上游 |

运维面(`127.0.0.1:9090`,loopback-only,不反代):`/metrics`、`/readyz`(`{db, upstream, disk}`)、`/debug/pprof/*`、`/debug/vars`。
管理后台(`127.0.0.1:8081`,loopback):`/healthz` 加直连的 `/api/*`。**没有登录端点**,这是设计。

成功返回裸实体,失败返回 `{"error":{"code","message"}}`;成功 completion body 只会在严格校验并改写公开模型 alias 后转发，原始上游错误 body/header 与 provider key 绝不透传。完整契约见 [`docs/references/backend/api.md`](docs/references/backend/api.md)。

## 配置

加载顺序是 env 默认,然后是 `settings` 表 DB 覆盖(运行时可改项可在后台修改)。完整面见 [`.env.example`](.env.example) 与 [`docs/references/backend/config.md`](docs/references/backend/config.md)。

机密 env-only,不入库、不 Dump、不进日志:`DASHSCOPE_API_KEY`(必填,支持逗号分隔多 key——key 池正是一个账号扛过单 key 冷却的方式)、`INSTALL_POW_SECRET`(仅启用 PoW 时必填)。`DASHSCOPE_WORKSPACE_ID` 是用于推导新加坡 Model Studio 视觉推理与实时 ASR endpoint 的非机密标识。

模型 ID 只有两个:`PUBLIC_MODEL_ID=anselm-auto`(客户端唯一看得见的)与 `MULTIMODAL_UPSTREAM_MODEL=qwen3.7-plus`(唯一真正到达上游的)。花费上限用整数 microUSD(`1,000,000 = US$1`);生产示例是 `GLOBAL_MONTHLY_SPEND_MICRO_USD=420000000`($420/月)。生产 body 上限 8 MiB，最多 8 个 inline media part / 3 MiB 解码媒体。会拒绝请求的使用护栏是每 install `MONTHLY_QUOTA=5000` 和 operator 全局月花费预算；body/message/media 形状与 `N_GLOBAL_CONCURRENCY` 是服务安全护栏。UTF-8 保守 prompt estimate 只用于记账报价，不再做上下文准入；实际 route 的 provider 才是 input hard limit 权威。

## 部署

VPS 上的 Caddy + systemd:

- Caddy 终结 TLS 并把 API 域名反代到 `127.0.0.1:8080`(SSE 即时下发)。根域可从 `deploy/site/` 提供纯静态说明页。Go 进程只绑 `127.0.0.1`,不直面公网。
- systemd socket activation 在普通 service restart 时持有 `:8080` fd。发版时会有意先停 Caddy、socket、service,形成一段 fail-closed 维护窗,确保 SQLite 快照到 commit 之间没有请求写账本。
- 只有本仓 `ci` 对 `main` push 全绿后,deploy workflow 才接收该次不可变 `head_sha`;失败、未完成或已经不是 `main` tip 的过期 CI 都绝不能部署。deploy 会 checkout 这个精确 SHA,并再次执行发布关键门项:gofmt/module verify/vet/build、race unit + integration e2e、公开 parser fuzz smoke、记账覆盖率地板、docs governance、rollback shell 模拟、高危 npm audit + 内嵌后台重建/drift、golangci-lint 与 govulncheck;全绿后才静态编译 `linux/amd64` → 全部 artifact 进入远端不可预测的 `0700` stage → 校验精确 regular-file 集与 SHA-256 manifest → 停写后快照 SQLite main/WAL/SHM → 安装并跑 loopback gate → 持久化本地 commit → 重开 Caddy。commit 前任一路径失败都会把 DB、binary/symlink、env、unit、Caddy、静态站与旧全局 rollback 入口(首次部署则精确恢复为不存在)作为一个兼容单元完整自动恢复。root filesystem 上的 transition marker 配合永久 systemd Caddy condition,即使进程死亡或整机重启也保持公网入口关闭;commit 后 Caddy 启动失败则绝不冒险回卷可能已承接流量的 DB。
- 生产强制配置 GitHub Environment secret `SERVER_KNOWN_HOSTS`,缺失或不含 `SERVER_HOST` 条目即 fail closed;不存在 `ssh-keyscan`/TOFU 回退。远端 data dir 为 `0700`,DB/WAL/SHM 与 secret env 为 `0600`;成功发版后只保留一个 root-only rollback bundle。
- 服务器安装 schema-aware 人工回滚命令:`sudo /usr/local/sbin/anselm-gateway-rollback`(交互确认),自动化用 `sudo /usr/local/sbin/anselm-gateway-rollback --yes`。若主机/进程崩溃留下持久 transition marker,必须先恢复 marker 指向的精确 checksummed bundle。最可靠的入口始终是 `sudo <marker 中的 bundle>/recovery/rollback.sh --recover-incomplete`(非交互再加 `--yes`);全局入口若已升级,也支持同样的 recovery mode。每个 bundle 都携带该版精确 recovery program,回滚又会恢复旧全局入口,因此不会与更旧的保留 READY bundle 发生格式错配。永久 Caddy guard 则作为 inert 的受管安全 artifact 保留。回滚会同时恢复 DB 快照及整套运行 artifact;schema migration 后只切 binary symlink 明确不受支持且不安全。

部署目标(域名、ACME 邮箱)经 GitHub secret 注入、不入库。生产默认包括仅为兼容保留的 `INPUT_TOKEN_CAP=0`、`MAX_TOKENS_CAP=16384`、`MAX_MESSAGES=4096`、`MAX_MESSAGE_CHARS=4194304`、`MAX_BODY_BYTES=8388608`、`MONTHLY_QUOTA=5000`，可选流控默认 `0`/`off`。见 [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) 与 [`deploy/`](deploy/)。

## 开发

```sh
make verify    # vet + build + test -race + docs(本地门禁)
make lint      # golangci-lint v2.6.1(含 depguard 分层检查)
make docs      # 文档治理门禁
```

测试覆盖四层 + loopback 全栈 e2e(`internal/e2e`,`integration` tag)。CI 对记账相关包强制覆盖地板(`app/quota` ≥ 70%、`app/chat` ≥ 65%),并跑 govulncheck、gofmt、fuzz smoke、SBOM 与后台构建漂移检查。

## 状态与范围

线上 `main` 运行中;管理后台已接线并提供,单轮与多轮工具调用已端到端测试。

这是一个刻意做薄的网关:两条固定内容形状路由、单 operator 共享预算、单节点 SQLite + 单写池(这正是原子记账能成立的前提)。它不是通用模型路由、不是多租户计费系统,也不横向扩展;运维面 loopback-only。它是为一个产品而写的,搬到别处用需要一些改造。

## 文档

`docs/` 是受治理的文档集,入口是 [`docs/INDEX.md`](docs/INDEX.md):

- [`concepts/architecture.md`](docs/concepts/architecture.md) —— 系统模型
- [`references/backend/`](docs/references/backend/) —— 与代码同步的契约(api / config / database / error-codes / invariants)
- [`decisions/`](docs/decisions/) —— ADR-001..012(设计决策,保持不可变)

## 许可

[MIT](LICENSE)。
