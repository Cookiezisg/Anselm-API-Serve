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
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Handler serves the dashboard API over an app/dashboard.Service. In builtin
// mode it also owns the login endpoints; in external mode it has no credential or
// session state and records a fixed actor because identity belongs to the IAP.
// Handler serves the loopback dashboard API. It holds NO credential, session or
// CSRF state: authentication belongs to the IAP in front of this listener, and
// the loopback bind is what makes that delegation safe rather than a promise.
//
// Handler 服务 loopback 后台 API。它**不持有**任何凭证、会话或 CSRF 状态:鉴权属于这个监听器
// **前面**的那道 IAP,而 loopback 绑定正是让这份委托**成立**、不停留在口头承诺的东西。
type Handler struct {
	svc *appdash.Service

	// diskDegraded is a cheap in-process diskguard accessor transport already
	// holds. Provider state is supplied to the use case through its narrow port.
	diskDegraded func() bool
}

// Config wires a Handler.
type Config struct {
	Service      *appdash.Service
	DiskDegraded func() bool
}

// New builds the handler.
func New(c Config) (*Handler, error) {
	return &Handler{svc: c.Service, diskDegraded: c.DiskDegraded}, nil
}

// Healthz is always reachable on the loopback dashboard listener for liveness.
func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
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

// PostConfig applies an override batch. A bad/empty
// body ⇒ 400 BAD_REQUEST; a rejected override ⇒ 400 CONFIG_REJECTED with the
// precise reason; success ⇒ the fresh dump.
func (h *Handler) PostConfig(w http.ResponseWriter, r *http.Request) {
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

// quotaResetRequest is deliberately reason-only: this operation always resets
// every install's current RESET_TZ month, never an arbitrary period or a spend
// balance. The reason is retained in the dashboard audit ring.
type quotaResetRequest struct {
	Reason string `json:"reason"`
}

// Ban flips an install to banned. Requires CSRF + a non-empty reason (audited).
func (h *Handler) Ban(w http.ResponseWriter, r *http.Request) {
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

// ResetAllMonthlyQuota restores the configured monthly request entitlement for
// every install in the current RESET_TZ month. It requires a non-empty audit
// reason and never changes pUSD spending or ledger history.
func (h *Handler) ResetAllMonthlyQuota(w http.ResponseWriter, r *http.Request) {
	var req quotaResetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&req); err != nil {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid quota reset body")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "reason is required (audited)")
		return
	}
	result, err := h.svc.ResetAllMonthlyQuota(r.Context(), h.actor(r), reason)
	if err != nil {
		if errors.Is(err, apierr.ErrQuotaResetBusy) {
			response.WriteError(w, apierr.ErrQuotaResetBusy)
			return
		}
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "quota reset failed")
		return
	}
	response.WriteJSON(w, http.StatusOK, result)
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

// actor records a fixed trust-boundary marker rather than an identity. The IAP's
// own audit log is the identity source; copying its forwarding headers into this
// ring would stand up a second, unasked-for PII store.
//
// actor 记的是一个固定的信任边界标记、不是身份。身份的事实源是 IAP 自己的审计日志;把它的转发
// 头拷进这个环,等于凭空立起第二个没人要的 PII 存储。
func (h *Handler) actor(_ *http.Request) string { return "external-iap" }

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
