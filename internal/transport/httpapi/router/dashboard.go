// Package router assembles the dashboard's loopback admin handler: the mux +
// middleware stack. It imports the dashboard handler + middleware + net/http
// ONLY (no infra) — bootstrap constructs the app/dashboard.Service and, for
// builtin mode only, the session/login state and passes them in via DashboardDeps.
package router

import (
	"net/http"
	"strings"

	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
)

// DashboardDeps bundles the constructed pieces BuildDashboardHandler wires. A nil
// Gate denotes external mode: a preceding IAP has authenticated the request and
// the loopback-only listener then exposes the API directly. A non-nil Gate denotes
// builtin mode. Static is the SPA file server; nil ⇒ a minimal shell.
type DashboardDeps struct {
	Handler *dashhandler.Handler
	Gate    *mwdash.Gate
	Static  http.Handler
}

// BuildDashboardHandler builds the routed + security-wrapped dashboard handler.
// /healthz and /api/bootstrap are always public on the loopback listener. In
// builtin mode the remaining API requires a Go session and mutating calls require
// CSRF; in external mode the IAP is the exclusive authentication boundary and the
// API is direct. Concrete API patterns always beat the SPA fallback.
func BuildDashboardHandler(d DashboardDeps) http.Handler {
	mux := http.NewServeMux()

	// Public liveness + non-sensitive SPA bootstrap.
	mux.HandleFunc("GET /healthz", d.Handler.Healthz)
	mux.HandleFunc("GET /api/bootstrap", d.Handler.Bootstrap)

	if d.Gate != nil {
		mux.HandleFunc("POST /login", d.Handler.Login)
		mux.HandleFunc("POST /logout", d.Handler.Logout)
		requireSession := d.Gate.RequireSession
		mux.Handle("GET /api/session", requireSession(http.HandlerFunc(d.Handler.Session)))
		mux.Handle("GET /api/overview", requireSession(http.HandlerFunc(d.Handler.Overview)))
		mux.Handle("GET /api/config", requireSession(http.HandlerFunc(d.Handler.GetConfig)))
		mux.Handle("POST /api/config", requireSession(http.HandlerFunc(d.Handler.PostConfig)))
		mux.Handle("GET /api/installs", requireSession(http.HandlerFunc(d.Handler.Installs)))
		mux.Handle("POST /api/installs/ban", requireSession(http.HandlerFunc(d.Handler.Ban)))
		mux.Handle("POST /api/installs/unban", requireSession(http.HandlerFunc(d.Handler.Unban)))
		mux.Handle("POST /api/quota/reset", requireSession(http.HandlerFunc(d.Handler.ResetAllMonthlyQuota)))
		mux.Handle("GET /api/audit", requireSession(http.HandlerFunc(d.Handler.Audit)))
		mux.Handle("GET /api/export", requireSession(http.HandlerFunc(d.Handler.Export)))
	} else {
		mux.HandleFunc("GET /api/overview", d.Handler.Overview)
		mux.HandleFunc("GET /api/config", d.Handler.GetConfig)
		mux.HandleFunc("POST /api/config", d.Handler.PostConfig)
		mux.HandleFunc("GET /api/installs", d.Handler.Installs)
		mux.HandleFunc("POST /api/installs/ban", d.Handler.Ban)
		mux.HandleFunc("POST /api/installs/unban", d.Handler.Unban)
		mux.HandleFunc("POST /api/quota/reset", d.Handler.ResetAllMonthlyQuota)
		mux.HandleFunc("GET /api/audit", d.Handler.Audit)
		mux.HandleFunc("GET /api/export", d.Handler.Export)
	}
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
