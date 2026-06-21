// Package router assembles the dashboard's loopback admin handler: the mux +
// middleware stack. It imports the dashboard handler + middleware + net/http
// ONLY (no infra) — bootstrap constructs the app/dashboard.Service and the
// session/login state and passes them in via DashboardDeps.
package router

import (
	"net/http"

	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
)

// DashboardDeps bundles the constructed pieces BuildDashboardHandler wires. The
// Handler carries the app/dashboard.Service + the bcrypt hash; Gate + Logins are
// the shared session/login state (also swept by bootstrap's sweep loop). Static is
// the SPA file server (infra/webassets embedded React build); nil ⇒ a minimal shell.
type DashboardDeps struct {
	Handler *dashhandler.Handler
	Gate    *mwdash.Gate
	Static  http.Handler
}

// BuildDashboardHandler builds the routed + security-wrapped dashboard handler.
// /healthz, /login, /logout are public (login establishes the session); every
// /api/* route is behind RequireSession; SecurityHeaders wraps ALL routes. The
// /api/*, /login, /logout patterns win ServeMux precedence over the "/" SPA
// fallback, so the SPA can never shadow an authenticated route or bypass auth.
func BuildDashboardHandler(d DashboardDeps) http.Handler {
	mux := http.NewServeMux()

	// Public liveness + auth entry points (not behind RequireSession).
	mux.HandleFunc("GET /healthz", d.Handler.Healthz)
	mux.HandleFunc("POST /login", d.Handler.Login)
	mux.HandleFunc("POST /logout", d.Handler.Logout)

	// All /api/* require a valid session. State-changing POSTs additionally require
	// a matching CSRF token (enforced inside each handler after RequireSession).
	requireSession := d.Gate.RequireSession
	mux.Handle("GET /api/session", requireSession(http.HandlerFunc(d.Handler.Session)))
	mux.Handle("GET /api/overview", requireSession(http.HandlerFunc(d.Handler.Overview)))
	mux.Handle("GET /api/config", requireSession(http.HandlerFunc(d.Handler.GetConfig)))
	mux.Handle("POST /api/config", requireSession(http.HandlerFunc(d.Handler.PostConfig)))
	mux.Handle("GET /api/installs", requireSession(http.HandlerFunc(d.Handler.Installs)))
	mux.Handle("POST /api/installs/ban", requireSession(http.HandlerFunc(d.Handler.Ban)))
	mux.Handle("POST /api/installs/unban", requireSession(http.HandlerFunc(d.Handler.Unban)))
	mux.Handle("GET /api/audit", requireSession(http.HandlerFunc(d.Handler.Audit)))
	mux.Handle("GET /api/export", requireSession(http.HandlerFunc(d.Handler.Export)))

	// A provided Static handler owns every non-API GET (bootstrap passes the
	// embedded React SPA from infra/webassets, which serves /static/ + the SPA
	// shell); when absent (e.g. router tests), a minimal placeholder shell.
	static := d.Static
	if static == nil {
		static = http.HandlerFunc(serveMinimalIndex)
	}
	mux.Handle("GET /", static)

	return mwdash.SecurityHeaders(mux)
}

// serveMinimalIndex is the nil-Static fallback shell (router tests / no SPA wired).
func serveMinimalIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><title>Anselm Admin</title><body>dashboard</body>"))
}
