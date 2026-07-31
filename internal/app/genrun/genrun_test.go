package genrun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// These assert the rules that used to be asserted three to five times over, once
// per capability. That is the whole point of the package: a rule with one home
// can be verified in one place, and a change to it cannot go green in four suites
// while staying broken in the fifth.
//
// 这里断言的是过去被断言了三到五遍、每个能力一遍的那些规则。这正是本包的全部意义:一条只有一个家
// 的规则可以在一个地方被验证,而对它的改动不可能在四套测试里绿着、在第五套里坏着。

type fakeAuth struct {
	status  dominstall.Status
	found   bool
	err     error
	lookups int
}

func (f *fakeAuth) LookupInstall(_ context.Context, id string) (string, dominstall.Status, bool, error) {
	f.lookups++
	if f.err != nil {
		return "", "", false, f.err
	}
	if !f.found {
		return "", "", false, nil
	}
	return id, f.status, true, nil
}

type denyAll struct{}

func (denyAll) Allow(string) bool { return false }

type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) }

type fakeQuota struct {
	reserveErr error
	settleErr  error
	rollbackEr error
	settled    []int64
	rolledBack int
}

func (f *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-07", Day: "2026-07-31"}
}

func (f *fakeQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return &domquota.Reservation{RequestID: "req_t", InstallID: installID, Period: p,
		Plan: plan, ReservedPUSD: plan.ReservedPUSD}, nil
}

func (f *fakeQuota) Settle(_ context.Context, _ *domquota.Reservation, actual int64) error {
	f.settled = append(f.settled, actual)
	return f.settleErr
}

func (f *fakeQuota) Rollback(context.Context, *domquota.Reservation) error {
	f.rolledBack++
	return f.rollbackEr
}

type countingMetrics struct{ settleFails, rollbackFails int }

func (m *countingMetrics) SettleFailure()   { m.settleFails++ }
func (m *countingMetrics) RollbackFailure() { m.rollbackFails++ }

func imagePlan(t *testing.T) billing.Plan {
	t.Helper()
	p, err := billing.NewUnitPlan(billing.ProviderQwen, billing.QwenImage20, billing.InputImages, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func TestAuthorizeGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auth    *fakeAuth
		rl      RateLimiter
		id      string
		want    *apierr.APIError
		lookups int
	}{
		{"empty id never reaches the store", &fakeAuth{found: true, status: dominstall.StatusActive},
			nil, "", apierr.ErrInvalidInstall, 0},
		{"store failure is ours, not the caller's", &fakeAuth{err: errors.New("db down")},
			nil, "ins_1", apierr.Internal(), 1},
		{"unknown install", &fakeAuth{found: false}, nil, "ins_1", apierr.ErrInvalidInstall, 1},
		{"banned install", &fakeAuth{found: true, status: dominstall.StatusBanned},
			nil, "ins_1", apierr.ErrAccountBanned, 1},
		{"paced install", &fakeAuth{found: true, status: dominstall.StatusActive},
			denyAll{}, "ins_1", apierr.ErrRateLimited, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Ports{Auth: tc.auth, RL: tc.rl, Quota: &fakeQuota{}, Clock: fakeClock{}})
			got, ae := r.Authorize(context.Background(), tc.id)
			if ae == nil || ae.Code != tc.want.Code || ae.Status != tc.want.Status {
				t.Fatalf("Authorize = %q,%v; want %s", got, ae, tc.want.Code)
			}
			if tc.auth.lookups != tc.lookups {
				t.Fatalf("store lookups = %d, want %d", tc.auth.lookups, tc.lookups)
			}
		})
	}

	t.Run("an active install passes and yields the stored id", func(t *testing.T) {
		r := New(Ports{Auth: &fakeAuth{found: true, status: dominstall.StatusActive},
			Quota: &fakeQuota{}, Clock: fakeClock{}})
		got, ae := r.Authorize(context.Background(), "ins_1")
		if ae != nil || got != "ins_1" {
			t.Fatalf("Authorize = %q,%v", got, ae)
		}
	})

	t.Run("an unwired authenticator is our fault, not a bad request", func(t *testing.T) {
		_, ae := Runner{}.Authorize(context.Background(), "ins_1")
		if ae == nil || ae.Status != 500 {
			t.Fatalf("Authorize on an empty Runner = %v, want 500", ae)
		}
	})
}

// TestDoSettlesTheDeterministicUnitCost: the quantity is known before the call, so
// a success closes at exactly the unit price and the caller gets its result.
func TestDoSettlesTheDeterministicUnitCost(t *testing.T) {
	q := &fakeQuota{}
	r := New(Ports{Quota: q, Clock: fakeClock{}})
	plan := imagePlan(t)

	out, ae := Do(context.Background(), r, "ins_1",
		Charge{Plan: plan, Class: billing.InputImages, Units: 1},
		func(context.Context) (string, bool, error) { return "https://x/y.png", false, nil })
	if ae != nil || out != "https://x/y.png" {
		t.Fatalf("Do = %q,%v", out, ae)
	}
	want, err := plan.UnitCost(billing.InputImages, 1)
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	if len(q.settled) != 1 || q.settled[0] != want {
		t.Fatalf("settled = %v, want [%d]", q.settled, want)
	}
	if q.rolledBack != 0 {
		t.Fatalf("a success must not roll anything back (got %d)", q.rolledBack)
	}
}

// TestDoKeepsTheChargeWhenTheOutcomeIsAmbiguous is GW-INV-50, the rule this
// package exists to state once: only a provably-unbilled rejection is refunded.
// Anything else — timeout, connect failure, 5xx, cancel — may already have cost
// the operator money, so it settles at the FULL quote.
//
// 这是 GW-INV-50,本包之所以存在、要只说一遍的那条规则:**只有**可证明未计费的拒绝才退。其余一切
// ——超时、连不上、5xx、取消——都可能已经花掉了 operator 的钱,故按**全额**报价结算。
func TestDoKeepsTheChargeWhenTheOutcomeIsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unbilled   bool
		wantSettle bool
	}{
		{"a provably-unbilled rejection is refunded", true, false},
		{"an ambiguous failure is charged in full", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			r := New(Ports{Quota: q, Clock: fakeClock{}})
			plan := imagePlan(t)

			_, ae := Do(context.Background(), r, "ins_1",
				Charge{Plan: plan, Class: billing.InputImages, Units: 1},
				func(context.Context) (string, bool, error) {
					return "", tc.unbilled, errors.New("upstream exploded")
				})
			if ae != apierr.ErrUpstreamError {
				t.Fatalf("Do error = %v, want ErrUpstreamError", ae)
			}
			if tc.wantSettle {
				if len(q.settled) != 1 || q.settled[0] != plan.ReservedPUSD {
					t.Fatalf("settled = %v, want [%d] (the full quote)", q.settled, plan.ReservedPUSD)
				}
				if q.rolledBack != 0 {
					t.Fatalf("an ambiguous outcome must never roll back (got %d)", q.rolledBack)
				}
			} else {
				if q.rolledBack != 1 || len(q.settled) != 0 {
					t.Fatalf("rolledBack=%d settled=%v; want exactly one rollback and no settle",
						q.rolledBack, q.settled)
				}
			}
		})
	}
}

// TestDoPassesAnUpstreamAPIErrorThrough: a provider rejection the infra client
// already normalized into a wire error must reach the client as that error, not
// be flattened into a generic upstream failure.
func TestDoPassesAnUpstreamAPIErrorThrough(t *testing.T) {
	q := &fakeQuota{}
	r := New(Ports{Quota: q, Clock: fakeClock{}})

	_, ae := Do(context.Background(), r, "ins_1",
		Charge{Plan: imagePlan(t), Class: billing.InputImages, Units: 1},
		func(context.Context) (string, bool, error) { return "", true, apierr.ErrBadRequest })
	if ae != apierr.ErrBadRequest {
		t.Fatalf("Do error = %v, want the upstream's own ErrBadRequest", ae)
	}
}

// TestDoCannotUnderChargeOnAFrozenCardFailure: if pricing the settlement fails,
// the fallback is the full reservation. Charging zero because the arithmetic
// broke would hand out the capability for free.
//
// 若给结算定价失败,兜底是**全额**预留。因为算术坏了就收零,等于白送这个能力。
func TestDoCannotUnderChargeOnAFrozenCardFailure(t *testing.T) {
	q := &fakeQuota{}
	r := New(Ports{Quota: q, Clock: fakeClock{}})
	plan := imagePlan(t)

	// A class the plan was not built for cannot be priced — the same shape a
	// mid-flight card drift would produce.
	_, ae := Do(context.Background(), r, "ins_1",
		Charge{Plan: plan, Class: billing.InputVideoSeconds, Units: 3},
		func(context.Context) (string, bool, error) { return "ok", false, nil })
	if ae != nil {
		t.Fatalf("Do = %v, want success", ae)
	}
	if len(q.settled) != 1 || q.settled[0] != plan.ReservedPUSD {
		t.Fatalf("settled = %v, want [%d] (the full quote)", q.settled, plan.ReservedPUSD)
	}
}

// TestBooksThatDoNotCloseAreObservable: a failed settle or rollback under-reports
// the operator's wallet until the orphan scanner finalizes it, so it must be
// counted rather than silently swallowed.
func TestBooksThatDoNotCloseAreObservable(t *testing.T) {
	t.Run("settle", func(t *testing.T) {
		mx := &countingMetrics{}
		r := New(Ports{Quota: &fakeQuota{settleErr: errors.New("disk full")}, Clock: fakeClock{}, Metrics: mx})
		_, _ = Do(context.Background(), r, "ins_1",
			Charge{Plan: imagePlan(t), Class: billing.InputImages, Units: 1},
			func(context.Context) (string, bool, error) { return "ok", false, nil })
		if mx.settleFails != 1 {
			t.Fatalf("settle failures = %d, want 1", mx.settleFails)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		mx := &countingMetrics{}
		r := New(Ports{Quota: &fakeQuota{rollbackEr: errors.New("disk full")}, Clock: fakeClock{}, Metrics: mx})
		_, _ = Do(context.Background(), r, "ins_1",
			Charge{Plan: imagePlan(t), Class: billing.InputImages, Units: 1},
			func(context.Context) (string, bool, error) { return "", true, errors.New("rejected") })
		if mx.rollbackFails != 1 {
			t.Fatalf("rollback failures = %d, want 1", mx.rollbackFails)
		}
	})
}

// TestReserveRefusalReachesTheClientVerbatim: a quota gate speaks its own wire
// error (which entitlement ran out is the caller's business); anything else is a
// failure of ours and becomes a 500.
func TestReserveRefusalReachesTheClientVerbatim(t *testing.T) {
	r := New(Ports{Quota: &fakeQuota{reserveErr: apierr.ErrQuotaExhausted}, Clock: fakeClock{}})
	_, ae := r.Reserve(context.Background(), "ins_1", imagePlan(t))
	if ae != apierr.ErrQuotaExhausted {
		t.Fatalf("Reserve error = %v, want ErrQuotaExhausted", ae)
	}

	r = New(Ports{Quota: &fakeQuota{reserveErr: errors.New("db down")}, Clock: fakeClock{}})
	if _, ae = r.Reserve(context.Background(), "ins_1", imagePlan(t)); ae == nil || ae.Status != 500 {
		t.Fatalf("Reserve error = %v, want 500", ae)
	}
}

// TestReserveNeedsOnlyTheWalletAndTheClock pins the guard's scope: the realtime
// path reserves from a service that was never given an authenticator, because the
// WebSocket already authorized. A guard written as "all ports" would break it.
//
// 这条钉住守卫的**范围**:实时那条路是从一个从未拿到鉴权器的服务上预留的,因为 WebSocket 早已鉴过权。
// 一个写成「所有端口」的守卫会弄坏它。
func TestReserveNeedsOnlyTheWalletAndTheClock(t *testing.T) {
	r := New(Ports{Quota: &fakeQuota{}, Clock: fakeClock{}})
	if _, ae := r.Reserve(context.Background(), "ins_1", imagePlan(t)); ae != nil {
		t.Fatalf("Reserve without an authenticator = %v, want success", ae)
	}
	if _, ae := (Runner{}).Reserve(context.Background(), "ins_1", imagePlan(t)); ae == nil || ae.Status != 500 {
		t.Fatalf("Reserve with no wallet = %v, want 500", ae)
	}
}

// TestSettleAndRollbackIgnoreANilReservation: the realtime path closes a session
// that may never have reserved (an unauthorized socket, a client that hung up
// before speaking), and that is not an error.
func TestSettleAndRollbackIgnoreANilReservation(t *testing.T) {
	q := &fakeQuota{}
	r := New(Ports{Quota: q, Clock: fakeClock{}})
	if err := r.Settle(context.Background(), nil, 10); err != nil {
		t.Fatalf("Settle(nil) = %v", err)
	}
	if err := r.Rollback(context.Background(), nil); err != nil {
		t.Fatalf("Rollback(nil) = %v", err)
	}
	if len(q.settled) != 0 || q.rolledBack != 0 {
		t.Fatalf("a nil reservation must not touch the wallet: settled=%v rolledBack=%d",
			q.settled, q.rolledBack)
	}
}
