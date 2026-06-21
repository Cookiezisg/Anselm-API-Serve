# AGENTS.md

> 跨工具的 AI 协作入口（[agents.md](https://agents.md) 约定）。本仓的工程纪律有**唯一事实源**，本文件只做指引、不放内容，避免又长成一份过时文档。

## 先读这三处

1. **[`CLAUDE.md`](CLAUDE.md)** —— 工程纪律最高法:三条铁律、依赖方向、不可破红线、门禁命令。**动代码前必读**。
2. **[`docs/INDEX.md`](docs/INDEX.md)** —— 文档体系会话入口(架构心智模型、逐字硬契约、ADR)。
3. **[`docs/references/backend/invariants.md`](docs/references/backend/invariants.md)** —— GW-INV-NN 不变量登记册,是一切改动的验收准绳。

## 30 秒定位

- 模块 `github.com/sunweilin/anselm/gateway`;入口 `cmd/gateway` → 组合根 `internal/bootstrap`。
- Clean Arch:`internal/{domain,app,infra,transport,bootstrap,pkg}`;依赖只能指向内层(depguard 强制)。
- 三监听器:业务 `:8080`(公网,经 Caddy)· admin `:9090`(loopback)· dashboard `:8081`(loopback)。
- 正确性优先:记账永不超卖、key 永不出端、admin 面绝不公网。

## 完成的定义

`go build ./... && go vet ./... && gofmt -l(空) && go test -race ./...` 全绿 + 相关 GW-INV 守住 + **同一提交内同步对应文档**(doc-code parity)。本地一键:`make verify`。
