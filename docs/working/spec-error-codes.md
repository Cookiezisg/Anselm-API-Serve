---
id: DOC-024
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/error-codes.md
---

# 错误码表(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

# Error Wire Codes — Code-Exact SPEC Extraction (anselm-gateway pre-rewrite tree)

Authoritative sources read:
- `<repo>/internal/httpx/errors.go` (canonical envelope + sentinel table)
- `<repo>/internal/httpx/middleware.go`
- `<repo>/internal/dashboard/http.go`, `auth.go`, `api.go`, `installs.go`, `export.go` (dashboard has its OWN parallel envelope)
- `internal/proxy/proxy.go`, `internal/proxy/audit.go`, `internal/install/install.go`, `internal/models/models.go`, `internal/quota/{handler.go,quota.go}`, `internal/health/health.go`

---

## 1. The error envelope shape

There are **two independent but byte-identical** envelope implementations. Both render the same JSON shape; the dashboard one additionally supports an optional `details` map.

### 1a. Business/gateway envelope — `internal/httpx/errors.go`

```go
type APIError struct {
	Status  int
	Code    string
	Message string
}
func NewError(status int, code, message string) *APIError
func (e *APIError) Error() string { return e.Code + ": " + e.Message }

type envelope struct {
	Error errorBody `json:"error"`
}
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
```

Renderers:
- `WriteError(w, err)` — type-asserts `err.(*APIError)`; **any non-`*APIError` is normalized to a generic `500 {"code":"INTERNAL","message":"internal error"}`** (no internal detail / upstream body / key leaked — §4.3 red line).
- `WriteErrorWith(w, status, code, message)` — explicit one-off; sets `Content-Type: application/json`, `w.WriteHeader(status)`, JSON-encodes the envelope.

Wire bytes (always exactly two fields):
```json
{"error":{"code":"<CODE>","message":"<message>"}}
```

### 1b. Dashboard envelope — `internal/dashboard/http.go`

Comment at line 29: *"envelope mirrors the main site"*. Distinct types, **not** imported from httpx:

```go
type errEnvelope struct {
	Error errBody `json:"error"`
}
type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`   // extra field, omitempty
}
func writeErr(w, status, code, message)                       // 2-field body
func writeErrDetails(w, status, code, message, details)       // adds "details"
func writeJSON(w, status, v)                                  // success = bare JSON value
```

So the dashboard's richest body is:
```json
{"error":{"code":"LOGIN_LOCKED","message":"too many attempts, retry later","details":{"retryAfterSec":30}}}
```
Only `LOGIN_LOCKED` currently uses `details` (`retryAfterSec`). All other dashboard errors emit the bare 2-field body.

### 1c. How it differs from the OpenAI error shape

| Aspect | This gateway | OpenAI |
|---|---|---|
| Top-level key | `error` (object) | `error` (object) |
| Code field | **`code`** = a stable UPPER_SNAKE wire code (e.g. `RATE_LIMITED`) | `code` (often `null`) + `type` (e.g. `invalid_request_error`) |
| `type` field | **absent** | present, primary discriminator |
| `param` field | **absent** | present (offending field) |
| `message` | client-safe, no upstream body/key | upstream message |
| Stability contract | the UPPER_SNAKE `code` is the machine contract; `message` is human-only | `type` + `code` are the contract |
| Extra | dashboard adds optional `details{}` map | n/a |

Net: clients must branch on `error.code` (UPPER_SNAKE), **not** on an OpenAI-style `error.type`. There is no `type`/`param`. Success bodies are bare JSON (no wrapper). This shape MUST be preserved verbatim in the rewrite.

---

## 2. Complete wire-code table (codes actually written to HTTP responses)

| Wire code | HTTP status | Meaning | Where raised (file:line, identifier) |
|---|---|---|---|
| `INVALID_TOKEN` | 401 | missing or invalid install token | `httpx.ErrInvalidToken`; `proxy.go:315,326`; `models.go:69,78`; `quota/handler.go:33,42` |
| `ACCOUNT_BANNED` | 403 | this install has been disabled | `httpx.ErrAccountBanned`; `proxy.go:332`; `models.go:82`; `quota/handler.go:46` |
| `RATE_LIMITED` | 429 | rate or daily sub-limit exceeded | `httpx.ErrRateLimited`; `proxy.go:349`; `quota/quota.go:124,138` (per-min rate + `DAILY_SUBLIMIT`) |
| `QUOTA_EXHAUSTED` | 429 | monthly free-tier quota exhausted | `httpx.ErrQuotaExhausted`; `quota/quota.go:107` (surfaced via `proxy.go:421`) |
| `UPSTREAM_BUSY` | 429 | upstream capacity is busy, retry shortly | `httpx.ErrUpstreamBusy`; `proxy.go:433,454,582`; queue-timeout/full, breaker open (`gobreaker.ErrOpenState`/`ErrTooManyRequests`), upstream 429; attempt outcomes `proxy.go:729,748` |
| `BUDGET_EXHAUSTED` | **402** | daily free-tier budget reached, try again tomorrow or use your own key | `httpx.ErrBudgetExhausted`; `quota/quota.go:156` (`GLOBAL_DAILY_BUDGET_TOKENS`); surfaced `proxy.go:421` |
| `BAD_REQUEST` | 400 | invalid request body (and several specific messages, see below) | `httpx.ErrBadRequest`; `proxy.go:307,368,381,393,401`; `proxy.go:232,244` (`"n>1 is not allowed"`); `proxy.go:239`; `install.go:123,140,258`; `models.go:60`; `quota/handler.go:28`; dashboard `BAD_REQUEST` (own envelope) many spots |
| `UPSTREAM_ERROR` | 502 | upstream model provider error | `httpx.ErrUpstreamError`; `proxy.go:596,867`; attempt outcomes `proxy.go:736,753,757,761,790` |
| `UPSTREAM_TIMEOUT` | 504 | upstream model provider timeout | `httpx.ErrUpstreamTimeout`; `proxy.go:682,734,755,788` |
| `INSTALL_RATE_LIMITED` | 429 | too many installs from this address, retry later | `httpx.ErrTooManyInstalls`; `install.go:170,171` (per-IP `INSTALL_PER_IP_HOUR`); also reused on PoW-challenge endpoint `install.go:272,273` |
| `INSTALL_CAP_REACHED` | 429 | install issuance is temporarily at capacity, retry later | `httpx.ErrInstallCapReached`; `install.go:181,182` (global daily coarse cap `INSTALL_GLOBAL_DAILY_CAP`) |
| `INSTALL_FP_LIMITED` | 429 | too many installs for this client, retry later | `httpx.ErrInstallFPLimited`; `install.go:192,193` (per-fingerprint daily `INSTALL_PER_FP_DAILY` / cooldown `INSTALL_PER_FP_COOLDOWN_SEC`) |
| `INSTALL_POW_REQUIRED` | 403 | proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW | `httpx.ErrInstallPoWRequired`; `install.go:337` (enforce mode, missing `X-PoW`) |
| `INSTALL_POW_INVALID` | 403 | invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry | `httpx.ErrInstallPoWInvalid`; `install.go:352` (HMAC forgery / expired challenge / difficulty miss / nonce reuse) |
| `DISK_LOW` | 503 | service temporarily read-only: low disk space | `httpx.ErrDiskLow`; `proxy.go:359` (diskguard `degraded()` true; `DISK_MIN_MB`/`DISK_MIN_PERCENT`) |
| `INTERNAL` | 500 | internal error | `WriteError` fallback (`errors.go:72`); `WriteErrorWith(...,"INTERNAL",...)` at `middleware.go:47` (panic recover), `install.go:166,177,188,199,211,268,279`, `proxy.go:321,547`, `models.go:74`, `quota/handler.go:38,53`; dashboard `export.go:30,46,52,59`, `installs.go:42,53,63,154` (own envelope) |
| `FORBIDDEN` | 403 | cross-origin requests are not allowed | `middleware.go:77` (`DenyCORS` — preflight `OPTIONS` with `Origin`) |
| **Dashboard-only** (own envelope) | | | |
| `UNAUTHENTICATED` | 401 | "login required" / "session invalid or expired" | `dashboard/http.go:73,78` (`requireSession`); `http.go:93` (`requireCSRF`, no session); `auth.go:117` (`handleSession`) |
| `CSRF_INVALID` | 403 | missing or invalid CSRF token | `dashboard/http.go:97` (`requireCSRF`, `X-CSRF-Token` mismatch via `constantTimeEq`) |
| `INVALID_CREDENTIALS` | 401 | invalid user or password | `dashboard/auth.go:80` (`handleLogin`; deliberately does not say which field, anti-enumeration) |
| `LOGIN_LOCKED` | 429 | too many attempts, retry later (+ `Retry-After` header + `details.retryAfterSec`) | `dashboard/auth.go:59` via `writeErrDetails` (per-IP exponential backoff lockout) |
| `CONFIG_REJECTED` | 400 | `err.Error()` (precise: unknown/secret/restart key, out-of-bounds, cross-field) | `dashboard/api.go:117` (`handleConfigUpdate` → `Cfg.ApplyOverrides`) |
| `INSTALL_NOT_FOUND` | 404 | no install with that id | `dashboard/installs.go:162` (ban/unban target missing) |

---

## 3. Audit-only / non-wire codes (recorded, NOT written to HTTP body)

These appear in greps but are **never** emitted via `WriteError`/`writeErr`. They are recorded into the audit/metrics record (`auditRecord.fail(status, code)` / `rec.errorCode`) for observability. The rewrite must keep them as audit labels, not envelope codes.

| Code | Recorded status | Where | Why not on wire |
|---|---|---|---|
| `CLIENT_CANCELED` | 499 | `proxy.go:451` `rec.fail(499, "CLIENT_CANCELED")` | client gave up while queued — *"nothing to write, just release quota"* (`outcomeRollback`) |

`errWireCode`/`errStatusCode` (`internal/proxy/audit.go:15,23`) extract `.Code`/`.Status` from an `*APIError` for audit, defaulting to `INTERNAL`/500 — they feed `rec.fail`, not the wire.

---

## 4. NOT wire codes (excluded — disambiguation for the rewrite)

Greps for UPPER_SNAKE literals surface many false positives; these are explicitly **not** error envelope codes:

- **Frontend-only** (TypeScript SPA, never server-emitted): `NETWORK_ERROR` (`internal/dashboard/ui/src/lib/api.ts:83` — `new ApiError(0,'NETWORK_ERROR',...)`), plus minified vendor tokens in `internal/dashboard/ui/dist/...` (`RC_TABLE_KEY`, `ES2020`, `CODE_LOGIC_ERROR`, `CALC_UNIT`, `SELECT_ALL`, etc.).
- **Config/env var names**, not codes: `DAILY_SUBLIMIT`, `INSTALL_PER_IP_HOUR`, `INSTALL_GLOBAL_DAILY_CAP`, `INSTALL_POW_MODE`, `GLOBAL_DAILY_BUDGET_TOKENS`, `DISK_MIN_MB`, `INPUT_TOKEN_CAP`, `MONTHLY_QUOTA`, etc. (`internal/config/*`).
- **Startup validation error string** (boot-time `error.Error()` prefix, not an HTTP envelope): `CONFIG_POW_SECRET_REQUIRED` (`internal/config/config.go:667` — `INSTALL_POW_MODE` set without `INSTALL_POW_SECRET`).
- **Prometheus label**, not a code: `code` as a metric label dimension (`internal/metrics/metrics.go:101,105,259`, labels `{handler,method,code}`).

---

## 5. Endpoints that bypass the error envelope entirely

These return 503/200 with a **different JSON shape** (no `error.code`) — the rewrite must NOT wrap them in the envelope:

- `/readyz` (`health.go:153 ReadyHandler`): `readyResponse{DB, Upstream}` with values `ok`/`down`/`degraded`; status 503 if any of `!dbOK || !upOK || diskLow`. Comment: *"never leaks raw errors (only ok/down/degraded)"*.
- `/healthz` (`health.go:183 LiveHandler`): always `200`, literal body `{"status":"ok"}`; never touches DB/upstream (OBS-3 red line).
- `/metrics`: Prometheus exposition (not JSON).

---

## 6. Header invariants tied to error responses (preserve in rewrite)

- `X-Request-ID` echoed on every response (`middleware.go:37`; reused client value sanitized via `sanitizeRID` — whitelist `[a-zA-Z0-9_-]`, max 64 chars, else regenerated 8-byte hex). Panic path logs only `rid`+category, never `rec` (may embed the Authorization/key) → `500 INTERNAL`.
- `Retry-After` header:
  - Dashboard `LOGIN_LOCKED`: `auth.go:58` `w.Header().Set("Retry-After", strconv.Itoa(retry))` (delta-seconds) **in addition to** body `details.retryAfterSec`.
  - Proxy upstream backoff math honors upstream `Retry-After` internally (`proxy.go:742,748`, `parseRetryAfter`) but it drives retry/backoff timing, not necessarily a downstream header on the busy response.
- Success completion responses set `X-Quota-Limit` / `X-Quota-Reset` (`proxy.go:912,913`) — not error-related but part of the contract.

---

## 7. Mapping note for the from-scratch rewrite (Foryx layering)

In the new `anselm-gateway` tree (Foryx-style `internal/transport/httpapi/{response,middleware,handlers}`):

- The two duplicated envelopes (`httpx` + `dashboard`) should collapse into **one** `internal/transport/httpapi/response` package exposing `WriteError(w, *APIError)` + `WriteErrorWith(w, status, code, message)` + a `details map[string]any` variant (only `LOGIN_LOCKED` needs it). Keep `APIError{Status,Code,Message}` and the `{"error":{"code","message"[,"details"]}}` wire bytes **exactly**.
- Sentinel codes belong in `internal/domain` (stable wire contract) and must keep their **exact** status/code/message strings — these are the spec, not arbitrary.
- The `WriteError` fallback (`non-*APIError → 500 INTERNAL "internal error"`, no detail leak) is a hard invariant.
- `CLIENT_CANCELED` (499) stays an **audit** label in the app/observability layer, never the response layer.
- `/healthz`, `/readyz`, `/metrics` must keep their non-envelope shapes.

