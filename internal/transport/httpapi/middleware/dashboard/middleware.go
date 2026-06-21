package dashboard

import (
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Gate bundles the per-request auth state shared by the dashboard wrappers: the
// session store + the Secure-cookie flag (true in prod behind Caddy HTTPS; only
// false under the dev-only insecure-cookie switch).
type Gate struct {
	Sessions     *SessionStore
	SecureCookie bool
}

// RequireSession is the gate for every /api/* route: it reads the session cookie,
// validates it against the server-side store (sliding the TTL), stashes the
// session in ctx, and — B14 — RE-ISSUES the Set-Cookie on every pass so the
// browser learns the slide (without this the cookie's own MaxAge would expire
// client-side at the original 12h even though the server kept sliding it). An
// absent/invalid/expired session ⇒ 401.
func (g *Gate) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			response.WriteErrorWith(w, http.StatusUnauthorized, "UNAUTHENTICATED", "login required")
			return
		}
		sess, ok := g.Sessions.get(c.Value)
		if !ok {
			response.WriteErrorWith(w, http.StatusUnauthorized, "UNAUTHENTICATED", "session invalid or expired")
			return
		}
		g.SetSessionCookie(w, sess.ID) // B14: re-issue so the browser tracks the slide.
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

// SetSessionCookie writes the session cookie with the production-safe attributes:
// HttpOnly + SameSite=Strict + Secure (dropped only by the dev-insecure flag) +
// the sliding MaxAge. Shared by login and the B14 re-issue so the attributes never
// drift between the two write sites.
func (g *Gate) SetSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   g.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearSessionCookie writes the logout cookie (empty value, MaxAge<0), mirroring
// the login cookie's Secure flag so the clear matches.
func (g *Gate) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   g.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// RequireCSRF enforces the double-submit check on a state-changing request: the
// X-CSRF-Token header must constant-time-equal the session's bound CSRF token. A
// mismatch (or missing header) ⇒ 403; no session ⇒ 401. Called at the top of each
// POST handler AFTER RequireSession has stashed the session.
func RequireCSRF(w http.ResponseWriter, r *http.Request) bool {
	sess := SessionFrom(r.Context())
	if sess == nil {
		response.WriteErrorWith(w, http.StatusUnauthorized, "UNAUTHENTICATED", "login required")
		return false
	}
	if !ConstantTimeEq(r.Header.Get(CSRFHeader), sess.CSRF) {
		response.WriteErrorWith(w, http.StatusForbidden, "CSRF_INVALID", "missing or invalid CSRF token")
		return false
	}
	return true
}

// SecurityHeaders applies the dashboard's default-safe headers to every route:
// nosniff + no-store (an admin surface must never be cached) + X-Frame-Options
// DENY + a strict CSP. script-src stays 'self' (no inline/eval); style-src adds
// 'unsafe-inline' — the one necessary relaxation for the AntD v5 CSS-in-JS runtime
// <style> injection (CSS cannot execute JS, so the script surface stays locked).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
