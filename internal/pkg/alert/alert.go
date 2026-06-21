// Package alert is the in-process alert STATE tracker over the failure surface:
// daily budget (80%/100%), open-reservation backlog (missed-settle leak signal),
// upstream breaker OPEN, disk read-only degradation, upstream error-rate spike,
// plus the neutral install-burst and per-token auto-throttle operational signals.
//
// WHY it only tracks state (no push): the Telegram/webhook delivery path was
// removed. This is the process-queryable view BELOW a real Prometheus +
// Alertmanager pipeline, not a replacement — the dashboard reads CurrentStatus.
//
// 红线:状态只含聚合数值与原因码,绝不含 key / prompt / token / ip。
package alert

import (
	"context"
	"sync"
	"time"
)

// Thresholds are the trip points. Zero values fall back to defaults in New.
type Thresholds struct {
	BudgetWarnFraction float64 // fire at used/limit >= this (default 0.80)
	ReservationsOpen   int64   // fire when open reservations >= this (default 100)
	UpstreamErrorBurst int64   // fire when upstream errors in one window >= this (default 20)
	InstallBurst       int64   // fire when installs issued in one window >= this (default 200)
	TokensThrottled    int64   // fire when installs currently auto-throttled >= this (default 1)
}

// Signals is the failure-surface snapshot the periodic caller feeds into Check.
// Every field is an aggregate the caller already holds: Check is a pure
// thresholder — it never reads metrics/DB and never carries key/prompt/ip.
type Signals struct {
	BudgetUsed         int64 // tokens used today
	BudgetLimit        int64 // daily budget limit
	ReservationsOpen   int64 // ledger rows still settled IS NULL
	BreakerOpen        bool  // upstream breaker is OPEN (shedding UPSTREAM_BUSY)
	DiskDegraded       bool  // read-only degradation active (low disk)
	UpstreamErrors     int64 // upstream error outcomes THIS window (delta)
	InstallsThisWindow int64 // installs created THIS window (delta) — Sybil-burst signal
	TokensThrottled    int64 // installs CURRENTLY under per-token auto-throttle
}

// AlertState is one condition's state for the dashboard. Firing is the live
// re-evaluated state; LastFiredAt is the most recent fire time (zero = never).
type AlertState struct {
	Reason      string    `json:"reason"`
	Severity    string    `json:"severity"`
	Firing      bool      `json:"firing"`
	Message     string    `json:"message"`
	LastFiredAt time.Time `json:"lastFiredAt,omitempty"`
}

// Stable wire reason codes for the dashboard.
const (
	reasonBudgetExhausted = "budget_exhausted"
	reasonBudgetHigh      = "budget_high"
	reasonReservations    = "reservations_open_backlog"
	reasonBreakerOpen     = "upstream_breaker_open"
	reasonDiskReadOnly    = "disk_read_only"
	reasonErrorBurst      = "upstream_error_burst"
	reasonInstallBurst    = "install_burst"
	reasonTokenThrottle   = "token_throttle"
)

// Alerter holds thresholds + the current firing state of each condition. The
// state map is mutex-guarded so the dashboard reader (CurrentStatus) never races
// the metrics-refresh writer (Check).
type Alerter struct {
	th  Thresholds
	now func() time.Time

	mu    sync.Mutex
	state map[string]AlertState // reason → current state
}

// Option configures an Alerter.
type Option func(*Alerter)

// WithClock injects a clock for deterministic tests.
func WithClock(now func() time.Time) Option {
	return func(a *Alerter) {
		if now != nil {
			a.now = now
		}
	}
}

// New builds an Alerter, defaulting any unset (<= 0) threshold.
func New(th Thresholds, opts ...Option) *Alerter {
	if th.BudgetWarnFraction <= 0 {
		th.BudgetWarnFraction = 0.80
	}
	if th.ReservationsOpen <= 0 {
		th.ReservationsOpen = 100
	}
	if th.UpstreamErrorBurst <= 0 {
		th.UpstreamErrorBurst = 20
	}
	if th.InstallBurst <= 0 {
		th.InstallBurst = 200
	}
	if th.TokensThrottled <= 0 {
		th.TokensThrottled = 1
	}
	a := &Alerter{
		th:    th,
		now:   time.Now,
		state: make(map[string]AlertState),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Check re-evaluates every condition's firing state. Meant for the periodic
// metrics-refresh loop: cheap, non-blocking, no I/O. A tripped condition
// refreshes LastFiredAt; a cleared one keeps its stamp but sets Firing=false.
func (a *Alerter) Check(_ context.Context, s Signals) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()

	// Budget 100% (critical) takes precedence over 80% (warning) — mutually
	// exclusive so the dashboard never shows both at once.
	budgetExhausted, budgetHigh := false, false
	if s.BudgetLimit > 0 {
		frac := float64(s.BudgetUsed) / float64(s.BudgetLimit)
		switch {
		case frac >= 1.0:
			budgetExhausted = true
		case frac >= a.th.BudgetWarnFraction:
			budgetHigh = true
		}
	}
	a.set(now, reasonBudgetExhausted, "critical", budgetExhausted,
		"daily token budget EXHAUSTED (used >= limit) — new requests shedding BUDGET_EXHAUSTED")
	a.set(now, reasonBudgetHigh, "warning", budgetHigh,
		"daily token budget high (used >= threshold of limit) — wallet guardrail approaching")
	a.set(now, reasonReservations, "warning", s.ReservationsOpen >= a.th.ReservationsOpen,
		"open reservations (settled IS NULL) backlog high — possible missed-settle leak; check reconciler")
	a.set(now, reasonBreakerOpen, "critical", s.BreakerOpen,
		"upstream circuit breaker OPEN — upstream unhealthy; requests shedding UPSTREAM_BUSY")
	a.set(now, reasonDiskReadOnly, "critical", s.DiskDegraded,
		"data disk low — read-only degradation active; new requests shedding DISK_LOW; free space or rotate WAL")
	a.set(now, reasonErrorBurst, "warning", s.UpstreamErrors >= a.th.UpstreamErrorBurst,
		"upstream error rate spiking — errored attempts this window crossed the burst ceiling; check upstream health")
	// Install burst is NEUTRAL: a viral spike or a Sybil wave both trip it.
	// Operators correlate with the install gates before acting — hence warning.
	a.set(now, reasonInstallBurst, "warning", s.InstallsThisWindow >= a.th.InstallBurst,
		"install issuance rate high this window — could be organic growth or a registration wave; review install gates if it is sustained")
	// Token auto-throttle is NEUTRAL: a reversible gentle slow-down before any ban.
	a.set(now, reasonTokenThrottle, "warning", s.TokensThrottled >= a.th.TokensThrottled,
		"installs currently under per-token anomaly auto-throttle — reversible gentle slow-down active; review the anomaly threshold if it is broad or sustained")
}

// set records a condition's current firing state, preserving (and refreshing on
// fire) its LastFiredAt. Caller holds a.mu.
func (a *Alerter) set(now time.Time, reason, severity string, firing bool, message string) {
	st := a.state[reason]
	st.Reason = reason
	st.Severity = severity
	st.Message = message
	st.Firing = firing
	if firing {
		st.LastFiredAt = now
	}
	a.state[reason] = st
}

// CurrentStatus returns a stable-ordered snapshot of every condition seen so far.
// Conditions appear once Check has run at least once (the refresh loop does so at
// boot). Safe to call concurrently with Check.
func (a *Alerter) CurrentStatus() []AlertState {
	a.mu.Lock()
	defer a.mu.Unlock()
	order := []string{
		reasonBudgetExhausted, reasonBudgetHigh, reasonReservations,
		reasonBreakerOpen, reasonDiskReadOnly, reasonErrorBurst,
		reasonInstallBurst, reasonTokenThrottle,
	}
	out := make([]AlertState, 0, len(order))
	for _, r := range order {
		if st, ok := a.state[r]; ok {
			out = append(out, st)
		}
	}
	return out
}
