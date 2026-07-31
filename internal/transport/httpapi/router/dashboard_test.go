package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
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
	return provider == billing.ProviderQwen
}

func (fakeProviders) BreakerOpen(provider billing.Provider) bool {
	return provider == billing.ProviderQwen
}

type fakeQuotaResetter struct {
	result appdash.QuotaResetResult
	err    error
	calls  int
}

func (f *fakeQuotaResetter) ResetAllMonthlyQuota(context.Context) (appdash.QuotaResetResult, error) {
	f.calls++
	return f.result, f.err
}

// newDashHandler builds the dashboard exactly as bootstrap does: no gate, no
// session, no CSRF. The listener is loopback-only and the login wall lives in the
// IAP in front of it, so a test that logged in first would be exercising a wall
// this process no longer owns.
//
// newDashHandler 按 bootstrap 的方式构造后台:无 gate、无 session、无 CSRF。监听器恒 loopback,
// 登录墙住在它前面的 IAP 里,故一个「先登录」的测试,考的是本进程已经不再拥有的那道墙。
func newDashHandler(t *testing.T, sampler *ratesample.Sampler) http.Handler {
	t.Helper()
	svc := appdash.New(appdash.Deps{
		Budget:    fakeBudget{},
		Providers: fakeProviders{},
		Rate:      sampler,
		Config:    fakeConfig{},
		QuotaReset: &fakeQuotaResetter{result: appdash.QuotaResetResult{
			Period: "2026-07", ResetInstalls: 2,
		}},
	})
	h, err := dashhandler.New(dashhandler.Config{Service: svc})
	if err != nil {
		t.Fatal(err)
	}
	return BuildDashboardHandler(DashboardDeps{Handler: h})
}

func TestOverviewExposesClosedProviderStatusShape(t *testing.T) {
	srv := newDashHandler(t, ratesample.New(60))

	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:5000"
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
	// The key is ALWAYS serialized, so an operator (and the SPA) never has to infer
	// capability from a field being absent.
	// 这个键**恒被序列化**,故运营者(以及 SPA)永远不必从「字段不在」去推断能力。
	if len(wire.Providers) != 1 {
		t.Fatalf("providers = %v, want exactly qwen", wire.Providers)
	}
	if got := wire.Providers["qwen"]; !got.Configured || !got.BreakerOpen {
		t.Fatalf("qwen = %+v, want configured/open", got)
	}
	if !wire.Aggregate {
		t.Fatal("the aggregate must be true while a configured provider's breaker is open")
	}
}

// The API answers directly, and the session endpoint does not exist at all — its
// absence is the assertion: a 404 proves no in-process login surface survived to
// drift out of sync with the real one.
//
// API **直接**作答,而 session 端点**根本不存在**——它的缺席就是断言:404 证明没有任何进程内登录
// 面残留下来、日后与真正那道墙各走各的。
func TestDashboardAPIIsDirectAndHasNoSessionEndpoint(t *testing.T) {
	srv := newDashHandler(t, ratesample.New(60))

	for _, path := range []string{"/api/overview", "/api/config"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"RATE_PER_MIN":"60"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST must not require an in-process CSRF token: got %d (%s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/session", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("session endpoint must not exist: want 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/quota/reset", strings.NewReader(`{"reason":"new cycle"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("quota reset must not require an in-process CSRF token: got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A reset still needs an auditable reason. That gate has nothing to do with
// authentication: it exists so the audit ring records WHY a month's entitlement
// was handed back, and losing the login wall must not quietly lose it too.
//
// 重置**仍然**需要一个可审计的理由。这道闸与鉴权无关:它的存在是为了让审计环记下「为什么」把
// 一个月的额度还了回去——撤掉登录墙不该把它也顺手带走。
func TestQuotaResetRequiresAnAuditableReason(t *testing.T) {
	srv := newDashHandler(t, ratesample.New(60))

	req := httptest.NewRequest(http.MethodPost, "/api/quota/reset", strings.NewReader(`{"reason":" "}`))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/quota/reset", strings.NewReader(`{"reason":"new cycle"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("quota reset: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var result appdash.QuotaResetResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Period != "2026-07" || result.ResetInstalls != 2 {
		t.Fatalf("quota reset result = %+v", result)
	}
}

// TestOverviewQPSConcurrentPolls: two concurrent /api/overview pollers each
// get a correct, independent recent-window snapshot (no shared last-point to steal).
func TestOverviewQPSConcurrentPolls(t *testing.T) {
	sampler := ratesample.New(60)
	for i := 0; i < 120; i++ {
		sampler.Observe(200) // feed the window so QPS > 0
	}
	srv := newDashHandler(t, sampler)

	var wg sync.WaitGroup
	results := make([]float64, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
			req.RemoteAddr = "127.0.0.1:5000"
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
	srv := newDashHandler(t, ratesample.New(60))
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
