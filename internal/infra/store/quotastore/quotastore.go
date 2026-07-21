// Package quotastore persists the provider-aware fixed-point spend ledger.
// Every aggregate mutation is one BEGIN IMMEDIATE transaction on the serialized
// writer, so monthly entitlement, the operator monthly spend wallet, daily
// accounting statistics, and the reservation state can never be observed
// partially applied.
package quotastore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

const (
	stateOpen       = "open"
	stateSettled    = "settled"
	stateRolledBack = "rolled_back"
	stateOrphaned   = "orphaned"
)

// Store performs atomic quota operations. It holds no live configuration; the
// caller passes one immutable Limits snapshot to each Reserve.
type Store struct {
	writer *orm.DB
	reader *orm.DB
}

func New(writer, reader *orm.DB) *Store { return &Store{writer: writer, reader: reader} }

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// Reserve atomically takes the per-install monthly request entitlement and the
// shared operator monthly pUSD wallet. The daily install/provider/global rows are
// still updated for audit/dashboard statistics, but they no longer deny traffic.
// Raw token counts never enter these tables; the frozen billing Plan is the
// conversion boundary.
func (s *Store) Reserve(ctx context.Context, installID string, plan billing.Plan, p quota.Period, lim quota.Limits) (*quota.Reservation, error) {
	if err := validatePlan(plan); err != nil {
		return nil, err
	}

	r := &quota.Reservation{
		RequestID:    newRequestID(),
		InstallID:    installID,
		Period:       p,
		Plan:         plan,
		ReservedPUSD: plan.ReservedPUSD,
	}
	err := s.writer.Transaction(ctx, func(tx *orm.DB) error {
		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO quota_monthly(install_id, period_month, requests) VALUES (?, ?, 0)`,
			installID, p.Month); err != nil {
			return fmt.Errorf("quotastore: month upsert: %w", err)
		}
		if err := execOne(ctx, tx, "month reserve",
			`UPDATE quota_monthly SET requests = requests + 1
			   WHERE install_id = ? AND period_month = ? AND requests < ?`,
			installID, p.Month, lim.MonthlyQuota); err != nil {
			if isConditionalMiss(err) {
				return quota.ErrMonthlyExhausted
			}
			return err
		}

		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO install_spend_daily(install_id, period_day, spend_pusd, requests)
			 VALUES (?, ?, 0, 0)`, installID, p.Day); err != nil {
			return fmt.Errorf("quotastore: install-day upsert: %w", err)
		}
		if err := execOne(ctx, tx, "install spend reserve",
			`UPDATE install_spend_daily SET spend_pusd = spend_pusd + ?
			   WHERE install_id = ? AND period_day = ?
			     AND spend_pusd <= ?`,
			plan.ReservedPUSD, installID, p.Day, int64(math.MaxInt64)-plan.ReservedPUSD); err != nil {
			return err
		}

		if lim.DailySublimit > 0 {
			if err := execOne(ctx, tx, "daily sublimit reserve",
				`UPDATE install_spend_daily SET requests = requests + 1
				   WHERE install_id = ? AND period_day = ? AND requests < ?`,
				installID, p.Day, lim.DailySublimit); err != nil {
				if isConditionalMiss(err) {
					return quota.ErrSublimitExceeded
				}
				return err
			}
			r.SublimitApplied = true
		}

		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO provider_spend_daily(provider, period_day, spend_pusd, requests)
			 VALUES (?, ?, 0, 0)`, string(plan.Provider), p.Day); err != nil {
			return fmt.Errorf("quotastore: provider-day upsert: %w", err)
		}
		if err := execOne(ctx, tx, "provider spend reserve",
			`UPDATE provider_spend_daily
			    SET spend_pusd = spend_pusd + ?, requests = requests + 1
			  WHERE provider = ? AND period_day = ?
			    AND spend_pusd <= ? AND requests < ?`,
			plan.ReservedPUSD, string(plan.Provider), p.Day,
			int64(math.MaxInt64)-plan.ReservedPUSD, int64(math.MaxInt64)); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO global_spend_daily(period_day, spend_pusd, requests) VALUES (?, 0, 0)`,
			p.Day); err != nil {
			return fmt.Errorf("quotastore: global-day upsert: %w", err)
		}
		if err := execOne(ctx, tx, "global spend reserve",
			`UPDATE global_spend_daily
			    SET spend_pusd = spend_pusd + ?, requests = requests + 1
			  WHERE period_day = ?
			    AND spend_pusd <= ? AND requests < ?`,
			plan.ReservedPUSD, p.Day, int64(math.MaxInt64)-plan.ReservedPUSD, int64(math.MaxInt64)); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO global_spend_monthly(period_month, spend_pusd, requests) VALUES (?, 0, 0)`,
			p.Month); err != nil {
			return fmt.Errorf("quotastore: global-month upsert: %w", err)
		}
		if err := execOne(ctx, tx, "global month reserve",
			`UPDATE global_spend_monthly
			    SET spend_pusd = spend_pusd + ?, requests = requests + 1
			  WHERE period_month = ? AND spend_pusd <= ?`,
			plan.ReservedPUSD, p.Month, lim.GlobalMonthlySpendPUSD-plan.ReservedPUSD); err != nil {
			if isConditionalMiss(err) {
				return quota.ErrBudgetExceeded
			}
			return err
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO spend_ledger(
			   request_id, install_id, provider, model, rate_card_id,
			   period_month, period_day, reserved_pusd, charged_pusd, state,
			   sublimit_applied, created_at, terminal_at
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, NULL)`,
			r.RequestID, installID, string(plan.Provider), plan.Model, plan.RateCardID,
			p.Month, p.Day, plan.ReservedPUSD, stateOpen, boolInt(r.SublimitApplied), time.Now().UTC()); err != nil {
			return fmt.Errorf("quotastore: spend ledger insert: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func validatePlan(plan billing.Plan) error {
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("quotastore: invalid billing plan: %w", err)
	}
	if plan.ReservedPUSD <= 0 {
		return fmt.Errorf("quotastore: zero-cost billing plan")
	}
	return nil
}

// Settle commits the authoritative pUSD charge. A top-up is deliberately not
// capped: the provider has already spent the money, so the ledger must record
// truth and subsequent reserves will observe an exhausted/overdrawn wallet.
func (s *Store) Settle(ctx context.Context, r *quota.Reservation, actualPUSD int64) error {
	if err := validateReservation(r); err != nil {
		return err
	}
	if actualPUSD < 0 {
		return fmt.Errorf("quotastore: negative settlement")
	}
	delta := r.ReservedPUSD - actualPUSD // positive=refund, negative=top-up.
	return s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE spend_ledger
			    SET state = ?, charged_pusd = ?, terminal_at = ?
			  WHERE request_id = ? AND state = ?
			    AND install_id = ? AND provider = ? AND model = ? AND rate_card_id = ?
			    AND period_month = ? AND period_day = ? AND reserved_pusd = ?
			    AND sublimit_applied = ?`,
			stateSettled, actualPUSD, time.Now().UTC(), r.RequestID, stateOpen,
			r.InstallID, string(r.Plan.Provider), r.Plan.Model, r.Plan.RateCardID,
			r.Period.Month, r.Period.Day, r.ReservedPUSD, boolInt(r.SublimitApplied))
		if err != nil {
			return fmt.Errorf("quotastore: ledger settle: %w", err)
		}
		if n, err := rowsAffected(res); err != nil {
			return fmt.Errorf("quotastore: ledger settle rows: %w", err)
		} else if n == 0 {
			return nil // another terminal operation won.
		} else if n != 1 {
			return fmt.Errorf("quotastore: ledger settle affected %d rows", n)
		}
		return adjustSpend(ctx, tx, r, delta)
	})
}

// Rollback exactly reverses every reservation for a definitely non-billable
// pre-output failure. No MAX(0, ...) is used: missing rows or underflow are
// accounting corruption and roll the entire transaction back, including CAS.
func (s *Store) Rollback(ctx context.Context, r *quota.Reservation) error {
	if err := validateReservation(r); err != nil {
		return err
	}
	return s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE spend_ledger
			    SET state = ?, charged_pusd = 0, terminal_at = ?
			  WHERE request_id = ? AND state = ?
			    AND install_id = ? AND provider = ? AND model = ? AND rate_card_id = ?
			    AND period_month = ? AND period_day = ? AND reserved_pusd = ?
			    AND sublimit_applied = ?`,
			stateRolledBack, time.Now().UTC(), r.RequestID, stateOpen,
			r.InstallID, string(r.Plan.Provider), r.Plan.Model, r.Plan.RateCardID,
			r.Period.Month, r.Period.Day, r.ReservedPUSD, boolInt(r.SublimitApplied))
		if err != nil {
			return fmt.Errorf("quotastore: ledger rollback: %w", err)
		}
		if n, err := rowsAffected(res); err != nil {
			return fmt.Errorf("quotastore: ledger rollback rows: %w", err)
		} else if n == 0 {
			return nil
		} else if n != 1 {
			return fmt.Errorf("quotastore: ledger rollback affected %d rows", n)
		}

		if r.SublimitApplied {
			if err := execOne(ctx, tx, "install rollback",
				`UPDATE install_spend_daily
				    SET spend_pusd = spend_pusd - ?, requests = requests - 1
				  WHERE install_id = ? AND period_day = ?
				    AND spend_pusd >= ? AND requests >= 1`,
				r.ReservedPUSD, r.InstallID, r.Period.Day, r.ReservedPUSD); err != nil {
				return err
			}
		} else if err := execOne(ctx, tx, "install rollback",
			`UPDATE install_spend_daily SET spend_pusd = spend_pusd - ?
			  WHERE install_id = ? AND period_day = ? AND spend_pusd >= ?`,
			r.ReservedPUSD, r.InstallID, r.Period.Day, r.ReservedPUSD); err != nil {
			return err
		}
		if err := execOne(ctx, tx, "provider rollback",
			`UPDATE provider_spend_daily
			    SET spend_pusd = spend_pusd - ?, requests = requests - 1
			  WHERE provider = ? AND period_day = ?
			    AND spend_pusd >= ? AND requests >= 1`,
			r.ReservedPUSD, string(r.Plan.Provider), r.Period.Day, r.ReservedPUSD); err != nil {
			return err
		}
		if err := execOne(ctx, tx, "global rollback",
			`UPDATE global_spend_daily
			    SET spend_pusd = spend_pusd - ?, requests = requests - 1
			  WHERE period_day = ? AND spend_pusd >= ? AND requests >= 1`,
			r.ReservedPUSD, r.Period.Day, r.ReservedPUSD); err != nil {
			return err
		}
		if err := execOne(ctx, tx, "global month rollback",
			`UPDATE global_spend_monthly
			    SET spend_pusd = spend_pusd - ?, requests = requests - 1
			  WHERE period_month = ? AND spend_pusd >= ? AND requests >= 1`,
			r.ReservedPUSD, r.Period.Month, r.ReservedPUSD); err != nil {
			return err
		}
		return execOne(ctx, tx, "month rollback",
			`UPDATE quota_monthly SET requests = requests - 1
			  WHERE install_id = ? AND period_month = ? AND requests >= 1`,
			r.InstallID, r.Period.Month)
	})
}

func validateReservation(r *quota.Reservation) error {
	if r == nil || r.RequestID == "" || r.InstallID == "" || r.ReservedPUSD <= 0 {
		return fmt.Errorf("quotastore: invalid reservation")
	}
	if r.ReservedPUSD != r.Plan.ReservedPUSD {
		return fmt.Errorf("quotastore: reservation/plan amount mismatch")
	}
	return validatePlan(r.Plan)
}

// adjustSpend applies the settle delta to the daily accounting statistics and
// the operator monthly budget wallet. Exact-one row checks and arithmetic guards
// prevent silent conservation loss.
func adjustSpend(ctx context.Context, tx *orm.DB, r *quota.Reservation, delta int64) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		if err := execOne(ctx, tx, "install settle refund",
			`UPDATE install_spend_daily SET spend_pusd = spend_pusd - ?
			  WHERE install_id = ? AND period_day = ? AND spend_pusd >= ?`,
			delta, r.InstallID, r.Period.Day, delta); err != nil {
			return err
		}
		if err := execOne(ctx, tx, "provider settle refund",
			`UPDATE provider_spend_daily SET spend_pusd = spend_pusd - ?
			  WHERE provider = ? AND period_day = ? AND spend_pusd >= ?`,
			delta, string(r.Plan.Provider), r.Period.Day, delta); err != nil {
			return err
		}
		if err := execOne(ctx, tx, "global settle refund",
			`UPDATE global_spend_daily SET spend_pusd = spend_pusd - ?
			  WHERE period_day = ? AND spend_pusd >= ?`,
			delta, r.Period.Day, delta); err != nil {
			return err
		}
		return execOne(ctx, tx, "global month settle refund",
			`UPDATE global_spend_monthly SET spend_pusd = spend_pusd - ?
			  WHERE period_month = ? AND spend_pusd >= ?`,
			delta, r.Period.Month, delta)
	}

	topUp := -delta
	ceiling := int64(math.MaxInt64) - topUp
	if err := execOne(ctx, tx, "install settle top-up",
		`UPDATE install_spend_daily SET spend_pusd = spend_pusd + ?
		  WHERE install_id = ? AND period_day = ? AND spend_pusd <= ?`,
		topUp, r.InstallID, r.Period.Day, ceiling); err != nil {
		return err
	}
	if err := execOne(ctx, tx, "provider settle top-up",
		`UPDATE provider_spend_daily SET spend_pusd = spend_pusd + ?
		  WHERE provider = ? AND period_day = ? AND spend_pusd <= ?`,
		topUp, string(r.Plan.Provider), r.Period.Day, ceiling); err != nil {
		return err
	}
	if err := execOne(ctx, tx, "global settle top-up",
		`UPDATE global_spend_daily SET spend_pusd = spend_pusd + ?
		  WHERE period_day = ? AND spend_pusd <= ?`,
		topUp, r.Period.Day, ceiling); err != nil {
		return err
	}
	return execOne(ctx, tx, "global month settle top-up",
		`UPDATE global_spend_monthly SET spend_pusd = spend_pusd + ?
		  WHERE period_month = ? AND spend_pusd <= ?`,
		topUp, r.Period.Month, ceiling)
}

// View returns the monthly request count, install daily spend (dashboard/client
// information), and the operator global monthly balance. Missing lazy rows read
// as zero.
func (s *Store) View(ctx context.Context, installID string, p quota.Period) (used, installSpendPUSD, globalSpendPUSD int64, err error) {
	err = s.reader.QueryRow(ctx,
		`SELECT requests FROM quota_monthly WHERE install_id = ? AND period_month = ?`,
		installID, p.Month).Scan(&used)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, 0, fmt.Errorf("quotastore: view monthly: %w", err)
	}
	err = s.reader.QueryRow(ctx,
		`SELECT spend_pusd FROM install_spend_daily WHERE install_id = ? AND period_day = ?`,
		installID, p.Day).Scan(&installSpendPUSD)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, 0, fmt.Errorf("quotastore: view install spend: %w", err)
	}
	err = s.reader.QueryRow(ctx,
		`SELECT spend_pusd FROM global_spend_monthly WHERE period_month = ?`, p.Month).Scan(&globalSpendPUSD)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, 0, fmt.Errorf("quotastore: view global monthly spend: %w", err)
	}
	return used, installSpendPUSD, globalSpendPUSD, nil
}

// ReconcileOrphans terminalizes aged unknown outcomes at their full reserved
// cost. It intentionally changes no balance: automatic refund would undercount
// a request the provider may already have billed.
func (s *Store) ReconcileOrphans(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := s.reader.Query(ctx,
		`SELECT request_id FROM spend_ledger WHERE state = ? AND created_at < ?`, stateOpen, cutoff)
	if err != nil {
		return 0, fmt.Errorf("quotastore: scan orphans: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("quotastore: scan orphan row: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("quotastore: iterate orphans: %w", err)
	}

	n := 0
	for _, id := range ids {
		won, err := s.reconcileOne(ctx, id)
		if err != nil {
			return n, err
		}
		if won {
			n++
		}
	}
	return n, nil
}

func (s *Store) reconcileOne(ctx context.Context, requestID string) (bool, error) {
	won := false
	err := s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE spend_ledger
			    SET state = ?, charged_pusd = reserved_pusd, terminal_at = ?
			  WHERE request_id = ? AND state = ?`,
			stateOrphaned, time.Now().UTC(), requestID, stateOpen)
		if err != nil {
			return fmt.Errorf("quotastore: reconcile ledger: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return fmt.Errorf("quotastore: reconcile rows: %w", err)
		}
		if n > 1 {
			return fmt.Errorf("quotastore: reconcile affected %d rows", n)
		}
		won = n == 1
		return nil
	})
	return won, err
}

func (s *Store) OpenReservations(ctx context.Context) (int64, error) {
	var n int64
	if err := s.reader.QueryRow(ctx,
		`SELECT COUNT(*) FROM spend_ledger WHERE state = ?`, stateOpen).Scan(&n); err != nil {
		return 0, fmt.Errorf("quotastore: count open reservations: %w", err)
	}
	return n, nil
}

type conditionalMiss struct{ op string }

func (e *conditionalMiss) Error() string { return "quotastore: " + e.op + ": condition not met" }

func isConditionalMiss(err error) bool {
	_, ok := err.(*conditionalMiss)
	return ok
}

func execOne(ctx context.Context, tx *orm.DB, op, query string, args ...any) error {
	res, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("quotastore: %s: %w", op, err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("quotastore: %s rows: %w", op, err)
	}
	if n == 0 {
		return &conditionalMiss{op: op}
	}
	if n != 1 {
		return fmt.Errorf("quotastore: %s affected %d rows", op, n)
	}
	return nil
}

func rowsAffected(res sql.Result) (int64, error) { return res.RowsAffected() }

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
