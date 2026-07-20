package chat

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// This is an integration test: a real *appchat.Service over stub ports, driven
// through the handler's httptest round-trip, so the Sink adapter (header/status/
// body relay) is exercised end-to-end.

func TestHandler_NonStreamRoundTrip(t *testing.T) {
	svc := newService(t, stubUpstream{body: `{"usage":{"total_tokens":9}}`})
	h := New(svc, 0)

	body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "usage") {
		t.Fatalf("body not relayed: %s", rec.Body.String())
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := New(newService(t, stubUpstream{}), 0)
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
}

func TestHandler_NoBearer401(t *testing.T) {
	h := New(newService(t, stubUpstream{}), 0)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"u","content":"x"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrInvalidToken.Status {
		t.Fatalf("missing bearer status: %d", rec.Code)
	}
}

func TestHandler_InjectedBodyLimitEnforced(t *testing.T) {
	// The handler caps the body at the INJECTED bodyLimit (the router passes
	// config MAX_BODY_BYTES): a body over the limit is a BodyError → 400
	// BAD_REQUEST at the use case's body gate, while the SAME body under an
	// exactly-fitting limit round-trips 200 — the injected bound is what governs.
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`

	over := New(newService(t, stubUpstream{body: `{"usage":{"total_tokens":1}}`}), 16)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	over.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("body over injected limit want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Fatalf("over-cap envelope want BAD_REQUEST, got %s", rec.Body.String())
	}

	fits := New(newService(t, stubUpstream{body: `{"usage":{"total_tokens":1}}`}), int64(len(body)))
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r2.Header.Set("Authorization", "Bearer tok")
	rec2 := httptest.NewRecorder()
	fits.ServeHTTP(rec2, r2)
	if rec2.Code != 200 {
		t.Fatalf("body at exactly the injected limit want 200, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestHandler_NonPositiveBodyLimitFallsBackToDefault(t *testing.T) {
	// bodyLimit <= 0 falls back to the domain contract default (256KiB, §5.3) so
	// a zero-value wiring can never mean "unbounded".
	for _, lim := range []int64{0, -1} {
		h := New(newService(t, stubUpstream{}), lim)
		if h.bodyLimit != domchat.BodyDecodeLimit {
			t.Fatalf("New(svc, %d) bodyLimit = %d, want default %d", lim, h.bodyLimit, domchat.BodyDecodeLimit)
		}
	}
}

// --- stub ports for the integration service ---

func newService(t *testing.T, up appchat.Upstream) *appchat.Service {
	t.Helper()
	cfg := &config.Config{
		PublicModelID:           "anselm-auto",
		TextUpstreamModel:       billing.DeepSeekV4Flash,
		MultimodalUpstreamModel: billing.KimiK26,
		MonthlyQuota:            100,
		MaxTokensCap:            100,
		InputTokenCap:           1_000_000,
		MaxMessages:             100,
		MaxMessageChars:         100000,
		MaxMediaParts:           8,
		MaxMediaDecodedBytes:    3 * 1024 * 1024,
		NGlobalConcurrency:      4,
		Location:                time.UTC,
	}
	return appchat.New(appchat.Deps{
		Auth:     stubAuth{},
		Quota:    &stubQuota{},
		Upstream: up,
		RL:       stubRL{},
		Config:   stubConfig{cfg},
		Clock:    stubClock{},
		BgWG:     &sync.WaitGroup{},
	})
}

type stubAuth struct{}

func (stubAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return "inst", dominstall.StatusActive, true, nil
}

type stubQuota struct{}

func (stubQuota) SnapshotPeriod(time.Time) domquota.Period { return domquota.Period{Day: "2026-06-20"} }
func (stubQuota) Reserve(_ context.Context, id string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	return &domquota.Reservation{
		RequestID: "r", InstallID: id, Period: p,
		Plan: plan, ReservedPUSD: plan.ReservedPUSD,
	}, nil
}
func (stubQuota) Settle(context.Context, *domquota.Reservation, int64) error { return nil }
func (stubQuota) Rollback(context.Context, *domquota.Reservation) error      { return nil }

type stubUpstream struct {
	body string
	aerr *apierr.APIError
}

func (u stubUpstream) Open(context.Context, billing.Provider, string, domchat.CompletionRequest, time.Duration) (appchat.UpstreamStream, *appchat.UpstreamFailure) {
	if u.aerr != nil {
		return nil, &appchat.UpstreamFailure{APIError: u.aerr}
	}
	return io.NopCloser(strings.NewReader(u.body)), nil
}
func (stubUpstream) Available(billing.Provider) bool   { return true }
func (stubUpstream) BreakerOpen(billing.Provider) bool { return false }

type stubRL struct{}

func (stubRL) Allow(string) bool                  { return true }
func (stubRL) SetKeyLimit(string, int, time.Time) {}

type stubConfig struct{ c *config.Config }

func (s stubConfig) Load() *config.Config { return s.c }

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Unix(1_700_000_000, 0) }
