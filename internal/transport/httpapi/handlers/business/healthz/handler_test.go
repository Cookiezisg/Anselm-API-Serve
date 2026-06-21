package healthz

import (
	"net/http/httptest"
	"testing"
)

func TestHealthz_Success(t *testing.T) {
	h := New()
	r := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: %q", ct)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Fatalf("body: %q", got)
	}
}

func TestHealthz_MethodNotAllowed(t *testing.T) {
	h := New()
	r := httptest.NewRequest("POST", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST must be 405, got %d", rec.Code)
	}
}
