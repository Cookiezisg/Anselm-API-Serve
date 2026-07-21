// Package dashboard is the thin HTTP handler layer for the loopback admin
// backend. It owns only net/http concerns (method/body and optional builtin
// session/CSRF wiring) and renders every outcome through
// transport/httpapi/response (the apierr envelope). All policy lives in
// app/dashboard; builtin auth mechanics live in the dashboard middleware. It
// imports app + pkg + those transport peers + net/http ONLY — never infra.
package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/pkg/clientip"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"

	"golang.org/x/crypto/bcrypt"
)

// Handler serves the dashboard API over an app/dashboard.Service. In builtin
// mode it also owns the login endpoints; in external mode it has no credential or
// session state and records a fixed actor because identity belongs to the IAP.
type Handler struct {
	svc      *appdash.Service
	gate     *mwdash.Gate
	logins   *mwdash.LoginLimiter
	user     string
	passHash []byte

	// diskDegraded is a cheap in-process diskguard accessor transport already
	// holds. Provider state is supplied to the use case through its narrow port.
	diskDegraded func() bool
	authMode     string
}

// BuiltinAuth is the complete credential/session capability used only when
// DASHBOARD_AUTH_MODE=builtin. Password is bcrypt-hashed by New and discarded.
type BuiltinAuth struct {
	Gate     *mwdash.Gate
	Logins   *mwdash.LoginLimiter
	User     string
	Password string
}

// Config wires a Handler. AuthMode is part of the public dashboard bootstrap
// response; BuiltinAuth must be non-nil exactly for builtin mode.
type Config struct {
	Service      *appdash.Service
	DiskDegraded func() bool
	AuthMode     string
	BuiltinAuth  *BuiltinAuth
}

// New builds the handler. It hashes a builtin password once (DefaultCost — login
// is rare) and never retains plaintext. External mode intentionally constructs no
// credential/session state at all.
func New(c Config) (*Handler, error) {
	if c.AuthMode != config.DashboardAuthModeBuiltin && c.AuthMode != config.DashboardAuthModeExternal {
		return nil, errString("dashboard handler requires builtin or external auth mode")
	}
	h := &Handler{
		diskDegraded: c.DiskDegraded,
		svc:          c.Service,
		authMode:     c.AuthMode,
	}
	if c.AuthMode == config.DashboardAuthModeExternal {
		if c.BuiltinAuth != nil {
			return nil, errString("external dashboard auth must not construct builtin credentials")
		}
		return h, nil
	}
	if c.BuiltinAuth == nil {
		return nil, errBuiltinAuthNotConfigured
	}
	if c.BuiltinAuth.Gate == nil || c.BuiltinAuth.Logins == nil || c.BuiltinAuth.User == "" || c.BuiltinAuth.Password == "" {
		return nil, errBuiltinAuthNotConfigured
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(c.BuiltinAuth.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	h.gate = c.BuiltinAuth.Gate
	h.logins = c.BuiltinAuth.Logins
	h.user = c.BuiltinAuth.User
	h.passHash = hash
	return h, nil
}

var errBuiltinAuthNotConfigured = errString("builtin dashboard auth not configured")

type errString string

func (e errString) Error() string { return string(e) }

// loginRequest / loginResponse are the POST /login wire shapes. The session id
// rides the HttpOnly cookie (never a body); the CSRF token is returned so the SPA
// can echo it on state-changing POSTs.
type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type loginResponse struct {
	CSRFToken string `json:"csrfToken"`
	User      string `json:"user"`
}

// Healthz is always reachable on the loopback dashboard listener for liveness.
func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Bootstrap describes the active dashboard authentication contract to the SPA.
// It intentionally carries no identity or secret; the SPA uses it only to select
// its builtin login flow or its IAP-trusted direct API flow.
func (h *Handler) Bootstrap(w http.ResponseWriter, _ *http.Request) {
	response.WriteJSON(w, http.StatusOK, struct {
		AuthMode string `json:"authMode"`
	}{AuthMode: h.authMode})
}

// Login authenticates {user,password}. B15: it DECODES + validates the body BEFORE
// reserving a failure slot, so a malformed body is a 400 that consumes NO slot and
// never calls Success(). Only a well-formed attempt reaches the per-IP gate; a
// credential failure (or a lockout) returns with no hint which factor failed and
// arms/escalates the lockout. A success clears the IP's counter and sets the cookie.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	ip := loginIP(r)

	// B15: decode + validate FIRST. A bad body must not consume a failure slot.
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024)).Decode(&req); err != nil {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid login body")
		return
	}

	// Now the single-hold gate: check lockout AND reserve a failure slot atomically.
	if !h.logins.Attempt(ip) {
		retry := h.logins.RetryAfter(ip)
		response.WriteError(w, apierr.LoginLocked(retry))
		return
	}

	userOK := mwdash.ConstantTimeEq(req.User, h.user)
	// Always run bcrypt (even on a wrong user) so the response time does not reveal
	// whether the username matched (timing-oracle defense).
	passErr := bcrypt.CompareHashAndPassword(h.passHash, []byte(req.Password))
	if !userOK || passErr != nil {
		// The failure was already reserved by Attempt(); leave it.
		response.WriteError(w, apierr.NewError(http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid user or password"))
		return
	}

	h.logins.Success(ip)
	sess := h.gate.Sessions.Create(h.user)
	h.gate.SetSessionCookie(w, sess.ID)
	response.WriteJSON(w, http.StatusOK, loginResponse{CSRFToken: sess.CSRF, User: h.user})
}

// Logout destroys the current session (if any) and clears the cookie. Idempotent
// and CSRF-free (a forged logout merely logs the victim out — no state damage —
// and requiring CSRF here would let a lost token wedge a user logged in forever).
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(mwdash.CookieName); err == nil && c.Value != "" {
		h.gate.Sessions.Destroy(c.Value)
	}
	h.gate.ClearSessionCookie(w)
	response.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Session is the recovery probe behind RequireSession: an authenticated caller
// gets {user, csrfToken} so the SPA can rehydrate its in-memory CSRF token after an
// F5. An absent/invalid session never reaches here (RequireSession 401s first), so
// NO token leaks to an unauthenticated peer.
func (h *Handler) Session(w http.ResponseWriter, r *http.Request) {
	sess := mwdash.SessionFrom(r.Context())
	if sess == nil {
		response.WriteErrorWith(w, http.StatusUnauthorized, "UNAUTHENTICATED", "login required")
		return
	}
	response.WriteJSON(w, http.StatusOK, loginResponse{CSRFToken: sess.CSRF, User: sess.User})
}

// Overview returns the live operational snapshot.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	o := h.svc.Overview(r.Context(), call(h.diskDegraded))
	response.WriteJSON(w, http.StatusOK, o)
}

// GetConfig returns the secret-free config read model.
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	response.WriteJSON(w, http.StatusOK, configResponse{Items: h.svc.ConfigDump()})
}

type configResponse struct {
	Items []appdash.DumpItem `json:"items"`
}

// PostConfig applies an override batch. Requires CSRF (state-changing). A bad/empty
// body ⇒ 400 BAD_REQUEST; a rejected override ⇒ 400 CONFIG_REJECTED with the
// precise reason; success ⇒ the fresh dump.
func (h *Handler) PostConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireCSRF(w, r) {
		return
	}
	var overrides map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&overrides); err != nil {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid config body")
		return
	}
	if len(overrides) == 0 {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "no overrides provided")
		return
	}
	items, err := h.svc.ConfigApply(r.Context(), h.actor(r), overrides)
	if err != nil {
		var rej *appdash.ErrConfigRejected
		if errors.As(err, &rej) {
			response.WriteErrorWith(w, http.StatusBadRequest, "CONFIG_REJECTED", rej.Reason)
			return
		}
		response.WriteError(w, err)
		return
	}
	response.WriteJSON(w, http.StatusOK, configResponse{Items: items})
}

// Installs lists installs newest-first with cursor pagination.
func (h *Handler) Installs(w http.ResponseWriter, r *http.Request) {
	cursor, limit := parsePagination(r)
	rows, next, more, err := h.svc.InstallsList(r.Context(), cursor, limit)
	if err != nil {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "failed to list installs")
		return
	}
	nextCursor := ""
	if more {
		nextCursor = strconv.Itoa(next)
	}
	response.WriteJSON(w, http.StatusOK, struct {
		Installs   []appdash.InstallRow `json:"installs"`
		NextCursor string               `json:"nextCursor"`
	}{Installs: rows, NextCursor: nextCursor})
}

// banRequest / unbanRequest bodies.
type banRequest struct {
	InstallID string `json:"install_id"`
	Reason    string `json:"reason"`
}

type unbanRequest struct {
	InstallID string `json:"install_id"`
}

// Ban flips an install to banned. Requires CSRF + a non-empty reason (audited).
func (h *Handler) Ban(w http.ResponseWriter, r *http.Request) {
	if !h.requireCSRF(w, r) {
		return
	}
	var req banRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&req); err != nil {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid ban body")
		return
	}
	id := strings.TrimSpace(req.InstallID)
	reason := strings.TrimSpace(req.Reason)
	if id == "" {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "install_id is required")
		return
	}
	if reason == "" {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "reason is required (audited)")
		return
	}
	if err := h.svc.Ban(r.Context(), h.actor(r), id, reason); err != nil {
		h.writeMutateErr(w, err)
		return
	}
	response.WriteJSON(w, http.StatusOK, map[string]string{"install_id": id, "status": "banned"})
}

// Unban flips an install back to active. Requires CSRF; reason optional.
func (h *Handler) Unban(w http.ResponseWriter, r *http.Request) {
	if !h.requireCSRF(w, r) {
		return
	}
	var req unbanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&req); err != nil {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid unban body")
		return
	}
	id := strings.TrimSpace(req.InstallID)
	if id == "" {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "install_id is required")
		return
	}
	if err := h.svc.Unban(r.Context(), h.actor(r), id); err != nil {
		h.writeMutateErr(w, err)
		return
	}
	response.WriteJSON(w, http.StatusOK, map[string]string{"install_id": id, "status": "active"})
}

// writeMutateErr maps a ban/unban error: a no-match id ⇒ 404 INSTALL_NOT_FOUND;
// anything else ⇒ 500 INTERNAL (never leak the underlying DB detail).
func (h *Handler) writeMutateErr(w http.ResponseWriter, err error) {
	var nf *appdash.ErrInstallNotFound
	if errors.As(err, &nf) {
		response.WriteErrorWith(w, http.StatusNotFound, "INSTALL_NOT_FOUND", "no install with that id")
		return
	}
	response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "status update failed")
}

// Audit returns recent audit events, keyset-paged by the monotonic seq.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	beforeSeq, limit := parseAuditCursor(r)
	events, next := h.svc.Audit(beforeSeq, limit)
	nextCursor := ""
	if next > 0 {
		nextCursor = strconv.FormatInt(next, 10)
	}
	response.WriteJSON(w, http.StatusOK, struct {
		Events     []appdash.AuditEvent `json:"events"`
		NextCursor string               `json:"nextCursor"`
	}{Events: events, NextCursor: nextCursor})
}

// Export streams a consistent SQLite snapshot as an attachment download, then
// cleans up the temp file. The terminal outcome (ok/interrupted) is audited via
// the service ring so every export attempt — including a download cut short
// mid-stream — leaves a trail in the queryable view, not only journald.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	body, size, filename, cleanup, err := h.svc.Export(r.Context())
	if err != nil {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "export snapshot failed")
		return
	}
	defer cleanup()
	if size == 0 {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "export produced an empty snapshot")
		return
	}

	a := h.actor(r)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// Headers already sent; the client sees a truncated download. Audit the
		// interrupted attempt so it does not vanish from the operator's audit view.
		h.svc.RecordAudit(appdash.AuditEvent{Action: "export", Target: filename, Outcome: "interrupted", Actor: a})
		return
	}
	h.svc.RecordAudit(appdash.AuditEvent{Action: "export", Target: filename, Outcome: "ok", Actor: a})
}

// --- small net/http helpers ---

// loginIP derives the lockout key exclusively from the direct TCP peer. Dashboard
// traffic reaches this loopback listener directly (including through an SSH
// tunnel), not through the business listener's trusted Caddy hop, so every
// X-Forwarded-For value is attacker-controlled here and must be ignored. Key
// removes the ephemeral source port and applies the shared IPv6 /64 bucketing.
func loginIP(r *http.Request) string {
	return clientip.Key(r.RemoteAddr)
}

// actor returns the builtin session user, or a fixed trust-boundary marker for an
// external IAP. The IAP's own audit log remains the identity source; copying
// forwarding headers into this audit ring would create a new PII store.
func (h *Handler) actor(r *http.Request) string {
	if h.gate != nil {
		if sess := mwdash.SessionFrom(r.Context()); sess != nil {
			return sess.User
		}
	}
	return "external-iap"
}

// requireCSRF only applies to the cookie-authenticated builtin capability.
// external mode does not use browser credentials at this hop: the Dashboard is
// loopback-only and its upstream IAP is the authentication boundary.
func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if h.gate == nil {
		return true
	}
	return mwdash.RequireCSRF(w, r)
}

// call invokes an optional bool accessor, treating nil as false.
func call(f func() bool) bool {
	if f == nil {
		return false
	}
	return f()
}

// parsePagination reads ?cursor=&limit= as an offset cursor (>=0) + limit clamped
// in the use case; here it just parses with sane defaults.
func parsePagination(r *http.Request) (cursor, limit int) {
	cursor = atoiDefault(r.URL.Query().Get("cursor"), 0)
	limit = atoiDefault(r.URL.Query().Get("limit"), 50)
	return cursor, limit
}

// parseAuditCursor reads the audit keyset cursor (?cursor=seq; absent/invalid =
// newest) + the limit.
func parseAuditCursor(r *http.Request) (beforeSeq int64, limit int) {
	_, limit = parsePagination(r)
	if c := r.URL.Query().Get("cursor"); c != "" {
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n > 0 {
			beforeSeq = n
		}
	}
	return beforeSeq, limit
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
