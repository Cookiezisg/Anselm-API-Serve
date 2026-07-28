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
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
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
	Reserve(ctx context.Context, installID string, plan billing.Plan, p quota.Period, lim Limits) (*quota.Reservation, error)
	Settle(ctx context.Context, r *quota.Reservation, actualPUSD int64) error
	Rollback(ctx context.Context, r *quota.Reservation) error
	// ReconcileOrphans closes aged open rows conservatively at their full reserved
	// spend. It never refunds an unknown provider charge and keeps request counts.
	ReconcileOrphans(ctx context.Context, olderThan time.Time) (int, error)
	// View reads the authoritative monthly count plus the install/global daily
	// pUSD balances. Provider availability is request-shape dependent and is
	// therefore enforced by Reserve rather than this provider-neutral view.
	View(ctx context.Context, installID string, p quota.Period) (used, installSpendPUSD, globalSpendPUSD int64, err error)
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
func (s *Service) Reserve(ctx context.Context, installID string, plan billing.Plan, p quota.Period) (*quota.Reservation, error) {
	r, err := s.repo.Reserve(ctx, installID, plan, p, s.cfg.Limits())
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
	case errors.Is(err, quota.ErrSublimitExceeded):
		return apierr.ErrRateLimited
	case errors.Is(err, quota.ErrBudgetExceeded):
		return apierr.ErrBudgetExhausted
	case errors.As(err, new(*quota.CategoryDailyExceededError)):
		// The denial names its own ledger, so each category maps to its own wire code. A default
		// that answered for an unknown category would tell the client to "try image generation
		// again tomorrow" about a capability it never touched.
		// 拒绝自己点名账本,故每个品类映射到自己的 wire 码。给未知品类兜一个 default,等于对着一个
		// 用户根本没碰过的能力说「明天再试图像生成」。
		var catErr *quota.CategoryDailyExceededError
		_ = errors.As(err, &catErr)
		switch catErr.Category {
		case quota.CategoryImage:
			return apierr.ErrImageQuotaExhausted
		case quota.CategoryVideo:
			return apierr.ErrVideoQuotaExhausted
		case quota.CategorySpeech:
			return apierr.ErrTTSQuotaExhausted
		case quota.CategoryVoice:
			return apierr.ErrVoiceQuotaExhausted
		}
		return err
	default:
		return err
	}
}

// Settle reconciles a reservation against actual usage. Errors are returned (not
// swallowed) so the caller can count SettleFailures + WARN (B2): a failed settle
// must be observable, not silently left for the orphan scanner to finalize at the
// full reservation.
func (s *Service) Settle(ctx context.Context, r *quota.Reservation, actualPUSD int64) error {
	return s.repo.Settle(ctx, r, actualPUSD)
}

// Rollback reverses all reservations for a pre-output failure. Returned errors
// are likewise the caller's to observe (B2).
func (s *Service) Rollback(ctx context.Context, r *quota.Reservation) error {
	return s.repo.Rollback(ctx, r)
}

// View is the read model for GET /v1/quota. The wire remains monthly-count
// based; Available additionally folds the operator global monthly spend gate.
type View struct {
	Limit     int64
	Used      int64 // authoritative monthly count.
	Remaining int64
	ResetAt   time.Time
	Available bool
}

// View builds the read model from the authoritative monthly count + global month
// budget.
func (s *Service) View(ctx context.Context, installID string, p quota.Period) (*View, error) {
	used, _, globalSpend, err := s.repo.View(ctx, installID, p)
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
		Available: remaining > 0 && globalSpend < lim.GlobalMonthlySpendPUSD,
	}, nil
}

// ReconcileOrphans is the background-loop policy: aged unknown outcomes are
// closed at their full reservation. Keeping the spend is the only crash-safe
// direction because the provider may already have billed before the process died.
func (s *Service) ReconcileOrphans(ctx context.Context, older time.Duration, now time.Time) (int, error) {
	return s.repo.ReconcileOrphans(ctx, now.Add(-older))
}
