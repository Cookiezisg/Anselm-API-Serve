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
			provider TEXT NOT NULL CHECK(provider IN ('deepseek','kimi')),
			period_day TEXT NOT NULL, spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0),
			PRIMARY KEY(provider, period_day))`,
		`CREATE TABLE global_spend_daily (
			period_day TEXT PRIMARY KEY, spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK(spend_pusd >= 0),
			requests INTEGER NOT NULL DEFAULT 0 CHECK(requests >= 0))`,
		`CREATE TABLE spend_ledger (
			request_id TEXT PRIMARY KEY, install_id TEXT NOT NULL,
			provider TEXT NOT NULL CHECK(provider IN ('deepseek','kimi')),
			model TEXT NOT NULL, rate_card_id TEXT NOT NULL,
			period_month TEXT NOT NULL, period_day TEXT NOT NULL,
			reserved_pusd INTEGER NOT NULL CHECK(reserved_pusd > 0),
			charged_pusd INTEGER CHECK(charged_pusd >= 0),
			state TEXT NOT NULL CHECK(state IN ('open','settled','rolled_back','orphaned')),
			sublimit_applied INTEGER NOT NULL CHECK(sublimit_applied IN (0,1)),
			created_at DATETIME NOT NULL, terminal_at DATETIME)`,
		`CREATE INDEX idx_spend_ledger_open ON spend_ledger(state, created_at)`,
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
	if provider == billing.ProviderKimi {
		model = billing.KimiK26
	}
	p, err := billing.NewPlan(provider, model, billing.InputStandard, prompt, output)
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	return p
}

func limits() quota.Limits {
	return quota.Limits{
		MonthlyQuota:          1_000_000,
		InstallDailySpendPUSD: 1_000_000_000_000_000,
		ProviderDailySpendPUSD: map[billing.Provider]int64{
			billing.ProviderDeepSeek: 1_000_000_000_000_000,
			billing.ProviderKimi:     1_000_000_000_000_000,
		},
		GlobalDailySpendPUSD: 1_000_000_000_000_000,
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
}

func TestReserveDenialsRollbackWholeTransaction(t *testing.T) {
	plan := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	cases := []struct {
		name string
		edit func(*quota.Limits)
		want error
	}{
		{"monthly", func(l *quota.Limits) { l.MonthlyQuota = 0 }, quota.ErrMonthlyExhausted},
		{"install", func(l *quota.Limits) { l.InstallDailySpendPUSD = plan.ReservedPUSD - 1 }, quota.ErrInstallSpendExceeded},
		{"provider", func(l *quota.Limits) { l.ProviderDailySpendPUSD[billing.ProviderDeepSeek] = plan.ReservedPUSD - 1 }, quota.ErrProviderSpendExceeded},
		{"global", func(l *quota.Limits) { l.GlobalDailySpendPUSD = plan.ReservedPUSD - 1 }, quota.ErrBudgetExceeded},
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

func TestReserveHonorsPreexistingConservativeWalletFloor(t *testing.T) {
	plan := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	const floor int64 = 9_000_000
	cases := []struct {
		name     string
		seed     func(context.Context, *Store) error
		limits   func(quota.Limits) quota.Limits
		wantDeny error
	}{
		{
			name: "install",
			seed: func(ctx context.Context, s *Store) error {
				_, err := s.writer.Exec(ctx,
					`INSERT INTO install_spend_daily(install_id,period_day,spend_pusd,requests)
					 VALUES ('ins_1',?, ?,0)`, testPeriod().Day, floor)
				return err
			},
			limits: func(l quota.Limits) quota.Limits {
				l.InstallDailySpendPUSD = floor
				return l
			},
			wantDeny: quota.ErrInstallSpendExceeded,
		},
		{
			name: "provider",
			seed: func(ctx context.Context, s *Store) error {
				_, err := s.writer.Exec(ctx,
					`INSERT INTO provider_spend_daily(provider,period_day,spend_pusd,requests)
					 VALUES ('deepseek',?, ?,1)`, testPeriod().Day, floor)
				return err
			},
			limits: func(l quota.Limits) quota.Limits {
				l.ProviderDailySpendPUSD[billing.ProviderDeepSeek] = floor
				return l
			},
			wantDeny: quota.ErrProviderSpendExceeded,
		},
		{
			name: "global",
			seed: func(ctx context.Context, s *Store) error {
				_, err := s.writer.Exec(ctx,
					`INSERT INTO global_spend_daily(period_day,spend_pusd,requests) VALUES (?, ?,1)`,
					testPeriod().Day, floor)
				return err
			},
			limits: func(l quota.Limits) quota.Limits {
				l.GlobalDailySpendPUSD = floor
				return l
			},
			wantDeny: quota.ErrBudgetExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			if err := tc.seed(ctx, s); err != nil {
				t.Fatalf("seed floor: %v", err)
			}
			_, err := s.Reserve(ctx, "ins_1", plan, testPeriod(), tc.limits(limits()))
			if !errors.Is(err, tc.wantDeny) {
				t.Fatalf("reserve err=%v want %v", err, tc.wantDeny)
			}
			if got := readMonth(t, s, "ins_1", testPeriod().Month); got != 0 {
				t.Fatalf("denied reserve leaked month count=%d", got)
			}
		})
	}
}

func TestReserveMissingProviderCapFailsClosed(t *testing.T) {
	s := newTestStore(t)
	lim := limits()
	delete(lim.ProviderDailySpendPUSD, billing.ProviderKimi)
	_, err := s.Reserve(context.Background(), "ins_1", mustPlan(t, billing.ProviderKimi, 1, 1), testPeriod(), lim)
	if !errors.Is(err, quota.ErrProviderLimitMissing) {
		t.Fatalf("err=%v want ErrProviderLimitMissing", err)
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

func TestProviderWalletsAreIsolatedButGlobalIsShared(t *testing.T) {
	s := newTestStore(t)
	ds := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	gm := mustPlan(t, billing.ProviderKimi, 1, 1)
	lim := limits()
	lim.ProviderDailySpendPUSD[billing.ProviderDeepSeek] = ds.ReservedPUSD
	lim.ProviderDailySpendPUSD[billing.ProviderKimi] = gm.ReservedPUSD
	lim.GlobalDailySpendPUSD = ds.ReservedPUSD + gm.ReservedPUSD
	reserve(t, s, "ins_ds", ds, lim)
	if _, err := s.Reserve(context.Background(), "ins_ds2", ds, testPeriod(), lim); !errors.Is(err, quota.ErrProviderSpendExceeded) {
		t.Fatalf("second DeepSeek err=%v want provider denial", err)
	}
	reserve(t, s, "ins_gm", gm, lim)
	if got, _ := readGlobal(t, s, testPeriod().Day); got != ds.ReservedPUSD+gm.ReservedPUSD {
		t.Fatalf("shared global=%d", got)
	}
}

func TestConcurrentGlobalReserveNoOversell(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderDeepSeek, 1, 1)
	const capN = 30
	const attempts = 50
	lim := limits()
	lim.GlobalDailySpendPUSD = capN * p.ReservedPUSD

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
	if got, _ := readGlobal(t, s, testPeriod().Day); got != capN*p.ReservedPUSD {
		t.Fatalf("global=%d want %d", got, capN*p.ReservedPUSD)
	}
}

func TestRollbackExactConservation(t *testing.T) {
	s := newTestStore(t)
	p := mustPlan(t, billing.ProviderKimi, 10, 5)
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
	r := reserve(t, s, "ins_1", mustPlan(t, billing.ProviderKimi, 10, 10), limits())
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
