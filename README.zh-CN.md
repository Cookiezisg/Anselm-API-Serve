# Anselm Gateway

[English](README.md) · 简体中文

一个纯 Go + SQLite 的单二进制网关,对客户暴露一个 OpenAI 兼容模型,内部确定性地走两个固定上游:纯文本走 DeepSeek V4 Flash,受支持的 inline 图片/视频走 Kimi K2.6。音频已有严格公共协议,但当前尚无部署路由。provider key 只留在服务端;悲观成本记账保证 operator 的美元预算不被超卖。它是为 Anselm 桌面 app 写的,但本身自包含。

它做三件事:

1. **路由** —— OpenAI 兼容的 `/v1/chat/completions`(流式与非流式,支持 `tools`/`tool_choice` 多轮工具调用)。整段 messages 历史决定 provider;客户不能选 provider,两路之间也不 fallback。
2. **计量** —— 进上游前,按精确 provider/model 费率卡换算成本,再原子预占每 install 月请求数和 operator 全局月花费预算。成功依上游 usage 结算;无法判定的失败保留保守扣费。
3. **设备绑定与限流** —— 每次调用都证明持有本机 Ed25519 私钥；复制 install id 或抓一条请求无法继续滥用。PoW 与领号闸继续增加批量造号成本。

代码采用 Clean Architecture(domain / app / infra / transport,加一个 bootstrap 组合根),依赖方向由 golangci-lint(depguard)强制。四层都有测试,另有一个 loopback 全栈端到端套件;CI 跑 race 测试、lint、govulncheck、gofmt,以及一项"内嵌后台的构建是否与源一致"的检查。

## 成本记账

两道会拒绝请求的护栏 —— 每 install 月请求数、operator 全局月花费 —— 在单写连接池的一个 `BEGIN IMMEDIATE` 事务里预占。不同 provider 的 token 向量绝不直接相加:每个请求先按冻结的精确模型费率卡换算,只有整数 picoUSD 成本进入共享钱包。install/provider/global 日表继续记录统计,不再作为流量闸门。

确定没有达到 provider 前的拒绝会回滚预占。一旦请求已交给 provider,缺 usage、超时、断连或崩溃都保留保守预占;完整 usage 则结算到计算成本。账本转移是 compare-and-swap 且幂等,因此失败模式是多扣,不是花了 operator 的钱却没记账。

## 架构

![架构与依赖方向:cmd 依赖 bootstrap,bootstrap 依赖 transport 与 infra;transport 依赖 app,app 依赖 domain;infra 也依赖 domain;transport、app、infra、domain 都依赖 pkg 叶内核。依赖只指向更稳定的内层,由 depguard 强制。](docs/assets/architecture.svg)

依赖只指向更稳定的内层,违反即构建失败(depguard):`domain` 无 infra import;`app` 在本包以 interface 声明所需 infra 端口;`*sql.Tx` 不漏出 `infra`;`bootstrap` 是唯一跨层 import 的包(它无人 import)。三个独立监听器:公网业务面、loopback-only 的运维/metrics/pprof 面、loopback 管理后台。

## 快速开始

```sh
cp .env.example .env          # 至少填 DEEPSEEK_API_KEY
set -a; source .env; set +a   # (bash/zsh;fish 见 .env.example)
make run                      # = go run ./cmd/gateway
curl -s localhost:8080/healthz   # → {"status":"ok"}
```

端到端推理刻意不提供「复制一个 token 就能 curl」的通道。Anselm sidecar 登记 Ed25519 公钥，把
加密私钥种子留在设备上，取得短期 challenge，并对每次请求的 method、authority、target 与 exact body
签名。完整紧凑 proof 契约见 [ADR-0015](docs/decisions/0015-device-bound-request-proof.md)。

## 确定性路由

客户只看到一个逻辑模型 `anselm-auto`;`GET /v1/models` 以及流式/非流式 completion 的顶层 `model` 都只返回这个 ID。客户传入的 `model` 绝不用来选 provider:

- 字符串 content,或只含 `text` part 的 content 数组,走 `deepseek-v4-flash`。
- 整段历史任意位置出现一个合法图片或视频 part,就走 `kimi-k2.6`。
- 合法 `input_audio` 会被公共协议接收，但在路由、记账前固定返回 `503 AUDIO_UNAVAILABLE`，直到部署音频上游。

Inline media 故意采用严格合同,且只允许在 `user` message 中出现。图片用 `image_url` part，URL 必须是 JPEG、PNG 或 WebP 的 base64 data URI；视频用 `video_url` part，URL 必须是 MP4 的 base64 data URI；音频用 `input_audio` object，`data` 是严格 raw base64，`format` 只能是与魔数匹配的 `wav` 或 `mp3`。远程 URL、PDF、文件、未知 part、MIME/魔数不匹配，以及超出 part/解码字节上限的媒体都会直接拒绝，不向上游转发。两路之间没有 fallback。未配 `KIMI_API_KEY` 时纯文本仍可用，合法图片/视频请求返回 `503 MULTIMODAL_UNAVAILABLE`。

网关只为模型选择和 reasoning 行为定义一个傻瓜式产品档位。thinking 永远开启：纯文本请求使用 DeepSeek `thinking.enabled` + `reasoning_effort=high`；媒体请求使用 Kimi `thinking.enabled`，不传 `reasoning_effort`。客户端传入的 `thinking`、`reasoning_effort` 不改变这个档位。`max_tokens` 这类调用参数仍保持 OpenAI 兼容透传：正数 `max_tokens` 会在 `MAX_TOKENS_CAP` 和实际模型 output hard limit 内 clamp 后转发；未传时 wire 不主动塞值，但账务按保守上限预留。纯文本实际有 DeepSeek 的 1M input context；媒体实际有 Kimi 的 262K input context，所以产品侧如果只展示一个上下文数字，应保守写 256K。

## 管理后台

内嵌的 React SPA(总览、配置编辑、install 封禁/审计、DB 导出)由二进制提供，始终只监听 loopback。`DASHBOARD_AUTH_MODE=builtin` 使用 Go 自己的 session + CSRF 登录，可经 SSH 隧道访问；`external` 把完整登录门禁交给前置 IAP（如 Cloudflare Access），但 Go 仍只绑定 `127.0.0.1:8081`。

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
| `GET` | `/v1/models` | device proof | OpenAI `{object:"list", data:[…]}`,内含唯一公开模型 ID |
| `GET` | `/v1/quota` | device proof | `{limit, used, remaining, resetAt, available}` |
| `GET` | `/healthz` | 无 | 进程存活,不碰 DB / 上游 |

运维面(`127.0.0.1:9090`,loopback-only,不反代):`/metrics`、`/readyz`(`{db, upstream, disk}`)、`/debug/pprof/*`、`/debug/vars`。
管理后台(`127.0.0.1:8081`,loopback):`GET /api/bootstrap` 指示 `builtin` 或 `external`；builtin 另提供 `/login`、`/logout` 和 session 保护的 `/api/*`，external 则由前置 IAP 认证后直接访问 `/api/*`。

成功返回裸实体,失败返回 `{"error":{"code","message"}}`;成功 completion body 只会在严格校验并改写公开模型 alias 后转发，原始上游错误 body/header 与 provider key 绝不透传。完整契约见 [`docs/references/backend/api.md`](docs/references/backend/api.md)。

## 配置

加载顺序是 env 默认,然后是 `settings` 表 DB 覆盖(运行时可改项可在后台修改)。完整面见 [`.env.example`](.env.example) 与 [`docs/references/backend/config.md`](docs/references/backend/config.md)。

机密 env-only,不入库、不 Dump、不进日志:`DEEPSEEK_API_KEY`(必填,逗号分隔多 key)、`KIMI_API_KEY`(可选,不配只禁用图片/视频)、`DASHBOARD_USER`/`DASHBOARD_PASSWORD`(仅 `DASHBOARD_AUTH_MODE=builtin` 必填)、`INSTALL_POW_SECRET`(仅启用 PoW 时必填)。`DASHBOARD_AUTH_MODE` 本身不是机密，但同样只能经 env 在启动时选择：`disabled`(默认)、`builtin`、`external`。

公开/provider 模型 ID 分别是 `PUBLIC_MODEL_ID=anselm-auto`、`TEXT_UPSTREAM_MODEL=deepseek-v4-flash`、`MULTIMODAL_UPSTREAM_MODEL=kimi-k2.6`。花费上限用整数 microUSD(`1,000,000 = US$1`);生产示例是 `GLOBAL_MONTHLY_SPEND_MICRO_USD=420000000`($420/月)。它使用 5 MiB request body,最多 8 个 inline media part / 3 MiB 解码媒体。会拒绝请求的使用护栏是每 install `MONTHLY_QUOTA=5000` 和 operator 全局月花费预算；有界输入/输出与 `N_GLOBAL_CONCURRENCY` 继续作为服务安全护栏。每分钟聊天频控、日请求子限、自动降速与领号频控默认都禁用(`0`/`off`)。

## 部署

VPS 上的 Caddy + systemd:

- Caddy 终结 TLS 并把 API 域名反代到 `127.0.0.1:8080`(SSE 即时下发)。根域可从 `deploy/site/` 提供纯静态说明页。Go 进程只绑 `127.0.0.1`,不直面公网。
- systemd socket activation 在普通 service restart 时持有 `:8080` fd。发版时会有意先停 Caddy、socket、service,形成一段 fail-closed 维护窗,确保 SQLite 快照到 commit 之间没有请求写账本。
- 只有本仓 `ci` 对 `main` push 全绿后,deploy workflow 才接收该次不可变 `head_sha`;失败、未完成或已经不是 `main` tip 的过期 CI 都绝不能部署。deploy 会 checkout 这个精确 SHA,并再次执行发布关键门项:gofmt/module verify/vet/build、race unit + integration e2e、公开 parser fuzz smoke、记账覆盖率地板、docs governance、rollback shell 模拟、高危 npm audit + 内嵌后台重建/drift、golangci-lint 与 govulncheck;全绿后才静态编译 `linux/amd64` → 全部 artifact 进入远端不可预测的 `0700` stage → 校验精确 regular-file 集与 SHA-256 manifest → 停写后快照 SQLite main/WAL/SHM → 安装并跑 loopback gate → 持久化本地 commit → 重开 Caddy。commit 前任一路径失败都会把 DB、binary/symlink、env、unit、Caddy、静态站与旧全局 rollback 入口(首次部署则精确恢复为不存在)作为一个兼容单元完整自动恢复。root filesystem 上的 transition marker 配合永久 systemd Caddy condition,即使进程死亡或整机重启也保持公网入口关闭;commit 后 Caddy 启动失败则绝不冒险回卷可能已承接流量的 DB。
- 生产强制配置 GitHub Environment secret `SERVER_KNOWN_HOSTS`,缺失或不含 `SERVER_HOST` 条目即 fail closed;不存在 `ssh-keyscan`/TOFU 回退。远端 data dir 为 `0700`,DB/WAL/SHM 与 secret env 为 `0600`;成功发版后只保留一个 root-only rollback bundle。
- 服务器安装 schema-aware 人工回滚命令:`sudo /usr/local/sbin/anselm-gateway-rollback`(交互确认),自动化用 `sudo /usr/local/sbin/anselm-gateway-rollback --yes`。若主机/进程崩溃留下持久 transition marker,必须先恢复 marker 指向的精确 checksummed bundle。最可靠的入口始终是 `sudo <marker 中的 bundle>/recovery/rollback.sh --recover-incomplete`(非交互再加 `--yes`);全局入口若已升级,也支持同样的 recovery mode。每个 bundle 都携带该版精确 recovery program,回滚又会恢复旧全局入口,因此不会与更旧的保留 READY bundle 发生格式错配。永久 Caddy guard 则作为 inert 的受管安全 artifact 保留。回滚会同时恢复 DB 快照及整套运行 artifact;schema migration 后只切 binary symlink 明确不受支持且不安全。

部署目标(域名、ACME 邮箱)经 GitHub secret 注入、不入库。生产默认闸为 `INPUT_TOKEN_CAP=131072`、`MAX_TOKENS_CAP=16384`、`MAX_MESSAGES=1024`、`MAX_MESSAGE_CHARS=262144`、`MONTHLY_QUOTA=5000`、`RATE_PER_MIN=0`、`DAILY_SUBLIMIT=0`、`INSTALL_GLOBAL_DAILY_CAP=0`、`INSTALL_PER_FP_DAILY=0`、`INSTALL_PER_FP_COOLDOWN_SEC=0`、`INSTALL_PER_IP_HOUR=0`、`TOKEN_ANOMALY_RPM=0`。见 [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) 与 [`deploy/`](deploy/)。

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
