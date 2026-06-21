// Package metrics is the Prometheus collector bundle (RED + the four golden
// signals + domain health) plus the runtime expvar gauges. It owns ONLY the
// registry and the metric objects; binding /metrics onto a loopback admin server
// (requireLoopback, GW-INV-13) is bootstrap's job, not this layer's.
//
// GW-INV-15 红线:label 一律低基数 —— 绝不把 install_id / token / prompt / ip 当
// label(会令时间序列爆炸 + 泄漏 PII)。本包构造时就只允许 outcome / handler /
// method / code / result 这几条固定低基数轴(code 即 HTTP 状态码,低基数)。
package metrics

import (
	"expvar"
	"net/http"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets cover the LLM long tail: a single chat completion can stream for
// tens of seconds, so buckets run to 60s (a default web bucket set buries the
// whole tail in +Inf).
var latencyBuckets = []float64{.1, .5, 1, 2, 5, 10, 30, 60}

// Metrics bundles every gateway collector behind one fresh registry (so each
// construction — incl. tests — is isolated). Built once at boot via New.
type Metrics struct {
	reg *prometheus.Registry

	// HTTP RED (business mux), wired via promhttp.InstrumentHandler* in Wrap.
	// Labels are the fixed low-cardinality triple handler/method/code — never
	// the request path (which carries install_id/token segments, PII red line).
	httpDuration *prometheus.HistogramVec
	httpRequests *prometheus.CounterVec
	httpInFlight prometheus.Gauge

	// Four golden signals + domain health.
	UpstreamRequests *prometheus.CounterVec // gateway_upstream_requests_total{outcome}
	UpstreamLatency  prometheus.Histogram   // gateway_upstream_latency_seconds
	InflightConc     prometheus.Gauge       // gateway_inflight_concurrency (= N_global occupancy)
	BudgetUsed       prometheus.Gauge       // gateway_budget_tokens_used
	BudgetLimit      prometheus.Gauge       // gateway_budget_tokens_limit
	ReservationsOpen prometheus.Gauge       // gateway_quota_reservations_open (settled IS NULL count)
	BreakerState     prometheus.Gauge       // gateway_breaker_state: 0=closed 1=half-open 2=open

	// Domain-health counters. None carry a label: the only safe dimension at each
	// emit site is high-cardinality/PII (install_id/ip/path), so these stay
	// aggregate "how often" signals.
	KeyCooldowns         prometheus.Counter // gateway_key_cooldowns_total
	RateLimiterEvictions prometheus.Counter // gateway_ratelimiter_evictions_total
	Panics               prometheus.Counter // gateway_panics_total
	InstallsCreated      prometheus.Counter // gateway_installs_created_total (Sybil-burst source)
	TokenThrottled       prometheus.Counter // gateway_token_throttled_total (M2 auto-throttle activations)
	TokensThrottledNow   prometheus.Gauge   // gateway_tokens_throttled (installs CURRENTLY throttled)

	// SettleFailures / RollbackFailures (B2 NEW): a failed Settle/Rollback used to
	// be swallowed (`_ = q.Settle(...)`), so the orphan scanner later refunded the
	// full reserved and under-billed. These counters make the failure observable —
	// the alert + dashboard see it instead of it vanishing into a silent reconcile.
	SettleFailures   prometheus.Counter // gateway_settle_failures_total
	RollbackFailures prometheus.Counter // gateway_rollback_failures_total

	// InstallPoW counts M2 领号 PoW outcomes by a LOW-CARDINALITY result label
	// (verified|failed|missing|shadow_pass): how much /install traffic already
	// carries a valid X-PoW, feeding the shadow→enforce rollout decision. No
	// install_id/ip dimension (PII red line); only the aggregate outcome bucket.
	InstallPoW *prometheus.CounterVec // gateway_install_pow_total{result}
}

// New registers every collector on a fresh registry and returns the bundle. The
// Go runtime + process collectors give CPU/mem/GC golden signals for free, and
// the runtime expvar gauges (gateway_goroutines / gateway_heap_alloc_bytes) are
// published here so /debug/vars carries them once the admin mux is wired.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto.With(reg)

	m := &Metrics{
		reg: reg,
		httpDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "HTTP request latency by handler/method/code.",
			Buckets: latencyBuckets,
		}, []string{"handler", "method", "code"}),
		httpRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_http_requests_total",
			Help: "Total HTTP requests by handler/method/code.",
		}, []string{"handler", "method", "code"}),
		httpInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_http_in_flight_requests",
			Help: "In-flight HTTP requests on the business mux.",
		}),
		UpstreamRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_requests_total",
			Help: "Upstream attempt outcomes (success|rollback|busy|timeout|error).",
		}, []string{"outcome"}),
		UpstreamLatency: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_upstream_latency_seconds",
			Help:    "Upstream connect→first-byte latency.",
			Buckets: latencyBuckets,
		}),
		InflightConc: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_inflight_concurrency",
			Help: "Current N_global semaphore occupancy.",
		}),
		BudgetUsed: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_budget_tokens_used",
			Help: "Global daily budget tokens used today.",
		}),
		BudgetLimit: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_budget_tokens_limit",
			Help: "Global daily budget token limit.",
		}),
		ReservationsOpen: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_quota_reservations_open",
			Help: "Ledger rows still unsettled (settled IS NULL) — missed-settle alarm source.",
		}),
		BreakerState: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_breaker_state",
			Help: "Process upstream breaker state: 0=closed 1=half-open 2=open.",
		}),
		KeyCooldowns: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_key_cooldowns_total",
			Help: "Upstream key cooldown activations (account-level key sheds).",
		}),
		RateLimiterEvictions: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_ratelimiter_evictions_total",
			Help: "Per-key minute-bucket LRU evictions (high rate ⇒ token-churn rate-limit-skirt signal; durable daily caps are the real guardrail).",
		}),
		Panics: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_panics_total",
			Help: "Handler panics caught by the recover middleware (panic-rate alarm source).",
		}),
		InstallsCreated: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_installs_created_total",
			Help: "Install tokens successfully issued (post-INSERT). A burst is the Sybil-registration signal feeding the install_burst alert.",
		}),
		TokenThrottled: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_token_throttled_total",
			Help: "M2 per-install token-anomaly auto-throttle activations (install crossed TOKEN_ANOMALY_RPM and was tightened on its minute bucket).",
		}),
		TokensThrottledNow: f.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_tokens_throttled",
			Help: "Installs currently under per-token anomaly auto-throttle (live count; reversible, auto-clears on cooldown).",
		}),
		SettleFailures: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_settle_failures_total",
			Help: "Failed ledger Settle calls (B2): a non-nil Settle error, formerly swallowed — makes under-bill-via-orphan-full-refund observable.",
		}),
		RollbackFailures: f.NewCounter(prometheus.CounterOpts{
			Name: "gateway_rollback_failures_total",
			Help: "Failed ledger Rollback calls (B2): a non-nil Rollback error, formerly swallowed — keeps a stuck reservation visible.",
		}),
		InstallPoW: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_install_pow_total",
			Help: "M2 install proof-of-work verification outcomes by result (verified|failed|missing|shadow_pass).",
		}, []string{"result"}),
	}

	// Runtime expvar gauges complement the Prometheus process collector with a
	// scrape-free loopback curl at /debug/vars. publishExpvar tolerates a
	// duplicate key so a second New (tests) cannot panic the global expvar map.
	publishExpvar("gateway_goroutines", func() any { return runtime.NumGoroutine() })
	publishExpvar("gateway_heap_alloc_bytes", func() any {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	})

	return m
}

// publishExpvar registers an expvar.Func, tolerating a duplicate key (expvar
// panics on re-Publish; a second New in tests must not crash the process).
func publishExpvar(name string, f func() any) {
	if expvar.Get(name) != nil {
		return
	}
	expvar.Publish(name, expvar.Func(f))
}

// Handler serves the registry over promhttp. Bootstrap mounts it loopback-only
// (GW-INV-13); this layer never decides the bind address.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the underlying registry so bootstrap can register additional
// collectors or build its own exposition handler if needed.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Wrap instruments one business handler with HTTP RED (in-flight gauge + request
// duration + request count) keyed by a fixed low-cardinality handler label (the
// route NAME, e.g. "chat_completions" — never the real path, which carries
// install_id/token segments, GW-INV-15).
func (m *Metrics) Wrap(label string, h http.Handler) http.Handler {
	labels := prometheus.Labels{"handler": label}
	return promhttp.InstrumentHandlerInFlight(m.httpInFlight,
		promhttp.InstrumentHandlerDuration(m.httpDuration.MustCurryWith(labels),
			promhttp.InstrumentHandlerCounter(m.httpRequests.MustCurryWith(labels), h)))
}
