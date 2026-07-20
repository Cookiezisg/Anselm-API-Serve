# Anselm Gateway

English · [简体中文](README.zh-CN.md)

A single-binary Go + SQLite gateway that exposes one OpenAI-compatible model while deterministically routing to two fixed upstreams: pure text goes to DeepSeek V4 Flash, and supported inline images/videos go to Kimi K2.6. Audio has a validated public protocol but no deployed route yet. Provider keys stay on the server, and pessimistic cost accounting prevents the operator's dollar budget from being oversold. It was built for the Anselm desktop app, but it is self-contained.

It does three things:

1. **Route** — OpenAI-compatible `/v1/chat/completions` (streaming and non-streaming, with `tools`/`tool_choice` for multi-turn tool calls). The complete message history determines the provider; the client cannot choose one, and there is no cross-provider fallback.
2. **Meter** — before each upstream call it converts the exact provider/model rate card into cost, then atomically reserves monthly count plus install, provider, and global daily spend. Successful calls settle against reported usage; ambiguous failures retain a conservative charge.
3. **Rate-limit** — anonymous install tokens (stored only as SHA-256), plus optional Proof-of-Work and rate-limit gates that are off by default.

The code uses clean architecture (domain / app / infra / transport, plus a bootstrap composition root), with the dependency direction enforced by golangci-lint (depguard). There are tests across all four layers plus a loopback end-to-end suite; CI runs race tests, lint, govulncheck, gofmt, and a check that the embedded dashboard build matches its source.

## Cost accounting

The four guardrails — monthly request count, per-install daily spend, per-provider daily spend, and global daily spend — are reserved in one `BEGIN IMMEDIATE` transaction on a single-writer pool. Token vectors from different providers are never added together: each request is priced with its frozen exact-model rate card, then only integer pico-US-dollar cost enters the shared wallets.

A rejection before the request can reach a provider rolls the reservation back. Once the request has been handed to a provider, missing usage, a timeout, a disconnect, or a crash keeps the conservative reservation; complete usage settles to the calculated cost. Ledger transitions are compare-and-swap and idempotent, so the failure mode is over-charging rather than silently spending operator money without accounting for it.

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
TOKEN=$(curl -s -XPOST localhost:8080/v1/install \
  -H 'Content-Type: application/json' -d '{}' | jq -r .token)

curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"anselm-auto","stream":true,
       "messages":[{"role":"user","content":"hello"}]}'

curl -s localhost:8080/v1/quota -H "Authorization: Bearer $TOKEN" | jq
```

## Deterministic routing

Clients see one logical model, `anselm-auto`; `GET /v1/models` and the top-level `model` in streaming/non-streaming completions always return that ID. The client-supplied `model` never selects a provider:

- String content, or content arrays containing only `text` parts, routes to `deepseek-v4-flash`.
- Any accepted image or video part anywhere in the complete history routes to `kimi-k2.6`.
- A valid `input_audio` part is accepted by the public protocol but returns `503 AUDIO_UNAVAILABLE` before routing or billing, until an audio-capable upstream is deployed.

Inline media is intentionally strict and allowed only in `user` messages. Images use an `image_url` base64 data URI for JPEG, PNG, or WebP; videos use a `video_url` base64 data URI for MP4; audio uses an `input_audio` object with strict raw base64 `data` and a MIME-matched `wav` or `mp3` `format`. Remote URLs, PDFs, files, unknown part types, MIME/magic mismatches, and media beyond the configured part/decoded-byte limits are rejected instead of forwarded. There is no fallback between providers. If `KIMI_API_KEY` is absent, text remains available and accepted image/video requests return `503 MULTIMODAL_UNAVAILABLE`.

The gateway owns the simple product tier for model choice and reasoning behavior. Thinking is always enabled: text requests use DeepSeek `thinking.enabled` with `reasoning_effort=high`; media requests use Kimi `thinking.enabled` and no `reasoning_effort`. Client-supplied `thinking` and `reasoning_effort` do not change this tier. Caller request knobs such as `max_tokens` remain OpenAI-compatible passthrough fields: a positive `max_tokens` is forwarded after clamping to `MAX_TOKENS_CAP` and the selected model's hard output limit, while an absent value is omitted on the wire and reserved conservatively for accounting. Text has DeepSeek's 1M input context; media has Kimi's 262K input context, so a single product-facing context number should be the conservative 256K.

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
| `GET` | `/v1/models` | Bearer token | OpenAI `{object:"list", data:[…]}` containing the single public model ID |
| `GET` | `/v1/quota` | Bearer token | `{limit, used, remaining, resetAt, available}` |
| `GET` | `/healthz` | none | Process liveness; does not touch DB or upstream |

Admin (`127.0.0.1:9090`, loopback-only, not proxied): `/metrics`, `/readyz` (`{db, upstream, disk}`), `/debug/pprof/*`, `/debug/vars`.
Dashboard (`127.0.0.1:8081`, loopback): `/login`, `/logout`, and session-protected `/api/*`.

Responses are bare entities on success and `{"error":{"code","message"}}` on failure. Successful completion bodies are relayed only after strict validation and public-model alias rewriting; raw upstream error bodies/headers and provider keys are never passed through. Full contract: [`docs/references/backend/api.md`](docs/references/backend/api.md).

## Configuration

Loading order is env defaults, then a `settings`-table DB overlay (runtime-editable knobs can be changed from the dashboard). Full surface: [`.env.example`](.env.example) and [`docs/references/backend/config.md`](docs/references/backend/config.md).

Secrets are env-only and are not persisted, dumped, or logged: `DEEPSEEK_API_KEY` (required, comma-separated for multiple keys), `KIMI_API_KEY` (optional, comma-separated; omitting it disables only image/video requests), `DASHBOARD_USER`/`DASHBOARD_PASSWORD` (paired, optional), and `INSTALL_POW_SECRET` (required only if PoW is enabled).

The public/provider model IDs are `PUBLIC_MODEL_ID=anselm-auto`, `TEXT_UPSTREAM_MODEL=deepseek-v4-flash`, and `MULTIMODAL_UPSTREAM_MODEL=kimi-k2.6`. Spend limits use integer microUSD (`1,000,000 = US$1`): `GLOBAL_DAILY_SPEND_MICRO_USD=14000000`, `INSTALL_DAILY_SPEND_MICRO_USD=5600000`, `DEEPSEEK_DAILY_SPEND_MICRO_USD=14000000`, and `KIMI_DAILY_SPEND_MICRO_USD=14000000` in the production example. It uses a 5 MiB request body with at most 8 inline media parts / 3 MiB decoded media. Other main guardrails include `MONTHLY_QUOTA`, `MAX_TOKENS_CAP` / `INPUT_TOKEN_CAP`, `N_GLOBAL_CONCURRENCY`, and `RATE_PER_MIN`. The example file keeps optional anti-abuse gates dormant for local development; the committed production deployment enables bounded input/output and install/request rate gates.

## Deployment

Caddy + systemd on a VPS:

- Caddy terminates TLS and reverse-proxies the API hostname to `127.0.0.1:8080` (SSE flushed). The root hostname can serve a static trust page from `deploy/site/`. The Go process binds `127.0.0.1` only and does not face the public internet directly.
- systemd socket activation normally holds the `:8080` fd across ordinary service restarts. A release intentionally stops Caddy, the socket, and the service before snapshot/migration, so there is a short fail-closed maintenance interval and no request can mutate SQLite between snapshot and commit.
- A successful repository-owned `ci` run for a push to `main` admits the exact `head_sha` to the deployment workflow; a failed, incomplete, or stale (no longer the `main` tip) CI run cannot deploy. The deployment job checks out that immutable SHA and repeats the release-critical gates (gofmt/module verification/vet/build, race unit and integration e2e tests, parser fuzz smoke, accounting coverage floors, docs governance, rollback shell tests, high-severity npm audit plus embedded-dashboard rebuild/drift check, golangci-lint, and govulncheck) before building static `linux/amd64`. It then transfers every artifact into an unpredictable remote `0700` stage → verifies an exact regular-file set and SHA-256 manifest → takes a stopped-writer SQLite main/WAL/SHM snapshot → installs and runs loopback gates → durably commits locally → reopens Caddy. Any failure before that commit point automatically restores DB, binary/symlink, env, units, Caddy, the static site, and the previous global rollback entry (including true absence on first deploy) as one compatibility unit. A root-filesystem transition marker plus a permanent systemd Caddy condition keeps ingress closed across process death or reboot; a Caddy start failure after commit never triggers a potentially lossy DB rewind.
- Production requires a pinned `SERVER_KNOWN_HOSTS` GitHub Environment secret; deployment fails closed when it is absent or has no entry for `SERVER_HOST`. There is no `ssh-keyscan`/TOFU fallback. The remote data directory is `0700`, DB/WAL/SHM and secret env are `0600`, and the successful release retains one root-only rollback bundle.
- A schema-aware manual rollback is installed as `sudo /usr/local/sbin/anselm-gateway-rollback` (interactive confirmation) or `sudo /usr/local/sbin/anselm-gateway-rollback --yes` (automation). If a host/process crash leaves the persistent transition marker, recover the exact checksummed bundle named there. The most reliable entry is always `sudo <bundle-from-marker>/recovery/rollback.sh --recover-incomplete` (add `--yes` non-interactively); the global command supports the same mode when it has already been upgraded. Every bundle carries its exact recovery program, while rollback restores the previous global entry so it cannot drift from an older retained READY bundle. The inert Caddy guard remains a managed safety artifact. Rollback restores the retained DB snapshot and all matching runtime artifacts; switching the binary symlink alone is unsupported and unsafe after a schema migration.

The deployment target (domain, ACME email) is injected from GitHub secrets and is not committed. Production defaults include `INPUT_TOKEN_CAP=131072`, `MAX_TOKENS_CAP=16384`, `MAX_MESSAGES=1024`, `MAX_MESSAGE_CHARS=262144`, `RATE_PER_MIN=8`, `DAILY_SUBLIMIT=100`, `INSTALL_GLOBAL_DAILY_CAP=100`, `INSTALL_PER_FP_DAILY=0`, `INSTALL_PER_FP_COOLDOWN_SEC=0` (both per-device install gates temporarily disabled during debugging), `INSTALL_PER_IP_HOUR=10`, and `TOKEN_ANOMALY_RPM=8`. See [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) and [`deploy/`](deploy/).

## Development

```sh
make verify    # vet + build + test -race + docs (local gate)
make lint      # golangci-lint v2.6.1 (includes the depguard layering check)
make docs      # documentation governance gate
```

Tests cover all four layers plus a loopback full-stack e2e (`internal/e2e`, `integration` tag). CI enforces coverage floors on the accounting packages (`app/quota` ≥ 70%, `app/chat` ≥ 65%) and runs govulncheck, gofmt, fuzz smoke, an SBOM step, and the dashboard-build drift check.

## Status and scope

It runs in production on `main`; the dashboard is wired and served, and single- and multi-turn tool use are tested end to end.

This is a thin gateway by design: two fixed content-shape routes, one operator's shared budget, and a single-node SQLite database with a single-writer pool (which is what makes atomic accounting work). It is not a general model router, a multi-tenant billing system, or a horizontally scalable service; the admin surface is loopback-only. It was written for one product, so reusing it elsewhere would take some adaptation.

## Documentation

`docs/` is a governed documentation set; the entry point is [`docs/INDEX.md`](docs/INDEX.md):

- [`concepts/architecture.md`](docs/concepts/architecture.md) — the system model
- [`references/backend/`](docs/references/backend/) — contracts kept in sync with the code (api / config / database / error-codes / invariants)
- [`decisions/`](docs/decisions/) — ADR-001..012 (design decisions, kept immutable)

## License

[MIT](LICENSE).
