package quota

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	dquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// fakeRepo records calls and returns scripted results, isolating the Service's
// policy (error mapping, availability rule, cutoff math) from any real DB.
type fakeRepo struct {
	reserveErr error
	reserveOut *dquota.Reservation
	gotLimits  Limits

	settleErr error
	rbErr     error

	used, installSpend, globalSpend int64
	viewErr                         error

	gotCutoff      time.Time
	reconcileN     int
	reconcileErr   error
	reconcileCalls int
}

func (f *fakeRepo) Reserve(_ context.Context, _ string, _ billing.Plan, _ dquota.Period, lim Limits) (*dquota.Reservation, error) {
	f.gotLimits = lim
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return f.reserveOut, nil
}
func (f *fakeRepo) Settle(_ context.Context, _ *dquota.Reservation, _ int64) error {
	return f.settleErr
}
func (f *fakeRepo) Rollback(_ context.Context, _ *dquota.Reservation) error { return f.rbErr }
func (f *fakeRepo) ReconcileOrphans(_ context.Context, olderThan time.Time) (int, error) {
	f.reconcileCalls++
	f.gotCutoff = olderThan
	return f.reconcileN, f.reconcileErr
}
func (f *fakeRepo) View(_ context.Context, _ string, _ dquota.Period) (int64, int64, int64, error) {
	return f.used, f.installSpend, f.globalSpend, f.viewErr
}

type fakeCfg struct {
	lim Limits
	loc *time.Location
}

func (c fakeCfg) Limits() Limits           { return c.lim }
func (c fakeCfg) Location() *time.Location { return c.loc }

func newService(repo *fakeRepo, lim Limits) *Service {
	return New(repo, fakeCfg{lim: lim, loc: time.UTC})
}

func testPlan(t *testing.T) billing.Plan {
	t.Helper()
	p, err := billing.NewPlan(billing.ProviderDeepSeek, billing.DeepSeekV4Flash, billing.InputStandard, 10, 10)
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	return p
}

func TestReserveMapsTypedDenialsToSentinels(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want *apierr.APIError
	}{
		{"monthly", dquota.ErrMonthlyExhausted, apierr.ErrQuotaExhausted},
		{"install-spend", dquota.ErrInstallSpendExceeded, apierr.ErrRateLimited},
		{"sublimit", dquota.ErrSublimitExceeded, apierr.ErrRateLimited},
		{"provider-spend", dquota.ErrProviderSpendExceeded, apierr.ErrBudgetExhausted},
		{"budget", dquota.ErrBudgetExceeded, apierr.ErrBudgetExhausted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newService(&fakeRepo{reserveErr: c.in}, Limits{})
			_, err := s.Reserve(context.Background(), "ins_1", testPlan(t), dquota.Period{})
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestReserveBudgetIs402(t *testing.T) {
	s := newService(&fakeRepo{reserveErr: dquota.ErrBudgetExceeded}, Limits{})
	_, err := s.Reserve(context.Background(), "ins_1", testPlan(t), dquota.Period{})
	var ae *apierr.APIError
	if !errors.As(err, &ae) || ae.Status != 402 {
		t.Fatalf("budget deny should be 402, got %v", err)
	}
}

func TestReserveInternalErrorPassesThrough(t *testing.T) {
	boom := errors.New("disk on fire")
	s := newService(&fakeRepo{reserveErr: boom}, Limits{})
	_, err := s.Reserve(context.Background(), "ins_1", testPlan(t), dquota.Period{})
	if !errors.Is(err, boom) {
		t.Fatalf("non-deny error should pass through, got %v", err)
	}
}

func TestReservePassesLiveLimitsSnapshot(t *testing.T) {
	repo := &fakeRepo{reserveOut: &dquota.Reservation{RequestID: "req_x"}}
	want := Limits{
		MonthlyQuota: 100, InstallDailySpendPUSD: 50, GlobalDailySpendPUSD: 1000, DailySublimit: 7,
		ProviderDailySpendPUSD: map[billing.Provider]int64{billing.ProviderDeepSeek: 500},
	}
	s := newService(repo, want)
	if _, err := s.Reserve(context.Background(), "ins_1", testPlan(t), dquota.Period{}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !reflect.DeepEqual(repo.gotLimits, want) {
		t.Fatalf("store got limits %+v, want %+v", repo.gotLimits, want)
	}
}

func TestSettleRollbackErrorsAreReturned(t *testing.T) {
	// B2: failures must be observable to the caller, never swallowed.
	se := errors.New("settle boom")
	if err := newService(&fakeRepo{settleErr: se}, Limits{}).
		Settle(context.Background(), &dquota.Reservation{}, 5); !errors.Is(err, se) {
		t.Fatalf("Settle should return its error, got %v", err)
	}
	re := errors.New("rollback boom")
	if err := newService(&fakeRepo{rbErr: re}, Limits{}).
		Rollback(context.Background(), &dquota.Reservation{}); !errors.Is(err, re) {
		t.Fatalf("Rollback should return its error, got %v", err)
	}
}

func TestViewAvailabilityRule(t *testing.T) {
	lim := Limits{MonthlyQuota: 100, InstallDailySpendPUSD: 500, GlobalDailySpendPUSD: 1000}
	cases := []struct {
		name                            string
		used, installSpend, globalSpend int64
		wantRemaining                   int64
		wantAvail                       bool
	}{
		{"plenty", 10, 100, 100, 90, true},
		{"month-exhausted", 100, 0, 0, 0, false},
		{"over-month-clamps", 150, 0, 0, 0, false},
		{"install-exhausted", 10, 500, 0, 90, false},
		{"install-over", 10, 600, 0, 90, false},
		{"global-exhausted", 10, 0, 1000, 90, false},
		{"global-over", 10, 0, 2000, 90, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeRepo{used: c.used, installSpend: c.installSpend, globalSpend: c.globalSpend}
			v, err := newService(repo, lim).View(context.Background(), "ins_1", dquota.Period{Month: "2026-06"})
			if err != nil {
				t.Fatalf("view: %v", err)
			}
			if v.Remaining != c.wantRemaining {
				t.Errorf("remaining = %d, want %d", v.Remaining, c.wantRemaining)
			}
			if v.Available != c.wantAvail {
				t.Errorf("available = %v, want %v", v.Available, c.wantAvail)
			}
			if v.Limit != lim.MonthlyQuota {
				t.Errorf("limit = %d, want %d", v.Limit, lim.MonthlyQuota)
			}
		})
	}
}

func TestViewResetAt(t *testing.T) {
	v, err := newService(&fakeRepo{}, Limits{MonthlyQuota: 1}).
		View(context.Background(), "ins_1", dquota.Period{Month: "2026-06"})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !v.ResetAt.Equal(want) {
		t.Errorf("resetAt = %s, want %s", v.ResetAt, want)
	}
}

func TestReconcileOrphansComputesCutoffFromNow(t *testing.T) {
	repo := &fakeRepo{reconcileN: 3}
	s := newService(repo, Limits{})
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	n, err := s.ReconcileOrphans(context.Background(), 10*time.Minute, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if want := now.Add(-10 * time.Minute); !repo.gotCutoff.Equal(want) {
		t.Errorf("cutoff = %s, want %s", repo.gotCutoff, want)
	}
}

func TestSnapshotPeriodUsesConfiguredLocation(t *testing.T) {
	s := newService(&fakeRepo{}, Limits{})
	p := s.SnapshotPeriod(time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	if p.Day != "2026-06-20" {
		t.Errorf("day = %q, want 2026-06-20", p.Day)
	}
}
