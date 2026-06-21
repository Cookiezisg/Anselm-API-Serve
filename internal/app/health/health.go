// Package health implements layered health policy: liveness (pure process,
// NEVER touches any dependency) and readiness (DB writability + cached upstream
// reachability + disk state aggregated into per-component states).
//
// GW-INV-13: /healthz must never touch the DB (DB jitter must NOT cause a
// restart storm); /readyz is loopback-only and surfaces dependency state.
// This is the app layer: pure aggregation policy over ports. The HTTP wire
// shape, the periodic upstream prober, and the disk probe all live in
// infra/bootstrap; Ready only READS the cached probe value fed via the ports.
package health

import (
	"context"
	"time"
)

// Component states reported per dependency in a Readiness. Kept as bare strings
// so transport can serialize them verbatim without re-mapping.
const (
	StateOK       = "ok"
	StateDegraded = "degraded"
	StateDown     = "down"
)

// DBChecker reports whether the DB can currently accept a write. Implemented in
// infra over the write/read pools; Ready never opens a transaction itself.
type DBChecker interface {
	// Writable returns nil when the DB is reachable and writable; any error
	// (collapsed to db:down) otherwise. Must honor ctx for a bounded probe.
	Writable(ctx context.Context) error
}

// UpstreamProbe exposes the CACHED background-probe result. Ready only reads it
// — it never triggers a live DeepSeek call (probing per request could pile onto
// or stall behind a struggling upstream).
type UpstreamProbe interface {
	// LastOK reports the last successful probe outcome and how long ago it was.
	// ok=false (never probed) or a stale age ⇒ upstream:degraded.
	LastOK() (ok bool, age time.Duration)
}

// DiskState reports REL-6 low-disk read-only degradation (GW-INV-29). Degraded
// disk means the gateway is voluntarily read-only, so the DB cannot be written.
type DiskState interface {
	Degraded() bool
}

// Readiness is the aggregated per-component readiness snapshot. OK is the
// overall verdict transport maps to 200 vs 503.
type Readiness struct {
	DB       string // ok | degraded | down
	Upstream string // ok | degraded
	Disk     string // ok | degraded
	OK       bool
}

// Checker aggregates the three readiness ports. db/upstream/disk may each be nil
// (treated as the conservative not-ready value) so partial wiring fails closed.
type Checker struct {
	db       DBChecker
	upstream UpstreamProbe
	disk     DiskState

	// freshFor bounds how old the cached upstream success may be before it
	// counts as degraded. Default 3× the prober interval (90s) is set by New.
	freshFor time.Duration
}

// New builds a Checker. freshFor ≤ 0 defaults to 90s (3× the 30s probe period):
// long enough to ride out a single missed probe, short enough to surface a real
// upstream outage promptly.
func New(db DBChecker, upstream UpstreamProbe, disk DiskState, freshFor time.Duration) *Checker {
	if freshFor <= 0 {
		freshFor = 90 * time.Second
	}
	return &Checker{db: db, upstream: upstream, disk: disk, freshFor: freshFor}
}

// Live is pure process liveness: ALWAYS ok, NEVER touches any dependency
// (GW-INV-13). It is a function, not a method, so it provably cannot reach the
// Checker's ports.
func Live() bool { return true }

// Ready aggregates the three components into a Readiness.
//
// Rules:
//   - disk degraded ⇒ db is reported degraded (REL-6 read-only reflects as
//     db:degraded) — even though the DB pings fine, we are voluntarily not
//     writable, so callers must see a non-ok DB.
//   - db unwritable ⇒ db:down (takes precedence over degraded).
//   - upstream stale/never-ok ⇒ upstream:degraded.
//   - OK is true only when no component is down AND no component is degraded;
//     transport maps !OK ⇒ 503.
func (c *Checker) Ready(ctx context.Context) Readiness {
	diskDegraded := c.disk == nil || c.disk.Degraded()

	dbState := StateDown
	if c.db != nil && c.db.Writable(ctx) == nil {
		// Writable, but a low-disk shed makes us read-only: report degraded so
		// monitoring sees the shed rather than a hard down.
		if diskDegraded {
			dbState = StateDegraded
		} else {
			dbState = StateOK
		}
	}

	upstreamState := StateDegraded
	if c.upstream != nil {
		if ok, age := c.upstream.LastOK(); ok && age <= c.freshFor {
			upstreamState = StateOK
		}
	}

	diskState := StateOK
	if diskDegraded {
		diskState = StateDegraded
	}

	ok := dbState == StateOK && upstreamState == StateOK && diskState == StateOK
	return Readiness{DB: dbState, Upstream: upstreamState, Disk: diskState, OK: ok}
}
