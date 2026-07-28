package quotastore

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "q.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	w.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = w.Close() })

	schema := []string{
		`CREATE TABLE quota_monthly (
			install_id TEXT NOT NULL, period_month TEXT NOT NULL,
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0),
			PRIMARY KEY(install_id, period_month))`,
		`CREATE TABLE install_spend_daily (
			install_id TEXT NOT NULL, period_day TEXT NOT NULL,
			spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0),
			PRIMARY KEY(install_id, period_day))`,
		`CREATE TABLE provider_spend_daily (
			provider TEXT NOT NULL CHECK(provider IN ('deepseek','qwen')),
			period_day TEXT NOT NULL, spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0),
			PRIMARY KEY(provider, period_day))`,
		`CREATE TABLE global_spend_daily (
			period_day TEXT PRIMARY KEY, spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0))`,
		`CREATE TABLE global_spend_monthly (
			period_month TEXT PRIMARY KEY, spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0))`,
		`CREATE TABLE spend_ledger (
			request_id TEXT PRIMARY KEY, install_id TEXT NOT NULL,
			provider TEXT NOT NULL CHECK(provider IN ('deepseek','qwen')),
			model TEXT NOT NULL, rate_card_id TEXT NOT NULL,
			period_month TEXT NOT NULL, period_day TEXT NOT NULL,
			reserved_pusd INTEGER NOT NULL CHECK(reserved_pusd > 0),
			charged_pusd INTEGER CHECK(charged_pusd >= 0),
			state TEXT NOT NULL CHECK(state IN ('open','settled','rolled_back','orphaned')),
			sublimit_applied INTEGER NOT NULL CHECK(sublimit_applied IN (0,1)),
			category TEXT NOT NULL DEFAULT '',
			category_units INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL, terminal_at DATETIME)`,
		`CREATE INDEX idx_spend_ledger_open ON spend_ledger(state, created_at)`,
		`CREATE TABLE install_category_daily (
			install_id TEXT NOT NULL, category TEXT NOT NULL, period_day TEXT NOT NULL,
			units INTEGER NOT NULL DEFAULT 0 CHECK(units >= 0),
			PRIMARY KEY(install_id, category, period_day))`,
	}
	for _, ddl := range schema {
		if _, err := w.Exec(ddl); err != nil {
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

func mustPlan(t *testing.T, provider billing.Provider, prompt, output int64) billing.Plan {
	t.Helper()
	model := billing.DeepSeekV4Flash
	if provider == billing.ProviderQwen {
		model = billing.Qwen37Plus
	}
	p, err := billing.NewPlan(provider, model, billing.InputStandard, prompt, output)
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	return p
}

func limits() quota.Limits {
	return quota.Limits{
		MonthlyQuota:           1_000_000,
		GlobalMonthlySpendPUSD: 1_000_000_000_000_000,
	}
}

func reserve(t *testing.T, s *Store, installID string, plan billing.Plan, lim quota.Limits) *quota.Reservation {
	t.Helper()
	r, err := s.Reserve(context.Background(), installID, plan, testPeriod(), lim)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

func readMonth(t *testing.T, s *Store, installID, month string) int64 {
	t.Helper()
	var n int64
	err := s.reader.QueryRow(context.Background(),
		`SELECT requests FROM quota_monthly WHERE install_id=? AND period_month=?`, installID, month).Scan(&n)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("read month: %v", err)
	}
	return n
}

func readInstall(t *testing.T, s *Store, installID, day string) (spend, requests int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT spend_pusd, requests FROM install_spend_daily WHERE install_id=? AND period_day=?`,
		installID, day).Scan(&spend, &requests)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read install: %v", err)
	}
	return spend, requests
}

func readProvider(t *testing.T, s *Store, provider billing.Provider, day string) (spend, requests int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT spend_pusd, requests FROM provider_spend_daily WHERE provider=? AND period_day=?`,
		string(provider), day).Scan(&spend, &requests)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read provider: %v", err)
	}
	return spend, requests
}

func readGlobal(t *testing.T, s *Store, day string) (spend, requests int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT spend_pusd, requests FROM global_spend_daily WHERE period_day=?`, day).Scan(&spend, &requests)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read global: %v", err)
	}
	return spend, requests
}

func readGlobalMonth(t *testing.T, s *Store, month string) (spend, requests int64) {
	t.Helper()
	err := s.reader.QueryRow(context.Background(),
		`SELECT spend_pusd, requests FROM global_spend_monthly WHERE period_month=?`, month).Scan(&spend, &requests)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read global month: %v", err)
	}
	return spend, requests
}

func readLedger(t *testing.T, s *Store, requestID string) (state string, charged sql.NullInt64) {
	t.Helper()
	if err := s.reader.QueryRow(context.Background(),
		`SELECT state, charged_pusd FROM spend_ledger WHERE request_id=?`, requestID).Scan(&state, &charged); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return state, charged
}

func TestReserveDebitsEveryPUSDWallet(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 100, 20)
	r := reserve(t, s, "ins_1", p, limits())
	if r.ReservedPUSD != p.ReservedPUSD || r.Plan.RateCardID != p.RateCardID {
		t.Fatalf("reservation lost frozen plan: %+v", r)
	}
	if got := readMonth(t, s, "ins_1", testPeriod().Month); got != 1 {
		t.Fatalf("month=%d want 1", got)
	}
	if got, _ := readInstall(t, s, "ins_1", testPeriod().Day); got != p.ReservedPUSD {
		t.Fatalf("install spend=%d want %d", got, p.ReservedPUSD)
	}
	if got, reqs := readProvider(t, s, p.Provider, testPeriod().Day); got != p.ReservedPUSD || reqs != 1 {
		t.Fatalf("provider=(%d,%d) want (%d,1)", got, reqs, p.ReservedPUSD)
	}
	if got, reqs := readGlobal(t, s, testPeriod().Day); got != p.ReservedPUSD || reqs != 1 {
		t.Fatalf("global=(%d,%d) want (%d,1)", got, reqs, p.ReservedPUSD)
	}
	if got, reqs := readGlobalMonth(t, s, testPeriod().Month); got != p.ReservedPUSD || reqs != 1 {
		t.Fatalf("global month=(%d,%d) want (%d,1)", got, reqs, p.ReservedPUSD)
	}
}

func TestReserveDenialsRollbackWholeTransaction(t *testing.T) {
	plan := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	cases := []struct {
		name string
		edit func(*quota.Limits)
		want error
	}{
		{"monthly", func(l *quota.Limits) { l.MonthlyQuota = 0 }, quota.ErrMonthlyExhausted},
		{"global-month", func(l *quota.Limits) { l.GlobalMonthlySpendPUSD = plan.ReservedPUSD - 1 }, quota.ErrBudgetExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			lim := limits()
			tc.edit(&lim)
			_, err := s.Reserve(context.Background(), "ins_1", plan, testPeriod(), lim)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if got := readMonth(t, s, "ins_1", testPeriod().Month); got != 0 {
				t.Fatalf("denial leaked month count=%d", got)
			}
			if got, _ := readGlobal(t, s, testPeriod().Day); got != 0 {
				t.Fatalf("denial leaked global spend=%d", got)
			}
		})
	}
}

func TestReserveHonorsPreexistingConservativeMonthlyWalletFloor(t *testing.T) {
	plan := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	const floor int64 = 9_000_000
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.writer.Exec(ctx,
		`INSERT INTO global_spend_monthly(period_month,spend_pusd,requests) VALUES (?, ?,1)`,
		testPeriod().Month, floor); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	lim := limits()
	lim.GlobalMonthlySpendPUSD = floor
	_, err := s.Reserve(ctx, "ins_1", plan, testPeriod(), lim)
	if !errors.Is(err, quota.ErrBudgetExceeded) {
		t.Fatalf("reserve err=%v want %v", err, quota.ErrBudgetExceeded)
	}
	if got := readMonth(t, s, "ins_1", testPeriod().Month); got != 0 {
		t.Fatalf("denied reserve leaked month count=%d", got)
	}
}

func TestReserveRejectsForgedPlanWithoutFrozenRateCard(t *testing.T) {
	s := newTestStore(t)
	good := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	forged := billing.Plan{
		Provider: good.Provider, Model: good.Model, RateCardID: good.RateCardID,
		InputClass: good.InputClass, PromptQuote: good.PromptQuote,
		OutputQuote: good.OutputQuote, ReservedPUSD: good.ReservedPUSD,
	}
	if _, err := s.Reserve(context.Background(), "ins_1", forged, testPeriod(), limits()); err == nil {
		t.Fatal("manually forged Plan must not cross the persistence boundary")
	}
	if got := readMonth(t, s, "ins_1", testPeriod().Month); got != 0 {
		t.Fatalf("forged plan mutated quota: %d", got)
	}
}

func TestProviderStatsAreRecordedButOnlyGlobalMonthDenies(t *testing.T) {
	s := newTestStore(t)
	ds := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	gm := mustPlan(t, billing.ProviderQwen, 1, 1)
	lim := limits()
	lim.GlobalMonthlySpendPUSD = ds.ReservedPUSD + gm.ReservedPUSD
	reserve(t, s, "ins_ds", ds, lim)
	reserve(t, s, "ins_gm", gm, lim)
	if _, err := s.Reserve(context.Background(), "ins_ds2", ds, testPeriod(), lim); !errors.Is(err, quota.ErrBudgetExceeded) {
		t.Fatalf("over global month err=%v want budget denial", err)
	}
	if got, _ := readGlobal(t, s, testPeriod().Day); got != ds.ReservedPUSD+gm.ReservedPUSD {
		t.Fatalf("shared global=%d", got)
	}
	if got, _ := readGlobalMonth(t, s, testPeriod().Month); got != ds.ReservedPUSD+gm.ReservedPUSD {
		t.Fatalf("shared global month=%d", got)
	}
}

func TestConcurrentGlobalReserveNoOversell(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	const capN = 30
	const attempts = 50
	lim := limits()
	lim.GlobalMonthlySpendPUSD = capN * p.ReservedPUSD

	var ok, denied int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Reserve(context.Background(), "ins_"+time.Unix(int64(i), 0).Format("150405"), p, testPeriod(), lim)
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
			case errors.Is(err, quota.ErrBudgetExceeded):
				atomic.AddInt64(&denied, 1)
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if ok != capN || denied != attempts-capN {
		t.Fatalf("ok=%d denied=%d", ok, denied)
	}
	if got, _ := readGlobalMonth(t, s, testPeriod().Month); got != capN*p.ReservedPUSD {
		t.Fatalf("global month=%d want %d", got, capN*p.ReservedPUSD)
	}
}

func TestRollbackExactConservation(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderQwen, 10, 5)
	lim := limits()
	lim.DailySublimit = 3
	r := reserve(t, s, "ins_1", p, lim)
	if !r.SublimitApplied {
		t.Fatal("sublimit flag not frozen")
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readMonth(t, s, r.InstallID, r.Period.Month); got != 0 {
		t.Fatalf("month=%d", got)
	}
	if spend, reqs := readInstall(t, s, r.InstallID, r.Period.Day); spend != 0 || reqs != 0 {
		t.Fatalf("install=(%d,%d)", spend, reqs)
	}
	if spend, reqs := readProvider(t, s, p.Provider, r.Period.Day); spend != 0 || reqs != 0 {
		t.Fatalf("provider=(%d,%d)", spend, reqs)
	}
	if spend, reqs := readGlobal(t, s, r.Period.Day); spend != 0 || reqs != 0 {
		t.Fatalf("global=(%d,%d)", spend, reqs)
	}
	if spend, reqs := readGlobalMonth(t, s, r.Period.Month); spend != 0 || reqs != 0 {
		t.Fatalf("global month=(%d,%d)", spend, reqs)
	}
	state, charged := readLedger(t, s, r.RequestID)
	if state != stateRolledBack || !charged.Valid || charged.Int64 != 0 {
		t.Fatalf("ledger=(%s,%v)", state, charged)
	}
}

func TestRollbackUnderflowRollsBackCASAndAllAdjustments(t *testing.T) {
	s := newTestStore(t)
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderDeepSeek, 1, 1), limits())
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE install_spend_daily SET spend_pusd=0 WHERE install_id=? AND period_day=?`, r.InstallID, r.Period.Day); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err == nil {
		t.Fatal("underflow must fail, not clamp to zero")
	}
	state, charged := readLedger(t, s, r.RequestID)
	if state != stateOpen || charged.Valid {
		t.Fatalf("CAS was not rolled back: (%s,%v)", state, charged)
	}
	if spend, reqs := readGlobal(t, s, r.Period.Day); spend != r.ReservedPUSD || reqs != 1 {
		t.Fatalf("global partially adjusted=(%d,%d)", spend, reqs)
	}
	if spend, reqs := readGlobalMonth(t, s, r.Period.Month); spend != r.ReservedPUSD || reqs != 1 {
		t.Fatalf("global month partially adjusted=(%d,%d)", spend, reqs)
	}
}

func TestSettleRefundAndTopUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actual func(int64) int64
	}{
		{"refund", func(r int64) int64 { return r / 4 }},
		{"top-up", func(r int64) int64 { return r + 12345 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderDeepSeek, 10, 10), limits())
			actual := tc.actual(r.ReservedPUSD)
			if err := s.Settle(context.Background(), r, actual); err != nil {
				t.Fatalf("settle: %v", err)
			}
			if got, _ := readInstall(t, s, r.InstallID, r.Period.Day); got != actual {
				t.Fatalf("install=%d want %d", got, actual)
			}
			if got, _ := readProvider(t, s, r.Plan.Provider, r.Period.Day); got != actual {
				t.Fatalf("provider=%d want %d", got, actual)
			}
			if got, _ := readGlobal(t, s, r.Period.Day); got != actual {
				t.Fatalf("global=%d want %d", got, actual)
			}
			if got, _ := readGlobalMonth(t, s, r.Period.Month); got != actual {
				t.Fatalf("global month=%d want %d", got, actual)
			}
			state, charged := readLedger(t, s, r.RequestID)
			if state != stateSettled || !charged.Valid || charged.Int64 != actual {
				t.Fatalf("ledger=(%s,%v)", state, charged)
			}
		})
	}
}

func TestSettleTopUpOverflowRollsBackLedgerAndEveryWallet(t *testing.T) {
	s := newTestStore(t)
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderDeepSeek, 10, 10), limits())
	const topUp int64 = 12345
	nearOverflow := math.MaxInt64 - topUp + 1
	ctx := context.Background()
	for _, update := range []struct {
		query string
		args  []any
	}{
		{`UPDATE install_spend_daily SET spend_pusd=? WHERE install_id=? AND period_day=?`, []any{nearOverflow, r.InstallID, r.Period.Day}},
		{`UPDATE provider_spend_daily SET spend_pusd=? WHERE provider=? AND period_day=?`, []any{nearOverflow, string(r.Plan.Provider), r.Period.Day}},
		{`UPDATE global_spend_daily SET spend_pusd=? WHERE period_day=?`, []any{nearOverflow, r.Period.Day}},
	} {
		if _, err := s.writer.Exec(ctx, update.query, update.args...); err != nil {
			t.Fatalf("seed near-overflow wallet: %v", err)
		}
	}

	if err := s.Settle(ctx, r, r.ReservedPUSD+topUp); err == nil {
		t.Fatal("top-up beyond int64 must fail atomically")
	}
	state, charged := readLedger(t, s, r.RequestID)
	if state != stateOpen || charged.Valid {
		t.Fatalf("ledger CAS survived failed top-up: state=%s charged=%v", state, charged)
	}
	if got, _ := readInstall(t, s, r.InstallID, r.Period.Day); got != nearOverflow {
		t.Fatalf("install wallet changed: got=%d want=%d", got, nearOverflow)
	}
	if got, _ := readProvider(t, s, r.Plan.Provider, r.Period.Day); got != nearOverflow {
		t.Fatalf("provider wallet changed: got=%d want=%d", got, nearOverflow)
	}
	if got, _ := readGlobal(t, s, r.Period.Day); got != nearOverflow {
		t.Fatalf("global wallet changed: got=%d want=%d", got, nearOverflow)
	}
}

func TestTerminalCASIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderDeepSeek, 10, 10), limits())
	actual := r.ReservedPUSD / 2
	if err := s.Settle(context.Background(), r, actual); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := s.Settle(context.Background(), r, 0); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("late rollback: %v", err)
	}
	if got, _ := readGlobal(t, s, r.Period.Day); got != actual {
		t.Fatalf("terminal loser adjusted balance=%d", got)
	}
}

func TestOrphanKeepsFullReservedSpend(t *testing.T) {
	s := newTestStore(t)
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderQwen, 10, 10), limits())
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE spend_ledger SET created_at=? WHERE request_id=?`, time.Now().UTC().Add(-time.Hour), r.RequestID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := s.ReconcileOrphans(context.Background(), time.Now().UTC().Add(-time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("reconcile=(%d,%v)", n, err)
	}
	if got, _ := readGlobal(t, s, r.Period.Day); got != r.ReservedPUSD {
		t.Fatalf("orphan refunded spend: got %d want %d", got, r.ReservedPUSD)
	}
	if got := readMonth(t, s, r.InstallID, r.Period.Month); got != 1 {
		t.Fatalf("orphan refunded month count=%d", got)
	}
	state, charged := readLedger(t, s, r.RequestID)
	if state != stateOrphaned || !charged.Valid || charged.Int64 != r.ReservedPUSD {
		t.Fatalf("orphan ledger=(%s,%v)", state, charged)
	}
}

func TestSettleVsOrphanRaceHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderDeepSeek, 10, 10), limits())
	if _, err := s.writer.Exec(context.Background(),
		`UPDATE spend_ledger SET created_at=? WHERE request_id=?`, time.Now().UTC().Add(-time.Hour), r.RequestID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	actual := r.ReservedPUSD / 3
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = s.Settle(context.Background(), r, actual) }()
	go func() {
		defer wg.Done()
		_, _ = s.ReconcileOrphans(context.Background(), time.Now().UTC().Add(-time.Minute))
	}()
	wg.Wait()
	got, _ := readGlobal(t, s, r.Period.Day)
	if got != actual && got != r.ReservedPUSD {
		t.Fatalf("race double-adjusted global=%d", got)
	}
	state, _ := readLedger(t, s, r.RequestID)
	if state != stateSettled && state != stateOrphaned {
		t.Fatalf("state=%s", state)
	}
}

func TestViewAndPeriodSnapshotReuse(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 10, 10)
	entry := quota.Period{Month: "2026-06", Day: "2026-06-20"}
	r, err := s.Reserve(context.Background(), "ins_1", p, entry, limits())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	used, installSpend, globalSpend, err := s.View(context.Background(), "ins_1", entry)
	if err != nil || used != 1 || installSpend != p.ReservedPUSD || globalSpend != p.ReservedPUSD {
		t.Fatalf("view=(%d,%d,%d,%v)", used, installSpend, globalSpend, err)
	}
	if err := s.Settle(context.Background(), r, 0); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got, _ := readGlobal(t, s, entry.Day); got != 0 {
		t.Fatalf("entry day=%d", got)
	}
	if got, _ := readGlobal(t, s, "2026-06-21"); got != 0 {
		t.Fatalf("next day touched=%d", got)
	}
}

func TestOpenReservationsCountsOnlyOpen(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	open := reserve(t, s, "ins_open", p, limits())
	done := reserve(t, s, "ins_done", p, limits())
	if err := s.Settle(context.Background(), done, done.ReservedPUSD); err != nil {
		t.Fatalf("settle: %v", err)
	}
	n, err := s.OpenReservations(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("open=(%d,%v)", n, err)
	}
	_ = open
}

func TestResetMonthlyRequestsRequiresTerminalLedgerAndPreservesSpend(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 100, 20)
	open := reserve(t, s, "ins_open", p, limits())
	settled := reserve(t, s, "ins_settled", p, limits())
	if err := s.Settle(context.Background(), settled, settled.ReservedPUSD); err != nil {
		t.Fatalf("settle: %v", err)
	}

	if _, err := s.ResetMonthlyRequests(context.Background(), testPeriod()); !errors.Is(err, quota.ErrMonthlyResetBlocked) {
		t.Fatalf("reset with open ledger error = %v, want ErrMonthlyResetBlocked", err)
	}
	if got := readMonth(t, s, "ins_open", testPeriod().Month); got != 1 {
		t.Fatalf("blocked reset changed open install count to %d, want 1", got)
	}
	if err := s.Settle(context.Background(), open, open.ReservedPUSD); err != nil {
		t.Fatalf("settle open: %v", err)
	}
	oldPeriod := quota.Period{Month: "2026-05", Day: "2026-05-31"}
	oldOpen, err := s.Reserve(context.Background(), "ins_previous", p, oldPeriod, limits())
	if err != nil {
		t.Fatalf("reserve old-period request: %v", err)
	}
	if _, err := s.ResetMonthlyRequests(context.Background(), testPeriod()); !errors.Is(err, quota.ErrMonthlyResetBlocked) {
		t.Fatalf("reset with old-period open ledger error = %v, want ErrMonthlyResetBlocked", err)
	}
	if err := s.Settle(context.Background(), oldOpen, oldOpen.ReservedPUSD); err != nil {
		t.Fatalf("settle old-period request: %v", err)
	}

	beforeSpend, beforeRequests := readGlobalMonth(t, s, testPeriod().Month)
	reset, err := s.ResetMonthlyRequests(context.Background(), testPeriod())
	if err != nil {
		t.Fatalf("reset monthly requests: %v", err)
	}
	if reset != 2 {
		t.Fatalf("reset installs = %d, want 2", reset)
	}
	for _, id := range []string{"ins_open", "ins_settled"} {
		if got := readMonth(t, s, id, testPeriod().Month); got != 0 {
			t.Fatalf("%s month count = %d, want 0", id, got)
		}
	}
	afterSpend, afterRequests := readGlobalMonth(t, s, testPeriod().Month)
	if afterSpend != beforeSpend || afterRequests != beforeRequests {
		t.Fatalf("quota reset changed global month spend/request stats: before=(%d,%d) after=(%d,%d)",
			beforeSpend, beforeRequests, afterSpend, afterRequests)
	}

	// A reservation made after the reset belongs entirely to the fresh counter;
	// its definite-unbilled rollback must still reach exactly zero.
	fresh := reserve(t, s, "ins_open", p, limits())
	if err := s.Rollback(context.Background(), fresh); err != nil {
		t.Fatalf("rollback fresh reservation: %v", err)
	}
	if got := readMonth(t, s, "ins_open", testPeriod().Month); got != 0 {
		t.Fatalf("fresh rollback count = %d, want 0", got)
	}
}

// speechPlan builds a frozen n-character speech plan (the second category).
func speechPlan(t *testing.T, chars int64) billing.Plan {
	t.Helper()
	p, err := billing.NewCharactersPlan(billing.ProviderQwen, billing.QwenAudio30TTSFlash, chars)
	if err != nil {
		t.Fatalf("characters plan: %v", err)
	}
	return p
}

// TestReserve_CategoriesAreIndependentLedgers proves the two categories never see each other: a
// speech reservation consumes CHARACTERS against SpeechDailyLimit, an exhausted image cap does not
// block speech (and vice versa), and each denial NAMES its own ledger so the app layer can map it
// to the right wire code. A shared counter — or a denial that cannot say which ledger it came
// from — would tell a user whose images ran out that speech is unavailable too.
//
// 两个品类互不相见:语音预留按**字符**对 SpeechDailyLimit 记账;图像限额耗尽不挡语音、反之亦然;
// 每个拒绝**点名**自己的账本,app 层才能映到正确 wire 码。共用计数器——或一个说不出自己来自哪个
// 账本的拒绝——会让图像用尽的用户被告知语音也没了。
func TestReserve_CategoriesAreIndependentLedgers(t *testing.T) {
	s := newTestStore(t)
	lim := limits()
	lim.ImageDailyLimit = 1
	lim.SpeechDailyLimit = 100

	img := reserve(t, s, "ins_a", imagePlan(t), lim)
	if img.CategoryApplied != quota.CategoryImage || img.CategoryUnits != 1 {
		t.Fatalf("image reservation = %q/%d", img.CategoryApplied, img.CategoryUnits)
	}
	sp := reserve(t, s, "ins_a", speechPlan(t, 60), lim)
	if sp.CategoryApplied != quota.CategorySpeech || sp.CategoryUnits != 60 {
		t.Fatalf("speech reservation = %q/%d, want speech/60", sp.CategoryApplied, sp.CategoryUnits)
	}

	// Image cap is now exhausted; speech still has 40 characters left. 图像满了,语音还剩 40。
	var catErr *quota.CategoryDailyExceededError
	_, err := s.Reserve(context.Background(), "ins_a", imagePlan(t), testPeriod(), lim)
	if !errors.As(err, &catErr) || catErr.Category != quota.CategoryImage {
		t.Fatalf("image denial = %v, want a denial naming the image ledger", err)
	}
	if !errors.Is(err, quota.ErrCategoryDailyExceeded) {
		t.Fatalf("category denial must still satisfy the umbrella sentinel: %v", err)
	}
	reserve(t, s, "ins_a", speechPlan(t, 40), lim)

	// Now speech is exhausted too, and ITS denial names the speech ledger. 语音也满了,拒绝点名语音。
	_, err = s.Reserve(context.Background(), "ins_a", speechPlan(t, 1), testPeriod(), lim)
	if !errors.As(err, &catErr) || catErr.Category != quota.CategorySpeech {
		t.Fatalf("speech denial = %v, want a denial naming the speech ledger", err)
	}
}

// TestRollback_ReversesSpeechCharacters proves the recorded-units reversal is unit-agnostic: a
// rolled-back 80-character reservation frees exactly 80 characters, not one "request".
func TestRollback_ReversesSpeechCharacters(t *testing.T) {
	s := newTestStore(t)
	lim := limits()
	lim.SpeechDailyLimit = 100

	r := reserve(t, s, "ins_a", speechPlan(t, 80), lim)
	if _, err := s.Reserve(context.Background(), "ins_a", speechPlan(t, 80), testPeriod(), lim); !errors.Is(err, quota.ErrCategoryDailyExceeded) {
		t.Fatalf("cap not enforced before rollback: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	reserve(t, s, "ins_a", speechPlan(t, 80), lim) // the freed 80 characters are available again
}

// imagePlan builds a frozen one-image plan (the first category).
func imagePlan(t *testing.T) billing.Plan {
	t.Helper()
	p, err := billing.NewImagesPlan(billing.ProviderQwen, billing.QwenImage20, 1)
	if err != nil {
		t.Fatalf("images plan: %v", err)
	}
	return p
}

// TestReserve_ImageCategoryDailyGate proves gate 2c: image reservations consume the per-install
// per-day image units inside the same transaction, deny with the typed sentinel at the cap, and
// leave non-category plans entirely unaffected.
func TestReserve_ImageCategoryDailyGate(t *testing.T) {
	s := newTestStore(t)
	lim := limits()
	lim.ImageDailyLimit = 2

	r1 := reserve(t, s, "ins_a", imagePlan(t), lim)
	if r1.CategoryApplied != quota.CategoryImage || r1.CategoryUnits != 1 {
		t.Fatalf("reservation category = %q/%d, want image/1", r1.CategoryApplied, r1.CategoryUnits)
	}
	reserve(t, s, "ins_a", imagePlan(t), lim)

	if _, err := s.Reserve(context.Background(), "ins_a", imagePlan(t), testPeriod(), lim); !errors.Is(err, quota.ErrCategoryDailyExceeded) {
		t.Fatalf("third image reserve err = %v, want ErrCategoryDailyExceeded", err)
	}
	// A different install has its own ledger; a chat plan on the capped install is untouched.
	reserve(t, s, "ins_b", imagePlan(t), lim)
	chat := reserve(t, s, "ins_a", mustPlan(t, billing.ProviderDeepSeek, 10, 10), lim)
	if chat.CategoryApplied != "" || chat.CategoryUnits != 0 {
		t.Fatalf("chat reservation carries category %q/%d, want none", chat.CategoryApplied, chat.CategoryUnits)
	}
}

// TestRollback_ReversesCategoryUnits proves the recorded-units reversal: after a rollback the
// freed unit is reservable again, and the reversal never re-reads live config (the cap passed at
// rollback time is irrelevant — the Reservation snapshot rules).
func TestRollback_ReversesCategoryUnits(t *testing.T) {
	s := newTestStore(t)
	lim := limits()
	lim.ImageDailyLimit = 1

	r := reserve(t, s, "ins_a", imagePlan(t), lim)
	if _, err := s.Reserve(context.Background(), "ins_a", imagePlan(t), testPeriod(), lim); !errors.Is(err, quota.ErrCategoryDailyExceeded) {
		t.Fatalf("cap not enforced before rollback: %v", err)
	}
	if err := s.Rollback(context.Background(), r); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	reserve(t, s, "ins_a", imagePlan(t), lim) // the freed unit is available again
}

// TestReserve_CategoryRecordsWhenCapDisabled proves a disabled cap (0) still RECORDS consumption:
// enabling the cap later starts from the truthful count instead of a blank ledger.
func TestReserve_CategoryRecordsWhenCapDisabled(t *testing.T) {
	s := newTestStore(t)
	lim := limits() // ImageDailyLimit zero-value = disabled

	reserve(t, s, "ins_a", imagePlan(t), lim)
	reserve(t, s, "ins_a", imagePlan(t), lim)

	var units int64
	row := s.reader.QueryRow(context.Background(),
		`SELECT units FROM install_category_daily WHERE install_id='ins_a' AND category='image'`)
	if err := row.Scan(&units); err != nil {
		t.Fatalf("scan units: %v", err)
	}
	if units != 2 {
		t.Fatalf("recorded units = %d, want 2 despite disabled cap", units)
	}

	lim.ImageDailyLimit = 2 // enabling now must deny immediately — history counts
	if _, err := s.Reserve(context.Background(), "ins_a", imagePlan(t), testPeriod(), lim); !errors.Is(err, quota.ErrCategoryDailyExceeded) {
		t.Fatalf("post-enable reserve err = %v, want ErrCategoryDailyExceeded", err)
	}
}
