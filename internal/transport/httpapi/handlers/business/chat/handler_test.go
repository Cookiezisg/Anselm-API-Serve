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
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// This is an integration test: a real *appchat.Service over stub ports, driven
// through the handler's httptest round-trip, so the Sink adapter (header/status/
// body relay) is exercised end-to-end.

func TestHandler_NonStreamRoundTrip(t *testing.T) {
	svc := newService(t, stubUpstream{body: `{"usage":{"total_tokens":9}}`})
	h := New(svc)

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
	h := New(newService(t, stubUpstream{}))
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
}

func TestHandler_NoBearer401(t *testing.T) {
	h := New(newService(t, stubUpstream{}))
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"u","content":"x"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrInvalidToken.Status {
		t.Fatalf("missing bearer status: %d", rec.Code)
	}
}

// --- stub ports for the integration service ---

func newService(t *testing.T, up appchat.Upstream) *appchat.Service {
	t.Helper()
	cfg := &config.Config{
		ModelAllowlist:     []string{"x"},
		DefaultModel:       "x",
		MonthlyQuota:       100,
		MaxTokensCap:       100,
		InputTokenCap:      1_000_000,
		MaxMessages:        100,
		MaxMessageChars:    100000,
		NGlobalConcurrency: 4,
		Location:           time.UTC,
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
func (stubQuota) Reserve(_ context.Context, id string, est int64, p domquota.Period) (*domquota.Reservation, error) {
	return &domquota.Reservation{RequestID: "r", InstallID: id, Period: p, Reserved: est}, nil
}
func (stubQuota) Settle(context.Context, *domquota.Reservation, int64) error { return nil }
func (stubQuota) Rollback(context.Context, *domquota.Reservation) error      { return nil }

type stubUpstream struct {
	body string
	aerr *apierr.APIError
}

func (u stubUpstream) Do(context.Context, []byte, bool, *config.Config) (appchat.UpstreamStream, *apierr.APIError) {
	if u.aerr != nil {
		return nil, u.aerr
	}
	return io.NopCloser(strings.NewReader(u.body)), nil
}
func (stubUpstream) BreakerOpen() bool { return false }

type stubRL struct{}

func (stubRL) Allow(string) bool                  { return true }
func (stubRL) SetKeyLimit(string, int, time.Time) {}

type stubConfig struct{ c *config.Config }

func (s stubConfig) Load() *config.Config { return s.c }

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Unix(1_700_000_000, 0) }
