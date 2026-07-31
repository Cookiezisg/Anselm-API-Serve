package speech

import (
	"context"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

func TestRealtimeURLDerivesWorkspaceWebSocketEndpoint(t *testing.T) {
	got, err := RealtimeURL("https://ws_1.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", DefaultModel)
	if err != nil {
		t.Fatalf("RealtimeURL: %v", err)
	}
	want := "wss://ws_1.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen3-asr-flash-realtime"
	if got != want {
		t.Fatalf("RealtimeURL = %q, want %q", got, want)
	}
}

func TestAuthorizeReturnsCanonicalInstallID(t *testing.T) {
	s := New(Deps{
		Auth:   fakeAuth{id: "ins_canonical", status: dominstall.StatusActive, found: true},
		RL:     allowAll{},
		Config: fakeConfig{cfg: &config.Config{}},
	})
	got, ae := s.Authorize(context.Background(), "public")
	if ae != nil {
		t.Fatalf("Authorize error = %v", ae)
	}
	if got != "ins_canonical" {
		t.Fatalf("install id = %q", got)
	}
}

func TestReserveBuildsASRAudioSecondsPlan(t *testing.T) {
	q := &fakeQuota{}
	s := New(Deps{Quota: q, Config: fakeConfig{cfg: &config.Config{}}, Clock: fakeClock{now: time.Date(2026, 7, 24, 1, 2, 3, 0, time.UTC)}})
	r, ae := s.Reserve(context.Background(), "ins_1", 120)
	if ae != nil {
		t.Fatalf("Reserve api error = %v", ae)
	}
	if r == nil || q.plan.Provider != billing.ProviderQwen || q.plan.Model != billing.Qwen3ASRFlashRealtime ||
		q.plan.InputClass != billing.InputAudioSeconds || q.plan.PromptQuote != 120 || q.plan.OutputQuote != 0 {
		t.Fatalf("bad plan/reservation: r=%+v plan=%+v", r, q.plan)
	}
	if q.period != (domquota.Period{Month: "2026-07", Day: "2026-07-24"}) {
		t.Fatalf("period = %+v", q.period)
	}
}

func TestSettleRoundsPCM16MonoBytesUpToSeconds(t *testing.T) {
	q := &fakeQuota{}
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, DefaultModel, billing.InputAudioSeconds, 120)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	s := New(Deps{Quota: q})
	r := &domquota.Reservation{RequestID: "req", InstallID: "ins", Plan: plan, ReservedPUSD: plan.ReservedPUSD}
	if err := s.Settle(context.Background(), r, 32_001); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	want, err := plan.UnitCost(billing.InputAudioSeconds, 2)
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	if q.settled != want {
		t.Fatalf("settled = %d, want %d", q.settled, want)
	}
}

func TestReserveMapsQuotaDenial(t *testing.T) {
	q := &fakeQuota{reserveErr: apierr.ErrQuotaExhausted}
	s := New(Deps{Quota: q, Clock: fakeClock{now: time.Now()}})
	_, ae := s.Reserve(context.Background(), "ins_1", 120)
	if ae != apierr.ErrQuotaExhausted {
		t.Fatalf("Reserve error = %v, want quota exhausted", ae)
	}
}

type fakeAuth struct {
	id     string
	status dominstall.Status
	found  bool
	err    error
}

func (f fakeAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return f.id, f.status, f.found, f.err
}

type allowAll struct{}

func (allowAll) Allow(string) bool { return true }

type fakeConfig struct{ cfg *config.Config }

func (f fakeConfig) Load() *config.Config { return f.cfg }

type fakeClock struct{ now time.Time }

func (f fakeClock) Now() time.Time { return f.now }

type fakeQuota struct {
	plan       billing.Plan
	period     domquota.Period
	reserveErr error
	settled    int64
	rolledBack bool
}

func (f *fakeQuota) SnapshotPeriod(now time.Time) domquota.Period {
	return domquota.SnapshotPeriod(now, time.UTC)
}
func (f *fakeQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	f.plan, f.period = plan, p
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return &domquota.Reservation{RequestID: "req_speech", InstallID: installID, Period: p, Plan: plan, ReservedPUSD: plan.ReservedPUSD}, nil
}
func (f *fakeQuota) Settle(_ context.Context, _ *domquota.Reservation, actualPUSD int64) error {
	f.settled = actualPUSD
	return nil
}
func (f *fakeQuota) Rollback(context.Context, *domquota.Reservation) error {
	f.rolledBack = true
	return nil
}
