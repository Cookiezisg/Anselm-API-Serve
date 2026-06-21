package quotastore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite" // pure-Go sqlite driver, registered as "sqlite".

	"github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// newTestStore opens a real on-disk sqlite file with the production accounting
// schema (usage/budget/ledger) split across a single-conn writer pool (matches
// production BEGIN IMMEDIATE serialization) and a concurrent read pool.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "q.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"

	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	w.SetMaxOpenConns(1) // single serialized writer, as in production.
	t.Cleanup(func() { _ = w.Close() })

	schema := []string{
		`CREATE TABLE usage (
			install_id TEXT NOT NULL, period TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0, tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (install_id, period))`,
		`CREATE TABLE budget (
			period TEXT PRIMARY KEY,
			tokens_used INTEGER NOT NULL DEFAULT 0, requests INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE ledger (
			request_id TEXT PRIMARY KEY, install_id TEXT NOT NULL, period_day TEXT NOT NULL,
			reserved INTEGER NOT NULL, settled INTEGER, created_at DATETIME NOT NULL)`,
		`CREATE INDEX idx_ledger_open ON ledger(settled, created_at)`,
	}
	for _, s := range schema {
		if _, err := w.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	r.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = r.Close() })

	return New(orm.Open(w), orm.Open(r))
}

func testPeriod() quota.Period { return quota.Period{Month: "2026-06", Day: "2026-06-20"} }

// generous limits so a specific gate can be isolated by lowering just that one.
func limits() quota.Limits {
	return quota.Limits{
		MonthlyQuota:         1_000_000,
		InstallDailyTokenCap: 1_000_000_000,
		GlobalDailyBudget:    1_000_000_000,
		DailySublimit:        0,
	}
}

func readUsage(t *testing.T, s *Store, installID, period string) (count, tokens int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT count, tokens FROM usage WHERE install_id = ? AND period = ?`,
		installID, period).Scan(&count, &tokens)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("readUsage: %v", err)
	}
	return count, tokens
}

func readBudget(t *testing.T, s *Store, day string) (used, requests int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT tokens_used, requests FROM budget WHERE period = ?`, day).Scan(&used, &requests)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("readBudget: %v", err)
	}
	return used, requests
}

func TestReserveLazyCreatesAndDebits(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if r.RequestID == "" || r.Reserved != 100 {
		t.Fatalf("bad reservation %+v", r)
	}
	if c, _ := readUsage(t, s, "ins_1", p.Month); c != 1 {
		t.Errorf("month count = %d, want 1", c)
	}
	if _, tok := readUsage(t, s, "ins_1", p.Day); tok != 100 {
		t.Errorf("day tokens = %d, want 100", tok)
	}
	if used, reqs := readBudget(t, s, p.Day); used != 100 || reqs != 1 {
		t.Errorf("budget = (%d,%d), want (100,1)", used, reqs)
	}
}

// GW-INV-01: N+1 concurrent reserves against a cap of N → exactly N succeed, no
// oversell. Run with -race; the single-writer BEGIN IMMEDIATE serializes the RMW.
func TestConcurrentReserveNoOversell(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	const cap = 50
	const attempts = cap + 25
	lim := limits()
	lim.MonthlyQuota = cap // gate 1 is the cap under test.

	var ok, denied int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Reserve(context.Background(), "ins_1", 1, p, lim)
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
			case errors.Is(err, quota.ErrMonthlyExhausted):
				atomic.AddInt64(&denied, 1)
			default:
				t.Errorf("unexpected reserve error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok != cap {
		t.Errorf("succeeded = %d, want exactly %d (oversell!)", ok, cap)
	}
	if denied != attempts-cap {
		t.Errorf("denied = %d, want %d", denied, attempts-cap)
	}
	if c, _ := readUsage(t, s, "ins_1", p.Month); c != cap {
		t.Errorf("final month count = %d, want %d (no oversell)", c, cap)
	}
}

func TestBudgetExhaustedReturnsTypedDeny(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	lim := limits()
	lim.GlobalDailyBudget = 100

	if _, err := s.Reserve(context.Background(), "ins_1", 80, p, lim); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	// 80 + 80 > 100 → budget gate denies.
	_, err := s.Reserve(context.Background(), "ins_2", 80, p, lim)
	if !errors.Is(err, quota.ErrBudgetExceeded) {
		t.Fatalf("got %v, want ErrBudgetExceeded", err)
	}
	// The denied reserve must have rolled back ENTIRELY: budget still at 80,
	// ins_2 took no month count.
	if used, _ := readBudget(t, s, p.Day); used != 80 {
		t.Errorf("budget = %d, want 80 (denied reserve must fully roll back)", used)
	}
	if c, _ := readUsage(t, s, "ins_2", p.Month); c != 0 {
		t.Errorf("ins_2 month count = %d, want 0", c)
	}
}

func TestRollbackConservationNetsZero(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	lim := limits()
	lim.DailySublimit = 100 // enabled so rollback must also reverse the sublimit +1.

	r, err := s.Reserve(context.Background(), "ins_1", 250, p, lim)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !r.SublimitApplied {
		t.Fatal("SublimitApplied should be true when DailySublimit>0")
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if c, tok := readUsage(t, s, "ins_1", p.Month); c != 0 || tok != 0 {
		t.Errorf("month row = (%d,%d), want (0,0)", c, tok)
	}
	// Day row: both the sublimit count +1 and the token reserve must net to zero.
	if c, tok := readUsage(t, s, "ins_1", p.Day); c != 0 || tok != 0 {
		t.Errorf("day row = (%d,%d), want (0,0)", c, tok)
	}
	if used, reqs := readBudget(t, s, p.Day); used != 0 || reqs != 0 {
		t.Errorf("budget = (%d,%d), want (0,0)", used, reqs)
	}
}

// B1 (off direction): sublimit DISABLED at reserve → flag false → rollback must
// NOT touch the day-row count even though the row exists from the token reserve.
func TestRollbackB1FlagNotConfig(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	lim := limits() // DailySublimit = 0.

	r, err := s.Reserve(context.Background(), "ins_1", 10, p, lim)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if r.SublimitApplied {
		t.Fatal("SublimitApplied should be false when DailySublimit==0")
	}
	// Simulate a hot-reload that ENABLED the sublimit between reserve & rollback.
	// Rollback must rely on the flag (false), not re-read config — so the day
	// count stays 0, not -0/decremented. Seed a sentinel count to detect a wrong
	// decrement.
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE usage SET count = 5 WHERE install_id = ? AND period = ?`, "ins_1", p.Day); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if c, _ := readUsage(t, s, "ins_1", p.Day); c != 5 {
		t.Errorf("day count = %d, want 5 (rollback must not touch it: flag was false)", c)
	}
}

func TestSettleRefundDelta(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 200, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// actual 50 < reserved 200 → refund 150.
	if err := s.Settle(context.Background(), r, 50); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if used, _ := readBudget(t, s, p.Day); used != 50 {
		t.Errorf("budget = %d, want 50 after refund", used)
	}
	if _, tok := readUsage(t, s, "ins_1", p.Day); tok != 50 {
		t.Errorf("day tokens = %d, want 50", tok)
	}
	// Month count is KEPT (output happened).
	if c, _ := readUsage(t, s, "ins_1", p.Month); c != 1 {
		t.Errorf("month count = %d, want 1 (kept on settle)", c)
	}
}

func TestSettleTopUpDelta(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// actual 300 > reserved 100 → top-up debit 200 → balances rise to 300.
	if err := s.Settle(context.Background(), r, 300); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if used, _ := readBudget(t, s, p.Day); used != 300 {
		t.Errorf("budget = %d, want 300 after top-up", used)
	}
	if _, tok := readUsage(t, s, "ins_1", p.Day); tok != 300 {
		t.Errorf("day tokens = %d, want 300", tok)
	}
}

func TestDoubleSettleIdempotent(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 200, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Settle(context.Background(), r, 50); err != nil {
		t.Fatalf("settle 1: %v", err)
	}
	// Second settle with a different actual must be a no-op (settled NOT NULL).
	if err := s.Settle(context.Background(), r, 0); err != nil {
		t.Fatalf("settle 2: %v", err)
	}
	if used, _ := readBudget(t, s, p.Day); used != 50 {
		t.Errorf("budget = %d, want 50 (second settle must be no-op)", used)
	}
}

// GW-INV-04: a settle and an orphan-reconcile racing the SAME row → exactly one
// adjusts balances. Run with -race.
func TestSettleVsReconcileRaceOneWinner(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 200, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Backdate the ledger row so the reconciler considers it an orphan.
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE ledger SET created_at = ? WHERE request_id = ?`,
		time.Now().UTC().Add(-time.Hour), r.RequestID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = s.Settle(context.Background(), r, 50) }()
	go func() {
		defer wg.Done()
		_, _ = s.ReconcileOrphans(context.Background(), time.Now().UTC().Add(-time.Minute))
	}()
	wg.Wait()

	// Exactly one winner: budget is either 50 (settle won) or 0 (reconcile won),
	// never 50-200 double-applied or anything else.
	used, _ := readBudget(t, s, p.Day)
	if used != 50 && used != 0 {
		t.Fatalf("budget = %d, want 50 (settle won) or 0 (reconcile won) — double adjust!", used)
	}
	// Whichever won, the ledger is now settled NOT NULL (terminal).
	var settled sql.NullInt64
	if err := s.reader.QueryRow(context.Background(),
		`SELECT settled FROM ledger WHERE request_id = ?`, r.RequestID).Scan(&settled); err != nil {
		t.Fatalf("read settled: %v", err)
	}
	if !settled.Valid {
		t.Fatal("ledger still settled NULL after settle+reconcile")
	}
}

func TestReconcileOnlyTouchesAgedNullRows(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()

	// orphan: aged + settled NULL (should be reconciled).
	orphan, err := s.Reserve(context.Background(), "ins_orphan", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve orphan: %v", err)
	}
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE ledger SET created_at = ? WHERE request_id = ?`,
		time.Now().UTC().Add(-time.Hour), orphan.RequestID); err != nil {
		t.Fatalf("backdate orphan: %v", err)
	}

	// fresh: settled NULL but recent (must be SKIPPED).
	fresh, err := s.Reserve(context.Background(), "ins_fresh", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve fresh: %v", err)
	}

	// settled: aged but already settled (must be SKIPPED).
	done, err := s.Reserve(context.Background(), "ins_done", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve done: %v", err)
	}
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE ledger SET created_at = ?, settled = 100 WHERE request_id = ?`,
		time.Now().UTC().Add(-time.Hour), done.RequestID); err != nil {
		t.Fatalf("settle done: %v", err)
	}

	n, err := s.ReconcileOrphans(context.Background(), time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d rows, want exactly 1 (only the aged-null orphan)", n)
	}
	// orphan refunded on budget+day tokens; +1 month count KEPT (GW-INV-06).
	if _, tok := readUsage(t, s, "ins_orphan", p.Day); tok != 0 {
		t.Errorf("orphan day tokens = %d, want 0 (refunded)", tok)
	}
	if c, _ := readUsage(t, s, "ins_orphan", p.Month); c != 1 {
		t.Errorf("orphan month count = %d, want 1 (kept)", c)
	}
	// fresh untouched.
	if _, tok := readUsage(t, s, "ins_fresh", p.Day); tok != 100 {
		t.Errorf("fresh day tokens = %d, want 100 (not reconciled)", tok)
	}
	_ = fresh
}

func TestRollbackThenSettleNoOp(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	r, err := s.Reserve(context.Background(), "ins_1", 100, p, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// A late settle after rollback must be a no-op (settled=0, not NULL).
	if err := s.Settle(context.Background(), r, 999); err != nil {
		t.Fatalf("late settle: %v", err)
	}
	if used, _ := readBudget(t, s, p.Day); used != 0 {
		t.Errorf("budget = %d, want 0 (settle after rollback is no-op)", used)
	}
}

func TestViewReadsCountAndBudget(t *testing.T) {
	s := newTestStore(t)
	p := testPeriod()
	if _, err := s.Reserve(context.Background(), "ins_1", 100, p, limits()); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	used, budgetUsed, err := s.View(context.Background(), "ins_1", p)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if used != 1 {
		t.Errorf("monthly used = %d, want 1", used)
	}
	if budgetUsed != 100 {
		t.Errorf("budgetUsed = %d, want 100", budgetUsed)
	}
	// Unknown install reads zeros, not an error.
	u, b, err := s.View(context.Background(), "ins_unknown", p)
	if err != nil || u != 0 || b != 100 {
		t.Errorf("unknown view = (%d,%d,%v), want (0,100,nil)", u, b, err)
	}
}

// PeriodSnapshotReuse: a reservation made on the entry day, then settled/rolled
// back AFTER the wall clock crosses midnight, must affect ONLY the entry day's
// rows — because the stored Period is reused, never recomputed (GW-INV-05).
func TestPeriodSnapshotReuseAcrossMidnight(t *testing.T) {
	s := newTestStore(t)
	entry := quota.Period{Month: "2026-06-20"[:7], Day: "2026-06-20"}
	r, err := s.Reserve(context.Background(), "ins_1", 100, entry, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// The reservation carries the entry period; settle uses r.Period, so even if
	// "now" were the next day, only the entry day row moves.
	if err := s.Settle(context.Background(), r, 0); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if used, _ := readBudget(t, s, "2026-06-20"); used != 0 {
		t.Errorf("entry-day budget = %d, want 0 (refunded on the entry day)", used)
	}
	// The next day's row was never created.
	if used, _ := readBudget(t, s, "2026-06-21"); used != 0 {
		t.Errorf("next-day budget = %d, want 0 (never touched)", used)
	}
}
