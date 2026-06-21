---
id: DOC-017
type: decision
status: active
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2099-12-31
audience: [human, ai]
---

# 0008 — 采用 Foryx 文档治理模型

## 背景 / Context

本网关从零重写，文档需与代码逐字同步、ADR 不可变、AI 会话有稳定入口。同仓 Foryx 平台已有一套成熟的 `docs/GOVERNANCE.md` 治理模型（6 类文档、frontmatter、doc-code parity、`make docs` 门禁、`decisions/` ADR 不可变）。与其另起一套，不如本地化复用——治理纪律本身是跨项目可迁移的。

## 决策 / Decision

**采用 Foryx `docs/GOVERNANCE.md` 模型，本地化到本网关。**

1. **6 类文档**：`concept` / `reference` / `how-to` / `decision` / `log` / `working`，各有目录、可变性、审阅周期。
2. **frontmatter 强制**：每篇非 `INDEX.md`/`archive/` 的 `.md` 带合法 frontmatter（`id: DOC-NNN`、`type`、`status`、`owner`、`created`、`reviewed`、`review-due`、`audience`）。
3. **doc-code parity**：`reference` 文档与代码逐字对得上；改代码即同提交同步对应文档（同步触发表）。
4. **`make docs` 门禁**：机械校验 frontmatter/必填/类型·状态/INDEX≤50 行/无孤儿链接，并入 `make verify`。
5. **ADR 不可变**：`decisions/` 下 ADR 创建后**绝不编辑**，被推翻时新建一篇 supersede、旧篇置 `superseded` + `superseded-by`。
6. **写作规约**：正文中文（标识符/路径/wire code/frontmatter 键保持原文）、高密度（表 > 列表 > 段落）、写 Why 不写 What、零历史叙述、单一事实源。

本网关的 DOC-id 分配：`DOC-000`=GOVERNANCE，`DOC-001`=architecture，`DOC-010..DOC-020`=本 11 篇 ADR。

## 理由 / Rationale

- Foryx 模型已被验证、机械可执行（门禁兜底），直接迁移省去自造治理的成本与磨合。
- ADR 不可变 + 单一事实源是「零历史包袱」的制度保证：演化进 git/archive，活文档只述当前事实。

## 取舍与后果 / Consequences

**为何不选：**

- **不做文档治理 / 临时 README**：薄网关单运营者诚然可暂缓（spec over-engineering 备注亦点出此为可 defer 项）——但从零重写正是建立纪律的最低成本时点，且 AI 协作强依赖稳定入口 + parity 门禁。本决策接受这一前置投入。
- **自造一套治理**：重复造轮、与同仓 Foryx 纪律分叉，跨项目认知成本翻倍。

**后果：**

- 建立 `docs/{concepts,references,decisions,how-to,working,archive}` 目录树 + `INDEX.md`（≤50 行 AI 入口）+ 本地化 `GOVERNANCE.md`。
- `make docs`（`cmd/docs`）并入 `make verify`，违规无法静默通过。
- 本 ADR 自身遵守它采纳的规则（合法 frontmatter、不可变）。

## 相关 / Links

- [GOVERNANCE](../GOVERNANCE.md) · [架构](../concepts/architecture.md)
