package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/app/health"
)

// stubChecker returns a fixed Readiness so tests pin the wire shape + status
// mapping without real DB/upstream/disk probes.
type stubChecker struct{ rd health.Readiness }

func (s stubChecker) Ready(context.Context) health.Readiness { return s.rd }

func TestReady_OK200(t *testing.T) {
	h := Ready(stubChecker{rd: health.Readiness{
		DB: health.StateOK, Upstream: health.StateOK, Disk: health.StateOK, OK: true,
	}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.DB != "ok" || body.Upstream != "ok" || body.Disk != "ok" {
		t.Fatalf("body = %+v, want all ok", body)
	}
}

func TestReady_NotReady503(t *testing.T) {
	h := Ready(stubChecker{rd: health.Readiness{
		DB: health.StateDown, Upstream: health.StateDegraded, Disk: health.StateOK, OK: false,
	}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.DB != "down" || body.Upstream != "degraded" {
		t.Fatalf("body = %+v, want db=down upstream=degraded", body)
	}
}

// disk-low surfaces as db:degraded + disk:degraded (the app policy) and is still
// !OK ⇒ 503; the disk field is carried verbatim in the body.
func TestReady_DiskDegraded503(t *testing.T) {
	h := Ready(stubChecker{rd: health.Readiness{
		DB: health.StateDegraded, Upstream: health.StateOK, Disk: health.StateDegraded, OK: false,
	}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body readyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Disk != "degraded" || body.DB != "degraded" {
		t.Fatalf("body = %+v, want disk=degraded db=degraded", body)
	}
}

func TestReady_RejectsNonGET(t *testing.T) {
	h := Ready(stubChecker{rd: health.Readiness{OK: true}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/readyz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
