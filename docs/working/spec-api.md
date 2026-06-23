---
id: DOC-021
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/api.md
---

# API 契约(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

# Anselm Gateway — Complete HTTP API Contract (CODE-EXACT spec extraction)

Source tree: `<repo>`. Old module path: `anselm-gateway` (new target: `anselm-gateway`). This contract is the authoritative behavioral spec the from-scratch rewrite MUST satisfy.

---

## 0. Three physically-isolated listeners (route surfaces)

| Surface | Default bind | Assembled in | Public via Caddy? | Routes |
|---|---|---|---|---|
| **Business** | `0.0.0.0:8080` | `internal/server/server.go` `BuildHandler` | Yes | `/v1/install`, `/v1/install/challenge`, `/v1/chat/completions`, `/v1/quota`, `/v1/models`, `/healthz` |
| **Admin/metrics** | `127.0.0.1:9090` (loopback-only, `requireLoopback` fail-fast) | `internal/metrics/admin.go` `NewAdminServer` | No (SSH tunnel only) | `/metrics`, `/readyz`, `/debug/pprof/*`, `/debug/vars` |
| **Dashboard** | `127.0.0.1:8081` (`DASHBOARD_ADDR`) | `internal/dashboard/dashboard.go` `routes()` | No (SSH tunnel only) | `/healthz`, `/login`, `/logout`, `/api/*`, `/static/`, `/` (SPA) |

Business middleware chain (outer→inner, `BuildHandler` lines 65-68): `Recover` (X-Request-ID + scoped logger + panic→`gateway_panics_total`) → `DenyCORS` → `MaxBody(256*1024)` → `ServeMux`. Each business route is additionally wrapped by `mx.Wrap("<label>", …)` for RED metrics with low-cardinality handler labels: `install`, `install_challenge`, `chat_completions`, `quota`, `models` (`/healthz` is NOT wrapped).

---

## 1. The bare-entity / error-envelope rule (global invariant)

Defined in `internal/httpx/errors.go` and mirrored verbatim by the dashboard (`internal/dashboard/http.go`).

- **Success** → the bare entity is JSON-encoded directly (NO wrapper). Helper `httpx.WriteJSON(w, status, v)`. Sets `Content-Type: application/json`, status, then `json.NewEncoder(w).Encode(v)`.
- **Failure** → a single uniform envelope: `{"error":{"code":"<STABLE_WIRE_CODE>","message":"<client-safe text>"}}`. Helpers `httpx.WriteError(err)` / `httpx.WriteErrorWith(status, code, message)`. A non-`*APIError` is normalized to `500 {"code":"INTERNAL","message":"internal error"}` — never leaks internal detail.
- `APIError` struct: `{Status int; Code string; Message string}`. Wire body type `envelope{Error errorBody}` where `errorBody{Code string \`json:"code"\`; Message string \`json:"message"\`}`.
- **Dashboard** uses an identical shape but its `errBody` adds an optional `Details map[string]any \`json:"details,omitempty"\`` (used only by the login lockout for `retryAfterSec`).

### Chat error envelope vs OpenAI (key divergence)
The chat endpoint does **NOT** emit OpenAI's `{"error":{"message","type","param","code"}}`. It emits the same gateway envelope `{"error":{"code","message"}}` as every other endpoint — `code` is a stable gateway wire code (e.g. `UPSTREAM_BUSY`, `QUOTA_EXHAUSTED`), `message` is gateway-authored client-safe text. Crucially (proxy.go `tryOnce`, comment "Never pass through upstream body/headers, 蓝图 §4.3"): the gateway **never passes through the upstream DeepSeek error body or headers** — every upstream non-2xx is classified and normalized to a gateway `APIError` (502 `UPSTREAM_ERROR` / 504 `UPSTREAM_TIMEOUT` / 429 `UPSTREAM_BUSY`). Successful chat responses ARE OpenAI-shaped because they are the upstream's own SSE frames / JSON body relayed verbatim (only whitelisted gateway headers added).

### Canonical wire-code table (`internal/httpx/errors.go` sentinels)
| Sentinel | Status | Code | Message |
|---|---|---|---|
| `ErrInvalidToken` | 401 | `INVALID_TOKEN` | missing or invalid install token |
| `ErrAccountBanned` | 403 | `ACCOUNT_BANNED` | this install has been disabled |
| `ErrRateLimited` | 429 | `RATE_LIMITED` | rate or daily sub-limit exceeded |
| `ErrQuotaExhausted` | 429 | `QUOTA_EXHAUSTED` | monthly free-tier quota exhausted |
| `ErrUpstreamBusy` | 429 | `UPSTREAM_BUSY` | upstream capacity is busy, retry shortly |
| `ErrBudgetExhausted` | 402 | `BUDGET_EXHAUSTED` | daily free-tier budget reached, try again tomorrow or use your own key |
| `ErrBadRequest` | 400 | `BAD_REQUEST` | invalid request body |
| `ErrUpstreamError` | 502 | `UPSTREAM_ERROR` | upstream model provider error |
| `ErrUpstreamTimeout` | 504 | `UPSTREAM_TIMEOUT` | upstream model provider timeout |
| `ErrTooManyInstalls` | 429 | `INSTALL_RATE_LIMITED` | too many installs from this address, retry later |
| `ErrInstallCapReached` | 429 | `INSTALL_CAP_REACHED` | install issuance is temporarily at capacity, retry later |
| `ErrInstallFPLimited` | 429 | `INSTALL_FP_LIMITED` | too many installs for this client, retry later |
| `ErrInstallPoWRequired` | 403 | `INSTALL_POW_REQUIRED` | proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW |
| `ErrInstallPoWInvalid` | 403 | `INSTALL_POW_INVALID` | invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry |
| `ErrDiskLow` | 503 | `DISK_LOW` | service temporarily read-only: low disk space |
| (inline, multiple sites) | 500 | `INTERNAL` | internal error |

---

## 2. Auth mechanism (gateway business endpoints)

Shared bearer extractor `httpx.Bearer(r)` (`internal/httpx/bearer.go`): reads `Authorization: Bearer <token>` (prefix-match `"Bearer "`, trims), returns `""` otherwise. All four token-gated endpoints (chat / quota / models, plus the shared lookup) call a single `AuthFunc(ctx, token) → (installID, status, found, error)`. The implementation is `install.LookupInstall` which hashes the token (`sha256`) and looks up `installs WHERE token_sha256 = ?`, opportunistically refreshing `last_seen_at` (≥10min throttle). Auth decision tree (identical across chat/quota/models):
- token `""` → `ErrInvalidToken` (401)
- `err != nil` → 500 `INTERNAL`
- `!found` → `ErrInvalidToken` (401)
- `status == "banned"` → `ErrAccountBanned` (403)

---

## 3. POST /v1/install (`internal/install/install.go` `ServeHTTP`)

| Aspect | Value |
|---|---|
| Method/Path | `POST /v1/install` |
| Auth | **None** (this mints the token). Sybil defenses instead (per-IP / global / per-fp / optional PoW) |
| Body limit | 256KiB (middleware) |

**Request JSON** (`installRequest`, unknown fields dropped via `json.Decoder` default):
```json
{ "fingerprint": "string (trimmed, truncated to 256 chars)",
  "client": "string (trimmed, truncated to 128 chars)" }
```
Both optional. Empty `fingerprint` is never a bucket key (per-fp gate skipped). Stored as `nullStr` (empty → SQL NULL).

**Success response** — bare entity (`installResponse`), `200`:
```json
{ "token": "gwk_<base64url(32 random bytes)>",
  "monthlyQuota": 12345,
  "resetAt": "RFC3339 (start of next month in RESET_TZ)" }
```
Token returned **only** at issuance; server stores only `sha256(token)` hex. Always a brand-new `installs` row + `install_id` `ins_<hex(8B)>` + fresh quota pool (no get-or-create, no fingerprint merge).

**Gate order** (each short-circuits to allow when its cap is 0/disabled, BEFORE any DB work):
1. PoW gate (`powGate`, only when `INSTALL_POW_MODE` ∈ {shadow, enforce}) — see header table below.
2. `checkAndBumpIPRate` (per-IP hourly, `INSTALL_PER_IP_HOUR`) → reject `INSTALL_RATE_LIMITED` (429), audit `gate=ip`.
3. `checkAndBumpGlobalRate` (`INSTALL_GLOBAL_DAILY_CAP`, 0=off) → reject `INSTALL_CAP_REACHED` (429), audit `gate=global`.
4. `checkAndBumpFPRate` (`INSTALL_PER_FP_DAILY` + `INSTALL_PER_FP_COOLDOWN_SEC`, both 0=off) → reject `INSTALL_FP_LIMITED` (429), audit `gate=fp`.

**Status codes**: `200` ok; `400 BAD_REQUEST` (non-POST or undecodable body); `403 INSTALL_POW_REQUIRED`/`INSTALL_POW_INVALID` (enforce); `429` (the three Sybil codes); `500 INTERNAL` (token gen / DB error).

**Special headers**: PoW request header `X-PoW: <challenge>.<nonce>` (read only in shadow/enforce). No `Retry-After` on any 429 here. `Content-Type: application/json` on all responses. `X-Request-ID` echoed by Recover middleware.

**Client-IP trust** (`clientIP`): trusts `X-Forwarded-For` **rightmost** segment ONLY when `RemoteAddr` is loopback (Caddy appends real peer); else `RemoteAddr`. IPv6 collapsed to `/64` for the rate key (`ipKey`).

---

## 4. GET /v1/install/challenge (`internal/install/install.go` `ChallengeHandler`)

| Aspect | Value |
|---|---|
| Method/Path | `GET /v1/install/challenge` |
| Auth | None; reuses the SAME per-IP install bucket (`checkAndBumpIPRate`) to prevent challenge-minting floods |

**Request**: none (GET).

**Success response** — bare entity (`challengeResponse`), `200`:
```json
{ "challenge": "base64url(random16 || unixTs(8B BE) || HMAC_SHA256(secret, random16||ts)[:16])",
  "difficulty": 0,
  "required": false }
```
`difficulty` = `INSTALL_POW_DIFFICULTY` (leading-zero-bit target). `required` = `true` **only** when `INSTALL_POW_MODE == enforce`; off/shadow report `false`. Challenge is stateless (no DB), TTL `powChallengeTTL = 120s`. Always answers even when `mode==off` (benign no-op, signed with `powDormantKey` placeholder) so the surface exists for clients to probe; `/install` simply never verifies in dormant mode.

**Status codes**: `200` ok; `400 BAD_REQUEST` (non-GET); `429 INSTALL_RATE_LIMITED` (per-IP bucket, audit `gate=pow_challenge`); `500 INTERNAL`.

**PoW verification chain** (`verifyPoW`, all must pass): `parsePoWHeader` (split on first `.`; challenge=first segment, nonce=remainder; both non-empty) → `verifyChallenge` (constant-time HMAC, freshness ±120s) → `meetsDifficulty` (`leadingZeroBits(SHA256(challenge+"."+nonce)) >= difficulty`) → `nonces.UseOnce(challenge)` (replay guard, LAST so a failing check never burns the challenge).

---

## 5. POST /v1/chat/completions (`internal/proxy/proxy.go` `ServeHTTP` + `forward`)

| Aspect | Value |
|---|---|
| Method/Path | `POST /v1/chat/completions` |
| Auth | Bearer install token (banned → 403) |
| Body limit | 256KiB (middleware + defensive re-cap `h.bodyLimit`) |

**Request JSON** — strict whitelist (`inboundRequest`; unknown fields dropped by not being declared, NOT errored):
```json
{ "model": "string (resolved against MODEL_ALLOWLIST; unknown/empty → DefaultModel = allowlist[0])",
  "messages": [ { "role": "string", "content": "string" } ],   // required, non-empty
  "stream": false,
  "temperature": 1.0,        // *float64, optional; forwarded as-is
  "max_tokens": 100,         // *int64, optional; clamped: min(client, MaxTokensCap) when client>0 && <cap
  "n": 1 }                   // *int, optional; n>1 REJECTED (both raw-probe + typed check)
```
Sanitized upstream body (`upstreamRequest`) forwards ONLY: `model`(rewritten), `messages`, `stream`, `temperature`(omitempty), `max_tokens`(clamped, always present), and `stream_options:{include_usage:true}` injected when `stream:true`. Everything else the client sent is dropped by construction.

**Input guardrails** (all 400 `BAD_REQUEST`, against a single per-request config snapshot): `n>1` → "n>1 is not allowed"; empty messages → "messages is required"; `len(messages) > MaxMessages` → "too many messages"; any `RuneCount(content) > MaxMessageChars` → "message content too large"; `estimatePromptTokens > InputTokenCap` → "input too large".

**Success response — TWO modes:**
- **`stream:true` → SSE.** Headers (`writeStreamHeaders` + status): `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Quota-Limit: <MonthlyQuota>`, `X-Quota-Reset: <RFC3339 month reset>`. First-byte-gated `200`. Then upstream SSE frames relayed line-by-line (each line + `\n`, flushed per frame) until a frame containing `[DONE]`. Per-frame 30s rolling write deadline (NOT a global write timeout). Settles from the `include_usage` final-frame usage; mid-stream disconnect KEEPS full reserved count (anti-abuse). Body shape = upstream's own OpenAI chat-completion-chunk frames.
- **`stream:false` → JSON.** Same `X-Quota-Limit`/`X-Quota-Reset` headers plus `Content-Type: application/json`, `200`, body = upstream's full JSON response relayed verbatim (read with 8MiB `LimitReader`). Settles from the response usage object.

**Special headers**: `X-Quota-Limit` (= `MonthlyQuota`), `X-Quota-Reset` (RFC3339, `MonthResetAt`) — emitted on BOTH success modes via `writeStreamHeaders`. `X-Request-ID` echoed. NO `X-Quota-Used`/`X-Quota-Remaining` header (those are only in the `/v1/quota` body). NO client-facing `Retry-After` (upstream Retry-After is consumed internally for backoff/breaker, never forwarded).

**Status codes / failure mapping**: `200` (success); `400 BAD_REQUEST`; `401 INVALID_TOKEN`; `403 ACCOUNT_BANNED`; `429 RATE_LIMITED` (per-install minute bucket OR quota day sub-limit); `429 QUOTA_EXHAUSTED` (monthly); `402 BUDGET_EXHAUSTED` (global daily budget); `429 UPSTREAM_BUSY` (breaker open / queue timeout / upstream 429); `502 UPSTREAM_ERROR`; `504 UPSTREAM_TIMEOUT`; `503 DISK_LOW` (REL-6 read-only degradation, shed before reservation); `500 INTERNAL`. Client-cancel while queued → no body written, audited `499 CLIENT_CANCELED` (internal audit code only, never a wire status).

**Pessimistic accounting invariant**: reserve `est = estimatePromptTokens + clampedMaxTokens` atomically (count / install-day-tokens / global-day-budget) BEFORE upstream; single pre-output rollback defense point (`outputStarted` flag); once first byte committed, never retried/double-billed; settle/rollback run on a detached context tracked by a shutdown WaitGroup.

---

## 6. GET /v1/quota (`internal/quota/handler.go` `QuotaHandler`)

| Aspect | Value |
|---|---|
| Method/Path | `GET /v1/quota` |
| Auth | Bearer install token (banned → 403) |

**Request**: none.

**Success response** — bare entity (`quotaResponse`), `200`:
```json
{ "limit": 0,        // MonthlyQuota
  "used": 0,         // authoritative monthly count (INCLUDES in-flight reservations = conservative upper bound)
  "remaining": 0,
  "resetAt": "RFC3339 month reset",
  "available": true } // remaining>0 && budgetUsed < GlobalDailyBudget
```
**Status codes**: `200`; `400 BAD_REQUEST` (non-GET); `401 INVALID_TOKEN`; `403 ACCOUNT_BANNED`; `500 INTERNAL`. No special headers beyond `Content-Type: application/json` + `X-Request-ID`. (No `X-Quota-*` headers here — the values are the body.)

---

## 7. GET /v1/models (`internal/models/models.go`)

| Aspect | Value |
|---|---|
| Method/Path | `GET /v1/models` |
| Auth | Bearer install token (banned → 403) — read-only but gated against anonymous scrape; reserves NO quota, bills nothing |

**Request**: none.

**Success response** — OpenAI-compatible list envelope (`listResponse`), `200`:
```json
{ "object": "list",
  "data": [ { "id": "<model id from live MODEL_ALLOWLIST>", "object": "model", "owned_by": "anselm-gateway" } ] }
```
Reads the LIVE allowlist on every request (hot-reload reflected with no restart). `created` field deliberately omitted (no per-model creation time). `data` may be empty array (never null — `make([]model,0,...)`).

**Status codes**: `200`; `400 BAD_REQUEST` (non-GET); `401 INVALID_TOKEN`; `403 ACCOUNT_BANNED`; `500 INTERNAL`.

> Note: this is a deviation from the strict "success = bare entity" rule — `/v1/models` intentionally returns the OpenAI `{object,data}` list envelope (NOT a bare array) for OpenAI client compatibility.

---

## 8. GET /healthz (`internal/health/health.go` `LiveHandler`)

| Aspect | Value |
|---|---|
| Method/Path | `GET /healthz` (public business mux; also a separate copy on the dashboard mux) |
| Auth | None |

**Success response** — `200`, `Content-Type: application/json`, literal bytes `{"status":"ok"}`. Pure process liveness; NEVER touches DB/upstream (OBS-3). Only health surface on the public mux (readiness is loopback-only). Not wrapped by `mx.Wrap`.

---

## 9. Admin port: GET /metrics, GET /readyz (`internal/metrics/admin.go`)

Loopback-only (`requireLoopback` fail-fast on bind). **No auth** (physical isolation is the control).

| Method/Path | Response |
|---|---|
| `GET /metrics` | Prometheus text exposition (`promhttp.HandlerFor(reg, …)`). Low-cardinality labels only (never token/IP/prompt). |
| `GET /readyz` | Layered JSON `readyResponse{db,upstream}` (`internal/health/health.go` `ReadyHandler`). |
| `/debug/pprof/*`, `GET /debug/vars` | pprof index/cmdline/profile/symbol/trace; expvar (`gateway_goroutines`, `gateway_heap_alloc_bytes`). |

**`/readyz` body** (bare entity): `{ "db": "ok|down|degraded", "upstream": "ok|degraded" }`.
- `db`: `down` if read OR write PingContext fails (~1s budget); `degraded` if DB pings fine but REL-6 disk-low read-only active; else `ok`.
- `upstream`: `degraded` if cached background TCP probe stale (last success older than 3× interval, default interval 30s); else `ok`.
- **Status**: `200` only when all healthy; `503 ServiceUnavailable` if `!dbOK || !upOK || diskLow`. Never leaks raw errors (only `ok/down/degraded`). `Content-Type: application/json`.

---

## 10. Dashboard (`internal/dashboard/*.go`) — loopback admin SPA backend

**Auth stack** (per `dashboard.go`): bcrypt login (`DASHBOARD_USER`/`DASHBOARD_PASSWORD`, hashed once at startup, plaintext discarded) → server-side session cookie → CSRF double-submit on state-changing POSTs → per-IP login backoff. Every route except `/healthz`, `/login`, `/logout`, `/static/`, `/` is behind `requireSession`.

**Session cookie** (`auth.go` `handleLogin`): name `anselm_dash`; `Path=/`; `HttpOnly=true`; `Secure=s.secureCookie` (true in prod; false only with `DASHBOARD_DEV_INSECURE_COOKIE`); `SameSite=Strict`; `MaxAge=int(sessionTTL)=12h`. Value = 256-bit crypto/rand hex session id. Logout sets the same cookie with empty value + `MaxAge=-1`.

**Global security headers** (`securityHeaders`, ALL dashboard routes): `X-Content-Type-Options: nosniff`; `Cache-Control: no-store`; `X-Frame-Options: DENY`; `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`.

**CSRF** (`http.go` `requireCSRF`): header `X-CSRF-Token` must constant-time-equal the session's bound csrf; mismatch → `403 CSRF_INVALID`; no session → `401 UNAUTHENTICATED`. Enforced inside each state-changing POST handler (config/ban/unban) AFTER `requireSession`. Logout does NOT require CSRF (idempotent, no state damage).

**Pagination** (`parsePagination`): `?cursor=` (non-neg int offset, default 0), `?limit=` (clamped [1,200], default 50). Audit uses keyset cursor (`?cursor=`=seq to page below).

| Method/Path | Auth | Request body | Success response (bare entity) | Status codes |
|---|---|---|---|---|
| `GET /healthz` | none | — | `{"status":"ok"}` (200, `application/json`) | 200 |
| `POST /login` | none (login backoff) | `{"user","password"}` (`loginRequest`, 4KiB cap) | `{"csrfToken","user"}` (`loginResponse`) | 200; 400 `BAD_REQUEST`; 401 `INVALID_CREDENTIALS` (no factor hint); 429 `LOGIN_LOCKED` (+`Retry-After` header + `details.retryAfterSec`) |
| `POST /logout` | cookie (no CSRF) | — | `{"ok":true}` | 200 (idempotent) |
| `GET /api/session` | session | — | `{"csrfToken","user"}` (`loginResponse`; CSRF rehydration after F5) | 200; 401 `UNAUTHENTICATED` |
| `GET /api/overview` | session | — | `overviewResponse` (see below) | 200; 401 |
| `GET /api/config` | session | — | `{"items":[config.DumpItem...]}` (secrets never present) | 200; 401 |
| `POST /api/config` | session + CSRF | `map[string]string` overrides (64KiB cap) | fresh `{"items":[...]}` | 200; 400 `BAD_REQUEST` (bad body / empty map); 400 `CONFIG_REJECTED` (validation, `message`=precise reason); 401; 403 `CSRF_INVALID` |
| `GET /api/installs` | session | — (`?cursor`,`?limit`) | `{"installs":[installRow...],"nextCursor":""}` | 200; 401; 500 `INTERNAL` |
| `POST /api/installs/ban` | session + CSRF | `{"install_id","reason"}` (`banRequest`, 8KiB; reason required, ≤256 chars) | `{"install_id","status":"banned"}` | 200; 400 (bad body / missing id / missing reason); 401; 403 `CSRF_INVALID`; 404 `INSTALL_NOT_FOUND`; 500 `INTERNAL` |
| `POST /api/installs/unban` | session + CSRF | `{"install_id"}` (`unbanRequest`, 8KiB; reason optional) | `{"install_id","status":"active"}` | 200; 400; 401; 403; 404 `INSTALL_NOT_FOUND`; 500 |
| `GET /api/audit` | session | — (`?cursor`=seq, `?limit`) | `{"events":[AuditEvent...],"nextCursor":""}` (keyset, newest-first) | 200; 401 |
| `GET /api/export` | session | — | **binary** SQLite snapshot (NOT JSON) | 200; 401; 500 `INTERNAL` |
| `GET /static/` | none | — | embedded asset bytes (explicit `Content-Type` via `contentTypeFor`) | 200; unknown asset → SPA fallback |
| `GET /` (catch-all) | none | — | `index.html` (SPA shell, `text/html; charset=utf-8`) | 200 |

**`overviewResponse`** (bare entity): `{ "budget":{"day","used","limit","remaining"}, "inflightConcurrency", "openReservations", "upstreamBreakerOpen", "diskDegraded", "alerts":[alert.AlertState], "recent":<rates>, "installsToday", "installGlobalCap" }`.

**`installRow`**: `{ "id":"ins_<hex>", "status", "createdAt":RFC3339, "lastSeenAt":RFC3339(omitempty), "todayTokens" }` — NO token, NO fingerprint, NO IP.

**`AuditEvent`**: `{ "seq", "at", "action", "target", "reason", "outcome", "actor" }` — only safe low-cardinality facts; never token/key/prompt/raw-IP. Outcomes: `ok|not_found|error|interrupted`.

**`GET /api/export` special headers** (`export.go`): `Content-Type: application/octet-stream`; `Content-Disposition: attachment; filename="anselm-gateway-<YYYYMMDD-HHMMSS>.db"`; `Content-Length: <size>`; status 200; body = `VACUUM INTO` temp-file snapshot streamed then deleted. Contains only on-disk DB state — never in-memory secrets.

**Login lockout** (`loginlimit.go`): `loginMaxFailures=5`, `loginBaseLockout=30s`, escalating (doubling, `over` clamped ≤16) up to `loginMaxLockout=15min`, entries swept after `loginEntryTTL=1h` idle. `429 LOGIN_LOCKED` carries `Retry-After: <ceil remaining seconds>` header AND body `details.retryAfterSec`. Same loopback/XFF-rightmost client-IP trust posture as proxy/install.

---

## 11. Cross-cutting headers summary

| Header | Where emitted |
|---|---|
| `X-Request-ID` | All business routes (Recover middleware; echoes sanitized client value or mints 8-byte hex) |
| `X-Quota-Limit`, `X-Quota-Reset` | `/v1/chat/completions` success only (both stream + non-stream) |
| `X-PoW: <challenge>.<nonce>` | Request header on `POST /v1/install` (shadow/enforce only) |
| `Retry-After` | Dashboard `POST /login` 429 lockout ONLY. NOT on any business 429. |
| `Set-Cookie` (`anselm_dash`; HttpOnly; Secure; SameSite=Strict; MaxAge) | Dashboard login/logout |
| CSP / nosniff / no-store / X-Frame-Options | All dashboard routes (`securityHeaders`) |
| `Content-Type: text/event-stream` + `Cache-Control: no-cache` | chat stream success |

## 12. CORS / body / method posture
- `DenyCORS`: `OPTIONS` with non-empty `Origin` → `403 {"code":"FORBIDDEN","message":"cross-origin requests are not allowed"}` (gateway is a localhost sidecar callee, never a browser).
- `MaxBody`: business body capped 256KiB; dashboard handlers cap per-route (login 4KiB, config 64KiB, ban/unban 8KiB).
- Wrong method on a registered Go 1.22 method-pattern route (e.g. `POST /v1/quota`) is handled by `ServeMux` (405), but each handler ALSO defensively rejects a wrong method with `400 BAD_REQUEST` when reached directly (proxy/quota/models/install/challenge all start with a method check returning `ErrBadRequest`).

---

### Key files (absolute paths)
- Route table: `<repo>/internal/server/server.go`
- Chat proxy: `<repo>/internal/proxy/proxy.go`, `throttle.go`
- Install + challenge: `<repo>/internal/install/install.go`, `pow.go`
- Models: `<repo>/internal/models/models.go`
- Quota handler: `<repo>/internal/quota/handler.go` (`View`/`Reserve` in `quota.go`)
- Health: `<repo>/internal/health/health.go`
- Error envelope + bearer + middleware: `<repo>/internal/httpx/errors.go`, `bearer.go`, `middleware.go`
- Admin server: `<repo>/internal/metrics/admin.go`
- Dashboard: `<repo>/internal/dashboard/{dashboard,http,auth,api,installs,audit,export,session,loginlimit,web}.go`

