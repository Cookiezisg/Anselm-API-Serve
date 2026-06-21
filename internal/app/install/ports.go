package install

import (
	"context"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/install"
)

// ConfigLoader is the runtime-config port: a snapshot of the live, atomically
// swapped Config. Satisfied structurally by *infra/configprovider.Provider. The
// use-case snapshots ONCE per call (h.cfg.Load()) and reads all gate knobs from
// that one value so a mid-call hot-edit cannot split a decision.
type ConfigLoader interface {
	Load() *config.Config
}

// Store is the install persistence port. The app declares it here and infra
// (installstore) satisfies it structurally — the app never imports infra. Every
// method is one atomic aggregate operation; *sql.Tx never crosses this boundary
// (ADR-005). The Sybil gates live INSIDE Issue's single transaction so a
// later-gate reject leaves NO earlier-gate counter consumed (B11), and disabled
// gates are skipped entirely so the dormant path writes only the installs row.
type Store interface {
	// Issue runs the enabled Sybil gates and, only if all admit, inserts a fresh
	// installs row — ALL in one BEGIN IMMEDIATE transaction. On a gate reject it
	// returns ok=false with the tripped Gate and writes nothing (atomic admit:
	// committed counters mean issuances, not attempts). It regenerates the install
	// id once on a UNIQUE collision (B13). gate counters prune stale windows
	// opportunistically inside the same tx (B8).
	Issue(ctx context.Context, p install.IssueParams) (install.IssueResult, error)

	// Lookup resolves a token hash to its install id+status from the read pool.
	// found=false on no row. It never refreshes last_seen (the throttled refresh
	// is a separate, app-gated write — B9).
	Lookup(ctx context.Context, tokenSHA256 string) (id string, status install.Status, found bool, err error)

	// RefreshLastSeen bumps installs.last_seen_at to now for id, DB-side throttled
	// (WHERE last_seen_at < now-interval). Best-effort: the app calls it only after
	// its in-Go LRU throttle says >interval elapsed, so the write pool is not hit
	// per auth (B9). A failure here must not fail auth.
	RefreshLastSeen(ctx context.Context, id string, now time.Time, interval time.Duration) error

	// InstallsToday returns today's (RESET_TZ) global issuance count from
	// install_global_rate, for the dashboard overview. A missing row reads as 0.
	InstallsToday(ctx context.Context, windowDay string) (int64, error)
}

// NonceUser is the PoW replay-once port (satisfied by *pkg/noncecache.Cache and
// re-exported by pkg/pow). Kept here so the app declares its own dependency.
type NonceUser interface {
	UseOnce(challenge string) bool
}

// InstallsCreatedCounter is the success counter port (gateway_installs_created_total).
// Optional: a nil collector makes Inc a no-op (tests, or metrics not wired).
type InstallsCreatedCounter interface {
	Inc()
}

// PoWCounter is the labeled PoW outcome counter port
// (gateway_install_pow_total{result}). Each request increments EXACTLY ONE
// mutually-exclusive label; shadow uses disjoint terminal labels (B12).
type PoWCounter interface {
	Inc(result string)
}
