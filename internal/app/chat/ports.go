// Package chat is the chat-spend use case: it orchestrates the exact cheapest-
// first gate order and the reserve→forward→settle saga (spec-behaviors §2) over
// PORTS it declares here. It imports domain/* + pkg/* + these ports ONLY — never
// net/http, never infra. It drives the HTTP response through a tiny Sink so it
// stays net/http-free and unit-testable (a fake Sink + fake ports cover every
// branch). The transport layer adapts http.ResponseWriter into a Sink and wires
// the concrete ports (quota.Service, upstream.Client, ratelimit.RateLimiter, …).
package chat

import (
	"context"
	"io"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// Sink is the minimal write surface the use case drives the response through. It
// is a SMALL net/http-free shim: transport adapts http.ResponseWriter +
// http.NewResponseController into it (SetWriteDeadline per frame), so app/chat
// never imports net/http and a fake Sink makes every relay branch unit-testable.
// Header mutation must precede WriteHeader; Write before WriteHeader implies 200
// (mirroring net/http) but the use case always calls WriteHeader explicitly.
type Sink interface {
	// SetHeader sets a response header; only valid before WriteHeader.
	SetHeader(key, value string)
	// WriteHeader commits the status line + headers exactly once.
	WriteHeader(status int)
	// Write streams body bytes (an SSE frame, or the full non-stream body).
	Write(p []byte) (int, error)
	// Flush pushes buffered bytes to the client (per-frame real-time delivery).
	Flush()
	// SetWriteDeadline rolls the per-frame write deadline (NOT a global timeout,
	// which would truncate a long stream). A zero/!ok deadline is a no-op.
	SetWriteDeadline(t time.Time)
}

// Authenticator resolves a bearer token to its install. Satisfied by
// *app/install.Service.LookupInstall. Status is the typed domain enum so the use
// case branches on StatusBanned without a stringly-typed compare.
type Authenticator interface {
	LookupInstall(ctx context.Context, token string) (id string, status dominstall.Status, found bool, err error)
}

// Quota is the accounting port. Satisfied by *app/quota.Service. The use case
// snapshots the period ONCE (entry) and threads the *Reservation through
// settle/rollback unchanged (GW-INV-05). Settle/Rollback errors are RETURNED
// (not swallowed) so the use case can count + WARN them (B2).
type Quota interface {
	SnapshotPeriod(now time.Time) domquota.Period
	Reserve(ctx context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error)
	Settle(ctx context.Context, r *domquota.Reservation, actualPUSD int64) error
	Rollback(ctx context.Context, r *domquota.Reservation) error
}

// Upstream is the provider router port. Satisfied by an infra/upstream adapter
// wired in bootstrap (app declares the port in stdlib/domain terms so it never
// imports infra). Open runs connect→first-byte through the breaker + bounded key failover
// and returns an UpstreamStream the use case relays + Closes, or an
// UpstreamFailure containing both the normalized error and an independently
// classified charge-exposure fact. Available exposes construction-time
// credential presence; BreakerOpen lets the use case fast-shed the selected
// provider before taking an N_global slot.
type Upstream interface {
	Open(ctx context.Context, provider billing.Provider, model string, req domchat.CompletionRequest, firstByteTimeout time.Duration) (UpstreamStream, *UpstreamFailure)
	Available(provider billing.Provider) bool
	BreakerOpen(provider billing.Provider) bool
}

// UpstreamFailure separates client-facing error normalization from accounting.
// Exposure's zero value is ChargePossible, so a malformed adapter fails safe by
// retaining the reservation rather than creating an accidental refund path.
type UpstreamFailure struct {
	APIError *apierr.APIError
	Exposure billing.ChargeExposure
}

// UpstreamStream is the relayed upstream body: the use case reads SSE frames /
// the non-stream body off it and Closes it. Kept to io.Reader+Closer (the only
// surface the relay needs) so the port stays infra-free; the upstream client's
// concrete *Stream satisfies it (the gateway normalizes non-2xx to a failure, so
// a returned stream is always the committed 2xx body — no status needed here).
type UpstreamStream interface {
	io.Reader
	Close() error
}

// RateLimiter is the per-install minute bucket. Satisfied by
// *infra/ratelimit.RateLimiter. Allow meters one request; SetKeyLimit is the M2
// throttle primitive the anomaly Throttle reaches through (same bucket, B1).
type RateLimiter interface {
	Allow(key string) bool
	SetKeyLimit(key string, limit int, until time.Time)
}

// Throttle is the M2 per-token anomaly auto-throttle (spec-behaviors §1.3).
// Observe feeds the install's sliding RPM and, on an anomaly, tightens the SAME
// rate-limiter bucket via SetKeyLimit, returning true so the use case sets the
// audit flag — it NEVER rejects the request (the tightened bucket bites on the
// NEXT Allow). Dormant (TOKEN_ANOMALY_RPM=0) ⇒ returns false with zero work.
type Throttle interface {
	Observe(installID string) bool
}

// DiskGuard is the REL-6 low-disk read-only degrade probe. Degraded()==true ⇒
// shed with DISK_LOW BEFORE any reservation (a DB write), so SQLite never fails
// mid-write and corrupts conservative accounting. nil-safe via a no-op default.
type DiskGuard interface {
	Degraded() bool
}

// Config is the live runtime-config snapshot port (Load once per request). The
// use case reads all guardrails + model resolution from ONE snapshot so a
// hot-reload never applies half-old/half-new bounds within a request (GW-INV-08).
type Config interface {
	Load() *config.Config
}

// Clock is the injectable time source (tests drive period rollover / deadlines).
type Clock interface {
	Now() time.Time
}

// Logger is the request-scoped structured logger port. WARN is never sampled
// (security/B2 events use it directly); the use case never logs prompt/key text.
type Logger interface {
	Warn(ctx context.Context, msg string, args ...any)
}

// Metrics is the observability port. Every method may be called concurrently and
// must be cheap + non-reentrant; a nil Metrics is replaced with a no-op so the
// hot path needn't nil-check. SettleFailures/RollbackFailures back the B2
// alarm: a failed terminal accounting write is a COUNTED, visible event.
type Metrics interface {
	Inflight(n int)                                     // current N_global occupancy gauge.
	Upstream(provider billing.Provider, outcome string) // gateway_upstream_requests_total{provider,outcome}.
	BillingDrift(provider billing.Provider)             // actual provider cost exceeded its hard quote.
	SettleFailure()                                     // a detached Settle returned an error (B2).
	RollbackFailure()                                   // a detached Rollback returned an error (B2).
}

// WaitGroupLike is the REL-4 shutdown barrier the detached settle/rollback
// goroutines register on, so graceful shutdown awaits accounting before closing
// the DB. Satisfied by *sync.WaitGroup.
type WaitGroupLike interface {
	Add(delta int)
	Done()
}
