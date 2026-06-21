---
id: DOC-027
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/overview.md
---

# 重写切片计划 + 包树 + ADR 索引(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

## 切片计划
## Ordered implementation slice plan (foundation-first)

Each slice is independently buildable + testable; later slices depend only on earlier ones. GW-INV ids are the acceptance criteria; tests port the named guards from the old tree.

| order | slice | builds | key invariants (GW-INV) | tests |
|---|---|---|---|---|
| 1 | **pkg kernel** | `pkg/{logx,reqid,idgen,clientip,noncecache,pow,ratesample,alert}` + `domain/apierr` (sentinels + APIError) | GW-INV-11,12,16,17,20; wire-code table verbatim | `TestRedactionNeverLeaksSecrets`, `FuzzIPKey`/`TestClientIP*`, `TestRecoverNeverLeaksPanicValueOrKey`, nonce-once UseOnce, idgen 16B+regenerate(B13), wire-code/status/message table assertions |
| 2 | **store + migrations** | `infra/sqlite` (W/R pools, DSN/PRAGMA), `infra/sqlite/migrations` (8 tables + `idx_ledger_open` + `schema_migrations`) | GW-INV-40 | `TestOpenCreatesSchema`, `TestPragmasWALAndForeignKeys`, `TestTuningPragmasApplied`, `TestWritePoolSingleConn`, `TestReadPoolBounded`, `TestCloseIdempotent`, `TestOpenBadPathFails`, fresh-clone migrate idempotency + version-table fail-on-future |
| 3 | **config** | `domain/config` (consts, validateSemantics, WorstCaseMemoryMiB), `infra/configprovider` (atomic Load/ApplyOverrides/LoadWithOverlay/Dump/Snapshot), `infra/store/settingsstore` | GW-INV-10,14,38,39 | `TestLoadSemanticValidation`, `TestMemoryBudget*`, `TestLoadInvalidTzPanics`/`TestRedline_TzFailFast`, `TestSnapshotRedactsSecrets`, `TestPowSecretNeverInSnapshotOrDump`, `Test*Overridable` + bounds parity, partial-persist rollback |
| 4 | **quota** | `domain/quota` (Period,Reservation,SublimitApplied), `app/quota` (Reserve/Settle/Rollback/ReconcileOrphans), `infra/store/quotastore` | GW-INV-01,02,04,05,06,07,09 (+B1,B2 fixes) | `TestConcurrent*NoOversell`, `TestBudgetExhausted`, `TestRollbackConservation`, `TestPeriodSnapshotReuse`, double-settle idempotency, orphan-vs-settle race, SublimitApplied-driven rollback (B1), settle-failure observable (B2) |
| 5 | **install + identity + PoW** | `domain/install`, `app/install` (issuance, Sybil read-then-commit gates, PoW gate, LookupInstall auth), `infra/store/installstore` | GW-INV-12,16,20,GW-INV-08-adjacent (+B8,B9,B11,B13 fixes) | `TestFreshTokenEachInstall`, `TestTokenStoredAsHash`, `TestFPStoredAsHashNotPlaintext`, `TestInstallRateLimitAuditsDistinctCode`, PoW verify-order + nonce-once-last + shadow disjoint labels (B12), rate-bucket prune (B8), throttled last-seen (B9), commit-on-success counters (B11) |
| 6 | **upstream client** | `infra/upstream` (redactingTransport, per-key breaker+cooldown, pickKey, retry policy, first-byte timer), `infra/ratelimit` | GW-INV-11,23,26,27,30 (+B5,B6 fixes) | `TestRedline_KeyNeverInLogs`, REL-3 failover (key0 401→cooldown→key1), `TestUpstream429MapsBusy`, retry bounded≤3/jitter, first-byte timer disarm-idempotent (B6), single-key unchanged |
| 7 | **proxy / chat** | `app/chat` (gate order, reserve→forward→settle, REL-5 rollback, anomaly throttle, process breaker), `domain/chat` (decode/estimate/clamp/resolveModel/sanitize) | GW-INV-01,02,03,08,21,22,24,25,26,28,31-37 (+B5 process-breaker fix) | `TestStreamingPassthroughAndSettle`, `TestHandlerClientDisconnectKeepsCount`, `TestPreOutputFailureRollsBackBudget`, `TestBreakerOpenFires`, `TestClientCancelWhileQueued`, `TestQueueWait*`, `TestDangerFieldsStripped`/`FuzzInboundDecode`, `TestRejectNGreaterThanOne`, `TestCheckMessageShape`, `FuzzEstimate*`, clamp/resolveModel units |
| 8 | **models** | `domain/model`, `app/model` (live allowlist), business handler | GW-INV-35 (allowlist), auth reuse (INVALID_TOKEN/ACCOUNT_BANNED) | live-reload allowlist reflection, empty `data` is `[]` not null, OpenAI list envelope, auth gate codes, non-GET→400 |
| 9 | **health / metrics / diskguard / alert** | `app/health` (live+ready), `infra/metrics`, `infra/diskguard`, `pkg/alert` wiring, `handlers/admin` (+requireLoopback) | GW-INV-13,15,29 | `TestAdminBindLoopbackOnly`, `TestRequireLoopbackResolvesHostnames`, OBS-2 metric presence + no high-cardinality labels, `TestDiskLowSheds`, diskguard flip/recover/fail-open/sync-prime, `/readyz` 3-state shape, `/healthz` never touches DB |
| 10 | **dashboard backend** | `app/dashboard` (overview/config/installs/audit/export), `handlers/dashboard`, session+CSRF+loginlimit middleware, `infra/webassets` | GW-INV-19 (+B14,B15,B16 fixes) | login/logout/CSRF, per-IP backoff 429+Retry-After+details, `requireSession` 401 on all /api/*, sliding cookie re-issue (B14), bad-body-before-attempt (B15), per-session/window QPS (B16), export octet-stream, installRow no token/fp/ip |
| 11 | **React frontend** | `ui/src` (pages, auth, lib/api, lib/types mirror), `go:embed` dist | GW-INV-19 (CSP/contract), ADR-003 client | TS typecheck, contract-mirror parity vs Go structs, fresh-clone `npm ci && build && git diff --exit-code` (B0 gate), error.code branching, 401-redirect |
| 12 | **bootstrap + e2e** | `internal/bootstrap` (Build, listeners w/ socket-activation, lifecycle ordered shutdown), `cmd/gateway/main.go`, e2e suite | GW-INV-18,24,GW-INV-39-persist; middleware chain order; ADR-004,010 | `TestE2E*` (non-stream relay+settle, max-body reject, model rewrite, post-output error full-settle), listener-collision fail-fast, `waitWithTimeout` two-branch (bgWG drains before Close), socket-activation fd preference + self-bind fallback |

## 包树
## Target package tree — `anselm-gateway`

Thin gateway: ~6 domain concerns + dashboard + pkg kernel. Sized smaller than Foryx (no 28-service fan-out). Layering mimics Foryx: `domain` (pure types + sentinels), `app` (use-cases/orchestration), `infra` (DB/HTTP-client/OS adapters), `transport/httpapi` (HTTP edge), `bootstrap` (composition root), `pkg` (cross-cutting kernel).

```
anselm-gateway
├── cmd/
│   └── gateway/
│       └── main.go                     # thin shell: parse env, call bootstrap.Build, run lifecycle, signal handling
├── internal/
│   ├── domain/                         # pure types, wire-code sentinels, period math; ZERO infra imports
│   │   ├── apierr/                     # APIError{Status,Code,Message[,Details]} + ALL wire-code sentinels (the spec)  [old internal/httpx/errors.go sentinels]
│   │   ├── quota/                      # Period{Month,Day}, Reservation{RequestID,InstallID,Period,Reserved,SublimitApplied}, guardrail value types  [old quota types]
│   │   ├── install/                    # Install entity, token/fp hashing contracts, InstallID/RequestID id-shapes, PoW challenge value type + difficulty math  [old install/pow types]
│   │   ├── chat/                       # inboundRequest/upstreamRequest/chatMessage, estimate contract, clampMaxTokens, resolveModel rules  [old proxy estimate/usage types]
│   │   ├── model/                      # model-catalog entity + OpenAI list envelope shape  [old models types]
│   │   └── config/                     # Config struct, all bound consts, tier enum, validateSemantics, WorstCaseMemoryMiB (pure validation, no env/db)  [old config.go pure parts]
│   ├── app/                            # use-cases: orchestrate domain over infra ports (interfaces declared here)
│   │   ├── quota/                      # Reserve/Settle/Rollback/ReconcileOrphans (3-guardrail tx orchestration); declares LedgerStore/UsageStore/BudgetStore ports  [old quota/quota.go]
│   │   ├── install/                    # issuance use-case + Sybil gate phase (ip/global/fp) + PoW gate + LookupInstall auth  [old install/install.go logic]
│   │   ├── chat/                       # the proxy use-case: gate order, reserve→forward→settle, REL-5 rollback, anomaly throttle, breaker orchestration  [old proxy/proxy.go,throttle.go,reliability.go logic]
│   │   ├── model/                      # live-allowlist catalog read  [old models/models.go logic]
│   │   ├── health/                     # liveness + readiness aggregation (db/upstream/disk)  [old health/health.go]
│   │   └── dashboard/                  # admin use-cases: overview, config-apply, install list/ban/unban, audit, export  [old dashboard/{api,installs,audit,export}.go logic]
│   ├── infra/                          # adapters implementing app ports; the only layer touching OS/DB/network
│   │   ├── sqlite/                     # W/R pool open + DSN/PRAGMA + migration runner (versioned)  [old store/store.go]
│   │   │   └── migrations/             # embedded numbered .sql migrations + schema_migrations table  [replaces old single schema const]
│   │   ├── store/                      # per-entity store impls over sqlite pools
│   │   │   ├── installstore/           # installs + install_ip_rate + install_global_rate + install_fp_rate DML  [old install.go DML]
│   │   │   ├── quotastore/             # usage + budget + ledger DML  [old quota.go DML]
│   │   │   └── settingsstore/          # settings overlay read/persist  [old config runtime/provider DML]
│   │   ├── upstream/                   # DeepSeek HTTP client: redactingTransport, per-key breaker+cooldown, pickKey, retry, first-byte timer  [old proxy/transport.go,reliability.go]
│   │   ├── ratelimit/                  # in-memory per-install minute token bucket + SetKeyLimit  [old proxy rl]
│   │   ├── diskguard/                  # statfs probe + atomic degraded flag + SetFloors  [old diskguard/diskguard.go]
│   │   ├── configprovider/             # atomic.Pointer[Config] Provider: Load/ApplyOverrides/LoadWithOverlay/Dump  [old config/runtime.go,provider.go]
│   │   ├── metrics/                    # Prometheus registry + RED Wrap + gauges + expvar  [old metrics/metrics.go]
│   │   └── webassets/                  # embedded SPA dist (go:embed) + contentTypeFor  [old dashboard/web.go]
│   ├── transport/
│   │   └── httpapi/
│   │       ├── response/               # ONE envelope: WriteJSON(bare success) + WriteError/WriteErrorWith/WithDetails; APIError→wire  [collapses old httpx/errors.go + dashboard/http.go]
│   │       ├── middleware/             # Recover(X-Request-ID, panic→metric), DenyCORS, MaxBody, securityHeaders, requireSession, requireCSRF, loginlimit  [old httpx/middleware.go + dashboard/{http,session,loginlimit}.go]
│   │       ├── handlers/               # thin HTTP handlers per endpoint calling app use-cases
│   │       │   ├── business/           # install, challenge, chat, quota, models, healthz handlers
│   │       │   ├── admin/              # metrics, readyz, pprof, expvar wiring  [old metrics/admin.go]
│   │       │   └── dashboard/          # login/logout/session/overview/config/installs/audit/export handlers  [old dashboard/{auth,api,installs,audit,export}.go]
│   │       └── router/                 # the 3 mux builders (business/admin/dashboard) + chain assembly  [old server/server.go + metrics/admin.go + dashboard/dashboard.go routes()]
│   ├── bootstrap/                      # composition root: env load → provider → stores → upstream → app services → 3 routers → lifecycle  [old cmd/server/main.go,admin.go wiring]
│   │   ├── build.go                    # Build(*App): wire everything, return servers + bgWG
│   │   ├── listeners.go                # socket-activation fd preference + self-bind fallback + requireLoopback  [old main.go listener funcs + metrics/admin.go requireLoopback]
│   │   └── lifecycle.go               # READY notify, reconciler/prober/diskguard loops, ordered shutdown (scanCancel→Shutdown→bgWG.Wait(30s)→st.Close)  [old main.go runReconciler/shutdown]
│   └── pkg/                            # cross-cutting kernel, importable by any layer, imports nothing internal except other pkg
│       ├── apierr/  → (moved to domain; pkg only if shared)  — see ADR-002
│       ├── logx/                       # slog JSON + redactAttr floor + slog.Any ban  [old logx/logx.go]
│       ├── reqid/                      # X-Request-ID mint + sanitizeRID  [old middleware helper]
│       ├── idgen/                      # ins_/req_/gwk_ id minting (16B install id, regenerate-on-conflict)  [old newInstallID/newRequestID, B13 fix]
│       ├── clientip/                   # XFF-rightmost-only-if-loopback + /64 collapse  [old install.go clientIP/ipKey, GW-INV-16]
│       ├── noncecache/                 # bounded LRU+TTL UseOnce  [old httpx.NonceCache]
│       ├── pow/                        # stateless challenge mint/verify (HMAC/freshness/difficulty)  [old install/pow.go crypto]
│       ├── ratesample/                 # server-side sliding-window QPS (no per-poll mutable sampler, B16 fix)  [old dashboard/rate.go]
│       └── alert/                      # AlertState aggregation (payload-shape low-cardinality)  [old alert.go]
└── docs/                               # Foryx-style doc governance (concepts/references/decisions/how-to/working)
```

### Old-concern → new-home map
| Old | New |
|---|---|
| `internal/httpx/errors.go` (sentinels) | `internal/domain/apierr` |
| `internal/httpx/errors.go` (writers) + `dashboard/http.go` envelope | `internal/transport/httpapi/response` (unified) |
| `internal/httpx/middleware.go`, `bearer.go` | `internal/transport/httpapi/middleware` (+ `pkg/reqid`) |
| `internal/quota/quota.go` | `app/quota` (logic) + `infra/store/quotastore` (DML) + `domain/quota` (types) |
| `internal/proxy/{proxy,throttle,reliability,estimate,usage,audit}.go` | `app/chat` (orchestration) + `infra/upstream` (client/breaker) + `infra/ratelimit` + `domain/chat` (types/estimate) |
| `internal/install/{install,pow}.go` | `app/install` + `infra/store/installstore` + `domain/install` + `pkg/{pow,clientip,noncecache}` |
| `internal/models/models.go` | `app/model` + `domain/model` + handler |
| `internal/health/health.go` | `app/health` + admin/business handlers |
| `internal/store/store.go` | `infra/sqlite` (pools/PRAGMA) + `infra/sqlite/migrations` |
| `internal/config/{config,runtime,provider}.go` | `domain/config` (pure) + `infra/configprovider` (atomic/overlay) + `infra/store/settingsstore` |
| `internal/metrics/{metrics,admin}.go` | `infra/metrics` + `handlers/admin` + `bootstrap/listeners` (requireLoopback) |
| `internal/diskguard` | `infra/diskguard` |
| `internal/dashboard/*.go` | `app/dashboard` + `handlers/dashboard` + `middleware` + `infra/webassets` |
| `internal/logx`, `internal/alert` | `pkg/logx`, `pkg/alert` |
| `cmd/server/{main,admin}.go` | `cmd/gateway/main.go` (shell) + `internal/bootstrap` |

## 依赖规则
## Dependency rules (enforce by import-lint, e.g. `depguard` / `go-arch-lint`)

Allowed import direction (a layer may import only layers strictly to its right):

```
cmd ─▶ bootstrap ─▶ transport/httpapi ─▶ app ─▶ domain
                 └─▶ infra ───────────────▶ domain
        transport, app, infra, domain  ─▶ pkg   (pkg imports only pkg + stdlib + 3rd-party)
```

Concrete enforceable rules:

1. **`internal/domain/...`** — imports ONLY stdlib + `internal/pkg/...`. MUST NOT import `app`, `infra`, `transport`, `bootstrap`, or any DB/HTTP/OS-bound 3rd-party. Pure types, wire-code sentinels, validation, period/estimate math.
2. **`internal/app/...`** — imports `domain` + `pkg`. Declares its infra needs as **ports (interfaces) defined in the app package**. MUST NOT import `infra`, `transport`, `bootstrap`, `database/sql`, `net/http` server types, or any concrete store/client. (Use-case may import `context`, `time`.)
3. **`internal/infra/...`** — imports `domain` + `pkg` + 3rd-party (sqlite driver, gobreaker, prometheus). Implements `app` ports structurally (no import of `app` needed — Go duck-typing; if a shared port interface is referenced, it lives in `app` and infra may import that one app pkg ONLY for the interface — prefer structural satisfaction). MUST NOT import `transport` or `bootstrap`. MUST NOT import sibling `app` use-cases.
4. **`internal/transport/httpapi/...`** — imports `app`, `domain`, `pkg`. Handlers depend on app use-cases via interfaces. `response` imports `domain/apierr` only. Middleware imports `pkg` + `domain/apierr`. MUST NOT import `infra` or `bootstrap`.
5. **`internal/bootstrap`** — the ONLY package allowed to import across `app` + `infra` + `transport` + `domain` + `pkg` simultaneously. It is the composition root. **Nothing imports bootstrap** (no cycle). `cmd/gateway` imports only `bootstrap`.
6. **`internal/pkg/...`** — leaf kernel: imports only stdlib + other `pkg` + vetted 3rd-party. MUST NOT import `domain`, `app`, `infra`, `transport`, `bootstrap`.
7. **No cross-domain coupling within a layer is forbidden in app** except via `domain` shared types: `app/chat` may use `app/quota` + `app/install` use-cases (composed in chat orchestration) — these are sibling app deps and ARE allowed (chat is the only orchestrator that legitimately composes quota+install+upstream). Keep this the single exception; all other app packages stay sibling-independent.
8. **Secrets boundary (lint-checkable)**: `domain/config` and `infra/configprovider` must never serialize secret fields; `Dump`/`Snapshot` return masked values. (Enforced by test, not import-lint.)

## ADR 索引
| ADR | Title | Decision (one line) |
|---|---|---|
| ADR-001 | Pessimistic three-guardrail reservation accounting | Reserve `est = promptEst + clampedMaxTokens` for monthly-count + install-day-tokens + global-day-budget in ONE `BEGIN IMMEDIATE` tx on the single-writer pool; settle on real usage, rollback once pre-output, reconcile orphans — crash always over-charges (GW-INV-01..10). |
| ADR-002 | Unified structured error type in domain | One `APIError{Status,Code,Message,Details}` with stable UPPER_SNAKE wire codes as `domain/apierr` sentinels; collapses the two duplicated old envelopes (httpx + dashboard) into one; `Details` (omitempty) only for `LOGIN_LOCKED`. |
| ADR-003 | Bare-success / error-envelope API contract (diverge from Foryx `{data}`) | Success = bare entity JSON (no wrapper), failure = `{"error":{"code","message"[,"details"]}}`, non-APIError normalizes to `500 INTERNAL`; `/v1/models` keeps OpenAI `{object,data}` list; `/healthz`,`/readyz`,`/metrics` keep non-envelope shapes. Deliberately NOT Foryx's `{"data"}`. |
| ADR-004 | Three physically-isolated listeners | Business `0.0.0.0:8080` (Caddy-fronted), admin `127.0.0.1:9090` (loopback fail-fast, no auth = physical control), dashboard `127.0.0.1:8081`; the three addrs must be distinct (config fail-fast) and admin must be loopback (requireLoopback at bind). |
| ADR-005 | SQLite W/R split pool + versioned migration framework | Single-writer pool (MaxOpenConns=1, `_txlock=immediate`) + bounded read pool (`READ_POOL_MAX_CONNS`); replace the idempotent `IF NOT EXISTS` schema-blob with embedded numbered `.sql` migrations + `schema_migrations` version table (forward-only, checksum-tracked, run on writer before serving); preserve the exact 8 tables + `idx_ledger_open`. |
| ADR-006 | Config tiers + atomic hot-reload overlay | Three tiers (runtime-hot / secret-env-only / startup-hard); `atomic.Pointer[Config]` lock-free read, write path = clone→apply→validateSemantics→all-or-nothing settings persist→swap→notify; env+overlay share identical bounds; secrets never persisted/dumped; per-request single `cfg.Load()` snapshot. |
| ADR-007 | M2 Sybil/PoW dormant-by-default | Every Sybil gate (`INSTALL_GLOBAL_DAILY_CAP`,`INSTALL_PER_FP_*`) defaults 0=disabled and short-circuits before any DB work; `INSTALL_POW_MODE` three-state off/shadow/enforce with stateless HMAC challenge, nonce-once-LAST verify order, and strong-consistency `INSTALL_POW_SECRET` requirement on Load + hot-edit. |
| ADR-008 | Doc governance adoption | Adopt Foryx `docs/GOVERNANCE.md` model (6 doc types, frontmatter, doc-code parity, `make docs` gate, ADRs immutable in `decisions/`), localized to the gateway. |
| ADR-009 | React/Vite/AntD dashboard clean architecture | SPA layered api-client / types-mirror / auth-context / pages, embedded via `go:embed` into `infra/webassets`, served behind session+CSRF; build artifact is a committed/CI-built embed target with a fresh-clone build gate (fixes B0). |
| ADR-010 | systemd socket-activation for the business listener | Business `:8080` prefers a socket-activation fd (`activation.Listeners`) so restarts keep the kernel backlog (zero dropped connections); self-bind fallback when not under systemd; admin/dashboard self-bind (short restart gap accepted, documented). |
| ADR-011 | Fault classification excludes client-cancel and 429 (B5/B3, B12, B16 immunity) | Single typed `faultClass` computed in one place: client-cancel → 499 CLIENT_CANCELED (non-fault, non-retry, audit-only), 429 → UPSTREAM_BUSY (non-fault, non-retry); only {5xx,timeout,connect} count toward the process breaker; metrics labels strictly low-cardinality and disjoint. |
