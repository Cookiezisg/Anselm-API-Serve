package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
)

// --- fakes for the app/dashboard ports ---

type fakeConfig struct{}

func (fakeConfig) Dump() []appdash.DumpItem {
	return []appdash.DumpItem{{Key: "RATE_PER_MIN", Value: "60"}}
}
func (fakeConfig) InstallGlobalCap() int64 { return 0 }
func (fakeConfig) ApplyOverrides(context.Context, map[string]string) (string, bool) {
	return "", true
}

type fakeBudget struct{}

func (fakeBudget) GlobalBudget(context.Context) (string, int64, int64, error) {
	return "2026-06-20", 1000, 100, nil
}

type fakeProviders struct{}

func (fakeProviders) Available(provider billing.Provider) bool {
	return provider == billing.ProviderDeepSeek
}

func (fakeProviders) BreakerOpen(provider billing.Provider) bool {
	return provider == billing.ProviderDeepSeek
}

// countingRate proves B16: each /api/overview poll calls Snapshot independently;
// concurrent pollers do not corrupt the shared sampler (pkg/ratesample is the real
// implementation; we wire it directly and Observe to feed a window).
func newDashHandler(t *testing.T, sampler *ratesample.Sampler) (http.Handler, *mwdash.Gate) {
	t.Helper()
	svc := appdash.New(appdash.Deps{
		Budget:    fakeBudget{},
		Providers: fakeProviders{},
		Rate:      sampler,
		Config:    fakeConfig{},
	})
	gate := &mwdash.Gate{Sessions: mwdash.NewSessionStore(0, nil), SecureCookie: false}
	h, err := dashhandler.New(dashhandler.Config{
		Service:  svc,
		Gate:     gate,
		Logins:   mwdash.NewLoginLimiter(nil),
		User:     "admin",
		Password: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return BuildDashboardHandler(DashboardDeps{Handler: h, Gate: gate}), gate
}

func TestOverviewExposesClosedProviderStatusShape(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	cookie, _ := login(t, srv, "admin", "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var wire struct {
		Providers map[string]struct {
			Configured  bool `json:"configured"`
			BreakerOpen bool `json:"breakerOpen"`
		} `json:"providers"`
		Aggregate bool `json:"upstreamBreakerOpen"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(wire.Providers) != 2 {
		t.Fatalf("providers = %v, want exactly deepseek/kimi", wire.Providers)
	}
	if got := wire.Providers["deepseek"]; !got.Configured || !got.BreakerOpen {
		t.Fatalf("deepseek = %+v, want configured/open", got)
	}
	if got := wire.Providers["kimi"]; got.Configured || got.BreakerOpen {
		t.Fatalf("kimi = %+v, want unconfigured/closed", got)
	}
	if !wire.Aggregate {
		t.Fatal("compatibility aggregate must remain true when one provider breaker is open")
	}
}

// login performs POST /login and returns the session cookie + csrf token.
func login(t *testing.T, srv http.Handler, user, pass string) (*http.Cookie, string) {
	t.Helper()
	body := strings.NewReader(`{"user":"` + user + `","password":"` + pass + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var lr struct {
		CSRFToken string `json:"csrfToken"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &lr)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == mwdash.CookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("login did not set the session cookie")
	}
	return cookie, lr.CSRFToken
}

func TestLoginSetsSecureSessionCookie(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	cookie, csrf := login(t, srv, "admin", "s3cret")
	if csrf == "" {
		t.Fatal("login must return a CSRF token")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie must be HttpOnly+SameSite=Strict: %+v", cookie)
	}
}

func TestLoginWrongPasswordNoHint(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"user":"admin","password":"wrong"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "INVALID_CREDENTIALS" {
		t.Fatalf("want INVALID_CREDENTIALS (no factor hint), got %q", env.Error.Code)
	}
}

// TestAllAPIRequireSession: every /api/* route 401s without a session.
func TestAllAPIRequireSession(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	routes := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/session"},
		{http.MethodGet, "/api/overview"},
		{http.MethodGet, "/api/config"},
		{http.MethodPost, "/api/config"},
		{http.MethodGet, "/api/installs"},
		{http.MethodPost, "/api/installs/ban"},
		{http.MethodPost, "/api/installs/unban"},
		{http.MethodGet, "/api/audit"},
		{http.MethodGet, "/api/export"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, nil)
		req.RemoteAddr = "127.0.0.1:5000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401 without session, got %d", r.method, r.path, rec.Code)
		}
	}
}

// TestSlidingCookieReissue (B14): every requireSession pass re-issues Set-Cookie
// so the browser tracks the slide.
func TestSlidingCookieReissue(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	cookie, _ := login(t, srv, "admin", "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated overview: want 200, got %d", rec.Code)
	}
	var reissued bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == mwdash.CookieName && c.Value == cookie.Value {
			reissued = true
		}
	}
	if !reissued {
		t.Fatal("B14: requireSession must re-issue the session cookie on every pass")
	}
}

// TestCSRFReject: a state-changing POST without the matching X-CSRF-Token is 403.
func TestCSRFReject(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	cookie, csrf := login(t, srv, "admin", "s3cret")

	// No CSRF header ⇒ 403.
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"RATE_PER_MIN":"60"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: want 403, got %d", rec.Code)
	}

	// Matching CSRF ⇒ 200.
	req = httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"RATE_PER_MIN":"60"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	req.Header.Set(mwdash.CSRFHeader, csrf)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid CSRF: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestLoginLockoutMalformedBodyKeepsSlot (B15): a malformed body is a 400 that
// does NOT consume a failure slot — so after many bad bodies a real credential is
// still accepted (the lockout never armed).
func TestLoginLockoutMalformedBodyKeepsSlot(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("{not json"))
		req.RemoteAddr = "127.0.0.1:5000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed body: want 400, got %d", rec.Code)
		}
	}
	// Despite 20 malformed bodies, a valid login still succeeds (no slot consumed).
	login(t, srv, "admin", "s3cret")
}

// TestLoginLockoutArmsAndReturns429 (B15): real failures arm the lockout, which
// returns 429 LOGIN_LOCKED with a Retry-After header + details.retryAfterSec.
func TestLoginLockoutArmsAndReturns429(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	var last *httptest.ResponseRecorder
	for i := 0; i < mwdash.LoginMaxFailures+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"user":"admin","password":"wrong"}`))
		req.RemoteAddr = "127.0.0.1:5000"
		last = httptest.NewRecorder()
		srv.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after lockout, got %d", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("429 lockout must carry a Retry-After header")
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(last.Body.Bytes(), &env)
	if env.Error.Code != "LOGIN_LOCKED" {
		t.Fatalf("want LOGIN_LOCKED code, got %q", env.Error.Code)
	}
	if _, ok := env.Error.Details["retryAfterSec"]; !ok {
		t.Fatal("lockout body must carry details.retryAfterSec")
	}
}

// TestLoginLockoutIgnoresForwardedFor pins the dashboard's direct-peer trust
// boundary. The loopback dashboard is reached directly (or through an SSH
// tunnel), so XFF is client-controlled and rotating it must not mint fresh login
// buckets. Ephemeral source-port changes from the same peer must not do so either.
func TestLoginLockoutIgnoresForwardedFor(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	for i := 0; i < mwdash.LoginMaxFailures; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"user":"admin","password":"wrong"}`))
		req.RemoteAddr = fmt.Sprintf("127.0.0.1:%d", 5000+i)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: want 401 before lockout, got %d", i+1, rec.Code)
		}
	}

	final := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"user":"admin","password":"wrong"}`))
	final.RemoteAddr = "127.0.0.1:65000"
	final.Header.Set("X-Forwarded-For", "203.0.113.250")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, final)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rotating XFF/source port bypassed direct-peer lockout: want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestOverviewQPSConcurrentPolls (B16): two concurrent /api/overview pollers each
// get a correct, independent recent-window snapshot (no shared last-point to steal).
func TestOverviewQPSConcurrentPolls(t *testing.T) {
	sampler := ratesample.New(60)
	for i := 0; i < 120; i++ {
		sampler.Observe(200) // feed the window so QPS > 0
	}
	srv, _ := newDashHandler(t, sampler)
	cookie, _ := login(t, srv, "admin", "s3cret")

	var wg sync.WaitGroup
	results := make([]float64, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
			req.RemoteAddr = "127.0.0.1:5000"
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			var o struct {
				Recent ratesample.Snapshot `json:"recent"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &o)
			results[idx] = o.Recent.QPS
		}(i)
	}
	wg.Wait()
	// Both pollers see the SAME correct window QPS (120 reqs / 60s window = 2.0),
	// not a halved/spiked value from stealing each other's last point.
	for i, q := range results {
		if q != 2.0 {
			t.Fatalf("poller %d: want QPS 2.0 (independent snapshot), got %v", i, q)
		}
	}
}

func TestSecurityHeadersOnAllRoutes(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
		h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("security headers missing: %v", h)
	}
}

func TestLogoutClearsCookieIdempotent(t *testing.T) {
	srv, _ := newDashHandler(t, ratesample.New(60))
	cookie, _ := login(t, srv, "admin", "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", rec.Code)
	}
	// The session is now invalid → /api/overview 401s.
	req = httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout overview: want 401, got %d", rec.Code)
	}
}
