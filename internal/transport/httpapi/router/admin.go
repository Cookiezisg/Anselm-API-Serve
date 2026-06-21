package router

import (
	"expvar"
	"net/http"
	"net/http/pprof"

	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/admin"
)

// AdminDeps wires the admin mux. Metrics is the promhttp exposition handler
// (infra/metrics.Handler()) passed in as a plain http.Handler so this transport
// layer never imports infra — bootstrap owns the registry and hands us the
// handler. Ready is the app/health readiness checker.
type AdminDeps struct {
	Metrics http.Handler
	Ready   admin.ReadyChecker
}

// BuildAdminHandler assembles the loopback-only admin mux: /metrics, /readyz,
// /debug/pprof/*, /debug/vars. It deliberately does NOT bind or enforce a
// loopback address — that is a bootstrap/listeners concern (slice 12,
// requireLoopback). Everything mounted here (esp. pprof + expvar, which expose
// goroutine/heap/cpu profiles and runtime gauges) is loopback-only and MUST be
// served on the isolated admin listener, never reverse-proxied (GW-INV-13).
func BuildAdminHandler(deps AdminDeps) http.Handler {
	mux := http.NewServeMux()

	if deps.Metrics != nil {
		mux.Handle("GET /metrics", deps.Metrics)
	}
	mux.Handle("GET /readyz", admin.Ready(deps.Ready))

	// pprof: loopback-only runtime profiling. Index plus the four sub-handlers it
	// links to; Index also serves the named runtime profiles (goroutine/heap/...)
	// under /debug/pprof/<name>.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// expvar: scrape-free runtime gauges (gateway_goroutines /
	// gateway_heap_alloc_bytes, published by infra/metrics) for a quick loopback
	// curl at /debug/vars, complementing the Prometheus process collector.
	mux.Handle("GET /debug/vars", expvar.Handler())

	return mux
}
