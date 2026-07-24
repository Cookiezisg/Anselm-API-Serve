# Anselm Gateway

English · [简体中文](README.zh-CN.md)

A single-binary Go + SQLite gateway that exposes one OpenAI-compatible model while deterministically routing to two fixed upstreams: pure text goes to DeepSeek V4 Flash, and supported inline images/videos go to Qwen3.7 Plus. Audio has a validated public protocol but no deployed route yet. Provider keys stay on the server, and pessimistic cost accounting prevents the operator's dollar budget from being oversold. It was built for the Anselm desktop app, but it is self-contained.

It does three things:

1. **Route** — OpenAI-compatible `/v1/chat/completions` (streaming and non-streaming, with `tools`/`tool_choice` for multi-turn tool calls). The complete message history determines the provider; the client cannot choose one, and there is no cross-provider fallback.
2. **Meter** — before each upstream call it converts the exact provider/model rate card into cost, then atomically reserves the per-install monthly request count plus the operator global monthly spend budget. Successful calls settle against reported usage; ambiguous failures retain a conservative charge.
3. **Bind and rate-limit** — every call proves possession of an installation Ed25519 key; copied ids and captured requests are unusable. Optional Proof-of-Work and issuance gates add Sybil cost.

The code uses clean architecture (domain / app / infra / transport, plus a bootstrap composition root), with the dependency direction enforced by golangci-lint (depguard). There are tests across all four layers plus a loopback end-to-end suite; CI runs race tests, lint, govulncheck, gofmt, and a check that the embedded dashboard build matches its source.

## Cost accounting

The two request-denying guardrails — per-install monthly request count and operator global monthly spend — are reserved in one `BEGIN IMMEDIATE` transaction on a single-writer pool. Token vectors from different providers are never added together: each request is priced with its frozen exact-model rate card, then only integer pico-US-dollar cost enters the shared wallet. Daily install/provider/global tables remain as accounting statistics, not traffic gates.

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

End-to-end inference intentionally is not a copyable curl token flow. The Anselm sidecar registers
an Ed25519 public key, keeps the encrypted private seed on the device, obtains a short-lived
challenge, and signs the method, authority, target, and exact body of every request. See
[ADR-0015](docs/decisions/0015-device-bound-request-proof.md) for the compact proof contract.

## Deterministic routing

Clients see one logical model, `anselm-auto`; `GET /v1/models` and the top-level `model` in streaming/non-streaming completions always return that ID. The client-supplied `model` never selects a provider:

- String content, or content arrays containing only `text` parts, routes to `deepseek-v4-flash`.
- Any accepted image or video part anywhere in the complete history routes to `qwen3.7-plus`.
- A valid `input_audio` part is accepted by the public protocol but returns `503 AUDIO_UNAVAILABLE` before routing or billing, until an audio-capable upstream is deployed.

Inline media is intentionally strict and allowed only in `user` messages. Images use an `image_url` base64 data URI for JPEG, PNG, or WebP; videos use a `video_url` base64 data URI for MP4; audio uses an `input_audio` object with strict raw base64 `data` and a MIME-matched `wav` or `mp3` `format`. Remote URLs, PDFs, files, unknown part types, MIME/magic mismatches, and media beyond the configured part/decoded-byte limits are rejected instead of forwarded. There is no fallback between providers. `DASHSCOPE_API_KEY` and `DASHSCOPE_WORKSPACE_ID` are required for the Qwen visual route; an incomplete deployment fails at startup instead of silently dropping a product capability.

The gateway owns the simple product tier for model choice and reasoning behavior. Thinking is always enabled: text requests use DeepSeek `thinking.enabled` with `reasoning_effort=high`; media requests use Qwen's top-level `enable_thinking=true`. Client-supplied `thinking` and `reasoning_effort` do not change this tier. A positive `max_tokens` is clamped to `MAX_TOKENS_CAP` and the selected model's output limit; an absent or non-positive value is normalized to that same explicit product cap, so wire behavior, accounting, and client context headroom agree. Both text and visual routes expose 1M input context; Qwen3.7 Plus exposes a 64K output ceiling. `GET /v1/models` publishes both route profiles in the namespaced `anselm_capabilities` extension, allowing the desktop agent to choose the budget dynamically instead of pretending one conservative number describes both routes.

## Dashboard

An embedded React SPA (overview, config editing, install ban/audit, DB export), served from the binary on a loopback-only port. `DASHBOARD_AUTH_MODE=builtin` keeps the built-in session + CSRF login and is reachable over SSH; `external` delegates the entire login wall to a preceding IAP such as Cloudflare Access, while Go still listens only on `127.0.0.1:8081`.

```sh
ssh -L 8081:127.0.0.1:8081 <user>@<server>   # then open http://localhost:8081
```

<!-- Add a screenshot at docs/assets/dashboard.png and uncomment: -->
<!-- ![Anselm Gateway dashboard](docs/assets/dashboard.png) -->

## API

Business surface (`127.0.0.1:8080`, public behind Caddy):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/install` | registration proof | Register a device public key; return `{installId, monthlyQuota, resetAt}` |
| `GET` | `/v1/proof/challenge` | none | Issue a cacheable five-minute request nonce |
| `POST` | `/v1/chat/completions` | device proof | OpenAI-compatible inference; SSE or JSON per `stream` |
| `GET` | `/v1/models` | device proof | OpenAI model list with one public ID plus route-specific `anselm_capabilities` |
| `GET` | `/v1/quota` | device proof | `{limit, used, remaining, resetAt, available}` |
| `GET` | `/healthz` | none | Process liveness; does not touch DB or upstream |

Admin (`127.0.0.1:9090`, loopback-only, not proxied): `/metrics`, `/readyz` (`{db, upstream, disk}`), `/debug/pprof/*`, `/debug/vars`.
Dashboard (`127.0.0.1:8081`, loopback): `GET /api/bootstrap` selects `builtin` or `external`; builtin additionally exposes `/login`, `/logout`, and session-protected `/api/*`, while external trusts the preceding IAP for direct `/api/*` access.

Responses are bare entities on success and `{"error":{"code","message"}}` on failure. Successful completion bodies are relayed only after strict validation and public-model alias rewriting; raw upstream error bodies/headers and provider keys are never passed through. Full contract: [`docs/references/backend/api.md`](docs/references/backend/api.md).

## Configuration

Loading order is env defaults, then a `settings`-table DB overlay (runtime-editable knobs can be changed from the dashboard). Full surface: [`.env.example`](.env.example) and [`docs/references/backend/config.md`](docs/references/backend/config.md).

Secrets are env-only and are not persisted, dumped, or logged: `DEEPSEEK_API_KEY` and `DASHSCOPE_API_KEY` (both required; each supports comma-separated keys), `DASHBOARD_USER`/`DASHBOARD_PASSWORD` (required only in `DASHBOARD_AUTH_MODE=builtin`), and `INSTALL_POW_SECRET` (required only if PoW is enabled). `DASHSCOPE_WORKSPACE_ID` is a non-secret endpoint identifier used to derive the Singapore Model Studio endpoint. `DASHBOARD_AUTH_MODE` itself is a non-secret, env-only startup trust-boundary choice: `disabled` (default), `builtin`, or `external`.

The public/provider model IDs are `PUBLIC_MODEL_ID=anselm-auto`, `TEXT_UPSTREAM_MODEL=deepseek-v4-flash`, and `MULTIMODAL_UPSTREAM_MODEL=qwen3.7-plus`. Spend limits use integer microUSD (`1,000,000 = US$1`): the production example sets `GLOBAL_MONTHLY_SPEND_MICRO_USD=420000000` ($420/month). Production accepts an 8 MiB request body with at most 8 inline media parts / 3 MiB decoded media. The request-denying usage guardrails are the per-install `MONTHLY_QUOTA=5000` and the operator global monthly spend budget; structural body/message/media limits and `N_GLOBAL_CONCURRENCY` remain service-safety guardrails. The conservative UTF-8 prompt estimate is accounting evidence only and never a context-admission gate; the selected upstream is the hard input-limit authority.

## Deployment

Caddy + systemd on a VPS:

- Caddy terminates TLS and reverse-proxies the API hostname to `127.0.0.1:8080` (SSE flushed). The root hostname can serve a static trust page from `deploy/site/`. The Go process binds `127.0.0.1` only and does not face the public internet directly.
- systemd socket activation normally holds the `:8080` fd across ordinary service restarts. A release intentionally stops Caddy, the socket, and the service before snapshot/migration, so there is a short fail-closed maintenance interval and no request can mutate SQLite between snapshot and commit.
- A successful repository-owned `ci` run for a push to `main` admits the exact `head_sha` to the deployment workflow; a failed, incomplete, or stale (no longer the `main` tip) CI run cannot deploy. The deployment job checks out that immutable SHA and repeats the release-critical gates (gofmt/module verification/vet/build, race unit and integration e2e tests, parser fuzz smoke, accounting coverage floors, docs governance, rollback shell tests, high-severity npm audit plus embedded-dashboard rebuild/drift check, golangci-lint, and govulncheck) before building static `linux/amd64`. It then transfers every artifact into an unpredictable remote `0700` stage → verifies an exact regular-file set and SHA-256 manifest → takes a stopped-writer SQLite main/WAL/SHM snapshot → installs and runs loopback gates → durably commits locally → reopens Caddy. Any failure before that commit point automatically restores DB, binary/symlink, env, units, Caddy, the static site, and the previous global rollback entry (including true absence on first deploy) as one compatibility unit. A root-filesystem transition marker plus a permanent systemd Caddy condition keeps ingress closed across process death or reboot; a Caddy start failure after commit never triggers a potentially lossy DB rewind.
- A pre-launch clean break is deliberately an explicit manual action, not normal deploy behavior: dispatch `deploy` from the current `main` tip with `reset_unlaunched_gateway_data=true` and the exact confirmation `RESET_UNLAUNCHED_GATEWAY_DATA`. The installer stops writers, snapshots first, and then removes only the validated gateway SQLite main/WAL/SHM files; any failure before commit restores that snapshot. This action destroys gateway installs, quotas, settings, and accounting state, so it is forbidden once production data matters.
- Production requires a pinned `SERVER_KNOWN_HOSTS` GitHub Environment secret; deployment fails closed when it is absent or has no entry for `SERVER_HOST`. There is no `ssh-keyscan`/TOFU fallback. The remote data directory is `0700`, DB/WAL/SHM and secret env are `0600`, and the successful release retains one root-only rollback bundle.
- A schema-aware manual rollback is installed as `sudo /usr/local/sbin/anselm-gateway-rollback` (interactive confirmation) or `sudo /usr/local/sbin/anselm-gateway-rollback --yes` (automation). If a host/process crash leaves the persistent transition marker, recover the exact checksummed bundle named there. The most reliable entry is always `sudo <bundle-from-marker>/recovery/rollback.sh --recover-incomplete` (add `--yes` non-interactively); the global command supports the same mode when it has already been upgraded. Every bundle carries its exact recovery program, while rollback restores the previous global entry so it cannot drift from an older retained READY bundle. The inert Caddy guard remains a managed safety artifact. Rollback restores the retained DB snapshot and all matching runtime artifacts; switching the binary symlink alone is unsupported and unsafe after a schema migration.

The deployment target (domain, ACME email) is injected from GitHub secrets and is not committed. Production defaults include compatibility-only `INPUT_TOKEN_CAP=0`, `MAX_TOKENS_CAP=16384`, `MAX_MESSAGES=4096`, `MAX_MESSAGE_CHARS=4194304`, `MAX_BODY_BYTES=8388608`, `MONTHLY_QUOTA=5000`, and disabled optional traffic throttles (`0`/`off`). See [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) and [`deploy/`](deploy/).

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
- [`decisions/`](docs/decisions/) — ADR-001..016 (design decisions, kept immutable)

## License

[MIT](LICENSE).
