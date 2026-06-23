# Anselm Gateway

English · [简体中文](README.zh-CN.md)

A single-binary Go + SQLite gateway that fronts one LLM upstream (DeepSeek) behind an OpenAI-compatible API. The upstream API key stays on the server, and usage is metered with pessimistic quota accounting so the operator's budget is not oversold. It was built for the Anselm desktop app, but it is self-contained.

It does three things:

1. **Proxy** — OpenAI-compatible `/v1/chat/completions` (streaming and non-streaming, with `tools`/`tool_choice` passed through for multi-turn tool calls). The upstream key is injected on a request clone and is never returned to clients.
2. **Meter** — before each upstream call it reserves against three guardrails (monthly request count / per-install daily token cap / global daily budget) in one SQLite transaction, then settles against real usage. A failure rolls back; a crash over-charges rather than under-charges.
3. **Rate-limit** — anonymous install tokens (stored only as SHA-256), plus optional Proof-of-Work and rate-limit gates that are off by default.

The code uses clean architecture (domain / app / infra / transport, plus a bootstrap composition root), with the dependency direction enforced by golangci-lint (depguard). There are tests across all four layers plus a loopback end-to-end suite; CI runs race tests, lint, govulncheck, gofmt, and a check that the embedded dashboard build matches its source.

## Quota accounting

![Quota accounting flow: a chat request snapshots the billing period once, then reserves against three guardrails in one BEGIN IMMEDIATE transaction. If all three pass it forwards to upstream and settles to real usage at the first byte; if any guardrail fails it rolls back to 429/402, and a failure before the first byte rolls back all three through one defer. A crash leaves a settled-IS-NULL row that a reconciler refunds.](docs/assets/quota-accounting.svg)

The three guardrails — `monthly count < quota`, `install daily tokens + estimate ≤ cap`, `global daily budget + estimate ≤ budget` — are checked in one `BEGIN IMMEDIATE` transaction on a single-writer pool, so concurrent requests do not race the read-modify-write. Billing happens once, at the upstream's first byte. Anything that fails before the first byte rolls back all three reservations through one defer. A crash leaves a `settled IS NULL` row that a reconciler refunds, so the failure mode is over-charging, not under-charging.

## Architecture

![Architecture and dependency direction: cmd depends on bootstrap, which depends on transport and infra; transport depends on app, which depends on domain; infra also depends on domain; and transport, app, infra, and domain all depend on the pkg leaf kernel. Dependencies point inward toward more stable layers, enforced by depguard.](docs/assets/architecture.svg)

Dependencies point inward toward more stable layers, and the build fails on a violation (depguard): `domain` has no infra imports, `app` declares the infra ports it needs as interfaces, `*sql.Tx` does not leave `infra`, and `bootstrap` is the only package that imports across layers (nothing imports it). There are three separate listeners: a public business API, a loopback-only admin/metrics/pprof surface, and a loopback dashboard.

## Quickstart

```sh
cp .env.example .env          # set at least DEEPSEEK_API_KEY
set -a; source .env; set +a   # (bash/zsh; fish: see .env.example)
make run                      # = go run ./cmd/gateway
curl -s localhost:8080/healthz   # → {"status":"ok"}
```

End to end (claim a token, then use it like any OpenAI-compatible endpoint):

```sh
TOKEN=$(curl -s -XPOST localhost:8080/v1/install | jq -r .token)

curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","stream":true,
       "messages":[{"role":"user","content":"hello"}]}'

curl -s localhost:8080/v1/quota -H "Authorization: Bearer $TOKEN" | jq
```

`model` is rewritten to the first entry of `MODEL_ALLOWLIST`, so clients can send any model name; `GET /v1/models` returns the actual list.

## Dashboard

An embedded React SPA (overview, config editing, install ban/audit, DB export), served from the binary behind session + CSRF auth on a loopback port. It starts only when `DASHBOARD_USER` and `DASHBOARD_PASSWORD` are set, and it is **not exposed to the public internet** — reach it over an SSH tunnel:

```sh
ssh -L 8081:127.0.0.1:8081 <user>@<server>   # then open http://localhost:8081
```

<!-- Add a screenshot at docs/assets/dashboard.png and uncomment: -->
<!-- ![Anselm Gateway dashboard](docs/assets/dashboard.png) -->

## API

Business surface (`127.0.0.1:8080`, public behind Caddy):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/install` | none | Issue an install token (`{token, monthlyQuota, resetAt}`); shown once |
| `POST` | `/v1/chat/completions` | Bearer token | OpenAI-compatible inference; SSE or JSON per `stream` |
| `GET` | `/v1/models` | Bearer token | OpenAI `{object:"list", data:[…]}` from the live allowlist; read-only |
| `GET` | `/v1/quota` | Bearer token | `{limit, used, remaining, resetAt, available}` |
| `GET` | `/healthz` | none | Process liveness; does not touch DB or upstream |

Admin (`127.0.0.1:9090`, loopback-only, not proxied): `/metrics`, `/readyz` (`{db, upstream, disk}`), `/debug/pprof/*`, `/debug/vars`.
Dashboard (`127.0.0.1:8081`, loopback): `/login`, `/logout`, and session-protected `/api/*`.

Responses are bare entities on success and `{"error":{"code","message"}}` on failure; the upstream body and key are not passed through. Full contract: [`docs/references/backend/api.md`](docs/references/backend/api.md).

## Configuration

Loading order is env defaults, then a `settings`-table DB overlay (runtime-editable knobs can be changed from the dashboard). Full surface: [`.env.example`](.env.example) and [`docs/references/backend/config.md`](docs/references/backend/config.md).

Secrets are env-only and are not persisted, dumped, or logged: `DEEPSEEK_API_KEY` (required, comma-separated for multiple keys), `DASHBOARD_USER`/`DASHBOARD_PASSWORD` (paired, optional), `INSTALL_POW_SECRET` (required only if PoW is enabled). Main guardrails: `GLOBAL_DAILY_BUDGET_TOKENS`, `INSTALL_DAILY_TOKEN_CAP` (must be ≤ the global budget), `MONTHLY_QUOTA`, `MAX_TOKENS_CAP` / `INPUT_TOKEN_CAP`, `N_GLOBAL_CONCURRENCY`, `RATE_PER_MIN`. The anti-abuse gates (`INSTALL_GLOBAL_DAILY_CAP`, `TOKEN_ANOMALY_RPM`, `INSTALL_POW_MODE`, …) default to `0`/`off`.

## Deployment

Caddy + systemd on a VPS:

- Caddy terminates TLS and reverse-proxies `<your-domain> → 127.0.0.1:8080` (SSE flushed). The Go process binds `127.0.0.1` only and does not face the public internet directly.
- systemd socket-activation holds the `:8080` fd so connections survive a restart.
- GitHub Actions on push to `main`: `vet + test -race` → static `linux/amd64` build → versioned binary + atomic symlink → a deploy gate (loopback `readyz`/`healthz`, with auto-rollback to the previous version on failure) → keep the last 5 versions.

The deployment target (domain, ACME email) is injected from GitHub secrets and is not committed. See [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) and [`deploy/`](deploy/).

## Development

```sh
make verify    # vet + build + test -race + docs (local gate)
make lint      # golangci-lint v2.6.1 (includes the depguard layering check)
make docs      # documentation governance gate
```

Tests cover all four layers (~48 `_test.go`) plus a loopback full-stack e2e (`internal/e2e`, `integration` tag). CI enforces coverage floors on the accounting packages (`app/quota` ≥ 70%, `app/chat` ≥ 65%) and runs govulncheck, gofmt, fuzz smoke, an SBOM step, and the dashboard-build drift check.

## Status and scope

It runs in production on `main`; the dashboard is wired and served, and single- and multi-turn tool use are tested end to end.

This is a thin gateway by design: one upstream (DeepSeek), one operator's budget, and a single-node SQLite database with a single-writer pool (which is what makes the atomic accounting work). It is not a multi-tenant LLM router, not a billing system, and not horizontally scalable; the admin surface is loopback-only. It was written for one product, so reusing it elsewhere would take some adaptation.

## Documentation

`docs/` is a governed documentation set; the entry point is [`docs/INDEX.md`](docs/INDEX.md):

- [`concepts/architecture.md`](docs/concepts/architecture.md) — the system model
- [`references/backend/`](docs/references/backend/) — contracts kept in sync with the code (api / config / database / error-codes / invariants)
- [`decisions/`](docs/decisions/) — ADR-001..011 (design decisions, kept immutable)

## License

[MIT](LICENSE).
