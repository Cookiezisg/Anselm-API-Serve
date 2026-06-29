// Package quota is the accounting use-case layer: it owns the reservation
// policy, the typed-repo-error → apierr-sentinel mapping, the /v1/quota View
// (availability semantics live HERE, not in transport), and the orphan
// reconciler loop policy. It declares its infra needs as PORTS (Repository,
// ConfigSource) and NEVER imports infra, database/sql, or net/http — the write
// transactions live structurally in infra/store/quotastore (UoW, ADR-005).
package quota

import (
	"context"
	"errors"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// Limits aliases the domain guardrail snapshot so callers can spell it as
// quota.Limits without reaching into domain; the type itself lives in domain so
// the infra store can satisfy the Repository port without importing app.
type Limits = quota.Limits

// ConfigSource is the port the Service reads live guardrails + tz through. infra
// (configprovider) satisfies it structurally; keeping it an interface keeps app
// free of any infra import while still seeing hot-reloaded values.
type ConfigSource interface {
	// Limits returns the current guardrail snapshot, taken atomically.
	Limits() Limits
	// Location is the reset-tz for period math (SnapshotPeriod / resetAt).
	Location() *time.Location
}

// Repository is the atomic-op port. Each method is ONE aggregate transaction in
// infra/store/quotastore (BEGIN IMMEDIATE on the writer); *sql.Tx never leaks
// here. Denials surface as the typed Err* sentinels above; the Reservation's
// SublimitApplied flag is set by the store IFF the gated +1 fired (B1).
type Repository interface {
	Reserve(ctx context.Context, installID string, est int64, p quota.Period, lim Limits) (*quota.Reservation, error)
	Settle(ctx context.Context, r *quota.Reservation, actual int64) error
	Rollback(ctx context.Context, r *quota.Reservation) error
	// ReconcileOrphans refunds budget+day-tokens for settled-NULL ledger rows
	// created before olderThan (now-cutoff is computed by the caller and passed as
	// olderThan), keeping the +1 month count. Returns the number reconciled.
	ReconcileOrphans(ctx context.Context, olderThan time.Time) (int, error)
	// View reads the authoritative monthly count + the day's budget usage.
	View(ctx context.Context, installID string, p quota.Period) (used, budgetUsed int64, err error)
}

// Service is the accounting use case over a Repository + ConfigSource.
type Service struct {
	repo Repository
	cfg  ConfigSource
}

// New wires the Service to its ports.
func New(repo Repository, cfg ConfigSource) *Service {
	return &Service{repo: repo, cfg: cfg}
}

// SnapshotPeriod takes the entry-snapshot period in the configured tz. Threaded
// UNCHANGED through Reserve/Settle/Rollback (GW-INV-05).
func (s *Service) SnapshotPeriod(now time.Time) quota.Period {
	return quota.SnapshotPeriod(now, s.cfg.Location())
}

// Reserve pre-debits the guardrails atomically, mapping a typed denial to its
// wire sentinel. The Limits snapshot is taken ONCE here and passed into the
// store so the whole reserve sees a single consistent config view.
func (s *Service) Reserve(ctx context.Context, installID string, est int64, p quota.Period) (*quota.Reservation, error) {
	r, err := s.repo.Reserve(ctx, installID, est, p, s.cfg.Limits())
	if err != nil {
		return nil, mapReserveErr(err)
	}
	return r, nil
}

// mapReserveErr maps the four typed denials to apierr sentinels in spec order;
// any other error is an internal fault and surfaces verbatim (the transport
// renderer normalizes non-APIError to INTERNAL).
func mapReserveErr(err error) error {
	switch {
	case errors.Is(err, quota.ErrMonthlyExhausted):
		return apierr.ErrQuotaExhausted
	case errors.Is(err, quota.ErrDayTokensExceeded):
		return apierr.ErrRateLimited
	case errors.Is(err, quota.ErrSublimitExceeded):
		return apierr.ErrRateLimited
	case errors.Is(err, quota.ErrBudgetExceeded):
		return apierr.ErrBudgetExhausted
	default:
		return err
	}
}

// Settle reconciles a reservation against actual usage. Errors are returned (not
// swallowed) so the caller can count SettleFailures + WARN (B2): a failed settle
// must be observable, not silently left for the orphan scanner to refund full.
func (s *Service) Settle(ctx context.Context, r *quota.Reservation, actual int64) error {
	return s.repo.Settle(ctx, r, actual)
}

// Rollback reverses all reservations for a pre-output failure. Returned errors
// are likewise the caller's to observe (B2).
func (s *Service) Rollback(ctx context.Context, r *quota.Reservation) error {
	return s.repo.Rollback(ctx, r)
}

// View is the read model for GET /v1/quota. The availability rule (remaining>0
// AND budgetUsed<GlobalDailyBudget) is owned HERE, not in transport, so every
// caller agrees on what "available" means. Reserves nothing, bills nothing.
type View struct {
	Limit     int64
	Used      int64 // authoritative monthly count.
	Remaining int64
	ResetAt   time.Time
	Available bool
}

// View builds the read model from the authoritative monthly count + day budget.
func (s *Service) View(ctx context.Context, installID string, p quota.Period) (*View, error) {
	used, budgetUsed, err := s.repo.View(ctx, installID, p)
	if err != nil {
		return nil, err
	}
	lim := s.cfg.Limits()
	remaining := lim.MonthlyQuota - used
	if remaining < 0 {
		remaining = 0
	}
	return &View{
		Limit:     lim.MonthlyQuota,
		Used:      used,
		Remaining: remaining,
		ResetAt:   quota.MonthResetAt(p, s.cfg.Location()),
		Available: remaining > 0 && budgetUsed < lim.GlobalDailyBudget,
	}, nil
}

// ReconcileOrphans is the background-loop policy: refund crash-left reservations
// older than `older` relative to `now`, so a day's budget is never chronically
// pinned by a process that died mid-flight (GW-INV-06). The cutoff is computed
// here (app owns the policy) and passed to the store as an absolute instant.
func (s *Service) ReconcileOrphans(ctx context.Context, older time.Duration, now time.Time) (int, error) {
	return s.repo.ReconcileOrphans(ctx, now.Add(-older))
}
