// Package install is the install identity use-case: mint a brand-new token each
// call (never echo/merge by fingerprint), run the Sybil gate phase in fixed order
// (per-IP/hour → global-daily-cap → per-fp daily+cooldown) with each gate
// short-circuiting before any DB work when disabled, the off/shadow/enforce PoW
// gate, and the throttled token→install auth lookup. It declares its infra needs
// as ports (ports.go) and maps every reject to an apierr wire code; it imports
// domain + pkg only, never infra.
//
// 领号用例:每次新 token(绝不按 fingerprint 回显/合并)、固定序 Sybil 闸(关时
// DB 前短路、dormant 零成本)、PoW 三态闸、节流的鉴权查找;拒绝映射到 apierr。
package install

import (
	"context"
	"sync"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/idgen"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
)

// LastSeenRefreshInterval is the minimum gap between last_seen_at writes for one
// install. The app's in-Go LRU throttles BEFORE the write pool (B9) and the
// store's UPDATE carries a matching WHERE predicate as a backstop, so a burst of
// auth lookups for one install collapses to one write per interval.
const LastSeenRefreshInterval = 10 * time.Minute

// fpDailyCapDisabledSentinel is the non-binding daily-cap ceiling used when the
// per-fp daily cap is off but the cooldown is on: far above the per-fp daily max
// (1e6) so `count < cap` is never the binding predicate and only the cooldown
// governs. Bound as a query param by infra, never interpolated.
const fpDailyCapDisabledSentinel int64 = 1 << 62

// Service is the install use-case. All collaborators are ports (interfaces) so
// infra is injected, not imported.
type Service struct {
	cfg    ConfigLoader
	store  Store
	nonces NonceUser
	mxNew  InstallsCreatedCounter // optional; nil → no-op.
	mxPoW  PoWCounter             // optional; nil → no-op.

	// now is the clock for every window/cooldown boundary. Defaults to time.Now;
	// tests inject a fixed/stepped clock to drive day-rollover and cooldown
	// assertions deterministically.
	now func() time.Time

	// lastSeen is the in-Go refresh throttle (B9): a bounded map of install id →
	// last-refresh time. The hot path checks it BEFORE touching the write pool, so
	// the single serialized writer is never contended by cosmetic timestamp writes.
	lastSeenMu sync.Mutex
	lastSeen   map[string]time.Time
}

// New builds the install service. mxNew/mxPoW may be nil (tests, or metrics not
// wired) — every emission site nil-checks. nonces is the PoW replay guard; pass a
// real *noncecache.Cache in production (inert unless PoW mode is shadow/enforce).
func New(cfg ConfigLoader, store Store, nonces NonceUser, mxNew InstallsCreatedCounter, mxPoW PoWCounter) *Service {
	return &Service{
		cfg:      cfg,
		store:    store,
		nonces:   nonces,
		mxNew:    mxNew,
		mxPoW:    mxPoW,
		now:      time.Now,
		lastSeen: make(map[string]time.Time),
	}
}

// SetClock overrides the clock (tests only).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// IssueResultView is the success payload for a freshly issued install: the
// plaintext token (returned ONLY here, never stored or re-displayed), the monthly
// quota, and the next-month reset boundary. The transport renders it.
type IssueResultView struct {
	Token        string
	MonthlyQuota int64
	ResetAt      time.Time
}

// Issue mints a brand-new install: a fresh token (store keeps only SHA-256), a
// new install id, and a fresh quota pool — NEVER echoing or merging by
// fingerprint (GW-INV-12). ipKey is the caller-derived hashed, /64-collapsed
// client IP (pkg/clientip): the transport owns IP derivation (it sees the
// request), the app owns the gate orchestration. It resolves the per-gate
// enable/ceilings from one config snapshot (a disabled gate is not passed to the
// store, so its table is never touched — dormant zero-cost), then performs the
// gates + INSERT atomically via the store. A gate reject maps to its distinct
// apierr code; the transport emits the unsampled WARN audit.
func (s *Service) Issue(ctx context.Context, req install.Request, ipKey string, unmetered bool) (IssueResultView, install.Gate, *apierr.APIError) {
	cfg := s.cfg.Load()
	now := s.now().UTC()

	token := idgen.Token()
	p := install.IssueParams{
		InstallID:   idgen.InstallID(),
		TokenSHA256: install.HashToken(token),
		Fingerprint: req.Fingerprint,
		Client:      req.Client,
		Now:         now,
		Unmetered:   unmetered,
	}
	// Resolve the per-gate enable/ceilings from the snapshot — but ONLY when metered.
	// An unmetered (whitelisted) re-mint leaves every gate zero so the store runs no
	// Sybil gate at all (operator god-mode); the brand-new token + fresh quota pool
	// is still minted exactly as always (GW-INV-12 unchanged).
	if !unmetered {
		p.IPGate = install.IPGate{
			Key:        ipKey,
			WindowHour: now.In(cfg.Location).Format("2006-01-02T15"),
			Max:        cfg.InstallPerIPHour,
		}
		p.GlobalGate = s.globalGate(cfg, now)
		p.FPGate = s.fpGate(cfg, req.Fingerprint, now)
	}
	return s.issueWith(ctx, cfg, token, p)
}

// IsUnmetered reports whether the presented bearer token is on the operator
// unmetered allowlist (WHITELIST_TOKEN_SHA256). It is the SINGLE whitelist decision
// point: chat, quota, and the install handler all route their god-mode check HERE so
// the token hashing + the lookup live in one place and can never diverge. An empty
// token or an empty allowlist returns false WITHOUT hashing (dormant zero-cost).
func (s *Service) IsUnmetered(token string) bool {
	if token == "" {
		return false
	}
	cfg := s.cfg.Load()
	if !cfg.HasUnmetered() {
		return false
	}
	return cfg.IsUnmetered(install.HashToken(token))
}

// issueWith runs the atomic gates+insert and maps the outcome.
func (s *Service) issueWith(ctx context.Context, cfg *config.Config, token string, p install.IssueParams) (IssueResultView, install.Gate, *apierr.APIError) {
	res, err := s.store.Issue(ctx, p)
	if err != nil {
		return IssueResultView{}, install.GateNone, apierr.Internal()
	}
	if !res.Admitted {
		return IssueResultView{}, res.Gate, gateError(res.Gate)
	}
	if s.mxNew != nil {
		s.mxNew.Inc()
	}
	return IssueResultView{
		Token:        token,
		MonthlyQuota: cfg.MonthlyQuota,
		ResetAt:      nextMonthReset(p.Now, cfg.Location),
	}, install.GateNone, nil
}

// globalGate resolves the global daily coarse-valve params; Enabled false when the
// cap is <= 0 (the store then never touches install_global_rate).
func (s *Service) globalGate(cfg *config.Config, now time.Time) install.GlobalGate {
	if cfg.InstallGlobalDailyCap <= 0 {
		return install.GlobalGate{Enabled: false}
	}
	return install.GlobalGate{
		Enabled:   true,
		WindowDay: now.In(cfg.Location).Format("2006-01-02"),
		Cap:       cfg.InstallGlobalDailyCap,
	}
}

// fpGate resolves the per-fp daily+cooldown params. Disabled (both sub-gates 0)
// OR an empty fingerprint → Enabled false (never bucket an empty fp; the store
// writes no fp row). A disabled sub-gate is made non-binding: the daily cap uses
// a sentinel ceiling and the cooldown cutoff is set far in the past so any stored
// last_at is newer-or-equal only when the cooldown is real.
func (s *Service) fpGate(cfg *config.Config, fp string, now time.Time) install.FPGate {
	dailyCap := cfg.InstallPerFPDaily
	cooldown := cfg.InstallPerFPCooldownSec
	if (dailyCap <= 0 && cooldown <= 0) || fp == "" {
		return install.FPGate{Enabled: false}
	}

	g := install.FPGate{
		Enabled:   true,
		FPSHA256:  install.HashFingerprint(fp),
		WindowDay: now.In(cfg.Location).Format("2006-01-02"),
	}
	if dailyCap > 0 {
		g.Cap = dailyCap
	} else {
		g.Cap = fpDailyCapDisabledSentinel // daily cap off → never the binding predicate.
	}
	if cooldown > 0 {
		g.Cutoff = now.Add(-time.Duration(cooldown) * time.Second)
	} else {
		// cooldown off → cutoff far in the future so any stored last_at is older →
		// the cooldown predicate never blocks.
		g.Cutoff = now.AddDate(1000, 0, 0)
	}
	return g
}

// gateError maps a tripped gate to its DISTINCT wire code (GW-INV-20).
func gateError(g install.Gate) *apierr.APIError {
	switch g {
	case install.GateIP:
		return apierr.ErrInstallRateLimited
	case install.GateGlobal:
		return apierr.ErrInstallCapReached
	case install.GateFP:
		return apierr.ErrInstallFPLimited
	default:
		return apierr.Internal()
	}
}

// LookupInstall resolves a token to its (installID, status, found) for auth. It
// hashes the token, looks up the install, and opportunistically refreshes
// last_seen_at — but THROTTLED IN-GO (B9): a bounded LRU of last-refresh stamps
// gates the write so the single serialized writer sees at most one cosmetic write
// per install per LastSeenRefreshInterval, NOT a write per auth. A refresh failure
// never fails the lookup (a missed activity timestamp is purely cosmetic).
func (s *Service) LookupInstall(ctx context.Context, token string) (id string, status install.Status, found bool, err error) {
	if token == "" {
		return "", "", false, nil
	}
	id, status, found, err = s.store.Lookup(ctx, install.HashToken(token))
	if err != nil || !found {
		return id, status, found, err
	}
	now := s.now().UTC()
	if s.shouldRefresh(id, now) {
		_ = s.store.RefreshLastSeen(ctx, id, now, LastSeenRefreshInterval)
	}
	return id, status, true, nil
}

// shouldRefresh is the in-Go throttle gate: true (and records now) only when the
// last recorded refresh for id is older than LastSeenRefreshInterval. Bounded by
// opportunistic pruning of stale entries so the map cannot grow without limit.
func (s *Service) shouldRefresh(id string, now time.Time) bool {
	s.lastSeenMu.Lock()
	defer s.lastSeenMu.Unlock()

	if last, ok := s.lastSeen[id]; ok && now.Sub(last) < LastSeenRefreshInterval {
		return false
	}
	// Opportunistic prune: drop entries whose throttle window has fully lapsed so a
	// churn of distinct installs cannot grow the map unbounded.
	if len(s.lastSeen) >= lastSeenMapPruneAt {
		for k, v := range s.lastSeen {
			if now.Sub(v) >= LastSeenRefreshInterval {
				delete(s.lastSeen, k)
			}
		}
	}
	s.lastSeen[id] = now
	return true
}

// lastSeenMapPruneAt triggers an opportunistic prune of the in-Go throttle map.
const lastSeenMapPruneAt = 4096

// InstallsToday returns today's (RESET_TZ) global issuance count for the
// dashboard overview; 0 when the valve is disabled / no row yet.
func (s *Service) InstallsToday(ctx context.Context) (int64, error) {
	cfg := s.cfg.Load()
	day := s.now().In(cfg.Location).Format("2006-01-02")
	return s.store.InstallsToday(ctx, day)
}

// nextMonthReset is the start of next month in loc — the quota reset boundary.
func nextMonthReset(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
}

// auditFields are the GW-INV-20 audit fields for a gate reject — only the ip_key,
// gate, and wire code; NEVER the fingerprint plaintext or any secret. Exposed so
// the transport emits the unsampled WARN with a consistent shape.
func AuditReject(ctx context.Context, ipKey string, gate install.Gate, code string) {
	logx.From(ctx).Warn("security_event", "event", "install_audit",
		"endpoint", "/v1/install", "ip_key", ipKey, "gate", string(gate),
		"error_code", code, "outcome", "reject")
}
