---
id: DOC-036
type: working
status: active
owner: @weilin
created: 2026-07-31
reviewed: 2026-07-31
review-due: 2026-10-29
audience: [human, ai]
landed-into:
---

# 仓库治理战役 —— 执行工单（可独立交接）

> **本文自足。** 接手者不需要读任何历史会话，读完本文即可开工。
> 一切与 [`CLAUDE.md`](../../CLAUDE.md) 冲突之处，以 `CLAUDE.md` 为准（它是最高法）。

## 0. 一句话

把仓库里**已经死掉但没人收尸**的东西彻底清掉（DeepSeek、两种用不上的后台登录模式、上一代
provider 化石、过期文档），把**克隆式增生**出来的重复代码合并，并**装上门禁**让它以后自己保持
干净。分 7 个阶段（0..6），每阶段一次提交、独立 `make verify` 全绿。

---

## 1. 当前进度

| 阶段 | 状态 |
|---|---|
| **0 装门禁** | ✅ **已完成**（2026-07-31 落地并 push main） |
| 1 域名收口 + 死配置化石 | ⬜ 下一个 |
| 2 DeepSeek 全仓下掉 | ⬜ |
| 3 Dashboard 收敛 external | ⬜ |
| 4 数据库压平 | ⬜ |
| 5 合并克隆 | ⬜ |
| 6 注释降噪与文档落地 | ⬜ |

**阶段 0 交付的东西，后续每个阶段都靠它验收：**

```sh
make verify                          # 已含下面全部三道新闸
go run ./cmd/docs -write-baseline    # 清理完一批后重新生成禁词基线
```

1. **`unused` linter**（`.golangci.yml`）——包级死符号闸。落地时删掉了它报的唯一一处：
   `internal/domain/chat/content.go` 里的死别名 `chatMessage`。
2. **配置面三方对账测试**（`internal/infra/configprovider/envparity_test.go`）——把 `.env.example`
   ↔ loader 实际读取集 ↔ `config.Specs()` 三者钉死，双向。它落地时照出并修好了 9 处漂移：
   1 个 ADR-0012 已删却留在模板里的键（`MEDIA_PUBLIC_BASE_URL`，还带着真实域名），
   8 个 loader 真的会读却从没写进模板的键（`GATEWAY_MODE`、`SPEECH_*`、`TTS_*`、`VIDEO_*`）。
   顺带把 `DASHSCOPE_NATIVE_BASE` 从硬写北京端点改回留空派生——`load.go` 注释明写
   「新加坡 workspace key 打北京端点答 401，写死区域正是它发生的方式」。
3. **禁词棘轮闸**（`cmd/docs/forbidden.go` + `cmd/docs/forbidden.baseline`）——见下。

### 禁词基线 = 待办清单（工具证出来的，不是猜的）

基线**双向精确**：命中多于基线是回归，**少于**也失败——所以每次清理必须在**同一提交**里
`go run ./cmd/docs -write-baseline` 收缩它。终局是空文件。

落地时的基线（95 条），也就是阶段 1–4 要清零的全部工作量：

| 禁词 | 文件数 | 出现次数 | 清零于 |
|---|---|---|---|
| `deepseek` | 67 | 528 | 阶段 2 |
| `dashboard-auth-mode` | 17 | 48 | 阶段 3 |
| `gemini` | 4 | 10 | 阶段 4 |
| `deployment-domain` | 4 | 6 | 阶段 1 |
| `media-public-base-url` | 3 | 6 | 阶段 2 |

**永久豁免**（不进基线，因为「提到它」正是它们的职责）：`docs/decisions/`（ADR 不可变，一篇讲
「撤掉 DeepSeek」的 ADR 必须能说出 DeepSeek）、`docs/archive/`、本文件、闸自己的两个文件。

---

## 2. 为什么要治（三类病 + 一个结构性缺口）

**一、该死的东西没死。** DeepSeek 已于 2026-07-28 撤出路由（`88e3530` + `8ed84b3`），但仍散布在
37 个非测试文件里。更要命的是 `deploy/build-stage.sh` 仍**硬要求** `DEEPSEEK_API_KEY`
（`: "${DEEPSEEK_API_KEY:?...}"`），所以在改代码之前删掉那个 GitHub secret 会让部署直接失败。
同类：`gemini`（更早一代 provider，Go 代码里一行都没有，却仍活在两个迁移文件的 `CHECK` 约束里）。

**二、只用一种却养着三种。** `DASHBOARD_AUTH_MODE` 有 `disabled`/`builtin`/`external` 三态，生产
只用 `external`。为另外两态背着：`session.go`、`loginlimit.go`、`BuiltinAuth` 结构与 5 处 nil 分支、
bcrypt、前端三个文件的分支、部署脚本的模式分派与三组模式测试。

**三、克隆式增生 —— 这是「逻辑没法看」的真病因。** 每加一个能力就复制上一个的形状，从不回头合并：

- `internal/domain/billing/billing.go`：6 组几乎逐字相同的 `NewXxxPlan`/`XxxCost`
  （audio-seconds / images / characters / video-seconds / voices），且 `NewPlan` 里 5 个分支形如
  `provider != Qwen || model != X || outputBound != 0 || promptBound < 1`。
- `internal/app/{image,tts,video,voice}`：六个端口接口（`Authenticator`/`RateLimiter`/`Config`/
  `Quota`/`Clock`/`Metrics`）、`Service`/`Deps`/`New`、`Available()`、
  auth→ban→rate-limit→reserve→upstream→settle/rollback 整套骨架**逐字相同**。以
  `app/image` 与 `app/tts` 对读可立即确认，真正的差异只有 5 处。

**结构性缺口（为什么能攒下来）**：`.golangci.yml` 把架构分层守得极硬（depguard 逐层 deny），但
**没开 `unused`**、没有配置对账测试、没有禁词门。"东西死了没人收尸"完全没设防。

---

## 3. 已拍板的决定（用户口头确认，代码/文档里尚无体现）

接手者**不要再就这四条征询**，直接照做：

1. **线上数据库可清**。仍是测试阶段，历史数据不用管 → 迁移可以压平成一个 `0001`，并用
   `RESET_UNLAUNCHED_GATEWAY_DATA` 清库部署。**客户端遇到 `INVALID_INSTALL` 会自动重新登记**
   （用户 2026-07-31 确认），故清库对客户端是无感的；同时清掉的还有 `settings` 表里
   dashboard 改过的运行时 overlay，配置回到 env 默认值——这是可接受的。
2. **后台登录只留 `external`**。`disabled` 与 `builtin` 整个删掉。用户原话：只用 external，且它
   只打到 localhost、没有泄露风险。
3. **代码注释保持双语，但大幅精简**。不是删中文，是删叙事。
4. **WRK-082（图/TTS/视频/音色）已完工**，可以落进 `references/` 四契约并清空 `docs/working/`。
5. **每阶段提交后 push `main`**（用户 2026-07-31 确认）。push `main` = 触发自动部署生产，
   全程共 7 次。理由：阶段 2、3、4 都改部署脚本，部署层的问题当场暴露胜过攒到最后一起爆；
   且发布有完整自动回滚（DB 快照 / 二进制 / env / units / Caddy 作为一个兼容单元），尚未上线。
   **前提不可省**：本地 `make verify` 全绿才 push，绝不推一个红的提交去让 CI 发现。

---

## 4. 方法论铁律（本战役的核心，违反则返工）

> 用户明确否定过两种做法：只靠 grep 找引用（"怎么可能保证搞完是干净的"），以及只靠工具不读代码
> （"不应该你自己亲自读代码，理解然后修改吗"）。**两者都要**：读代码产生判断，工具证明没有遗漏。

1. **删除靠编译器穷举，不靠 grep。** 要删一个东西，先删它的**定义**（常量 / 结构体字段 / 枚举值），
   再跑 `go build ./...`——编译器报出的每一处就是完整清单，一个不漏。grep 会漏掉换行的、别名的、
   间接的；类型系统不会。
2. **编译器看不见字符串**，故它的盲区各配一道机械闸：

   | 盲区 | 闸 |
   |---|---|
   | env 键名 | `internal/infra/configprovider/envparity_test.go` 三方对账（已在 stash 中） |
   | SQL / schema | 迁移压平 + golden-schema 测试（阶段 4） |
   | 文档 / shell / 前端 | 禁词棘轮门（阶段 0 剩余部分） |
   | 包级死符号 | `unused` linter（已在 stash 中） |
   | 全程序死代码 | `go run golang.org/x/tools/cmd/deadcode@latest ./cmd/gateway` |

3. **每阶段一次提交，独立 `make verify` 全绿**才进下一阶段。
4. **doc-code parity**：碰了 GOVERNANCE §7 触发表里的东西，同一提交同步文档。
5. **整体重述**：碰到的每一段都重写成"现在是什么"，不旁追加、不留旧痕（CLAUDE.md §1.7）。
6. **阶段 5、6 逐文件读、逐文件重写、逐文件提交**，不要塞进一个大提交——工具判不了"这段注释是
   施工日志还是状态陈述"，只能靠人审。
7. **ADR 不可变**。要推翻某条决策，加 superseding 说明，不改原文。

---

## 5. 七个阶段

### 阶段 0 — 装门禁 ✅ 已完成

交付内容与基线数据见第 1 节。三道闸（`unused` / 配置三方对账 / 禁词棘轮）均已进 `make verify`，
落地时顺手修好了它们照出的 10 处问题（1 个死别名 + 9 处配置面漂移）。

后续每个阶段的固定收尾动作：清理完 → `go run ./cmd/docs -write-baseline` → `make verify` 全绿 →
提交（基线与清理**同一提交**）→ push main。

---

### 阶段 1 — 域名收口 + 死配置化石

**域名泄漏共 7 处**（`grep -rnE '\banselm\.[a-z]{2,10}\b'`，实测零误报）：

- `.env.example`（stash 中随 `MEDIA_PUBLIC_BASE_URL` 一并删除，已解决）
- `internal/domain/chat/medialease_test.go`
- `internal/pkg/secureurl/secureurl_test.go`
- `docs/how-to/cloudflare-deployment.md`（**3 处**）
- `deploy/site/index.html`
- `internal/infra/webassets/ui/dist/assets/*.js`（构建产物；改源码后重建 dist）

测试里改成 `example.com` 之类的占位符即可；`deploy/site/index.html` 与 dist 需确认是否应改为
构建期注入（`GATEWAY_DOMAIN`/`SITE_DOMAIN` 已经是 GitHub secret，走同一条路）。

**其余**：
- 删空目录 `docs/references/domains/`（只有 `.gitkeep`，建立后从未使用）
- 7 篇 `status: superseded` 的文档移进 `docs/archive/`：`docs/working/slice-plan.md` +
  `docs/working/spec-{api,behaviors,config,database,error-codes,invariants}.md`
  （`docs/archive/` 已建好但从未使用；`cmd/docs` 对 `archive/` 免检 frontmatter）
- 同步 `docs/INDEX.md`（它现在还指向 `working/slice-plan.md`，移走后会变成孤儿链接 → `make docs` 失败）

---

### 阶段 2 — DeepSeek 全仓下掉

**做法**：先删 `billing.ProviderDeepSeek` 常量与 `config.Config.DeepSeekAPIKeys` 字段，
`go build ./...` 会列出全部引用，再逐个处理。**不要用 grep 当清单。**

Go 侧关键落点（行号仅为提示，以符号名为准）：

| 文件 | 处理 |
|---|---|
| `internal/domain/billing/billing.go` | 删 `ProviderDeepSeek`、`DeepSeekV4Flash`、`DeepSeekInputLimit`/`DeepSeekOutputLimit`、费率卡条目、`Cost()` 里的 DeepSeek 分支、`legacyMaxPUSDPerToken` + `LegacyMaxPUSDPerToken()`（v1 迁移专用，阶段 4 一并消失） |
| `internal/domain/config/config.go` | 删 `DeepSeekAPIKeys`/`DeepSeekBaseURL`/`TextUpstreamModel`。**`ValidateSemantics` 里那段无条件的 DeepSeek 文本 plan 校验改为 Qwen**——现状是"死路由必查、活路由（Qwen）选查"，正好反了 |
| `internal/domain/config/spec.go` | 删 `TEXT_UPSTREAM_MODEL`、`DEEPSEEK_BASE_URL` 两行 |
| `internal/app/chat/service.go` | `routeFor` 已退化成忽略入参返常量（`_ = modality; return ProviderQwen, cfg.MultimodalUpstreamModel, ...`）→ 整个函数删掉，调用点直读配置 |
| `internal/app/model/catalog.go` | ⚠️ **live bug**：`Text.Available` 判的是 `len(cfg.DeepSeekAPIKeys) > 0`，`Text.OutputLimit` 报的是 DeepSeek 的 384K，而实到上游是 Qwen（64K 上限）。改为 Qwen 双半可用性 + Qwen 上限。**这是删 secret 会踩的第二个雷**：不改它，删掉 key 后 `/v1/models` 会告诉桌面端"文本聊天不可用" |
| `internal/infra/configprovider/load.go` | 删 env 读取与 `ErrDeepSeekKeyRequired` |
| `internal/infra/upstream/client.go` · `internal/infra/chatprovider/registry.go` · `internal/bootstrap/build.go` | 删 `BackendDeepSeek`、registry 槽位、client 构造、熔断器指标初始化 |
| `internal/pkg/logx/logx.go` | 脱敏 key 名单 |

Go 之外：`deploy/build-stage.sh`（**硬要求那行必须先删**）、`deploy/build_stage_test.sh`、
`.github/workflows/deploy.yml`、`.env.example`、6 篇 docs、`README.md`、`README.zh-CN.md`、
`CLAUDE.md`、`docs/INDEX.md`；ADR-0001/0006/0013/0016 加 superseding 说明。

**收尾（人工）**：告知用户删除 GitHub repository secret `DEEPSEEK_API_KEY`。**顺序不可颠倒**：
代码与部署脚本先改完、部署成功一次，再删 secret。

---

### 阶段 3 — Dashboard 收敛成 `external` 单模式

删掉整个 `DashboardAuthMode` 枚举（连同 `disabled` 与 `builtin`）。

**后果需知悉**：`disabled` 原本表示"不挂载 dashboard"（`bootstrap/build.go` 里
`if mode != Disabled` 才挂载）。收敛后 dashboard **永远挂载**，但仍只绑 `127.0.0.1:8081`、
绑不上即 fail-fast，`CLAUDE.md` §3 的"admin 面绝不公网"红线不破。

落点：

- `internal/domain/config/config.go`：删 `DashboardAuthMode`/`DashboardUser`/`DashboardPassword`/
  `DashboardDevInsecureCookie`、`ValidateDashboardAuthMode` 与三个常量
- 删文件：`internal/transport/httpapi/middleware/dashboard/session.go`、`loginlimit.go`
- `internal/transport/httpapi/handlers/dashboard/handler.go`：删 `BuiltinAuth` 与 5 处 nil 分支、bcrypt
- `internal/transport/httpapi/router/dashboard.go`：删 `/login`、`/logout`
- `internal/bootstrap/build.go`：删模式分派
- 前端 `internal/infra/webassets/ui/src/`：`auth/AuthContext.tsx`、`lib/api.ts`、`lib/types.ts` 去分支，
  **必须重建 `dist/`**（CI 有嵌入产物漂移门，不重建会红）
- `deploy/build-stage.sh` 模式分派、`deploy/build_stage_test.sh` 三组模式测试、`deploy.yml` 三个 secret
- ADR-0014 加 superseding 说明

**收尾（人工）**：告知用户删除 GitHub secret `DASHBOARD_AUTH_MODE`、`DASHBOARD_USER`、
`DASHBOARD_PASSWORD`。连同阶段 2，repository secret 从 13 个降到 9 个。

---

### 阶段 4 — 数据库压平

数据可清（第 3 节决定 1），故不必背迁移历史。

- `internal/infra/sqlite/migrations/` 七个文件 → 一个干净的 `0001_init.sql`。清掉：
  `provider` CHECK 里的 `'deepseek'`/`'gemini'`、临时表名 `provider_spend_daily_next` /
  `spend_ledger_next`、v1 三表 `usage`/`ledger`/`budget`、`accounting_v2_migration_guard`
- 新增 golden-schema 测试：启库 → dump schema → 与基准文件逐字比。此后 schema 任何漂移即红
- 重写 `docs/references/backend/database.md`
- **部署**：`workflow_dispatch` + `reset_unlaunched_gateway_data=true` + 确认串
  `RESET_UNLAUNCHED_GATEWAY_DATA`。这会清掉全部 install / 配额 / 账务——**已获用户明确授权**

---

### 阶段 5 — 合并克隆（行为保持的结构重构）

#### 第 0 步（**独立提交，零行为改动**）：行为冻结

**先冻结现状，再动代码。** 没有这一步，"测试通过"只说明测试没覆盖到，不说明行为没变。

这一步不改任何生产代码，只新增测试，把**当前的实际输出**钉成可执行断言：

1. **费率 golden 表**：遍历全部 `(InputClass, model, units)` 组合，把 `NewPlan`/`NewXxxPlan` 算出的
   `ReservedPUSD` 与各 `XxxCost` 的**实际数字**打成表提交进去（例：`qwen3.7-plus`，1M input +
   64K output → 恰好 19,200,000,000,000 pUSD）。重构后任何一个数字变了立即红。
   同时覆盖 `Cost(Usage)` 的 token 路径：正常 usage、缺失 usage、malformed usage、cache 命中。
2. **`/v1/models` JSON 快照**：在**生产同款配置**下（`MAX_TOKENS_CAP=16384`、四个能力全开、
   两把 key 都在）渲染 `anselm_capabilities` 全文并逐字比对。
   ⚠️ 这条同时锁死阶段 2 的 `catalog.go` 修复**不改变**当前输出——已核算：
   `Text.InputLimit` 1M→1M、`Text.OutputLimit` `min(16384,384000)`→`min(16384,65536)` **都是 16384**、
   `Text.Available` 两把 key 生产上都在故都为 true。那个修复拆的是"删 key 时才引爆的雷"，
   不是改当前行为。
3. **错误信封快照**：每个 wire code → status + message 逐字比对（`error-codes.md` 是清单）。

先落这个提交，再做下面的合并。

#### 合并本体

**这一阶段不改任何行为。** 上面的 golden + 现有测试 + `internal/e2e` 整栈 + GW-INV 全套是准绳；
覆盖率地板（`app/quota` ≥70%、`app/chat` ≥65%）不得下降。

- **`domain/billing`**：6 组 `NewXxxPlan`/`XxxCost` 折成表驱动——一张
  `map[InputClass]struct{model string; minUnits int64}` + 一个 `NewUnitPlan(class, units)` +
  一个 `(Plan).UnitCost(units)`。`NewPlan` 的 5 分支 switch 随之消失。`Cost(Usage)` 保留给 token 路径。
  顺带把物理上被追加在私有 helper **之后**的 `NewVideoSecondsPlan`/`VideoSecondsCost` 归位。
- **`app/`**：抽出共享 runner（建议新包 `internal/app/genrun`），owns 六个端口接口 + `Available`
  双半规则 + 整套 reserve→upstream→settle/rollback 骨架。`image`/`tts`/`video`/`voice` 各自只留
  真正的差异：能力开关字段、plan 构造、cost 函数、unavailable 错误、upstream 闭包，以及各自独有
  的那一点（`image` 的 `Edit` data-URL 形状闸、`tts` 的 `resolveVoice`、`video` 的异步签名句柄）。
  depguard 允许 app→app 同层 import。
- **`infra/upstream`**：`imagegen.go`/`ttsgen.go`/`videogen.go`/`voicegen.go` 共用的 DashScope 原生
  POST + 错误归一 → 一个 `nativeCall` helper。
- **`domain/config`**：`Config` 字段按逻辑分组重排（现按施工时间排——`VoiceAccountCeiling` 夹在
  `ImageUpstreamModel` 与 `ImageDailyLimit` 之间，把图像配置劈成两半）。修 `spec.go` 里那段孤儿
  注释：内容讲 `VOICE_ACCOUNT_CEILING`，却贴在 `VOICE_DAILY_LIMIT` 头上。
- **清理 `deadcode` 报告里未被其它阶段覆盖的死函数**（见第 6 节清单）。注意 `deadcode` 从 `main`
  算可达性，**仅被测试使用的函数也会被报为不可达**——删之前逐个确认。

---

### 阶段 6 — 注释降噪、文档落地、门禁转正

**注释**（双语保留、大幅精简）：每处只陈述"现在是什么"。删掉施工日志（"此前"/"以前"/"上一笔"/
踩坑回忆）、**116 处工单号**（`WRK-082`/`H9`/`批B`/`P8`/`代拍 C5`/`M2 批次3`/`reviewer B3`——这些
指向另一个仓库的工作文档，读代码的人打不开）、修中英混排断句疤痕。

已知疤痕 4 处（其余用正则 `[a-zA-Z]{3,}[\x{4e00}-\x{9fff}]` 扫）：

- `internal/infra/upstream/voicegen.go` —— `…而不是从回包里去debug的理由。`
- `internal/transport/httpapi/handlers/business/audio/handler.go` —— `// chunking长 text HERE would…`
- `internal/domain/model/model.go` —— `// discovery-driven model-list事实源). PURE domain…`
- `internal/domain/billing/billing.go` —— `// the upstream bills its真实 list price regardless.`

参考量级：非测试 Go 共 21,316 行代码配 5,442 行纯注释（26%）。

**文档**：
- WRK-082 已完工 → `docs/working/multimodal-generation.md` 落进 `references/` 四契约 +
  `invariants.md` + ADR，标 `landed-into` 后归档；连同本文一起，`docs/working/` 清空
- 拆 `docs/references/backend/api.md` 的巨型表格单元格（最长单行 **1,913 字符**，一个 Markdown
  表格 cell 里塞了 500 多字的论文）
- `CLAUDE.md`、`README.md`、`README.zh-CN.md`、`docs/concepts/architecture.md`、`docs/INDEX.md`
  按现状整体重述（它们现在仍描述"文本→DeepSeek、图片/视频→Qwen"的双 provider 模型，**已不成立**）

**门禁转正**：禁词门 baseline 应已空，删掉 baseline 机制改为硬失败；新增注释规约检查
（工单号正则、中英混排正则、Markdown 单行长度上限）。

---

## 6. 已实测数据（省去重复调查）

### `deadcode` 全程序不可达（16 处）

`go run golang.org/x/tools/cmd/deadcode@latest ./cmd/gateway`

```
internal/domain/billing/billing.go        LegacyMaxPUSDPerToken     → 阶段 2/4 随 v1 账本消失
internal/domain/chat/chat.go              SanitizeUpstream          → 阶段 5 确认后删
internal/domain/chat/usage.go             ParseUsageLine            → 阶段 5 确认后删
internal/domain/chat/usage.go             ParseUsageBody            → 阶段 5 确认后删
internal/domain/media/media.go            ValidUploadState          → 阶段 5 确认后删
internal/domain/media/media.go            ValidLeaseState           → 阶段 5 确认后删
internal/infra/sqlite/sqlite.go           DefaultConfig             → 阶段 5 确认后删
internal/infra/upstream/client.go         New                       → 阶段 5 确认后删
internal/pkg/alert/alert.go               WithClock                 → 阶段 5 确认后删
internal/pkg/idgen/idgen.go               RequestID                 → 阶段 5 确认后删
internal/pkg/idgen/idgen.go               MediaFetchToken           → 阶段 5 确认后删
internal/pkg/logx/logx.go                 SampledInfo               → 阶段 5 确认后删
internal/pkg/ratesample/ratesample.go     WithClock                 → 阶段 5 确认后删
.../middleware/dashboard/loginlimit.go    LoginLimiter.Locked       → 阶段 3 随 builtin 消失
.../middleware/dashboard/loginlimit.go    LoginLimiter.Sweep        → 阶段 3 随 builtin 消失
.../middleware/dashboard/session.go       SessionStore.Sweep        → 阶段 3 随 builtin 消失
```

⚠️ `deadcode` 从 `main` 算可达性，**仅被测试引用的函数同样会被报为不可达**。删之前逐个确认。

### 改动次数最多的文件（churn，指示"被反复改到变形"的地方）

```
35  docs/references/backend/api.md          25  internal/infra/configprovider/load.go
29  docs/references/backend/invariants.md   24  internal/domain/config/config.go
29  docs/references/backend/config.md       22  internal/bootstrap/build.go
21  README.md                               19  internal/domain/apierr/apierr.go
```

---

## 7. 陷阱清单（踩过或已确认）

1. **`make verify` 必须跑，不能只跑 `go test ./...`。** 集成 e2e 带 `-tags=integration` build tag，
   `go test ./...` **看不见它**——它曾整包红着过好几个提交无人知晓（Makefile 里有这段教训的记录）。
2. **本地 lint 必须跑。** `make verify` 已含 `make lint`；少了它，本地会对着一份比 CI 宽的规则说"全绿"。
3. **删 GitHub secret 的顺序**：先改代码与部署脚本 → 部署成功一次 → 再删 secret。反过来会让部署
   在打包阶段失败，且报错信息（`DEEPSEEK_API_KEY is required`）离病因十万八千里。
4. **前端改了必须重建 `dist/`**，CI 有嵌入产物漂移门。
5. **移动文档会制造孤儿链接**，`make docs` 会红。移动前先 grep 谁链接了它。
6. **`docs/INDEX.md` 有 50 行硬上限**（`make docs` 强制）。
7. **ADR 不可变**，只能加 superseding 说明。
8. **禁词扫描不要跳过 `dist/`**——构建产物里有 1 处域名命中。
9. **`.env.example` 的 `DASHSCOPE_NATIVE_BASE` 必须留空**，硬写区域会让新加坡 workspace key 打
   北京端点答 401（同一把 key，一个区域 200 另一个 401）。

---

## 8. 验收

每阶段结束：

```sh
make verify    # vet + build + test -race + e2e(-tags=integration) + lint + docs
gofmt -l .     # 须为空
```

阶段专属：

- **阶段 2/3**：`go build ./...` 是完整性证明；另确认 `/v1/models` 的 `anselm_capabilities` 里
  text profile 报的是 Qwen 的上限与可用性
- **阶段 4**：golden-schema 测试；清库部署后 `curl /healthz`、`/readyz`，重新登记一个 install
  走通 `/v1/quota`
- **阶段 5**：全量测试 + 覆盖率地板不降；`internal/e2e` 整栈绿
- **最终真机冒烟**（花真钱，用户已授权）：chat（文本 + 图片）、`/v1/images/generations`、
  `/v1/images/edits`、`/v1/audio/speech`、`/v1/voices` 登记、`/v1/videos/generations` + 轮询

## 9. 不做的事

- 不改任何记账语义、不动 GW-INV 任何一条不变量的**内容**（只改表述位置）
- 不动 `internal/pkg/` 叶内核（`clientip`/`pow`/`noncecache`/`orm` 等），它们没有克隆问题
- 不重排 git 历史
- 不擅自扩大范围：遇到本文未覆盖的问题，记下来问用户，不要顺手改
