package install

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
)

// stub implements the Service port: it records inputs and returns scripted
// outcomes so the handler's wiring (PoW-first order, IP-key derivation, body
// decode, reject mapping) is exercised without a real DB.
type stub struct {
	powAE   *apierr.APIError
	issueAE *apierr.APIError
	gate    dominstall.Gate
	view    appinstall.IssueResultView

	gotPoWHeader string
	gotIPKey     string
	gotReq       dominstall.Request
	gotUnmetered bool

	unmetered bool // IsUnmetered result (operator whitelist god-mode re-mint).
	powCalls  int
}

func (s *stub) PoWGate(_ context.Context, powHeader, ipKey string) *apierr.APIError {
	s.powCalls++
	s.gotPoWHeader = powHeader
	s.gotIPKey = ipKey
	return s.powAE
}

func (s *stub) IsUnmetered(string) bool { return s.unmetered }

func (s *stub) Issue(_ context.Context, req dominstall.Request, ipKey string, unmetered bool) (appinstall.IssueResultView, dominstall.Gate, *apierr.APIError) {
	s.gotReq = req
	s.gotIPKey = ipKey
	s.gotUnmetered = unmetered
	if s.issueAE != nil {
		return appinstall.IssueResultView{}, s.gate, s.issueAE
	}
	return s.view, dominstall.GateNone, nil
}

func TestInstall_Success(t *testing.T) {
	reset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	s := &stub{view: appinstall.IssueResultView{Token: "gwk_abc", MonthlyQuota: 1234, ResetAt: reset}}
	h := New(s)

	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{"fingerprint":"  fp  ","client":"cli"}`))
	r.Header.Set("X-PoW", "ch.nonce")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var got body
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token != "gwk_abc" || got.MonthlyQuota != 1234 {
		t.Fatalf("body: %+v", got)
	}
	if got.ResetAt != reset.Format(time.RFC3339) {
		t.Fatalf("resetAt: %q", got.ResetAt)
	}
	// The handler must hand the PoW header through to the gate.
	if s.gotPoWHeader != "ch.nonce" {
		t.Fatalf("X-PoW not forwarded: %q", s.gotPoWHeader)
	}
	// NewRequest normalization (trim) must happen before Issue sees the fingerprint.
	if s.gotReq.Fingerprint != "fp" || s.gotReq.Client != "cli" {
		t.Fatalf("req not normalized: %+v", s.gotReq)
	}
}

func TestInstall_PoWReject(t *testing.T) {
	s := &stub{powAE: apierr.ErrInstallPoWRequired}
	h := New(s)
	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != apierr.ErrInstallPoWRequired.Status {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INSTALL_POW_REQUIRED") {
		t.Fatalf("code not rendered: %s", rec.Body.String())
	}
}

func TestInstall_GateReject(t *testing.T) {
	s := &stub{issueAE: apierr.ErrInstallRateLimited, gate: dominstall.GateIP}
	h := New(s)
	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != apierr.ErrInstallRateLimited.Status {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INSTALL_RATE_LIMITED") {
		t.Fatalf("code: %s", rec.Body.String())
	}
}

// Operator god-mode re-mint: a whitelisted bearer (IsUnmetered → true) skips the
// PoW gate entirely AND threads unmetered=true into Issue, even when PoW would
// otherwise reject. The brand-new token still comes back.
func TestInstall_UnmeteredSkipsPoWAndGates(t *testing.T) {
	reset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	s := &stub{
		unmetered: true,
		powAE:     apierr.ErrInstallPoWRequired, // would reject if PoW ran
		view:      appinstall.IssueResultView{Token: "gwk_new", MonthlyQuota: 1234, ResetAt: reset},
	}
	h := New(s)
	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer gwk_whitelisted")
	r.Header.Set("X-PoW", "ch.nonce")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("unmetered re-mint must succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	if s.powCalls != 0 {
		t.Fatalf("PoW gate must be skipped on the unmetered path, got %d calls", s.powCalls)
	}
	if !s.gotUnmetered {
		t.Fatal("unmetered flag must be threaded into Issue")
	}
}

func TestInstall_BadBody(t *testing.T) {
	s := &stub{}
	h := New(s)
	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrBadRequest.Status {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestInstall_MethodNotAllowed(t *testing.T) {
	h := New(&stub{})
	r := httptest.NewRequest("GET", "/v1/install", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
}
