package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/app/health"
)

type stubChecker struct{ rd health.Readiness }

func (s stubChecker) Ready(context.Context) health.Readiness { return s.rd }

func buildHandler(rd health.Readiness) http.Handler {
	return BuildAdminHandler(AdminDeps{
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("# HELP gateway_up\ngateway_up 1\n"))
		}),
		Ready: stubChecker{rd: rd},
	})
}

func TestAdminMux_Readyz200(t *testing.T) {
	h := buildHandler(health.Readiness{DB: "ok", Upstream: "ok", Disk: "ok", OK: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAdminMux_Readyz503(t *testing.T) {
	h := buildHandler(health.Readiness{DB: "down", Upstream: "ok", Disk: "ok", OK: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAdminMux_MetricsServes(t *testing.T) {
	h := buildHandler(health.Readiness{OK: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gateway_up") {
		t.Fatalf("metrics body missing exposition: %q", rec.Body.String())
	}
}

func TestAdminMux_ExpvarServes(t *testing.T) {
	h := buildHandler(health.Readiness{OK: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/vars", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAdminMux_PprofMounted(t *testing.T) {
	h := buildHandler(health.Readiness{OK: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// /healthz (liveness) is the BUSINESS mux's job, never the admin mux. Assert it
// is not mounted here so the loopback admin surface never carries the public
// liveness probe.
func TestAdminMux_NoHealthz(t *testing.T) {
	h := buildHandler(health.Readiness{OK: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (healthz is business mux only)", rec.Code)
	}
}
