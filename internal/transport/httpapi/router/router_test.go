package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoHandler writes the given label so a test can assert which route served the
// request (proving the method+path table dispatches correctly).
func echoHandler(label string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(label))
	})
}

// stubWrapper records the labels it wraps so the test proves each business route
// (and ONLY those) gets a RED label, and /healthz does not.
type stubWrapper struct{ labels []string }

func (s *stubWrapper) Wrap(label string, h http.Handler) http.Handler {
	s.labels = append(s.labels, label)
	return h
}

func testRoutes() routes {
	return routes{
		install:   echoHandler("install"),
		challenge: echoHandler("challenge"),
		chat:      echoHandler("chat"),
		quota:     echoHandler("quota"),
		models:    echoHandler("models"),
		healthz:   echoHandler("healthz"),
	}
}

func TestRouter_DispatchAndLabels(t *testing.T) {
	mx := &stubWrapper{}
	h := assemble(testRoutes(), mx, nil)

	cases := []struct {
		method, path, want string
	}{
		{"POST", "/v1/install", "install"},
		{"GET", "/v1/install/challenge", "challenge"},
		{"POST", "/v1/chat/completions", "chat"},
		{"GET", "/v1/quota", "quota"},
		{"GET", "/v1/models", "models"},
		{"GET", "/healthz", "healthz"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 200 || rec.Body.String() != c.want {
			t.Fatalf("%s %s → code=%d body=%q want %q", c.method, c.path, rec.Code, rec.Body.String(), c.want)
		}
	}

	// Exactly the five business routes are RED-labeled; /healthz is NOT.
	want := []string{"install", "install_challenge", "chat_completions", "quota", "models"}
	if strings.Join(mx.labels, ",") != strings.Join(want, ",") {
		t.Fatalf("RED labels = %v, want %v", mx.labels, want)
	}
}

func TestRouter_RequestIDEchoed(t *testing.T) {
	h := assemble(testRoutes(), nil, nil)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("X-Request-ID not echoed: %q", got)
	}
}

func TestRouter_CORSDenied(t *testing.T) {
	h := assemble(testRoutes(), nil, nil)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin must be 403, got %d", rec.Code)
	}
}

func TestRouter_NotFound(t *testing.T) {
	h := assemble(testRoutes(), nil, nil)
	r := httptest.NewRequest("GET", "/v1/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path must be 404, got %d", rec.Code)
	}
}

func TestRouter_MethodNotAllowed(t *testing.T) {
	// The Go 1.22 mux returns 405 for a known path with a wrong method.
	h := assemble(testRoutes(), nil, nil)
	r := httptest.NewRequest("GET", "/v1/install", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method on /v1/install must be 405, got %d", rec.Code)
	}
}

func TestRouter_PanicRecovered(t *testing.T) {
	rt := testRoutes()
	rt.quota = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	var panics int
	h := assemble(rt, nil, counterFunc(func() { panics++ }))

	r := httptest.NewRequest("GET", "/v1/quota", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic must render 500, got %d", rec.Code)
	}
	if panics != 1 {
		t.Fatalf("panic counter = %d, want 1", panics)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("must not leak panic value: %s", rec.Body.String())
	}
}

// counterFunc adapts a func into the PanicCounter port for the panic test.
type counterFunc func()

func (f counterFunc) Inc() { f() }
