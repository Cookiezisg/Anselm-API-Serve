package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appdeviceproof "github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
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
}

func (s *stub) PoWGate(_ context.Context, powHeader, ipKey string) *apierr.APIError {
	s.gotPoWHeader = powHeader
	s.gotIPKey = ipKey
	return s.powAE
}

func (s *stub) Issue(_ context.Context, req dominstall.Request, _ []byte, _, ipKey string) (appinstall.IssueResultView, dominstall.Gate, *apierr.APIError) {
	s.gotReq = req
	s.gotIPKey = ipKey
	if s.issueAE != nil {
		return appinstall.IssueResultView{}, s.gate, s.issueAE
	}
	return s.view, dominstall.GateNone, nil
}

func TestInstall_Success(t *testing.T) {
	reset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	s := &stub{view: appinstall.IssueResultView{InstallID: "ins_abc", MonthlyQuota: 1234, ResetAt: reset}}
	bodyRaw := `{"fingerprint":"  fp  ","client":"cli"}`
	r, proofSvc := signedInstallRequest(t, bodyRaw)
	h := New(s, proofSvc)
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
	if got.InstallID != "ins_abc" || got.MonthlyQuota != 1234 {
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
	h := New(s, appdeviceproof.New(nil, noncecache.New(time.Minute)))
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
	r, proofSvc := signedInstallRequest(t, `{}`)
	h := New(s, proofSvc)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != apierr.ErrInstallRateLimited.Status {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INSTALL_RATE_LIMITED") {
		t.Fatalf("code: %s", rec.Body.String())
	}
}

func TestInstall_BadBody(t *testing.T) {
	s := &stub{}
	h := New(s, appdeviceproof.New(nil, noncecache.New(time.Minute)))
	r := httptest.NewRequest("POST", "/v1/install", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrBadRequest.Status {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestInstall_MethodNotAllowed(t *testing.T) {
	h := New(&stub{}, appdeviceproof.New(nil, noncecache.New(time.Minute)))
	r := httptest.NewRequest("GET", "/v1/install", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("GET must be 405, got %d", rec.Code)
	}
}

func signedInstallRequest(t *testing.T, body string) (*http.Request, *appdeviceproof.Service) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	svc := appdeviceproof.New(nil, noncecache.New(time.Minute))
	challenge := svc.IssueChallenge()
	bh := sha256.Sum256([]byte(body))
	thumb := sha256.Sum256(public)
	b64 := base64.RawURLEncoding
	payload, err := json.Marshal(struct {
		Version int    `json:"v"`
		KeyID   string `json:"kid"`
		Issued  int64  `json:"iat"`
		ID      string `json:"jti"`
		Nonce   string `json:"nonce"`
		Method  string `json:"htm"`
		Target  string `json:"htu"`
		Body    string `json:"bh"`
	}{1, b64.EncodeToString(thumb[:]), time.Now().Unix(), "test-jti", challenge.Nonce,
		http.MethodPost, "example.com/v1/install", b64.EncodeToString(bh[:])})
	if err != nil {
		t.Fatal(err)
	}
	encoded := b64.EncodeToString(payload)
	r := httptest.NewRequest(http.MethodPost, "/v1/install", strings.NewReader(body))
	r.Header.Set(proofhttp.HeaderPublicKey, b64.EncodeToString(public))
	r.Header.Set(proofhttp.HeaderProof, encoded+"."+b64.EncodeToString(ed25519.Sign(private, []byte(encoded))))
	return r, svc
}
