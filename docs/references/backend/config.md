---
id: DOC-006
type: reference
status: active
owner: @weilin
created: 2026-06-21
reviewed: 2026-07-21
review-due: 2026-10-19
audience: [human, ai]
---

# 配置全面（config）

> `internal/domain/config/spec.go` 是 dashboard/apply registry，`config.go` 是边界与跨字段语义事实源，`internal/infra/configprovider/load.go` 是 env/default 事实源。金额在 env/dashboard 使用整数 microUSD，进入账本后精确换算为 pUSD：`1 microUSD=10^6 pUSD`、`1 USD=10^12 pUSD`。路由/计价决策见 [ADR-0012](../../decisions/0012-deterministic-capability-routing-and-cost-ledger.md)。

## 1. 三层级与 secret 边界

| Tier | dashboard | settings | 生效方式 |
|---|---|---|---|
| `TierRuntimeHot` | 可编辑 | 可持久化 | clone→全量校验→单 tx persist→atomic swap |
| `TierStartupHard` | 只读（在 `Specs` 中的项） | 禁止 | env + restart |
| secret（故意不在 `Specs`） | 不出现 | 禁止 | env + restart |

Secrets：`DEEPSEEK_API_KEY`、`KIMI_API_KEY`、`DASHBOARD_USER`/`DASHBOARD_PASSWORD`、`INSTALL_POW_SECRET`。它们不能被 apply、不能进入 `settings`/Dump，Snapshot 只报告掩码状态或已配置 key 数量；raw bytes 永不输出。

## 2. runtime-hot registry（`Specs()` 顺序）

`Bounded` 数值在 env-load 与 overlay 路径共用同一闭区间；`MAX_BODY_BYTES` 与 `N_GLOBAL_CONCURRENCY` 虽可持久化，实际装配容量要 restart 才变化。

| key | 默认 | Min | Max | Restart | 语义 |
|---|---:|---:|---:|---|---|
| `PUBLIC_MODEL_ID` | `anselm-auto` | — | — | 否 | 唯一 client-facing 逻辑模型 id；非空；不选择 provider |
| `GLOBAL_MONTHLY_SPEND_MICRO_USD` | 420,000,000 | 1 | 9,000,000,000,000 | 否 | operator 全局月花费钱包（默认 $420/月） |
| `MONTHLY_QUOTA` | 5000 | 1 | 1,000,000,000 | 否 | per-install 月请求次数 |
| `MAX_TOKENS_CAP` | 4096 | 1 | 1,000,000 | 否 | caller `max_tokens` 的 operator 保险丝；缺省请求不主动写 wire `max_tokens`，账务仍按此上限保守预留 |
| `INPUT_TOKEN_CAP` | 16384 | 0 | 10,000,000 | 否 | 文本/tools 保守 estimate 上限；0=禁用；**不是媒体 token 上限** |
| `MAX_MESSAGES` | 256 | 1 | 100,000 | 否 | 完整 history 的 message 数上限 |
| `MAX_MESSAGE_CHARS` | 131072 | 1 | 16,777,216 | 否 | 单 message 文本 rune 上限 |
| `MAX_MEDIA_PARTS` | 8 | 1 | 64 | 否 | 整请求 image+video+audio part 数上限 |
| `MAX_MEDIA_DECODED_BYTES` | `min(3MiB, MAX_BODY_BYTES×3/4)` | 1 | 8,388,608 | 否 | 整请求累计 decoded media bytes；同时必须 ≤ body cap |
| `MAX_BODY_BYTES` | 262144 | 4096 | 8,388,608 | **是** | business chat body cap；中间件装配一次 |
| `N_GLOBAL_CONCURRENCY` | 8 | 1 | 100,000 | **是** | 两 provider 共享的总 upstream 在飞 cap |
| `RATE_PER_MIN` | 0 | 0 | 10,000,000 | 否 | per-install 分钟令牌桶；0=禁用 |
| `DAILY_SUBLIMIT` | 0 | 0 | 1,000,000,000 | 否 | per-install 日请求次数子限；0=禁用 |
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

## 3. startup-hard / env-only

### 3.1 `Specs()` 中的 dashboard 只读项

| key | 默认 | 约束 / 语义 |
|---|---|---|
| `TEXT_UPSTREAM_MODEL` | `deepseek-v4-flash` | 必须是 DeepSeek 的精确已编译 rate card；纯文本路由 |
| `MULTIMODAL_UPSTREAM_MODEL` | `kimi-k2.6` | Kimi 启用时必须是精确已编译 rate card；媒体路由 |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | remote 必须 HTTPS；仅 canonical loopback IP literal 可 HTTP（不信任 `localhost`/hosts/NSS，拒绝 `127.0.0.1.` 等尾点拼写以免绕过 `HTTP_PROXY` loopback 特判）；无 userinfo/query/fragment；去尾 `/`；调用 `/chat/completions` |
| `KIMI_BASE_URL` | `https://api.moonshot.ai/v1` | 同一 credential-safe URL policy；去尾 `/`；调用 `/chat/completions` |
| `GOMEMLIMIT_MIB` | 768 | ≥0；0=禁用 heap soft limit |
| `SQLITE_CACHE_KIB` | 32768 | >0；per connection |
| `READ_POOL_MAX_CONNS` | 4 | >0 |
| `SQLITE_MMAP_MB` | 256 | ≥0；0=禁用 |
| `ADMIN_ADDR` | `127.0.0.1:9090` | 与另两监听互异，必须 loopback |
| `DASHBOARD_ADDR` | `127.0.0.1:8081` | 与另两监听互异，必须 loopback |
| `LISTEN_ADDR` | `127.0.0.1:8080` | 与另两监听互异 |
| `RESET_TZ` | `Asia/Shanghai` | `LoadLocation` 失败 panic，无 UTC fallback |
| `GATEWAY_DB_PATH` | `anselm-gateway.db` | SQLite 文件 |

### 3.2 其它 startup env（不在 dashboard registry）

| key | 默认 | 约束 / 语义 |
|---|---:|---|
| `SQLITE_WAL_AUTOCHECKPOINT` | 4000 | ≥0 pages |
| `MEM_BUDGET_MIB` | 2048 | >0 |
| `MEM_SAFETY_MARGIN_MIB` | 400 | ≥0 |
| `LOG_LEVEL` | `info` | process log level |
| `DASHBOARD_DEV_INSECURE_COOKIE` | `false` | 仅 dev；生产 cookie 恒 Secure |

## 4. secret-env-only

| key | 约束 / 缺失行为 |
|---|---|
| `DEEPSEEK_API_KEY` | **必填**；逗号分隔、trim、过滤空 key；最终为空 → `ErrDeepSeekKeyRequired`，process 不启动 |
| `KIMI_API_KEY` | 可选；同样支持逗号分隔多 key；为空则不构造 Kimi backend，文本/readiness 正常，合法多模态返回 `503 MULTIMODAL_UNAVAILABLE` |
| `DASHBOARD_USER` / `DASHBOARD_PASSWORD` | 同设或同空；半配 fail-fast；同设才启 dashboard auth |
| `INSTALL_POW_SECRET` | 不自动生成；`shadow|enforce` 必须非空，`off` 可空 |

每个 backend 的 URL、key pool 与 breaker 在 construction 时冻结。Kimi 缺 key 不是“坏 key”或 readiness fault，而是该 deployment 没有多模态 capability。

## 5. 跨字段语义（每次 env-load / overlay / hot batch 都跑）

1. `GLOBAL_MONTHLY_SPEND_MICRO_USD>0`，并且任一已启用 route 的单请求最坏 quote 必须能装入该月预算；否则配置 fail-fast/热改拒绝。
2. `PUBLIC_MODEL_ID` 非空；client id 与两个实际模型 id 没有映射选择关系。
3. 统一产品档位固定为 thinking-on：DeepSeek route 注入 `thinking.enabled` + `reasoning_effort=high`；Kimi route 注入 `thinking.enabled` 且不传 `reasoning_effort`。client-supplied thinking/effort 均不改变该档位；client `max_tokens` 是调用参数，只在模型/`MAX_TOKENS_CAP` 边界内透传。
4. `TEXT_UPSTREAM_MODEL` 必须精确等于已知 DeepSeek rate card；`INPUT_TOKEN_CAP≤1,000,000`；`min(MAX_TOKENS_CAP,384,000)` 与文本输入 quote 的最坏成本必须装入全局月预算。
5. **仅当 `KIMI_API_KEY` 已配置**，`MULTIMODAL_UPSTREAM_MODEL` 必须精确等于已知 Kimi rate card，且完整 `262,144` input + `32,768` output quote（`380,108.8 microUSD`）必须装入全局月预算。未配 key 时 inactive-Kimi 关系不阻断纯文本启动；以后加 key 重启时会一次性 fail-fast 校验。此预留不意味着 `INPUT_TOKEN_CAP` 能估算图片/视频 token；媒体形状/bytes 单独受限并交 Kimi 判定实际 token。音频虽计入公共媒体形状/bytes 闸，但当前没有 provider 配置可使其可路由。
6. `1≤MAX_MEDIA_PARTS≤64`，`1≤MAX_MEDIA_DECODED_BYTES≤MAX_BODY_BYTES`。
7. `INSTALL_POW_MODE∈{shadow,enforce}` 时必须已有 env-only secret。

违反任一项 fail-fast，未知模型绝不以旧价格继续运行。金额 rate card 逐字值见 [ADR-0012](../../decisions/0012-deterministic-capability-routing-and-cost-ledger.md)。

## 6. PERF-2 与热改原子性

最坏 RSS=`GOMEMLIMIT_MIB + (SQLITE_CACHE_KIB/1024)×(1+READ_POOL_MAX_CONNS) + SQLITE_MMAP_MB`。若超过 `MEM_BUDGET_MIB−MEM_SAFETY_MARGIN_MIB`：`GOMEMLIMIT_MIB=0` 时 WARN 放行，否则 `ErrMemoryBudget` fail-fast。

`ApplyOverrides` 对 base clone 依 key 排序 apply；未知/secret/startup-hard 均指名拒绝；随后重跑全部跨字段语义。Provider 在写锁内先单事务持久化、成功后 atomic swap；任一错误不落半份 settings、不发布半份 Config。每个业务请求只读取一次 Config snapshot。

## 7. v1 overlay 迁移

迁移 `0002_provider_spend_ledger.sql` 曾将旧 `GLOBAL_DAILY_BUDGET_TOKENS` / `INSTALL_DAILY_TOKEN_CAP` 换为 retired daily spend settings；`0004_global_monthly_budget.sql` 将历史 `global_spend_daily` 聚合到 `global_spend_monthly`，并删除旧 daily/provider/install spend settings。新版本不读取这些旧 env/settings 名称；`PUBLIC_MODEL_ID` 是唯一公开模型配置。
