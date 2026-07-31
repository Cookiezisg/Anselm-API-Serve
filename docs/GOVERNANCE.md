---
id: DOC-000
type: concept
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-07-20
review-due: 2026-10-18
audience: [human, ai]
---

# anselm-gateway 文档规范（Documentation Governance）

> 本文件定义 `github.com/sunweilin/anselm/gateway` 仓库**全部**文档如何被创建、组织、同步、淘汰。**它是强制规范，与代码纪律（`CLAUDE.md`）同级。**
> 本文件自身遵守它定义的一切规则（frontmatter / 类型 / 生命周期），作为标准的活样板。
> 本规范的采纳决策见 [`ADR-008`](decisions/0008-doc-governance-adoption.md)：照搬 Foryx 治理模型、本地化到本网关。

---

## 0. 强制性与执行设计（先读这条）

文档规范若不被执行，等于不存在。本规范用**三层冗余**确保 Claude（及人）完整遵循——任一层兜住，三层叠加几乎不漏：

| 层 | 机制 | 作用 |
|---|---|---|
| **① 常驻** | `CLAUDE.md`（每次会话**自动加载**、工程纪律唯一事实源）内嵌「文档纪律」节：核心条款 + §7 触发表精要 + §12 收尾清单 | 使 Claude **每次会话都已读到**这些规则——**无「不知道」借口** |
| **② 规范** | 本文件：具体到 **if-then** 的触发表（§7）、可逐条勾的收尾清单（§12）、机械可判的门禁项（§11） | 把「文档要同步」从空泛原则变成**可机械执行**的指令 |
| **③ 门禁** | `make docs`（§11，`cmd/docs`、并入 `make verify`）机械校验 frontmatter / 必填 / 类型·状态·目录 / review-due / working 90 天 / INDEX≤50 / 孤儿链接 | 捕捉人或 AI 的疏漏，**让违规无法静默通过** |

**三条铁律（违反 = 严重 Bug，与编译失败同级）：**

1. **文档与代码物理同步**：改了代码却没在**同一提交**同步对应文档 → 这次改动**未完成**。文档落后于代码 = Bug。
2. **触发即停**：任何时候发现文档与代码不符，**立刻停下修文档**（记 `[doc-fix]` dev log），再继续原任务。
3. **不确定就查本规范**；本规范没覆盖的 → 按 §1 核心原则推导，并**回头给本规范补一条**（规范随项目生长）。

---

## 1. 核心原则

1. **文档-代码物理同步（doc-code parity）**：`reference` 类文档必须与代码**逐字**对得上——端点 / 配置键 / 表列 / wire code / GW-INV 编号一一吻合。代码是事实，文档是其精确投影。
2. **单一事实源**：每个事实只在**一处**权威记录（见 §10 权威层级）；他处引用、不复制。本网关的硬契约（API / config / DB / error-code / 不变量 / 行为）各有唯一权威文档，互不重叠。
3. **零历史包袱**：这是 from-scratch 重写。文档**只描述当前物理事实**，禁止「旧 `internal/` 树曾如何、后来拆成」的演化叙述（历史在 git 与 `archive/`；新旧映射只活在 ADR 的「取舍」节）。
4. **写 Why 不写 What**：What 看代码 / 结构即知；文档的价值在解释**为什么这样设计**、有哪些取舍与边界（如 provider rate card 先换算 pUSD 再原子预留、崩溃永远多计、admin 免鉴权靠物理回环）。
5. **高密度**：表格优先、要点优先、删一切 fluff（「本节将介绍…」之类）。本规范自身即范例。
6. **中文**：所有文档正文用**中文**；代码标识符、路径、wire code、frontmatter 字段名、GW-INV / ADR 编号保持原文。
7. **状态即重述（state 文档整体重述、非追加）**：描述「当前状态」的 `concept` 文档（`concepts/architecture.md` / `CLAUDE.md` / 本规范）每次变更**必须整体重述到当前事实**——改一个状态 = 重写相关部分，使全文读起来像「一直如此」，旧状态**不留痕迹**（历史在 git）。**绝不在旧内容旁追加**；只增不删 = 文档腐烂之源。两种更新 mode 不混：`reference` 文档按 **§1.1 精确同步**（投影代码），`concept` / state 文档按**本条整体重述**。

---

## 2. 文档类型（6 类）

每篇文档必须在 frontmatter 声明**唯一** `type`，它决定写入规则、审阅周期、淘汰协议。

| `type` | 用途 | 可变性 | 审阅周期 | 目录 |
|---|---|---|---|---|
| `concept` | 架构解释、设计理念、心智模型 | 随系统演进 | 季度 | `concepts/` |
| `reference` | 必须与代码精确一致的契约 / 规格 | **每次代码改动即同步** | 每次代码改动 | `references/` |
| `how-to` | 分步操作手册（部署 / 运维 / 本地起服务） | 流程变更时更新 | 半年 | `how-to/` |
| `decision` | ADR——为何选 X 不选 Y | **正文不可变**（只新建 supersede；旧篇只改 lifecycle metadata） | 永不 | `decisions/` |
| `log` | 时间序进度 / 决策日志 | **仅追加** | 永不 | `references/changelog.md` |
| `working` | 在研、临时、过程性（如本轮重写 SPEC） | 落地前活跃 | **90 天上限** | `working/` |

---

## 3. Frontmatter 标准

除 `INDEX.md`（入口，豁免）与 `archive/`（只读墓地，豁免）外，**每篇 `.md` 必须**以下列 frontmatter 开头：

```yaml
---
id: DOC-NNN          # 唯一编号，创建时分配（见 §4）
type: concept        # §2 六类之一
status: active       # draft | active | superseded | deprecated | archived
owner: @weilin
created: YYYY-MM-DD
reviewed: YYYY-MM-DD  # 最近一次人工审阅
review-due: YYYY-MM-DD # 下次到期审阅（= reviewed + 该类型周期）
audience: [human, ai]  # 读者：human / ai / 二者
superseded-by:        # status→superseded 时填，指向取代它的 DOC-id/路径
landed-into:          # 仅 working：结论提取进哪篇 concepts/references 后填
---
```

`status` 缺失或非法值、`type` 非六类之一、必填字段为空 → `make docs` 失败（§11）。

---

## 4. 命名与 ID

- **ID**：`DOC-NNN`，三位递增、全仓唯一、创建即分配、**永不复用**。`DOC-000` = 本规范。
- **文件名**：`kebab-case.md`。
  - `reference` 文档名 = 其对应的契约面（如 `references/backend/config.md` 对应全配置契约），便于 1:1 对照。
  - `decision` 文档：`NNNN-简短标题.md`（如 `0001-pessimistic-three-guardrail-reservation.md`），`NNNN` = ADR 序号、与 `DOC-NNN` 独立、与 SPEC 的 `ADR-NNN` 对齐。
- **目录归属由 `type` 决定**（§2 表末列），不得错放。

---

## 5. 目录地图（canonical）

网关是**确定性 capability 薄网关**：文本与受支持图片/视频同走 Qwen3.7 Plus，以 pUSD 成本账本统一护栏；共 7 个业务关注点（quota / install / chat / media / model / health / dashboard）+ React 仪表盘前端 + pkg 内核。

```
docs/
├── INDEX.md                 ← AI 会话入口（≤50 行，§11 强制）
├── GOVERNANCE.md            ← 本规范
├── concepts/
│   └── architecture.md      ← 双 provider + 4 层 + 3 监听器 + pUSD 记账心智模型
├── references/
│   ├── backend/             ← 现行硬契约（reference）
│   │   ├── overview.md     ← 后端总览与契约导航
│   │   ├── api.md          ← 三 mux HTTP + chat content/route
│   │   ├── config.md       ← 全配置层级与边界
│   │   ├── database.md     ← 15 张应用表（0001 的 8 表 + 0002 的 5 表 + 0005 的 2 表）+ schema_migrations
│   │   ├── error-codes.md  ← 全 wire code → status/message
│   │   └── invariants.md   ← GW-INV-NN 验收准绳
│   └── domains/             ← 保留的 reference 扩展位（当前仅 .gitkeep）
├── decisions/               ← ADR 正文仅追加；新 ADR supersede 旧 ADR
├── how-to/                  ← 操作手册（当前仅 .gitkeep）
├── working/                 ← 重写期 SPEC（已 landed/superseded，非现行事实源）
├── assets/                  ← 文档图片/图表资产
└── archive/                 ← 只读墓地（被取代 / 终止，豁免 frontmatter）
```

- 每个文件夹放且仅放其声明类型的文档；空文件夹用 `.gitkeep`（内含一行职责说明）占位。
- 新增类别 = 先在本 §5 + §2 登记，再建目录。
- `references/backend/` 的 6 篇现行硬契约（overview / api / config / database / error-codes / invariants）是 doc-code parity 的承重墙，§7 触发表逐条钉住它们。`database.md` 所述 15 张应用表中，v1 accounting 三表只读保留供审计/迁移，v2 五表是当前 provider-aware pUSD 账本，另有两张 install-bound 媒体 staging/lease 表。

---

## 6. 生命周期

```
draft → active → superseded → archived
              └→ deprecated → archived
```

| 状态 | 含义 | 规则 |
|---|---|---|
| `draft` | 起草中、未生效 | 非权威，不得被他处依赖 |
| `active` | 权威、单一事实源 | 唯一可被依赖的状态 |
| `superseded` | 被更新文档取代 | 填 `superseded-by`；通常移 `archive/`，仍被 `INDEX.md` 列为历史指针的 working 文档按 §9 处理 |
| `deprecated` | 主动淘汰中 | 标记后移 `archive/` |
| `archived` | 只读，住 `archive/` | **不可再改**；历史 / 参考用 |

`decision` 不走「superseded→改正文」——ADR 的决策内容不可变，被推翻时**新建**一篇 ADR，旧篇只更新 `status: superseded` 与 `superseded-by`。当前运行时的 capability/provider/pUSD 决策以 ADR-0017 为准；更早的 raw-token 账本与过时表数/`MODEL_ALLOWLIST` 结论均不再约束当前实现。

---

## 7. 同步触发表（★ doc-code parity 的执行点）

**改了左列代码 → 必须在同一提交更新右列文档。** 这是「文档=代码精确投影」落地为可机械执行的 if-then，已本地化到本网关的关注点（API / config / DB / error-code / 不变量 / 动态行为 / ADR / 前端 wire）：

| 代码改动 | 必须同步的文档 |
|---|---|
| 新增 / 改 HTTP 端点（business / admin / dashboard 任一 mux） | `references/backend/api.md`；若改契约导航/面边界，同步 `references/backend/overview.md` |
| 新增 / 改配置键或其边界 / 层级（runtime-hot / secret-env-only / startup-hard） | `references/backend/config.md` |
| 新增 / 改 DB 表、列、索引、迁移 | `references/backend/database.md` |
| 新增 / 改 error wire code（UPPER_SNAKE）或其 status/message | `references/backend/error-codes.md` |
| 新增 / 改一条不变量（记账原子性 / 鉴权 / provider 隔离 / 物理回环…） | `references/backend/invariants.md`（GW-INV-NN） |
| 改 capability 路由、provider/model/rate card、fallback 规则或 pUSD 账务边界 | 按投影面同步 `api.md` / `config.md` / `database.md` / `invariants.md`；新决策另建 ADR |
| 改动态行为 / 状态机（reserve→settle/rollback、provider breaker、PoW、节流、磁盘退化） | 同步承载该行为的 `references/backend/{overview,api,config,database,invariants}.md` 子集 |
| 改前端 wire（DTO / envelope / `error.code` 分支 / 金额字段 / CSRF·session） | `references/backend/api.md` + 被影响的 `config.md` / `database.md` / `error-codes.md` |
| 架构决策（选型 / 取舍 / 新增一条契约规则） | `decisions/` 新建一篇 ADR（NNNN 续号，对齐 SPEC 的 ADR-NNN） |
| 架构 / 分层 / 域 / 监听器 / 路线状态变更 | **整体重述** `concepts/architecture.md` 相关节（§1.7，非追加） |
| 工程规则 / 设计原则 / 依赖方向纪律变更 | **整体重述** `CLAUDE.md` 相关节（§1.7，非追加） |

**两种更新 mode 不混（§1.1 vs §1.7）**：`reference` 文档行 = **精确同步**（增量改到逐字吻合代码）；`architecture.md` / `CLAUDE.md` 行 = **整体重述**（把相关节重写到当前状态、删尽旧状态，绝不在旁追加）。表为高频清单、非穷举——「代码改了而某文档因此失真」一律适用。

---

## 8. 写作规约

- **语言**：正文中文；标识符 / 路径 / wire code / frontmatter 键 / GW-INV / ADR 编号保持原文。
- **密度**：表格 > 列表 > 段落。删除 meta 废话、礼貌性过渡、「显然」「众所周知」。
- **只写 Why**：解释设计动机、取舍、边界、坑（如「为何跨 provider 只能合并 pUSD」「为何 admin 监听器不鉴权」）。What（有哪些字段 / 端点 / 码）让结构自述或链向 reference。
- **零历史叙述 + 重述维护**：不写「旧 `internal/proxy` 原来…后来拆成…」，当前事实 only（演化进 git / `decisions/` / `archive/`；旧→新映射只在 ADR 取舍节）。维护 state 文档（architecture.md / CLAUDE.md / 本规范）时**整体重述、非追加**（§1.7）。
- **交叉引用**：用相对链接指向权威源（`[api.md](references/backend/api.md)`），**不复制**内容——复制即制造第二事实源、必然 drift。
- **删 / 移文档**：必须同时修掉所有指向它的链接（`INDEX.md` 及他处），不留孤儿链接（§11 校验）。

---

## 9. working 文档协议

`working/` 文档**最长 90 天**。本轮 from-scratch 重写 SPEC 即 working 文档的典型——它驱动 slice plan，落地后须沉淀进权威契约。落地时：

1. 把结论提取进对应的 `concepts/` 或 `references/` 文档（那才是权威源；后端契约提取进 `references/backend/*`，架构心智模型进 `concepts/architecture.md`，选型进 `decisions/`）。
2. frontmatter 填 `landed-into:` = 目标文档路径。
3. 置 `status: superseded`；若仍被 `INDEX.md` 显式列为历史参考，可暂留 `working/`，但不得再作现行事实源。
4. 不再需要历史指针时，先修正 `INDEX.md` 及所有链接，再 `git mv` 到 `archive/`。

超过 90 天且 `landed-into` 为空的 working 文档由 `make docs` 标记为错误。

---

## 10. 权威层级

文档冲突时，**高者胜**：

```
CLAUDE.md  >  references/  >  concepts/  >  working/  >  archive/
```

`CLAUDE.md` 是工程纪律最高法；`reference` 因「必须等于代码」而高于解释性的 `concept`；`working`（含重写 SPEC）一旦落地即被 `references/` 取代、降为非权威；`archive/` 最低（仅历史）。

---

## 11. 质量门禁（`make docs`）

`make docs`（跑 `cmd/docs`）是 `make verify` 的一环，机械强制：

1. 所有非 `archive/`、非 `INDEX.md` 的 `.md` 都有合法 frontmatter，且必填字段齐。
2. `type` ∈ §2 六类；`status` ∈ §6 五态；除豁免项外的 type↔目录归属与 §2/§5 一致。
3. `review-due` 已过期 → **警告**（不阻断）。
4. `working/` 文档 >90 天且 `landed-into` 空 → **失败**。
5. `INDEX.md` ≤ 50 行。
6. 无孤儿链接（文档内相对链接指向的文件都存在）。

> **覆盖**：`cmd/docs`（`make docs`，并入 `make verify`）机械强制上列 1–6。ADR 正文不可变需比对 git 历史，尚未机械化，靠 §12 收尾清单 #6 + `decisions/` 目录纪律守。§12 是人工前置层，与门禁并行。

---

## 12. Claude 收尾清单（★★ 每次代码改动「完成」前逐条勾）

声明任何代码改动**完成**之前，逐条自检——任一项未过 = 改动**未完成**，回去补：

1. ☐ 这次改动碰了 §7 触发表里的东西吗（API / config / DB / error-code / 不变量 / 行为 / provider·rate-card·pUSD 账务 / 前端 wire / 架构决策 / 架构·域·监听器状态 / 工程规则）？→ 对应文档**同一提交**更新了吗？
2. ☐ 改的是 `reference` 文档吗？它和代码**逐字**对得上吗（端点 / 配置键 / 表列 / wire code / GW-INV 编号 一一吻合）？
3. ☐ 改的是**状态文档**（`concepts/architecture.md` / `CLAUDE.md` / 本规范）吗？→ 是**整体重述到当前状态**吗（没在旧内容旁追加、没留旧状态痕迹，§1.7）？
4. ☐ 新建文档有合法 frontmatter（§3）吗？`type` / `status` / `id` 对吗？放对目录（§5）了吗？
5. ☐ 删 / 移过文档吗？→ 所有指向它的链接（`INDEX.md` 及他处）都修了吗（无孤儿链接）？
6. ☐ 动过 `decisions/` 里的 ADR 正文吗？→ **禁止**（只能新建 ADR supersede；旧 ADR 只可更新 lifecycle metadata）。
7. ☐ working 文档（含重写 SPEC）落地了吗（提取进 concepts/references + 填 `landed-into` + 置 `superseded`；无历史指针后移 `archive/`）？

---

## 13. 修改本规范

本规范是 `concept` 文档，随项目演进。改它需：① 更新 `reviewed` / `review-due`；② 若改了执行机制（§0 / §7 / §12），**同步更新 `CLAUDE.md` 的「文档纪律」节**——二者必须一致，否则常驻层与规范层冲突、执行即失效。
