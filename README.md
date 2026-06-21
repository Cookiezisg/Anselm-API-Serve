# anselm-gateway

一个纯 Go + SQLite 的单二进制薄网关,给 Anselm(Flutter)客户端一个内置免费 DeepSeek 档:API key 只保存在服务端,客户端不直连上游。

`anselm-gateway` 在 DeepSeek 的 OpenAI 兼容接口前加了一层。它做三件事:

1. 代理推理。OpenAI 兼容的 `/v1/chat/completions`(流式 + 非流式 + tools/agentic),DeepSeek key 注入在请求副本上,不出服务端。
2. 配额记账。进上游前在单个 SQLite 事务里原子预占三道闸(月度次数 / 单 install 日 token / 全局日预算),据实结算;失败回滚,崩溃只会多扣不会少扣,不超卖预算。
3. 反滥用。匿名 install token(只存 SHA-256)+ 可选的 PoW / 限流闸(默认全部休眠),用于限制免费匿名档被批量滥用。

线上部署在一台海外 VPS 上,公网 TLS 由 Caddy 终结,后端三个监听器全部绑定 loopback。域名等部署目标信息不写进仓库(见[部署](#部署))。

## 特性

- OpenAI 兼容:`/v1/chat/completions`、`/v1/models`,SSE 流式与非流式均支持,客户端可按"又一个 OpenAI 兼容供应商"接入。
- 免费档支持 agentic:透传 `tools` / `tool_choice`,多轮工具调用保留 `tool_calls` / `tool_call_id` / `reasoning_content`(DeepSeek 思维模型多轮的必要条件)。
- 悲观三闸记账:reserve → forward → settle 事务 saga,单 `BEGIN IMMEDIATE`、单写连接序列化,首字节前任何失败回滚全部三闸,首字节后不重试、计费一次。
- 密钥不外泄:DeepSeek key 注入在 `req.Clone()` 上,上游错误归一化后不透传,日志 / 指标 / 审计不含 key / prompt / token / ip。
- 三个物理隔离监听器:公网业务面、loopback-only 运维面(metrics / pprof)、loopback 管理后台;三地址互斥,否则启动期 fail-fast。
- 严格输入白名单:`model` 强改写到 allowlist,参数 clamp,危险字段剥离,拒绝 `n>1`,body ≤ 256 KiB。
- 反滥用开关(默认休眠):install 限流、Sybil 粗阀、per-token 异常降速、领号 PoW(off / shadow / enforce 三态),默认全部关闭并短路,需要时热开。
- 管理后台:React SPA + 会话 / CSRF 保护的 `/api`,提供实时快照、配置热改、install 封禁、审计、一键 DB 导出。
- 可靠性:在飞请求 ≤ `N_GLOBAL_CONCURRENCY`,过载有界排队,断路器只统计真实上游故障(排除 client-cancel 与 429),低磁盘只读降级,systemd socket-activation 使 8080 跨重启不丢连接,关停时 DB 最后关闭。
- 单二进制、零外部依赖:纯 Go(`CGO_ENABLED=0`)+ 单文件 SQLite(WAL),前端 `dist` 已 `go:embed` 进二进制,部署不需要 Node。

## 适用范围

| 是 | 不是 |
|---|---|
| 单个 operator 自托管、给自家 Anselm App 用户开的免费 DeepSeek 档 | 通用 / 多租户 LLM 路由(只对接 DeepSeek 一个上游) |
| 一个钱包、一份预算的额度封顶器 | 计费系统(无 per-user 计费 / BYO-key / 模型市场) |
| 匿名但限流的身份(install + 可选 PoW,仅用于挡 Sybil) | 用户账号 / 鉴权身份提供方 |
| 单节点 SQLite + 单写池(原子记账正确性的基础,刻意为之) | 可横向扩展 / 分片 / 多写副本的服务 |
| 一个薄网关 | 完整 LLM 平台(RAG / 向量库 / 提示管理在主项目,不在这) |

这是单作者维护的产品网关,不是社区项目。它运行在 `main` 上、已上线;若作为通用服务部署,需要相当的改造。

## 快速开始

本地运行:

```sh
cp .env.example .env          # 至少填 DEEPSEEK_API_KEY
# 注入 env(bash/zsh):set -a; source .env; set +a
# fish:  for l in (grep -v '^#' .env); set -gx (string split -m1 = $l); end
make run                      # = go run ./cmd/gateway
curl -s http://127.0.0.1:8080/healthz   # → {"status":"ok"}
```

端到端示例(领号 → 推理):

```sh
# 1) 领一个 install token(无鉴权)
TOKEN=$(curl -s -XPOST http://127.0.0.1:8080/v1/install | jq -r .token)

# 2) 当作 OpenAI 兼容端点使用
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","stream":true,
       "messages":[{"role":"user","content":"用一句话自我介绍"}]}'

# 3) 查额度
curl -s http://127.0.0.1:8080/v1/quota -H "Authorization: Bearer $TOKEN" | jq
```

`model` 会被强改写到 `MODEL_ALLOWLIST` 的首项,客户端填任意 model 名都可;`GET /v1/models` 返回真实可用清单。

## API

业务面(`127.0.0.1:8080`,经 Caddy 暴露公网):

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| `POST` | `/v1/install` | 无 | 领 install token,返回 `{token, monthlyQuota, resetAt}`;token 只回显一次 |
| `GET` | `/v1/install/challenge` | 无 | 领号 PoW challenge(默认休眠时 `required=false`) |
| `POST` | `/v1/chat/completions` | Bearer install token | OpenAI 兼容推理,按 `stream` 返 SSE 或裸 JSON |
| `GET` | `/v1/models` | Bearer install token | OpenAI `{object:"list", data:[…]}`,源自实时 `MODEL_ALLOWLIST`,只读不计费 |
| `GET` | `/v1/quota` | Bearer install token | `{limit, used, remaining, resetAt, available}` |
| `GET` | `/healthz` | 无 | 纯进程存活,不访问 DB / 上游(公网唯一健康面) |

运维面(`127.0.0.1:9090`,仅 loopback,不公网、不经 Caddy;隔离靠物理绑定而非中间件):

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/metrics` | Prometheus 指标 |
| `GET` | `/readyz` | 就绪探针,分层 JSON `{db, upstream, disk}`,任一不可用返 503 |
| `GET` | `/debug/pprof/*`、`/debug/vars` | pprof + expvar 诊断(经 SSH 隧道访问) |

管理后台(`127.0.0.1:8081`,loopback;仅当 `DASHBOARD_USER` + `DASHBOARD_PASSWORD` 都配置时才启动,经 Caddy 反代到 `dashboard.<域>`):

- 公开:`GET /healthz`、`POST /login`(bcrypt + per-IP 退避锁定)、`POST /logout`
- 会话保护:`GET /api/{session,overview,config,installs,audit,export}`
- 会话 + CSRF:`POST /api/config`、`POST /api/installs/{ban,unban}`
- `GET /`:嵌入的 React SPA + `/static/` 资源

响应信封:成功为裸实体(字段在顶层,无 `data` 包裹);失败为 `{"error":{"code","message"}}`,非 `APIError` 归一化为 `500 INTERNAL`,上游 body / key 不透传。

## 架构

Clean Architecture(参照姊妹项目),依赖只指向更稳定的内层,由 `golangci-lint` depguard 强制:

```
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg
```

| 层 | 职责 |
|---|---|
| `domain` | 纯类型 + wire code + 周期 / 估算 / clamp / PoW 难度等规则;只依赖 stdlib + `pkg`,财务与安全不变量住在这里 |
| `app` | 用例编排(chat / install / quota / model / health / dashboard);在本包声明 infra 端口,禁止 import infra / `database/sql` / HTTP |
| `infra` | 唯一访问 OS / DB / 网络的适配器:SQLite 读写池 + 迁移、DeepSeek 客户端、限流、磁盘哨兵、config provider、metrics、内嵌 SPA |
| `transport` | HTTP 边界:统一信封、中间件、瘦 handler、三个 mux builder;不含业务逻辑 |
| `bootstrap` | 唯一可跨全层的组合根,且无人 import 它(无环) |
| `pkg` | 横切叶内核(logx / reqid / idgen / clientip / noncecache / pow / alert 等) |

三个监听器:业务面(公网,Bearer)、运维面(loopback-only,无应用鉴权、靠绑定隔离)、管理后台(loopback,会话 + CSRF)。三地址必须互异,否则启动 fail-fast。只有业务面 8080 走 systemd socket-activation(跨重启不丢连接)。

记账模型(每个 chat 请求):

```
入口快照 Period{月,日} ──▶ reserve(单 BEGIN IMMEDIATE,写池 MaxOpenConns=1 序列化)
      ├─ 月度请求数   < MONTHLY_QUOTA           ┐
      ├─ install 日 token + est ≤ 日 token cap  ├─ 任一条件 UPDATE 影响 0 行 ⇒ 整事务回滚 + 对应 429/402
      └─ 全局日预算 + est ≤ 日预算              ┘
   ──▶ forward 上游(首字节 = 计费一次的边界;首字节前任何失败 → 单一 defer 回滚全部三闸)
   ──▶ settle(按真实 usage 补差;`settled IS NULL` 单赢家 CAS 幂等)
```

崩溃只会留下 `settled IS NULL` 的账目行,由启动时 + 每 5 分钟的 reconcile 全额退还,因此崩溃只会多扣不会少扣。`*sql.Tx` 不漏出 infra,app 端口只暴露一个原子聚合操作。

安全约束:key 注入在 clone 上、不出服务端;token 只存 SHA-256,`/install` 不回显旧 token;运维面 loopback-only(绑定 fail-fast);机密 env-only,不入 DB / Dump / 日志;日志指标不含 key / prompt / token / ip,label 低基数;XFF 仅信任回环直连对端;TLS 由 Caddy 独家终结。完整登记册见 [`docs/references/backend/invariants.md`](docs/references/backend/invariants.md)(GW-INV-NN)。

## 配置

加载顺序为 env 默认 ← `settings` 表 DB 覆盖;运行时可改项可在管理后台在线热改、原子生效。完整配置面见 [`.env.example`](.env.example) 与 [`docs/references/backend/config.md`](docs/references/backend/config.md)。

机密(env-only,不入 DB / Dump / 日志):

| 机密 | 必填 | 说明 |
|---|---|---|
| `DEEPSEEK_API_KEY` | 是 | 上游 key,支持逗号分隔多 key(首个为主) |
| `DASHBOARD_USER` / `DASHBOARD_PASSWORD` | 成对可选 | 配齐才启动管理后台;半配 fail-fast |
| `INSTALL_POW_SECRET` | PoW 激活时必填 | 领号 PoW 的 HMAC 签名密钥,不自动生成 |

关键配置项(默认值取自代码):

| 项 | 默认 | 含义 |
|---|---|---|
| `GLOBAL_DAILY_BUDGET_TOKENS` | 必填 >0 | 全局每日 token 预算闸 |
| `INSTALL_DAILY_TOKEN_CAP` | 必填 >0 | 单 install 日 token 子配额(须 ≤ 全局预算,且 ≥ 输入+输出上限,否则 fail-fast) |
| `MONTHLY_QUOTA` | `5000` | 单 install 月度请求次数配额 |
| `MAX_TOKENS_CAP` / `INPUT_TOKEN_CAP` | `4096` / `16384` | 单请求输出 / 输入 token 上限(clamp) |
| `N_GLOBAL_CONCURRENCY` | `8` | 账号级在飞并发硬上限(多 key / 排队不放大,改后重启生效) |
| `RATE_PER_MIN` | `20` | per-install 每分钟令牌桶 |
| `QUEUE_WAIT_MS` | `1500` | 过载有界等待窗口,超时返 `429 UPSTREAM_BUSY`(`0` = 立即拒) |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `60` | 上游 connect → 首 header 超时(不覆盖流式 body) |

休眠开关(默认全部 `0` / `off`,零开销短路):`DAILY_SUBLIMIT`、`INSTALL_GLOBAL_DAILY_CAP`、`INSTALL_PER_FP_DAILY`、`TOKEN_ANOMALY_RPM`、`INSTALL_POW_MODE`。内存 / SQLite 调优项(如 `GOMEMLIMIT_MIB`=768)为启动期硬约束,改后需重启,并有最坏 RSS 自检防 OOM。

## 部署

线上为海外 VPS + Caddy + systemd:

- Caddy 独家终结 TLS(ACME 需 80 + 443 都开放),反代 `<api 域> → 127.0.0.1:8080`(SSE 设 `flush_interval -1`)、`dashboard.<root> → 127.0.0.1:8081`;后端只绑 `127.0.0.1`,不直面公网。域名经部署机密注入,不在仓库里。
- systemd socket-activation:`anselm-gateway.socket` 持有 8080 的 listen fd,`Type=notify` 服务接管,使连接跨 restart 不丢失。
- GitHub Actions(push `main` 触发,见 [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml)):`vet + test -race` → 静态编译 linux/amd64 → 版本化二进制 + 原子 symlink 切版 → 部署 gate(本机 `readyz` / `healthz` 不过则自动完整回滚到上一版)→ 仅保留最近 5 版。`caddy validate` 通过才覆盖 Caddyfile;所有 `uses:` 钉 40 位 SHA;绑定受审批的 `production` environment。

GitHub 仓库 Secrets(共 9 项):`SSH_PRIVATE_KEY`、`SERVER_KNOWN_HOSTS`、`SERVER_HOST`、`SERVER_USER`、`DEEPSEEK_API_KEY`、`DASHBOARD_USER`、`DASHBOARD_PASSWORD`、`GATEWAY_DOMAIN`、`ACME_EMAIL`。其中 `GATEWAY_DOMAIN` / `ACME_EMAIL` 设为 secret 是为了让公开仓库与 Actions 日志都不暴露部署位置(secret 在日志中自动掩码)。其余运行参数(model allowlist、预算、上限等)是 `deploy.yml` 里的明文配置,不进 secret。

注意:生产应配置 `SERVER_KNOWN_HOSTS` 钉死服务器 SSH 指纹;未配置则回退到 `ssh-keyscan` TOFU(可被 MITM,仅 dev 可接受)。手动回滚兜底:[`deploy/rollback.sh`](deploy/rollback.sh)。

## 开发与测试

```sh
make build     # → bin/gateway(本机)
make run       # go run ./cmd/gateway
make verify    # vet + build + test -race + docs(本地门禁)
make lint      # golangci-lint v2.6.1(含 depguard 分层门)
make docs      # 文档治理门禁(frontmatter / 类型·目录 / INDEX≤50 / 孤儿链接)
```

质量门(CI,见 [`.github/workflows/ci.yml`](.github/workflows/ci.yml)):`-race` 全量;golangci-lint(errcheck / staticcheck / gosec / govet / depguard 等);`govulncheck`(含周期重扫);`gofmt` 零漂移;fuzz smoke(chat / clientip / pow 解析器);前端漂移门(`npm ci && npm run build` 后 `git diff --exit-code dist`,证明内嵌 React 产物与源一致);SBOM(SPDX);覆盖率地板 `app/quota ≥ 70%`、`app/chat ≥ 65%`。

测试现状:48 个 `_test.go` 覆盖四层,外加 1 个 e2e(`internal/e2e`,`integration` tag,17 个用例,真 loopback 全栈黑盒)。钱路覆盖最重(实测 `app/quota` 95.7%、`app/chat` 86.3%)。已知不足:组合根 `internal/bootstrap`、`cmd/*`、以及管理后台的端到端路径无独立测试(后台靠单测覆盖,无 loopback-only 全栈断言)。

## 文档

`docs/` 是文档体系(治理规范 DOC-000),入口为 [`docs/INDEX.md`](docs/INDEX.md):

- [`GOVERNANCE.md`](docs/GOVERNANCE.md):文档如何写 / 同步 / 淘汰(强制规范)
- [`concepts/architecture.md`](docs/concepts/architecture.md):系统心智模型
- [`references/backend/`](docs/references/backend/):与代码逐字一致的硬契约(api / config / database / error-codes / invariants / overview)
- [`decisions/`](docs/decisions/):ADR-001..011(不可变取舍记录)

工程纪律(面向人与 AI 协作者)的最高法是根目录 [`CLAUDE.md`](CLAUDE.md)。

## 项目状态与已知缺口

- 线上运行:clean-arch 重写已落 `main` 并取代旧部署(prod 热修已并入),已上线;单轮与多轮 agentic 已端到端验证。
- 管理后台已接线:后端 `/api/*` 完整,React SPA 已构建、`go:embed` 进二进制(`internal/infra/webassets`)并由组合根挂载(`bootstrap/build.go` 传 `webassets.Handler()`),dashboard `/` 直接服务 React 应用 + `/static/` 资源。
- 反滥用默认休眠:PoW、Sybil 闸、per-token 降速、日次数子限额默认关闭,需要时热开。
- 无 HA:单进程 + 单文件 SQLite + 单写池,刻意为之(原子记账的正确性基础),不横向扩展。

## 许可

私有项目,暂无开源许可证(All rights reserved)。代码与运行配置含面向特定部署的机密边界,如需复用请先联系作者。
