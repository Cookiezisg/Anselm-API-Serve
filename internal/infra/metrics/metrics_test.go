package metrics

import (
	"expvar"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// scrape gathers the exposition text from a freshly built Metrics' Handler.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	ts := httptest.NewServer(m.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestRegistrationSmoke: New registers every collector without panicking and the
// promhttp handler serves the full exposition (registration smoke + present).
func TestRegistrationSmoke(t *testing.T) {
	m := New()
	// Drive one request through Wrap so the HTTP RED families gather.
	wrapped := m.Wrap("chat_completions", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	// Touch every collector so each appears in the exposition.
	m.UpstreamRequests.WithLabelValues("success").Inc()
	m.UpstreamLatency.Observe(0.5)
	m.InflightConc.Set(2)
	m.BudgetUsed.Set(123)
	m.BudgetLimit.Set(1000)
	m.ReservationsOpen.Set(1)
	m.BreakerState.Set(0)
	m.KeyCooldowns.Inc()
	m.RateLimiterEvictions.Inc()
	m.Panics.Inc()
	m.InstallsCreated.Inc()
	m.TokenThrottled.Inc()
	m.TokensThrottledNow.Set(3)
	m.SettleFailures.Inc()
	m.RollbackFailures.Inc()
	m.InstallPoW.WithLabelValues("verified").Inc()

	out := scrape(t, m)
	for _, want := range []string{
		"gateway_http_requests_total",
		"gateway_http_request_duration_seconds",
		"gateway_http_in_flight_requests",
		"gateway_upstream_requests_total",
		"gateway_upstream_latency_seconds",
		"gateway_inflight_concurrency",
		"gateway_budget_tokens_used",
		"gateway_budget_tokens_limit",
		"gateway_quota_reservations_open",
		"gateway_breaker_state",
		"gateway_key_cooldowns_total",
		"gateway_ratelimiter_evictions_total",
		"gateway_panics_total",
		"gateway_installs_created_total",
		"gateway_token_throttled_total",
		"gateway_tokens_throttled",
		"gateway_settle_failures_total",
		"gateway_rollback_failures_total",
		"gateway_install_pow_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// TestNoHighCardinalityLabels: GW-INV-15 铁律 — no metric may declare an
// install_id / token / ip / prompt label. We inspect the gathered metric
// families' label names directly (not just the text), so a leak is caught even
// if no series has been observed yet.
func TestNoHighCardinalityLabels(t *testing.T) {
	m := New()
	// Observe at least one series on each labeled vec so the families gather.
	m.httpRequests.WithLabelValues("chat_completions", "POST", "200").Inc()
	m.UpstreamRequests.WithLabelValues("success").Inc()
	m.InstallPoW.WithLabelValues("verified").Inc()

	families, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"install_id": true, "token": true, "ip": true, "ip_key": true, "prompt": true}
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if banned[lp.GetName()] {
					t.Errorf("high-cardinality label %q on metric %q", lp.GetName(), mf.GetName())
				}
			}
		}
	}

	// Belt-and-suspenders: the rendered text must not contain a banned label key.
	out := scrape(t, m)
	for _, b := range []string{"install_id=", "token=", "ip_key=", "ip=", "prompt="} {
		if strings.Contains(out, b) {
			t.Errorf("high-cardinality label leaked in exposition: %q", b)
		}
	}
}

// TestWrapRecordsStatusAndLatency: Wrap instruments a handler so a request flows
// through with the handler/method/code triple recorded on both the duration
// histogram and the request counter.
func TestWrapRecordsStatusAndLatency(t *testing.T) {
	m := New()
	h := m.Wrap("chat_completions", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418 — a distinctive code to assert on.
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("wrapped handler code=%d", rec.Code)
	}

	// The request counter must hold exactly one observation on the curried triple.
	// promhttp lower-cases the method label, so the value is "post".
	var dm dto.Metric
	c, err := m.httpRequests.GetMetricWithLabelValues("chat_completions", "post", "418")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 1 {
		t.Fatalf("request counter = %v, want 1", got)
	}

	// The duration histogram must have recorded one sample on the same triple.
	o, err := m.httpDuration.GetMetricWithLabelValues("chat_completions", "post", "418")
	if err != nil {
		t.Fatal(err)
	}
	var dh dto.Metric
	if err := o.(interface{ Write(*dto.Metric) error }).Write(&dh); err != nil {
		t.Fatal(err)
	}
	if got := dh.GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("duration histogram sample count = %d, want 1", got)
	}
}

// TestExpvarRuntimeGaugesPublished: New publishes the two runtime expvar gauges
// (gateway_goroutines / gateway_heap_alloc_bytes) and a second New does not panic
// on the duplicate-key Publish.
func TestExpvarRuntimeGaugesPublished(t *testing.T) {
	_ = New()
	_ = New() // second build must not panic on the global expvar map.

	for _, name := range []string{"gateway_goroutines", "gateway_heap_alloc_bytes"} {
		if expvar.Get(name) == nil {
			t.Errorf("expvar %q not published", name)
		}
	}
}
