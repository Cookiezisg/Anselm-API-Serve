package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecover_RequestIDEchoed(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}), nil)

	// Client-supplied safe id is reused.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("safe id not reused: %q", got)
	}

	// Absent id is minted (non-empty).
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/", nil))
	if rec2.Header().Get("X-Request-ID") == "" {
		t.Fatalf("missing minted id")
	}
}

func TestRecover_PanicTo500NoLeak(t *testing.T) {
	panicked := 0
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom secret=topsecret")
	}), func() { panicked++ })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != 500 {
		t.Fatalf("status: %d", rec.Code)
	}
	if panicked != 1 {
		t.Fatalf("onPanic hook should fire once: %d", panicked)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "INTERNAL") {
		t.Fatalf("expected INTERNAL: %s", body)
	}
	if strings.Contains(body, "topsecret") {
		t.Fatalf("panic value leaked into response: %s", body)
	}
}

func TestDenyCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := DenyCORS(next)

	// Cross-origin (Origin present) → 403.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("cross-origin should be 403: %d", rec.Code)
	}

	// No Origin (sidecar path) → passes.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/", nil))
	if rec2.Code != 204 {
		t.Fatalf("same-origin should pass: %d", rec2.Code)
	}
}

func TestMaxBody_Capped(t *testing.T) {
	var readErr error
	h := MaxBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}), 8)

	big := strings.NewReader(strings.Repeat("x", 100))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", big))
	if readErr == nil {
		t.Fatalf("expected read error from over-cap body")
	}
}
