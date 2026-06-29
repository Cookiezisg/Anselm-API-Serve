package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

type stubAuth struct {
	id        string
	status    dominstall.Status
	found     bool
	err       error
	unmetered bool
}

func (s stubAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return s.id, s.status, s.found, s.err
}
func (s stubAuth) IsUnmetered(string) bool { return s.unmetered }

type stubViewer struct {
	view         *appquota.View
	err          error
	gotUnmetered bool
}

func (stubViewer) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Day: "2026-06-20"}
}
func (s *stubViewer) View(_ context.Context, _ string, _ domquota.Period, unmetered bool) (*appquota.View, error) {
	s.gotUnmetered = unmetered
	return s.view, s.err
}

func authOK() stubAuth { return stubAuth{id: "ins_1", status: dominstall.StatusActive, found: true} }

func TestQuota_Success(t *testing.T) {
	reset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	v := &appquota.View{Limit: 1000, Used: 200, Remaining: 800, ResetAt: reset, Available: true}
	h := New(authOK(), &stubViewer{view: v})

	r := httptest.NewRequest("GET", "/v1/quota", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Quota-Limit"); got != "1000" {
		t.Fatalf("X-Quota-Limit: %q", got)
	}
	if got := rec.Header().Get("X-Quota-Reset"); got != reset.Format(time.RFC3339) {
		t.Fatalf("X-Quota-Reset: %q", got)
	}
	var got body
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Limit != 1000 || got.Used != 200 || got.Remaining != 800 || !got.Available {
		t.Fatalf("body: %+v", got)
	}
}

func TestQuota_NoBearer401(t *testing.T) {
	h := New(authOK(), &stubViewer{})
	r := httptest.NewRequest("GET", "/v1/quota", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrInvalidToken.Status {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestQuota_Banned403(t *testing.T) {
	h := New(stubAuth{id: "x", status: dominstall.StatusBanned, found: true}, &stubViewer{})
	r := httptest.NewRequest("GET", "/v1/quota", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrAccountBanned.Status {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ACCOUNT_BANNED") {
		t.Fatalf("code: %s", rec.Body.String())
	}
}

func TestQuota_NotFound401(t *testing.T) {
	h := New(stubAuth{found: false}, &stubViewer{})
	r := httptest.NewRequest("GET", "/v1/quota", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrInvalidToken.Status {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestQuota_AuthError500(t *testing.T) {
	h := New(stubAuth{err: errors.New("db")}, &stubViewer{})
	r := httptest.NewRequest("GET", "/v1/quota", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestQuota_MethodNotAllowed(t *testing.T) {
	h := New(authOK(), &stubViewer{})
	r := httptest.NewRequest("POST", "/v1/quota", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST must be 405, got %d", rec.Code)
	}
}
