// Package router assembles the dashboard's loopback admin handler: the mux +
// middleware stack. It imports the dashboard handler + middleware + net/http
// ONLY (no infra) — bootstrap constructs the app/dashboard.Service and passes it
// in via DashboardDeps.
package router

import (
	"net/http"
	"strings"

	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
)

// DashboardDeps bundles the constructed pieces BuildDashboardHandler wires.
// Static is the SPA file server; nil ⇒ a minimal shell.
type DashboardDeps struct {
	Handler *dashhandler.Handler
	Static  http.Handler
}

// BuildDashboardHandler builds the routed + security-wrapped dashboard handler.
//
// There is no authentication middleware here, and that is the design rather than
// an omission: this handler is only ever mounted on a loopback listener whose
// bind is fail-fast, and a preceding IAP (Cloudflare Access, Tailscale, an SSH
// tunnel) owns the login wall. Adding a second, weaker wall inside the process
// would not add a boundary — it would add a thing to keep in sync with the real
// one. Concrete API patterns always beat the SPA fallback.
//
// 这里**没有**鉴权中间件,而这是设计、不是遗漏:本 handler 只会挂在一个 fail-fast 绑定的
// loopback 监听器上,而登录墙归**前置**的 IAP(Cloudflare Access / Tailscale / SSH 隧道)所有。
// 在进程内再加一道更弱的墙,不会多出一条边界——只会多出一样要和真边界保持同步的东西。
func BuildDashboardHandler(d DashboardDeps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.Handler.Healthz)
	mux.HandleFunc("GET /api/overview", d.Handler.Overview)
	mux.HandleFunc("GET /api/config", d.Handler.GetConfig)
	mux.HandleFunc("POST /api/config", d.Handler.PostConfig)
	mux.HandleFunc("GET /api/installs", d.Handler.Installs)
	mux.HandleFunc("POST /api/installs/ban", d.Handler.Ban)
	mux.HandleFunc("POST /api/installs/unban", d.Handler.Unban)
	mux.HandleFunc("POST /api/quota/reset", d.Handler.ResetAllMonthlyQuota)
	mux.HandleFunc("GET /api/audit", d.Handler.Audit)
	mux.HandleFunc("GET /api/export", d.Handler.Export)
	// A provided Static handler owns every non-API GET (bootstrap passes the
	// embedded React SPA from infra/webassets, which serves /static/ + the SPA
	// shell); when absent (e.g. router tests), a minimal placeholder shell.
	static := d.Static
	if static == nil {
		static = http.HandlerFunc(serveMinimalIndex)
	}
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A method-specific /api/ fallback conflicts with the GET / SPA pattern
		// under Go's ServeMux specificity rules, so keep the boundary in this
		// final GET fallback instead. POST/other unknown API paths naturally 405.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	}))

	return mwdash.SecurityHeaders(mux)
}

// serveMinimalIndex is the nil-Static fallback shell (router tests / no SPA wired).
func serveMinimalIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><title>Anselm Admin</title><body>dashboard</body>"))
}
