---
id: DOC-026
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/overview.md
---

# 动态行为/状态机 + 14-bug 免疫规则(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

# Anselm-Gateway — Dynamic Behavior Spec (for from-scratch rewrite)

Source of truth: the pre-rewrite tree at `<repo>`. Every identifier, field name, and number below is quoted exactly from that code. The rewrite (module `anselm-gateway`, Foryx-style clean architecture) MUST satisfy each invariant here.

Key source files read:
- `internal/proxy/proxy.go`, `reliability.go`, `transport.go`, `throttle.go`, `audit.go`, `estimate.go`, `usage.go`
- `internal/quota/quota.go`
- `internal/install/install.go`, `pow.go`
- `internal/diskguard/diskguard.go`
- `internal/config/config.go` (PoW enum + secret invariant), `internal/httpx/errors.go` (wire codes)
- `REVIEW-AND-PLAN.md` (bugs B0–B16)

---

## 1. State Machines

### 1.1 Quota reservation lifecycle (`internal/quota/quota.go`)

The accounting core gates **three guardrails** in a single `BEGIN IMMEDIATE` write transaction. The write pool is `MaxOpenConns=1` / DSN `_txlock=immediate`, so all writes serialize — read-modify-write cannot interleave. A `Period{Month "2006-01", Day "2006-01-02"}` is snapshotted ONCE at request entry (`SnapshotPeriod`, in `cfg.Load().Location` tz) and reused across the whole lifecycle (never recomputed → survives midnight rollover / concurrency).

States: `(none) → RESERVED → SETTLED | ROLLED_BACK | RECONCILED`.

```
                    Reserve(installID, est, period)
                    [single BEGIN IMMEDIATE tx]
                         |
   gate 1: usage.count+1 WHERE count < MonthlyQuota ──fail(n==0)──► ErrQuotaExhausted (429 QUOTA_EXHAUSTED)
   gate 2: usage.tokens+est WHERE tokens+est <= InstallDailyTokenCap ──fail──► ErrRateLimited (429 RATE_LIMITED)
   gate 2b: IF DailySublimit>0: day-row count+1 WHERE count < DailySublimit ──fail──► ErrRateLimited
   gate 3: budget.tokens_used+est, requests+1 WHERE used+est <= GlobalDailyBudget ──fail──► ErrBudgetExhausted (402 BUDGET_EXHAUSTED)
   step 4: INSERT ledger(request_id, install_id, period_day, reserved=est, settled=NULL, created_at=now UTC)
                         |
                      Commit  ─────────────────────────────────────────► RESERVED
                                       Reservation{RequestID, InstallID, Period, Reserved=est}
```

Any gate's `RowsAffected()==0` ⇒ whole tx ROLLBACK (deferred `tx.Rollback()` when `!committed`) + matching `APIError`. `RequestID = "req_" + hex(8 random bytes)` (`newRequestID`).

From RESERVED, exactly one terminal transition fires:

**SETTLE** (`Settle(ctx, r, actual)`): output happened; reconcile reserved vs actual.
- `actual` floored at 0; `delta = r.Reserved - actual` (`>0` refund, `<0` top-up debit).
- Idempotency guard: `UPDATE ledger SET settled=? WHERE request_id=? AND settled IS NULL`. If `RowsAffected==0` ⇒ already settled/reconciled ⇒ commit, no balance change.
- If `delta != 0`: `budget.tokens_used -= delta WHERE period=Day` AND `usage.tokens -= delta WHERE install_id, period=Day`.
- NOTE: Settle adjusts only budget + install-day-tokens — it does NOT touch the monthly `count` (count is KEPT once output started, anti-stream-abuse §7.2).

**ROLLBACK** (`Rollback(ctx, r)`): pre-output failure; reverse ALL THREE (+ optional sublimit).
- Guard: `UPDATE ledger SET settled=0 WHERE request_id=? AND settled IS NULL`. `RowsAffected==0` ⇒ commit, no-op.
- Reverse: (1) `budget.tokens_used -= Reserved, requests -= 1`; (2) `usage.tokens -= Reserved` (install day); (3) `usage.count -= 1` (month); (4) IF `s.cfg.Load().DailySublimit > 0`: `usage.count -= 1` (day row).
- **B1 LIVE-CONFIG BUG HERE**: step (4) reads a FRESH `s.cfg.Load().DailySublimit` instead of a flag recorded at reserve time → drift under hot-reload (see §4 / B1).

**RECONCILE-ORPHAN** (`ReconcileOrphans(ctx, older)`): background loop sweeps crash-left rows.
- Reads (read pool) `ledger WHERE settled IS NULL AND created_at < now-older`.
- Per orphan, own write tx: `UPDATE ledger SET settled=reserved WHERE request_id=? AND settled IS NULL`; if raced (`RowsAffected==0`) skip. Else `budget.tokens_used -= reserved` AND `usage.tokens -= reserved` (NOT count — the +1 is kept).
- **B2 INTERACTION (P2)**: a *failed* Settle leaves `settled IS NULL`; the orphan scanner then refunds the *full reserved*, so a failed top-up (actual>est) under-bills by the entire `actual`. The detached `settle()`/`rollback()` goroutines do `_ = h.q.Settle(...)` / `_ = h.q.Rollback(...)` — error swallowed; `rec.outcome` set to `outcomeSettle` BEFORE the goroutine runs ⇒ audit reports success regardless.

Observability: `OpenReservations` = `COUNT(*) WHERE settled IS NULL` → `gateway_quota_reservations_open` gauge (OBS-2 missed-settle alarm).

Detachment: proxy runs Settle/Rollback on `detached(parent) = context.WithoutCancel(parent)` in goroutines tracked by a shared `*sync.WaitGroup` so graceful shutdown waits for accounting before closing the DB (REL-4 red-line: never close DB mid-settle).

**Invariants the rewrite MUST keep:** single `BEGIN IMMEDIATE`; entry-snapshot `Period`; `settled IS NULL` idempotency on Settle/Rollback/Reconcile; conservative bias (crash ⇒ over-charge, never under-charge, EXCEPT the B2 hole which must be closed); monthly count kept once output starts.

---

### 1.2 Circuit breaker — process-wide (`internal/proxy/reliability.go` `newUpstreamBreaker`, sony/gobreaker/v2)

Three states (`gobreaker`): **Closed → Open → Half-Open → Closed/Open**.

Settings (exact): `Name="deepseek-upstream"`, `MaxRequests=1` (half-open single probe), `Interval=30s` (rolling failure-counter window), `Timeout=10s` (Open→Half-Open delay).

`ReadyToTrip` (Closed→Open):
```
ConsecutiveFailures >= 5  → trip
OR (Requests >= 10 AND TotalFailures/Requests > 0.5) → trip
```

State usage in proxy:
- Fast-path shed (`proxy.go:431`): if `breaker.State()==StateOpen` ⇒ rollback reservation, write `ErrUpstreamBusy` (429 UPSTREAM_BUSY), `rec.outcome=outcomeBusy`, mark `outcomeBusy`, **WITHOUT taking an N_global slot** (red-line: breaker-shed must not occupy account-level concurrency).
- Through-path: `breaker.Execute(func)` wraps connect→first-byte; on Open `Execute` returns `gobreaker.ErrOpenState` / `ErrTooManyRequests` ⇒ also UPSTREAM_BUSY.
- `OnStateChange` ⇒ WARN `upstream_breaker_state_change` + `metrics.BreakerState.Set(...)`.

**What counts as a fault** (`breakerFault=true` → returns `errUpstreamFault`, ONE failure per request attempt-set):
- Transient connect/TLS/transport error that is a real timeout (`timedOut.get()` || `context.DeadlineExceeded` || `isTimeout(err)`) → `breakerFault=true`, 504.
- Generic connect/transport error (current code, the B5 site) → `breakerFault=true`, 502.
- Upstream non-2xx: `504` → fault; `retryableStatus`∈{502,503,504} → fault; other non-2xx (e.g. 400) → fault (502).
- Stream Peek failure → fault.

**What MUST be EXCLUDED (never a fault):**
- **Client cancel / disconnect** (`context.Canceled` with `ctx.Err()!=nil` and not a timeout). *Currently MIS-counted → B5/B3 DoS amplifier.*
- **429 (UPSTREAM_BUSY)** — its own class, `breakerFault=false`, not retryable.
- **Per-key account signal (401/403)** — `breakerFault=false` (isolated to the per-key breaker, REL-3).
- **errKeyOpen** (a single key's breaker open) — `breakerFault=false`, retryable so failover rotates keys.
- **Post-output disconnects** — never reach the breaker (it lives entirely in the pre-output window).

State value mapping for the gauge via `metrics.BreakerStateValue(to)`.

### 1.2b Circuit breaker — per-key (`internal/proxy/transport.go` `newKeyBreaker`)

One `gobreaker.CircuitBreaker[*http.Response]` per configured DeepSeek key + a `cooldownUntil` deadline. Settings: `Name="deepseek-key"`, `MaxRequests=1`, `Interval=30s`, `Timeout=20s`, same `ReadyToTrip` (≥5 consecutive OR >50% over ≥10).

`pickKey(now)`: round-robin cursor `rr`, skip keys `onCooldown(now)` or `State()==StateOpen`; if all gated, return the first as least-bad fallback.

`accountLevelFault` decisions (what trips a key vs not):
- 401/403 → `setCooldown(now+10m)`, WARN `upstream_key_cooldown reason=auth`, `KeyCooldowns.Inc()`, returns `true` (counts as breaker failure).
- 429 → NOT a key fault; but a 429 with `Retry-After > 5s` → `setCooldown(now+d)` (per-key rate pause), returns `false`.
- `>= 500` → returns `true` (counted toward per-key breaker).
- else → `false`.

Open/`ErrTooManyRequests` from a key's Execute surfaces as `errKeyOpen` to the retry layer (retryable, NOT process fault). `errAccountFault` returns the response to the caller for normalization while still recording the per-key failure. Key material is injected on a `req.Clone()` (`Authorization: Bearer <key>`) and NEVER on the caller's request, NEVER logged (only `key_index`).

---

### 1.3 Per-token anomaly throttle (`internal/proxy/throttle.go` `anomalyTracker`)

Reversible per-install slow-down BEFORE any ban. Default **dormant**: `TOKEN_ANOMALY_RPM <= 0` ⇒ `Observe` short-circuits `return false` with zero allocation/lock.

```
Observe(installID):
  c = cfg.Load(); rpm = c.TokenAnomalyRPM
  if rpm <= 0: return false                          # DORMANT
  prune window stamps older than now-60s; append now  # sliding 60s tally, LRU-bounded at max=16384
  if installID in throttledUntil:
     if now < until: return true                      # still cooling; don't re-fire metric
     else delete(throttledUntil); republish gauge     # cooldown elapsed
  if len(stamps) <= rpm: return false                 # NORMAL
  if c.RatePerMin <= 0: return false                  # can't land a throttle on a disabled limiter; no bookkeeping
  # ANOMALY: TRIP
  throttledRate = max(1, c.RatePerMin / c.TokenThrottleFactor)   # FACTOR bounded [1,1000]
  until = now + TokenThrottleCooldownSec seconds
  rl.SetKeyLimit(installID, throttledRate, until)      # tighten the SAME shared limiter bucket (B1)
  throttledUntil[installID] = until; republish gauge; TokenThrottled.Inc()
  return true
```

Critical design rules:
- **Same bucket, not a second limiter.** Tightening reuses `h.rl` (the per-install minute gate). A fresh limiter would give the abuser a brand-new FULL window — the exact key being slowed (reviewer B1). The rewrite MUST tighten the existing bucket.
- **Privacy zero-body.** Holds only opaque `installID` + nano stamps — never prompt/token text.
- **Bounded memory.** `container/list` LRU pinned at `defaultAnomalyTrackerMax = 16384`; `evictLocked` drops LRU windows AND their `throttledUntil` (shared lifecycle, no leak under churn).
- **Gauge `gateway_tokens_throttled`** mirrors `len(throttledUntil)`. `Sweep()` (off hot path, called by metrics-refresh via `TokensThrottledNow()`) prunes expired so the gauge auto-decays without traffic.
- Throttled hit ⇒ `rec.throttled=true` ⇒ unsampled WARN security event.

---

### 1.4 PoW three-state (`internal/install/pow.go` + `install.go powGate/verifyPoW`, config enum)

Enum (`config.go`): `PowModeOff="off"` (default), `PowModeShadow="shadow"`, `PowModeEnforce="enforce"`; `validatePowMode` fail-fasts on anything else. **Strong-consistency invariant** (`validatePowSecretPresent`, on BOTH Load and hot-override): effective mode ∈ {shadow, enforce} REQUIRES non-empty env-only `INSTALL_POW_SECRET`, else fail-fast / reject override. mode=off needs no secret.

`powGate(r)` admit/reject by mode:
```
mode != shadow AND mode != enforce  → ADMIT (dormant: no header read, no metric)   # "" / off / invalid all dormant
X-PoW empty:
    powMetric("missing")
    enforce → REJECT ErrInstallPoWRequired (403 INSTALL_POW_REQUIRED)
    shadow  → powMetric("shadow_pass"); ADMIT          # [B12: double-labels "missing"+"shadow_pass"]
verifyPoW(cfg, hdr) == true:
    powMetric("verified"); ADMIT
verifyPoW == false (present but invalid):
    powMetric("failed")
    enforce → REJECT ErrInstallPoWInvalid (403 INSTALL_POW_INVALID)
    shadow  → powMetric("shadow_pass") + WARN install_pow_shadow outcome=shadow_pass_invalid; ADMIT  # [B12 again]
```

**Verify order (MUST be exactly this, cheapest-forgery-defense first; single-use mark LAST):**
```
verifyPoW(cfg, hdr):
  1. parsePoWHeader(hdr) → (challenge, nonce, ok)        # SplitN on first '.'; reject empty/missing-sep
     not ok → false
  2. verifyChallenge(secret, challenge, now):            # HMAC  (constant-time) THEN freshness
        - base64url decode; require len == powRawLen (40 = 16 rand + 8 ts + 16 mac)
        - recompute HMAC-SHA256(secret, rand||ts), subtle.ConstantTimeCompare truncated to 16 bytes
        - ts = bigendian unix secs; reject |now - issued| > powChallengeTTL (120s)  [both future & past]
     fail → false
  3. meetsDifficulty(challenge, nonce, cfg.InstallPowDifficulty):  # DIFFICULTY
        - SHA256(challenge + "." + nonce); leadingZeroBits >= difficulty
     fail → false
  4. nonces.UseOnce(challenge):                          # NONCE-ONCE (B4) — LAST
        - first call true (admit + consume); replay within TTL false
     fail → false
  → true (all pass)
```
Rationale enforced by ordering: HMAC rejects a forged challenge before any SHA hashing; nonce is burned ONLY on a fully-valid solve (a request failing an earlier check never consumes the challenge). Challenge is **stateless** (no DB/table): `base64url(random16 || unixTs(8) || HMAC(secret, random16||ts)[:16])`. Challenge endpoint shares the per-IP install bucket (`checkAndBumpIPRate`) so it can't be hammered to mint challenges (O3); `Required: mode==enforce`.

---

### 1.5 Disk-degrade (`internal/diskguard/diskguard.go`, REL-6)

Two states: **Normal ⇄ Degraded** (atomic `degraded` flag, lock-free hot-path read).

```
Check():
  free,total,err = statfs(path)
  err != nil → WARN diskguard_probe_failed; LEAVE state unchanged    # FAIL-OPEN (no wedge on probe glitch)
  low = (minBytes>0 AND free<minBytes)
        OR (minPercent>0 AND total>0 AND free*100/total < minPercent)
  prev = degraded.Swap(low)
  low && !prev → WARN disk_low_degraded (enter)
  !low && prev → INFO disk_recovered (leave)                          # AUTO-CLEAR on recovery
```

- Floors `minBytes`/`minPercent` are atomics, hot-swappable via `SetFloors` (DISK_MIN_MB / DISK_MIN_PERCENT); `0` disables each.
- Starts `false` (optimistic) — `main` MUST call `Check()` synchronously before serving (else first request passes on a full disk); `Run` also primes via `Check()` before its first tick (`interval` default 30s), drains on ctx cancel (on bgWG / REL-4).
- `statfsReal` uses `Bavail` (unprivileged free), saturates uint64→int64.
- Consumed by proxy: when `h.degraded()` is true, shed with `ErrDiskLow` (503 DISK_LOW) **before any quota reservation** (DB write) — prevents SQLite silently failing mid-write and corrupting conservative accounting.

---

## 2. The EXACT `/v1/chat/completions` gate order (cheapest-first)

From `proxy.go ServeHTTP` → `forward` → `tryOnce`. Every step rejects before the next, so the most expensive work (DB write, upstream call) is reached only after the cheap gates pass. `cfg = h.cfg.Load()` is snapshotted ONCE (step 3b) and reused for all guardrails + model resolution within the request.

| # | Gate | Check | Reject (status / wire code) |
|---|------|-------|------------------------------|
| 0 | Method | `r.Method == POST` | 400 BAD_REQUEST |
| 1 | Auth — bearer | `httpx.Bearer(r) != ""` | 401 INVALID_TOKEN |
| 1b | Auth — lookup | `authFn(ctx, token)`; `err` → 500 INTERNAL; `!found` → 401; `status=="banned"` → reject | 401 INVALID_TOKEN / 403 ACCOUNT_BANNED / 500 INTERNAL |
| 2 | Anomaly observe + rate limit | `throttle.Observe(installID)` (sets `rec.throttled`); then `rl.Allow(installID)` (per-install minute bucket, in-memory) | 429 RATE_LIMITED |
| 2b | Disk degrade (REL-6) | `degraded == nil \|\| !degraded()` — BEFORE any reservation | 503 DISK_LOW |
| 3 | Body read + decode | `io.ReadAll(MaxBytesReader(.., bodyLimit=256*1024))`; `decodeInbound` (raw n>1 probe + strict whitelist decode + typed n>1); non-empty `messages` | 400 BAD_REQUEST (incl. "n>1 is not allowed", "messages is required") |
| 3b | SEC-1 shape | `checkMessageShape(messages, MaxMessages, MaxMessageChars)` — array len ≤ MaxMessages, each content runes ≤ MaxMessageChars | 400 BAD_REQUEST ("too many messages" / "message content too large") |
| 4 | Input token cap | `estimatePromptTokens(messages) <= InputTokenCap` (conservative: max(bytes/3, runes)+8/msg, ×1.2 ceil) | 400 BAD_REQUEST ("input too large") |
| 5 | Build upstream body | `resolveModel(cfg, in.Model)` (force to allowlist, default = `cfg.DefaultModel`), `clampMaxTokens(in.MaxTokens, MaxTokensCap)`, `sanitizeUpstream` (whitelist + inject `stream_options.include_usage` on stream). `est = promptEst + maxTok` | (no reject — pure transform) |
| 6 | Quota reserve | `q.Reserve(ctx, installID, est, period)` (the 3-guardrail tx, §1.1) | 429 QUOTA_EXHAUSTED / 429 RATE_LIMITED / 402 BUDGET_EXHAUSTED / 500 |
| 6b | Breaker fast-path (REL-2) | `breaker.State() != StateOpen`; if Open ⇒ rollback resv, **no N_global slot** | 429 UPSTREAM_BUSY |
| 7 | N_global semaphore (REL-7) | `acquireSlot(ctx)`: take free slot immediately, else wait up to `QueueWait`; on fail → rollback resv; `ctx.Err()!=nil` ⇒ 499 CLIENT_CANCELED (no busy charge), else 429 UPSTREAM_BUSY. cap stays `N_global` (queue never amplifies concurrency); `QueueWait=0` ⇒ binary immediate reject | 429 UPSTREAM_BUSY / 499 CLIENT_CANCELED |
| 8 | Forward (`forward`→`breaker.Execute`→`attemptUpstream`→`tryOnce`) | connect→first-byte with bounded retry (maxAttempts=3, base 200ms, cap 3s, full jitter; retry only pre-output, only transient 502/503/504/connect; NOT 429), first-byte timer = `UpstreamHeaderTimeout`; on first byte (stream Peek) / 2xx (non-stream) ⇒ `outputStarted=true` | normalized 429/502/504; REL-5 deferred rollback if `!outputStarted` |
| 9 | Relay + settle | stream: SSE frames to `[DONE]`, parse `usage.total_tokens` (`include_usage`), Settle on actual (or full est if no usage / disconnect). non-stream: `io.ReadAll(LimitReader 8MiB)`, Settle on parsed usage. Count KEPT once output started | — |

After slot acquire: `setInflight(len(h.sem))` gauge; `defer { <-h.sem; setInflight }` releases. Whole request emits ONE audit line at finalization (`rec.emit`, INFO 10%-sampled, WARN never sampled for security outcomes).

---

## 3. Bug → design-rule immunity table (14 confirmed bugs)

For each reviewed bug, the construction rule that makes the rewrite immune. (B0 is repo-integrity, included since the plan tracks 14 confirmed; B3 folds into B5; "14 confirmed" = B0,B1,B2,B5,B6,B8,B9,B11,B12,B13,B14,B15,B16 + B3 as the subset.)

| Bug | Defect (code-exact) | Design rule that makes the rewrite immune by construction |
|-----|---------------------|------------------------------------------------------------|
| **B0** Clean checkout doesn't compile | `web.go:23` `//go:embed all:ui/dist` but `internal/dashboard/ui/` is 0 tracked files | Embed targets MUST be committed (or built in CI before the Go job) with a CI drift-guard (`npm ci && npm run build && git diff --exit-code`). Fresh-clone `go build ./...` is a CI gate. No green build that depends on local disk state. |
| **B5/B3** Client-cancel trips PROCESS breaker (DoS amplifier) | `proxy.go:736` returns `breakerFault:true` for generic `client.Do` error incl. `context.Canceled`; backoff select `:681-682` hardcodes `breakerFault=true` on `<-ctx.Done()`; same at stream Peek `:787-790` | Fault classification is a single typed `faultClass`, computed in one place, that **excludes client-cancel by construction**: `errors.Is(err, context.Canceled) && ctx.Err()!=nil && !timedOut` ⇒ class = ClientCancel (499 CLIENT_CANCELED, retryable=false, breakerFault=false). Backoff `<-ctx.Done()` ⇒ ClientCancel, not fault. Belt-and-suspenders: both breakers' `gobreaker.Settings.IsExcluded` excludes `context.Canceled`. Only {5xx, timeout, connect} ever increment the breaker. |
| **B1** DailySublimit hot-reload between Reserve & Rollback corrupts day counter | `Reserve` gates `count+1` on snapshot `cfg` (`:77`), `Rollback` gates `count-1` on fresh `s.cfg.Load()` (`:275`); `Reservation` records no flag | The `Reservation` carries an explicit `SublimitApplied bool` set true ONLY when the gated +1 actually executed (after its `RowsAffected` check). Rollback reverses the +1 iff `r.SublimitApplied` — never re-reads live config. Same entry-snapshot discipline already used for `Period`, applied to every conditional reservation. Each reservation reverses exactly what it took. |
| **B2** Swallowed Settle/Rollback errors; orphan refunds full reserved → under-bill | `proxy.go:893/904` `_ = h.q.Settle/Rollback`; `rec.outcome` set before goroutine; no failure metric | Settle/Rollback errors are captured in the accounting layer; non-nil ⇒ low-cardinality counters `SettleFailures`/`RollbackFailures` + unsampled WARN (request_id/install_id/period/err) emitted AFTER the audit line. Outcome is recorded from the actual result, not pre-set. A failed settle is visible, not silently reconciled as a full release. |
| **B6** First-byte timer `Stop()` races AfterFunc on success → truncation / spurious 502 | `tryOnce` `time.AfterFunc(UpstreamHeaderTimeout, cancelUp)`; `Stop()` doesn't await a running AfterFunc; success path may see `cancelUp` fire after commit (`:765-792`) | Disarming is idempotent and atomic: an `armed` flag; AfterFunc no-ops if already cleared (`if !armed.swapClear() { return }`); both success paths (Peek-ok, 2xx) call `armed.swapClear()` before returning. Output-start and timer-fire can never both act on the upstream context. |
| **B8** Sybil rate tables grow unbounded; `install_ip_rate` always-on leaker | `INSTALL_PER_IP_HOUR` bounded `[1,1e6]` (min 1, can't disable) ⇒ `checkAndBumpIPRate` INSERTs every install/challenge; zero `DELETE FROM` in source | Every persisted rate bucket has a bounded retention. Opportunistic prune inside the same bump tx (`DELETE ... WHERE ip_key=? AND window_hour < currentWindow`; lexical compare valid for `"2006-01-02T15"`), retaining ≥ current window. Only the current window is ever read; history is never accumulated. No schema change. |
| **B9** `refreshLastSeen` write-pool UPDATE on EVERY auth lookup | `install.go:613` unconditional call; `:626-630` `st.W` UPDATE runs every authed request (WHERE only throttles row modification, not the statement); serializes against quota on MaxOpenConns=1 | Hot-path throttle is in Go BEFORE touching the write pool: a bounded LRU of per-install last-refresh timestamps; `ExecContext` only when >10min elapsed. The single serialized writer is never contended by cosmetic timestamp writes. (roadmap U1) |
| **B14** Session cookie MaxAge fixed at login (12h absolute) | `auth.go:86-96` `Max-Age=43200` computed once at login; server slides `sess.expires` but never re-issues the cookie | Sliding TTL is delivered to the client: a single `setSessionCookie(w, id)` helper (`MaxAge=int(sessionTTL.Seconds())`) called from BOTH login AND every successful `requireSession` pass. Server-side and client-side TTL can't diverge. |
| **B15** Malformed login body consumes a lockout slot → credential-free DoS | `auth.go:49-68` `logins.attempt(ip)` runs BEFORE body decode; a 400 on bad body never calls `success()`, so the failure slot persists | Body decode + validation happens BEFORE the brute-force `attempt(ip)` gate. A malformed request returns 400 without ever reserving a failure slot. Do NOT call `success()` on the bad-request path (it would enable counter-reset abuse). Lockout slots are spent only on genuine credential attempts. |
| **B11** Install rejected by a later Sybil gate still consumed earlier gates' counters | `install.go:164-195` gates commit in independent sequential txns; INSERT is after all gates ⇒ IP/global counters bump even when fp gate rejects; dashboard surfaces them as issuances | Sybil gating is a read-only check phase for ALL gates, then a single commit-on-success bump after the INSERT succeeds (atomic admit). Counters mean "issuances", not "attempts past gate N". (Alt: dashboard reads `gateway_installs_created_total`.) Either way committed counters reflect only successful issuances. |
| **B12** Shadow PoW double-labels metric | `powGate` increments BOTH the outcome label (`missing`/`failed`) AND `shadow_pass` ⇒ `gateway_install_pow_total` labels overlap, `sum by(result)` over-counts | Exactly ONE disjoint label per request. Shadow outcomes get distinct terminal labels (`shadow_pass_missing` / `shadow_pass_invalid`), never an overlapping second Inc. `sum(result)` == request count by construction. |
| **B13** 64-bit install id, no conflict handling → collision = hard 500 | `newInstallID` uses 8 bytes; `installs` INSERT has no `ON CONFLICT` | Install ids are 16 bytes; INSERT has a single regenerate-on-conflict retry (idempotent issuance). A collision is recoverable, never a 500. (The `rand.Read` half is a non-issue: Go 1.24+ `crypto/rand.Read` never errors.) |
| **B16** Shared `rateSampler` overwritten by every overview poll | `dashboard/rate.go:37-49` one `rateSampler` per Server; ≥2 concurrent tabs corrupt the `dt` → halved/spiky QPS | Rate/QPS is computed server-side over a fixed sliding window in the metrics layer (no per-poll mutable sampler), or the sampler is keyed per session id. Concurrent readers can't corrupt each other's deltas. |

---

## Cross-cutting invariants to carry into the rewrite

1. **Conservative accounting**: reserve high (`promptEst + clampedMaxTokens`), settle on real usage, crash over-charges. The ONLY exception (B2 hole) must be closed — a failed settle must be observable, not silently reconciled to a full refund.
2. **N_global is an account-level hard cap** (`cap = N_global`): breaker-shed and queue-wait never occupy/inflate it.
3. **Pre-output rollback discipline (REL-5)**: a single deferred rollback fires iff `outputStarted == false`. Retry (REL-1) + breakers (REL-2/3) live entirely pre-output; produced output is never retried/double-billed; monthly count is kept once output starts (anti-stream-abuse §7.2).
4. **Detached + WaitGroup-tracked accounting (REL-4)**: Settle/Rollback run on `context.WithoutCancel(parent)` and are awaited before DB close.
5. **Dormant-by-default safety knobs**: `TOKEN_ANOMALY_RPM=0`, `DailySublimit=0`, `INSTALL_POW_MODE=off`, `INSTALL_GLOBAL_DAILY_CAP=0`, per-fp gates `0` — each must short-circuit with byte-for-byte unchanged behavior and zero added DB work.
6. **Privacy zero-body**: no prompt/token/key/fingerprint plaintext in logs/metrics/memory; fingerprints/tokens stored only as SHA-256; ip_key collapsed to /64; key material only ever `key_index`.
7. **Strong-consistency PoW**: effective shadow/enforce requires a present env secret on BOTH Load and hot-override (fail-fast / reject).
8. **Single config snapshot per request**: `cfg.Load()` once per request, reused across all guardrails + model resolution, so a hot-reload never applies half-old/half-new bounds within one request (the generalized form of the B1 fix).

