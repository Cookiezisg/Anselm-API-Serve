// Package quotastore is the accounting persistence (UoW, ADR-005): it implements
// the app/quota Repository port STRUCTURALLY (it never imports app) over the
// separated SQLite pools. Every multi-row atomic write is ONE orm.Transaction on
// the single-writer pool (BEGIN IMMEDIATE), so the read-modify-write of each
// guardrail cannot interleave with another reserve/settle/rollback. *sql.Tx
// never leaks out of this package. The deny categories are returned as the
// domain's typed sentinels (domain/quota); the app maps them to wire codes.
package quotastore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// Store performs atomic quota operations. Writes go to the single serialized
// writer (BEGIN IMMEDIATE); reads (View, orphan scan) go to the bounded read
// pool. It holds no config — the live Limits snapshot is passed in per Reserve.
type Store struct {
	writer *orm.DB
	reader *orm.DB
}

// New wires the store to the separated pools.
func New(writer, reader *orm.DB) *Store {
	return &Store{writer: writer, reader: reader}
}

// newRequestID mints a ledger key: "req_" + hex(8 random bytes). rand.Read from
// crypto/rand never returns a short read on a healthy OS; on the impossible error
// the zeroed bytes still yield a syntactically valid (just non-random) id, which
// the UNIQUE primary key would reject on the astronomically unlikely collision.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// Reserve pre-debits the guardrails in ONE BEGIN IMMEDIATE transaction. Each
// gate is a conditional UPDATE; RowsAffected()==0 returns the matching domain
// sentinel and the orm.Transaction rolls the whole tx back (it commits only on a
// nil return). Gates run in spec order: month count → install-day tokens →
// optional daily sublimit → global-day budget → INSERT ledger(settled NULL).
// SublimitApplied is set true IFF the gated +1 actually executed (B1).
func (s *Store) Reserve(ctx context.Context, installID string, est int64, p quota.Period, lim quota.Limits) (*quota.Reservation, error) {
	rid := newRequestID()
	r := &quota.Reservation{RequestID: rid, InstallID: installID, Period: p, Reserved: est}

	err := s.writer.Transaction(ctx, func(tx *orm.DB) error {
		// Gate 1: monthly count < MonthlyQuota. Lazy-create the month row first.
		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO usage(install_id, period, count, tokens) VALUES (?, ?, 0, 0)`,
			installID, p.Month); err != nil {
			return fmt.Errorf("quotastore: month upsert: %w", err)
		}
		res, err := tx.Exec(ctx,
			`UPDATE usage SET count = count + 1
			   WHERE install_id = ? AND period = ? AND count < ?`,
			installID, p.Month, lim.MonthlyQuota)
		if err != nil {
			return fmt.Errorf("quotastore: month update: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return quota.ErrMonthlyExhausted
		}

		// Gate 2: install-day tokens + est <= InstallDailyTokenCap.
		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO usage(install_id, period, count, tokens) VALUES (?, ?, 0, 0)`,
			installID, p.Day); err != nil {
			return fmt.Errorf("quotastore: day upsert: %w", err)
		}
		res, err = tx.Exec(ctx,
			`UPDATE usage SET tokens = tokens + ?
			   WHERE install_id = ? AND period = ? AND tokens + ? <= ?`,
			est, installID, p.Day, est, lim.InstallDailyTokenCap)
		if err != nil {
			return fmt.Errorf("quotastore: day token update: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return quota.ErrDayTokensExceeded
		}

		// Gate 2b: optional per-install daily request sublimit (day-row count).
		if lim.DailySublimit > 0 {
			res, err = tx.Exec(ctx,
				`UPDATE usage SET count = count + 1
				   WHERE install_id = ? AND period = ? AND count < ?`,
				installID, p.Day, lim.DailySublimit)
			if err != nil {
				return fmt.Errorf("quotastore: sublimit update: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return quota.ErrSublimitExceeded
			}
			// The gated +1 fired — record it so Rollback reverses exactly this,
			// independent of any later DailySublimit hot-reload (B1).
			r.SublimitApplied = true
		}

		// Gate 3: global-day budget + est <= GlobalDailyBudget.
		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO budget(period, tokens_used, requests) VALUES (?, 0, 0)`,
			p.Day); err != nil {
			return fmt.Errorf("quotastore: budget upsert: %w", err)
		}
		res, err = tx.Exec(ctx,
			`UPDATE budget SET tokens_used = tokens_used + ?, requests = requests + 1
			   WHERE period = ? AND tokens_used + ? <= ?`,
			est, p.Day, est, lim.GlobalDailyBudget)
		if err != nil {
			return fmt.Errorf("quotastore: budget update: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return quota.ErrBudgetExceeded
		}

		// Step 4: ledger row (settled NULL) for crash reconciliation.
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger(request_id, install_id, period_day, reserved, settled, created_at)
			   VALUES (?, ?, ?, ?, NULL, ?)`,
			rid, installID, p.Day, est, time.Now().UTC()); err != nil {
			return fmt.Errorf("quotastore: ledger insert: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Settle reconciles reserved vs actual: refund (actual<est) or top-up
// (actual>est) on budget + install-day tokens, marking the ledger row settled.
// The `settled IS NULL` CAS makes Settle/Rollback/Reconcile mutually exclusive
// single-winners (GW-INV-04): a lost race commits a no-op without touching
// balances. Monthly count is intentionally KEPT (output happened, §7.2).
func (s *Store) Settle(ctx context.Context, r *quota.Reservation, actual int64) error {
	if actual < 0 {
		actual = 0
	}
	delta := r.Reserved - actual // >0 refund, <0 top-up debit.

	return s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE ledger SET settled = ? WHERE request_id = ? AND settled IS NULL`,
			actual, r.RequestID)
		if err != nil {
			return fmt.Errorf("quotastore: ledger settle: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already settled/rolled-back/reconciled — no-op winner.
		}
		if delta != 0 {
			if err := adjustDay(ctx, tx, r.Period.Day, r.InstallID, delta); err != nil {
				return err
			}
		}
		return nil
	})
}

// Rollback reverses ALL reservations for a pre-output failure: budget tokens +
// requests, install-day tokens, monthly count, and the day-row sublimit count
// IFF r.SublimitApplied (B1 — never re-reads config). Same `settled IS NULL` CAS
// single-winner guard (settled=0). Decrements floor at 0 as defense-in-depth;
// the CAS guard is the primary correctness mechanism (a crash can only overcount).
func (s *Store) Rollback(ctx context.Context, r *quota.Reservation) error {
	return s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE ledger SET settled = 0 WHERE request_id = ? AND settled IS NULL`,
			r.RequestID)
		if err != nil {
			return fmt.Errorf("quotastore: ledger rollback: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // lost the CAS — no-op.
		}

		// 1) global-day budget: refund full reserved + the request count.
		if _, err := tx.Exec(ctx,
			`UPDATE budget SET tokens_used = MAX(0, tokens_used - ?), requests = MAX(0, requests - 1)
			   WHERE period = ?`,
			r.Reserved, r.Period.Day); err != nil {
			return fmt.Errorf("quotastore: budget rollback: %w", err)
		}
		// 2) install-day tokens: refund full reserved.
		if _, err := tx.Exec(ctx,
			`UPDATE usage SET tokens = MAX(0, tokens - ?) WHERE install_id = ? AND period = ?`,
			r.Reserved, r.InstallID, r.Period.Day); err != nil {
			return fmt.Errorf("quotastore: day token rollback: %w", err)
		}
		// 3) monthly count: refund the +1.
		if _, err := tx.Exec(ctx,
			`UPDATE usage SET count = MAX(0, count - 1) WHERE install_id = ? AND period = ?`,
			r.InstallID, r.Period.Month); err != nil {
			return fmt.Errorf("quotastore: month rollback: %w", err)
		}
		// 4) daily sublimit +1 IFF it fired at reserve time (B1).
		if r.SublimitApplied {
			if _, err := tx.Exec(ctx,
				`UPDATE usage SET count = MAX(0, count - 1) WHERE install_id = ? AND period = ?`,
				r.InstallID, r.Period.Day); err != nil {
				return fmt.Errorf("quotastore: sublimit rollback: %w", err)
			}
		}
		return nil
	})
}

// adjustDay applies a settle delta to both balances: refund (delta>0) lowers
// usage, top-up (delta<0) raises it. Floors at 0 on the refund direction as
// defense-in-depth; a top-up (negative delta) legitimately increases the values.
func adjustDay(ctx context.Context, tx *orm.DB, day, installID string, delta int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE budget SET tokens_used = MAX(0, tokens_used - ?) WHERE period = ?`,
		delta, day); err != nil {
		return fmt.Errorf("quotastore: budget settle: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE usage SET tokens = MAX(0, tokens - ?) WHERE install_id = ? AND period = ?`,
		delta, installID, day); err != nil {
		return fmt.Errorf("quotastore: usage settle: %w", err)
	}
	return nil
}

// View reads the authoritative monthly count + the day's global budget usage
// from the read pool. Missing rows (lazy-create not yet triggered) read as 0.
func (s *Store) View(ctx context.Context, installID string, p quota.Period) (used, budgetUsed int64, err error) {
	err = s.reader.QueryRow(ctx,
		`SELECT count FROM usage WHERE install_id = ? AND period = ?`,
		installID, p.Month).Scan(&used)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("quotastore: view usage: %w", err)
	}
	err = s.reader.QueryRow(ctx,
		`SELECT tokens_used FROM budget WHERE period = ?`, p.Day).Scan(&budgetUsed)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("quotastore: view budget: %w", err)
	}
	return used, budgetUsed, nil
}

// ReconcileOrphans refunds budget + install-day tokens for ledger rows still
// settled IS NULL created before `cutoff` (crash-left reservations), marking
// settled=reserved and KEEPING the +1 month count (GW-INV-06: conservative).
// Each orphan is its own write tx with the same `settled IS NULL` CAS, so a row
// that raced a real settle/rollback is skipped (RowsAffected==0). Returns the
// number actually reconciled. The candidate scan uses the read pool; only the
// per-row mutation touches the writer (minimizing write-pool hold time).
func (s *Store) ReconcileOrphans(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := s.reader.Query(ctx,
		`SELECT request_id, install_id, period_day, reserved
		   FROM ledger WHERE settled IS NULL AND created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("quotastore: scan orphans: %w", err)
	}
	type orphan struct {
		reqID, installID, day string
		reserved              int64
	}
	var list []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.reqID, &o.installID, &o.day, &o.reserved); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("quotastore: scan orphan row: %w", err)
		}
		list = append(list, o)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("quotastore: iterate orphans: %w", err)
	}

	n := 0
	for _, o := range list {
		reconciled, err := s.reconcileOne(ctx, o.reqID, o.installID, o.day, o.reserved)
		if err != nil {
			return n, err
		}
		if reconciled {
			n++
		}
	}
	return n, nil
}

// reconcileOne reconciles a single orphan in one write tx. The `settled IS NULL`
// CAS makes it a no-op (false) when a real settle/rollback won the race.
func (s *Store) reconcileOne(ctx context.Context, reqID, installID, day string, reserved int64) (bool, error) {
	reconciled := false
	err := s.writer.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx,
			`UPDATE ledger SET settled = reserved WHERE request_id = ? AND settled IS NULL`,
			reqID)
		if err != nil {
			return fmt.Errorf("quotastore: reconcile ledger: %w", err)
		}
		if cnt, _ := res.RowsAffected(); cnt == 0 {
			return nil // raced a settle/rollback — skip.
		}
		if _, err := tx.Exec(ctx,
			`UPDATE budget SET tokens_used = MAX(0, tokens_used - ?) WHERE period = ?`,
			reserved, day); err != nil {
			return fmt.Errorf("quotastore: reconcile budget: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE usage SET tokens = MAX(0, tokens - ?) WHERE install_id = ? AND period = ?`,
			reserved, installID, day); err != nil {
			return fmt.Errorf("quotastore: reconcile usage: %w", err)
		}
		reconciled = true
		return nil
	})
	return reconciled, err
}

// OpenReservations counts settled-IS-NULL ledger rows — the missed-settle alarm
// source (OBS-2 gateway_quota_reservations_open). Read pool.
func (s *Store) OpenReservations(ctx context.Context) (int64, error) {
	var n int64
	if err := s.reader.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger WHERE settled IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("quotastore: count open reservations: %w", err)
	}
	return n, nil
}
