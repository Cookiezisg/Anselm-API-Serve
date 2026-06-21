---
id: DOC-022
type: working
status: superseded
owner: @weilin
created: 2026-06-20
reviewed: 2026-06-20
review-due: 2026-09-18
audience: [human, ai]
landed-into: ../references/backend/config.md
---

# 配置全表(抽取)

> 本轮 from-scratch 重写的**抽取契约**(白纸重写验收准绳)。per-slice 落地后转入 references/ 并填 landed-into。来源:旧 _legacy/ 代码 + AGENTS.md。

# Anselm Gateway — Complete Config Surface (SPEC extraction)

Authoritative source files (current pre-rewrite tree):
- `<repo>/internal/config/config.go` — `Load()`, all per-field bounds, `validateSemantics`, `validateMemoryBudget`, `WorstCaseMemoryMiB`, PoW helpers, `Snapshot`.
- `<repo>/internal/config/runtime.go` — `Provider` (atomic hot-swap), `ApplyOverrides`, `persistOverrides`, `Dump`/`DumpItem`. (Despite the filename, this file holds the Provider; the overlay/specs are in provider.go.)
- `<repo>/internal/config/provider.go` — `overrideSpec`/`overrideSpecs()`, `reqInt`/`reqInt64`, `applyOne`, `LoadWithOverlay`, `applyOverlay`, `clone`, `readSettings`.
- `<repo>/.env.example`
- `<repo>/internal/metrics/admin.go` — `requireLoopback` (the `ADMIN_ADDR` loopback rule is enforced at bind time, NOT in `config.Load`).

Tier legend:
- **runtime-hot** = `kindRuntime` in `overrideSpecs()`; dashboard-editable, persisted to `settings` table, atomically hot-reloaded. (`N_GLOBAL_CONCURRENCY` is `kindRuntime` but flagged `RestartRequired` — value persists/validates hot, semaphore capacity only re-takes effect on restart.)
- **secret-env-only** = never in `overrideSpecs()`, never persisted to `settings`, never in `Dump`/`Snapshot` true value (masked only).
- **startup-hard** = `kindRestart` in `overrideSpecs()` (or never registered at all); env-only, dashboard read-only, change requires restart. Includes the PERF-2 memory-budget self-check inputs (must never hot-reload or they bypass the worst-case RSS guard).

---

## 1. Full config key table

| key | tier | default | exact min/max bounds | one-line meaning |
|---|---|---|---|---|
| `DEEPSEEK_API_KEY` | secret-env-only | (none — required) | non-empty after comma-split+trim, else `fmt.Errorf("DEEPSEEK_API_KEY is required")` | DeepSeek upstream key(s); comma-separated multi-key, first is primary. |
| `DEEPSEEK_BASE_URL` | runtime-hot? **NO — not in registry → startup-hard/env-only** | `https://api.deepseek.com` (trailing `/` stripped via `TrimRight`) | none (string) | DeepSeek API base URL. |
| `MODEL_ALLOWLIST` | runtime-hot | (none — required) | non-empty list after comma-split+trim; override path: `len(list)==0` → `MODEL_ALLOWLIST must not be empty` | Real DeepSeek model ids; client `model` is force-rewritten to first entry (`DefaultModel`). String list, no numeric range (`bounded:false`). |
| `MONTHLY_QUOTA` | runtime-hot | `5000` | `[1, 1_000_000_000]` (`maxMonthlyQuota`) | Monthly per-install request-count quota (user-visible). |
| `GLOBAL_DAILY_BUDGET_TOKENS` | runtime-hot | `0` (but `0` is rejected → must be `>0`) | `[1, 1_000_000_000_000]` (`maxGlobalDailyBudget`); extra rule: `<=0` → "must be > 0 (the only wallet guardrail)" | Daily global token budget — the only wallet guardrail. |
| `INSTALL_DAILY_TOKEN_CAP` | runtime-hot | `0` (rejected → must be `>0`) | `[1, 1_000_000_000_000]` (`maxInstallDailyTokenCap`); extra: `<=0` → "must be > 0" | Per-install daily token sub-quota. |
| `MAX_TOKENS_CAP` | runtime-hot | `4096` | `[1, 1_000_000]` (`maxTokensCap`) | Per-request output token ceiling (clamp). |
| `INPUT_TOKEN_CAP` | runtime-hot | `16384` | `[1, 10_000_000]` (`maxInputTokenCap`) | Per-request conservative prompt-token estimate ceiling. |
| `MAX_MESSAGES` | runtime-hot | `256` | `[1, 100_000]` (`maxMessages`) | messages array length cap (OWASP API4 amplification guard). |
| `MAX_MESSAGE_CHARS` | runtime-hot | `131072` (128 KiB) | `[1, 16*1024*1024]` = `[1, 16777216]` (`maxMessageChars`) | Single message content char cap (rune count). |
| `N_GLOBAL_CONCURRENCY` | runtime-hot (restart-effective) | `8` | `[1, 100_000]` (`maxNGlobalConcurrency`) | Account-level in-flight hard cap; editable/persisted but semaphore resize needs restart. |
| `RATE_PER_MIN` | runtime-hot | `20` | `[0, 10_000_000]` (`maxRatePerMin`) — **floor is 0** | Per-install minute token-bucket size. |
| `DAILY_SUBLIMIT` | runtime-hot | `0` (=disabled) | `[0, 1_000_000_000]` (`maxDailySublimit`) | Optional per-install daily request sub-limit; 0=disabled. |
| `INSTALL_PER_IP_HOUR` | runtime-hot | `10` | `[1, 1_000_000]` (`maxInstallPerIPHour`) | /install per-IP hourly cap (Sybil). |
| `INSTALL_GLOBAL_DAILY_CAP` | runtime-hot | `0` (=disabled) | `[0, 100_000_000]` (`maxInstallGlobalDailyCap`) | Global daily /install issuance coarse valve; 0=disabled. |
| `INSTALL_PER_FP_DAILY` | runtime-hot | `0` (=disabled) | `[0, 1_000_000]` (`maxInstallPerFPDaily`) | Per-fingerprint daily issuance cap; 0=disabled (empty fp always passes). |
| `INSTALL_PER_FP_COOLDOWN_SEC` | runtime-hot | `0` (=disabled) | `[0, 86_400]` (`maxInstallPerFPCooldownSec`) | Per-fingerprint min inter-issuance interval (sec); 0=disabled. |
| `INSTALL_POW_MODE` | runtime-hot | `off` (`PowModeOff`) | closed enum `off\|shadow\|enforce` (`validatePowMode`); else `INSTALL_POW_MODE %q invalid`. String spec, `bounded:false`. |领号 PoW three-state: off (dormant) / shadow (verify-but-allow) / enforce (require+verify, else 403). |
| `INSTALL_POW_DIFFICULTY` | runtime-hot | `20` | `[1, 32]` (`maxInstallPowDifficulty`) | Required leading-zero **bits** of `SHA256(challenge.nonce)`. |
| `INSTALL_POW_SECRET` | secret-env-only | (empty → source `disabled`; set → source `configured`) | none (raw bytes); **never minted**; cross-field: required when mode≠off | Challenge-HMAC signing secret; env-only, never in settings/Dump/logs. |
| `TOKEN_ANOMALY_RPM` | runtime-hot | `0` (=disable whole auto-throttle) | `[0, 10_000_000]` (`maxTokenAnomalyRPM`) | Per-install anomaly RPM trip point; 0 disables auto-throttle. |
| `TOKEN_THROTTLE_FACTOR` | runtime-hot | `4` | `[1, 1000]` (`maxTokenThrottleFactor`) — only read when `TOKEN_ANOMALY_RPM>0` | Throttle divisor (throttled rate = RATE_PER_MIN/factor); 1=escape hatch. |
| `TOKEN_THROTTLE_COOLDOWN_SEC` | runtime-hot | `300` | `[1, 86_400]` (`maxTokenThrottleCooldownSec`) — only read when `TOKEN_ANOMALY_RPM>0` | Single throttle duration (sec) before auto-revert. |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | runtime-hot | `60` | `[1, 600]` (`maxUpstreamHeaderTimeoutSec`); stored as `time.Duration` ×`time.Second` | Upstream connect→header timeout; never covers streaming body. |
| `QUEUE_WAIT_MS` | runtime-hot | `1500` | `[0, 60_000]` (`maxQueueWaitMS`); stored ×`time.Millisecond` | REL-7 backpressure wait window when N_global full; 0=binary immediate reject. |
| `GOMEMLIMIT_MIB` | startup-hard (`kindRestart`) | `768` | `>= 0` (0=disable soft limit); `<0` → "must be >= 0 (0=disable)" | `debug.SetMemoryLimit` soft ceiling. |
| `SQLITE_CACHE_KIB` | startup-hard (`kindRestart`) | `32768` (32 MiB/conn) | `> 0`; `<=0` → "must be > 0" | Per-connection SQLite page cache (KiB). |
| `SQLITE_MMAP_MB` | startup-hard (`kindRestart`) | `256` | `>= 0` (0=disable mmap); `<0` → "must be >= 0 (0=disable mmap)". Stored as `SQLiteMmapBytes = mmapMB*1024*1024` | SQLite `mmap_size` (MB on env, bytes internally). |
| `SQLITE_WAL_AUTOCHECKPOINT` | **startup-hard (env-only; NOT in overrideSpecs)** | `4000` | `>= 0`; `<0` → "must be >= 0" | WAL auto-checkpoint trigger page count. Note: validated in Load but absent from `overrideSpecs()` → not even surfaced read-only in Dump. |
| `READ_POOL_MAX_CONNS` | startup-hard (`kindRestart`) | `4` | `> 0`; `<=0` → "must be > 0" | Read-only pool concurrency cap (each conn = one cache copy). |
| `MEM_BUDGET_MIB` | **startup-hard (env-only; NOT in overrideSpecs)** | `2048` | `> 0`; `<=0` → "must be > 0" | Total memory budget MiB (PERF-2 self-check). |
| `MEM_SAFETY_MARGIN_MIB` | **startup-hard (env-only; NOT in overrideSpecs)** | `400` | `>= 0`; `<0` → "must be >= 0" | Headroom reserved for OS/runtime bursts (PERF-2). |
| `DISK_MIN_MB` | runtime-hot | `500` | `[0, 1024*1024*1024]` = `[0, 1073741824]` (`maxDiskMinMB`) | REL-6 absolute free-disk floor MiB; below → read-only degrade 503 DISK_LOW. |
| `DISK_MIN_PERCENT` | runtime-hot | `5` | `[0, 100]` (hardcoded, not a const); `<0 \|\| >100` → "must be 0..100 (0=disable percent floor)" | Free-disk percent floor; 0=disable percent check. |
| `RESET_TZ` | startup-hard (`kindRestart`) | `Asia/Shanghai` | must resolve via `time.LoadLocation`; failure → **panic** (never UTC fallback) | Period-reset timezone (`Location`). |
| `LISTEN_ADDR` | startup-hard (`kindRestart`) | `127.0.0.1:8080` | must differ from DASHBOARD_ADDR (cross-field) | Business HTTP listener. |
| `ADMIN_ADDR` | startup-hard (`kindRestart`) | `127.0.0.1:9090` | must differ from DASHBOARD_ADDR; **must be loopback** — enforced at bind in `metrics/admin.go::requireLoopback`, not in Load | /metrics admin listener (loopback-only). |
| `DASHBOARD_ADDR` | startup-hard (`kindRestart`) | `127.0.0.1:8081` | must `!=` LISTEN_ADDR and `!=` ADMIN_ADDR (cross-field fail-fast) | Dashboard loopback listener (physically isolated). |
| `LOG_LEVEL` | **startup-hard (env-only; NOT in overrideSpecs)** | `info` | none (string; `debug\|info\|warn\|error` documented, not enforced in config) | slog level. |
| `GATEWAY_DB_PATH` | startup-hard (`kindRestart`) | `anselm-gateway.db` | none (string) | SQLite file path. |
| `DASHBOARD_USER` | secret-env-only | `""` | cross-field: both-or-neither with DASHBOARD_PASSWORD | Dashboard auth username. |
| `DASHBOARD_PASSWORD` | secret-env-only | `""` | cross-field: both-or-neither with DASHBOARD_USER | Dashboard auth password. |
| `DASHBOARD_DEV_INSECURE_COOKIE` | **startup-hard (env-only; NOT in overrideSpecs)** | `false` | `strconv.ParseBool`; invalid → "invalid bool" | DEV-ONLY: drop Secure flag on session cookie (never in prod). |

Notes on tiering edge cases (code-exact):
- `DEEPSEEK_BASE_URL`, `SQLITE_WAL_AUTOCHECKPOINT`, `MEM_BUDGET_MIB`, `MEM_SAFETY_MARGIN_MIB`, `LOG_LEVEL`, `DASHBOARD_DEV_INSECURE_COOKIE` are read in `Load()` but are **NOT present in `overrideSpecs()`** — they are env-only and do not even appear in the dashboard Dump (unlike `kindRestart` items, which are surfaced read-only). They are effectively startup-hard/env-only.
- The `kindRestart` entries actually registered in `overrideSpecs()` (surfaced read-only in Dump, writes rejected with "startup hard-constraint — change requires a restart, not hot-reload"): `GOMEMLIMIT_MIB`, `SQLITE_CACHE_KIB`, `READ_POOL_MAX_CONNS`, `SQLITE_MMAP_MB`, `ADMIN_ADDR`, `DASHBOARD_ADDR`, `LISTEN_ADDR`, `RESET_TZ`, `GATEWAY_DB_PATH`.
- Env parsing helpers: `getInt`/`getInt64` use `strings.TrimSpace`, empty→default, parse error → `%s: invalid int %q`; `getBool` uses `strconv.ParseBool`; `getStr` empty→default. Overlay path uses `reqInt`/`reqInt64` (same `boundInt`/`boundInt64` floor+ceiling).

---

## 2. All bound constants (config.go `const` block, code-exact)

| const | value |
|---|---|
| `maxMonthlyQuota` | `1_000_000_000` |
| `maxGlobalDailyBudget` | `1_000_000_000_000` |
| `maxInstallDailyTokenCap` | `1_000_000_000_000` |
| `maxTokensCap` | `1_000_000` |
| `maxInputTokenCap` | `10_000_000` |
| `maxMessages` | `100_000` |
| `maxMessageChars` | `16 * 1024 * 1024` (16777216) |
| `maxNGlobalConcurrency` | `100_000` |
| `maxRatePerMin` | `10_000_000` |
| `maxDailySublimit` | `1_000_000_000` |
| `maxInstallPerIPHour` | `1_000_000` |
| `maxInstallGlobalDailyCap` | `100_000_000` |
| `maxInstallPerFPDaily` | `1_000_000` |
| `maxInstallPerFPCooldownSec` | `86_400` |
| `maxInstallPowDifficulty` | `32` |
| `maxTokenAnomalyRPM` | `10_000_000` |
| `maxTokenThrottleFactor` | `1000` |
| `maxTokenThrottleCooldownSec` | `86_400` |
| `maxQueueWaitMS` | `60_000` |
| `maxUpstreamHeaderTimeoutSec` | `600` |
| `maxDiskMinMB` | `1024 * 1024 * 1024` (1073741824) |

PoW mode enum constants: `PowModeOff = "off"`, `PowModeShadow = "shadow"`, `PowModeEnforce = "enforce"`.

---

## 3. `validateSemantics` cross-field rules (exhaustive)

`(*Config).validateSemantics()` runs on the env Load path (end of `Load()`), on the startup overlay assembly (`applyOverlay` → `LoadWithOverlay`), AND on every hot override batch (`ApplyOverrides` after all `applyOne`). It enforces exactly these three rules, in this order:

1. **INSTALL_DAILY_TOKEN_CAP ≤ GLOBAL_DAILY_BUDGET_TOKENS**
   - Condition: `if c.InstallDailyTokenCap > c.GlobalDailyBudget` → error.
   - Error: `SEC-2 config: INSTALL_DAILY_TOKEN_CAP %d must be <= GLOBAL_DAILY_BUDGET_TOKENS %d (a single install must not be able to drain the whole daily wallet — the sub-cap would be meaningless)`.

2. **INPUT_TOKEN_CAP + MAX_TOKENS_CAP ≤ INSTALL_DAILY_TOKEN_CAP**
   - Condition: `maxPerReq := c.InputTokenCap + c.MaxTokensCap; if maxPerReq > c.InstallDailyTokenCap` → error.
   - Error: `SEC-2 config: INPUT_TOKEN_CAP %d + MAX_TOKENS_CAP %d = %d must be <= INSTALL_DAILY_TOKEN_CAP %d (else a single request's worst-case reservation always exceeds the install daily sub-cap and no call can ever succeed)`.
   - Rationale: worst-case per-request reservation = `promptEst + clampedMaxTokens` ≤ `INPUT_TOKEN_CAP + MAX_TOKENS_CAP`; if it exceeds the daily sub-cap even the day's first request always trips RATE_LIMITED.

3. **PoW secret present when effective mode ≠ off** (`validatePowSecretPresent(c.InstallPowMode, c.InstallPowSecret)`)
   - Condition: if `mode == PowModeShadow || mode == PowModeEnforce` and `len(secret) == 0` → error.
   - Error: `CONFIG_POW_SECRET_REQUIRED: INSTALL_POW_MODE %s requires INSTALL_POW_SECRET (env-only); set it and restart before enabling PoW`.
   - mode=off needs no secret (dormant).
   - This same predicate is ALSO run on the hot-edit path inside the `INSTALL_POW_MODE` spec's `apply` (`provider.go`): a hot flip to shadow/enforce while the base-inherited `c.InstallPowSecret` is empty is rejected there too. Because the secret is env-only (absent from `overrideSpecs`), the correct ops flow is: env-config secret + restart (secret enters base Config) → then hot-flip mode.

Additional cross-field / structural rules NOT inside `validateSemantics` but enforced in `Load()` (still fail-fast at startup):

- **DASHBOARD_ADDR ≠ LISTEN_ADDR**: `DASHBOARD_ADDR %q must not equal LISTEN_ADDR %q (the three listeners must be physically isolated)`.
- **DASHBOARD_ADDR ≠ ADMIN_ADDR**: `DASHBOARD_ADDR %q must not equal ADMIN_ADDR %q (the three listeners must be physically isolated)`.
- **DASHBOARD_USER ⇔ DASHBOARD_PASSWORD both-or-neither**: `if (c.DashboardUser == "") != (c.DashboardPassword == "")` → `DASHBOARD_USER and DASHBOARD_PASSWORD must be set together (or both empty)`.
- **INSTALL_POW_MODE in enum** (`validatePowMode`): typo → `INSTALL_POW_MODE %q invalid: must be one of off|shadow|enforce`.
- **GLOBAL_DAILY_BUDGET_TOKENS > 0** and **INSTALL_DAILY_TOKEN_CAP > 0** (positivity, distinct from the bound check).
- **RESET_TZ resolvable** → else `panic` (never silent UTC fallback; 蓝图 §7.1 红线).
- **ADMIN_ADDR loopback** — enforced at bind time in `metrics/admin.go::requireLoopback` (literal IP must `IsLoopback()`; hostname must resolve and EVERY resolved IP must be loopback; bare port / `0.0.0.0` rejected). Errors: `ADMIN_ADDR %q must bind a loopback host...`, `ADMIN_ADDR host %q is not loopback...`, `ADMIN_ADDR host %q does not resolve to a loopback address...`, `ADMIN_ADDR host %q resolves to non-loopback %s...`. This is NOT in `config.Load`.

---

## 4. PERF-2 worst-case-RSS formula (exact)

### Estimate — `(*Config).WorstCaseMemoryMiB() int`

```
cacheMiB := c.SQLiteCacheKiB / 1024
mmapMiB  := int(c.SQLiteMmapBytes / (1024 * 1024))
return c.GoMemLimitMiB + cacheMiB*(1 + c.ReadPoolMaxConns) + mmapMiB
```

Formula in words (the load-bearing multiplier):

```
worst-case RSS (MiB) = GOMEMLIMIT_MIB
                     + (SQLITE_CACHE_KIB/1024) × (1 + READ_POOL_MAX_CONNS)
                     + (SQLITE_MMAP_BYTES / 1MiB)
```

- The cache multiplier is **`(1 + READ_POOL_MAX_CONNS)`** — write pool 1 copy + read pool `READ_POOL_MAX_CONNS` copies — NOT the commonly-miscalculated `×2`. `cache_size` is per-connection.
- mmap is counted **once** (both pools share the same file pages).
- Default arithmetic: `768 + (32768/1024)*(1+4) + (256MiB/1MiB)` = `768 + 32*5 + 256` = `768 + 160 + 256` = **1184 MiB**.

### Guard — `(*Config).validateMemoryBudget() error`

```
worst   := c.WorstCaseMemoryMiB()
ceiling := c.MemBudgetMiB - c.MemSafetyMarginMiB    // default 2048 - 400 = 1648
if worst <= ceiling { return nil }                  // 1184 <= 1648 → OK by default
if c.GoMemLimitMiB == 0 {
    // heap unbounded → advisory only: WARN to stderr, return nil (NOT fail-fast)
}
return error   // PERF-2 memory budget exceeded
```

- Pass condition: **`WorstCaseMemoryMiB() ≤ MEM_BUDGET_MIB − MEM_SAFETY_MARGIN_MIB`**.
- When `GOMEMLIMIT_MIB == 0` (heap soft-limit disabled): the estimate is advisory; on overflow it WARN-logs to stderr and returns nil (never fail-fast). WARN text:
  `WARN config: worst-case memory %dMiB exceeds budget %dMiB - safety %dMiB = %dMiB, but GOMEMLIMIT_MIB=0 (heap unbounded) so this is advisory; size the box accordingly`.
- Otherwise (GOMEMLIMIT_MIB > 0) overflow → fail-fast error:
  `PERF-2 memory budget exceeded: worst-case RSS ≈ %dMiB (GOMEMLIMIT %d + cache %dMiB×(1+READ_POOL %d) + mmap %dMiB) > MEM_BUDGET_MIB %d - MEM_SAFETY_MARGIN_MIB %d = %dMiB; lower SQLITE_CACHE_KIB / READ_POOL_MAX_CONNS / GOMEMLIMIT_MIB / SQLITE_MMAP_MB`.
- This is why the four cache/mmap inputs (`GOMEMLIMIT_MIB`, `SQLITE_CACHE_KIB`, `READ_POOL_MAX_CONNS`, `SQLITE_MMAP_MB`) are `kindRestart` — hot-reloading any of them would bypass this self-check.

---

## 5. Hot-reload / overlay mechanics (for the rewrite)

- **Read path**: `Provider.Load()` is lock-free (`atomic.Pointer[Config]`); the request hot path reads `cfg.Load()` and may hold the immutable pointer for a whole request.
- **Write path**: `Provider.ApplyOverrides(ctx, map[string]string)` under `mu`: clone current → deterministic sorted-key apply via `applyOne` → `validateSemantics` → `persistOverrides` (single all-or-nothing `BeginTx`/`Commit` into `settings(key,value,updated_at)` via upsert `ON CONFLICT(key) DO UPDATE`) → `cur.Store(next)` → notify `OnReload` listeners. Persist happens BEFORE swap; nil DB (`config.Static`) skips persist but still swaps+notifies.
- **`applyOne` rejection classes**: unknown-or-secret key → `%s: not a runtime-overridable setting (unknown or secret; secrets are env-only)`; `kindRestart` key → `%s: startup hard-constraint — change requires a restart, not hot-reload`.
- **Startup assembly**: `LoadWithOverlay(db)` = `Load()` (env) → `readSettings` (`SELECT key,value FROM settings`) → `applyOverlay` (clone + sorted apply + `validateSemantics`). A corrupt/invalid persisted overlay fails fast (never silent fallback to env).
- **Bounds parity**: env Load and overlay `reqInt`/`reqInt64` share the exact same `boundInt`/`boundInt64` floors+ceilings (same `max*` consts), so the two paths can never diverge.
- **`Dump()`/`DumpItem`**: surfaces every `overrideSpecs()` item with live value, `Editable` (`kind==kindRuntime`), `RestartRequired` (`kind==kindRestart` OR `key=="N_GLOBAL_CONCURRENCY"`), and inclusive `Min`/`Max` only for `bounded` numeric runtime knobs (client pre-validation hint = same server bounds). Secrets never appear (absent from registry).
- **`Snapshot()`**: startup banner attrs; secrets masked — DeepSeek keys → `sk-*** (%d configured)`, PoW secret → `powSecretMasked()` (`configured`/`disabled`), dashboard auth → `dashboardAuthMasked()` (`disabled` / `configured (user+password set)`). Raw secret bytes never logged.

---

## 6. Default worst-case sanity (from .env.example defaults)

`GOMEMLIMIT_MIB=768`, `SQLITE_CACHE_KIB=32768`, `READ_POOL_MAX_CONNS=4`, `SQLITE_MMAP_MB=256`, `MEM_BUDGET_MIB=2048`, `MEM_SAFETY_MARGIN_MIB=400`:
`768 + 32×(1+4) + 256 = 1184 MiB ≤ 2048 − 400 = 1648 MiB` → passes self-check with ~464 MiB additional headroom above the safety margin.

