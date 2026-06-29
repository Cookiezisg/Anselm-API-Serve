// Package install is the thin HTTP handler for POST /v1/install: it owns only the
// net/http concerns — method guard, IP-key derivation, X-PoW header read, body
// decode — and delegates every policy decision to app/install. The PoW gate runs
// FIRST (cheapest flood defense, §3) before the body is even decoded; the Sybil
// gates + INSERT live atomically inside app/install.Issue. A gate reject emits the
// unsampled WARN audit (app's AuditReject) and renders its DISTINCT apierr code.
package install

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/clientip"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Service is the app/install surface the handler drives. Declared as an interface
// (not the concrete *appinstall.Service) so the handler is unit-testable with a
// stub; the concrete service satisfies it structurally.
type Service interface {
	PoWGate(ctx context.Context, powHeader, ipKey string) *apierr.APIError
	Issue(ctx context.Context, req dominstall.Request, ipKey string, unmetered bool) (appinstall.IssueResultView, dominstall.Gate, *apierr.APIError)
	// IsUnmetered reports whether an optional bearer (the device's existing
	// whitelisted token) puts this re-mint on the operator god-mode path: PoW + all
	// Sybil gates skipped. Empty/non-whitelisted bearer → false (normal gates).
	IsUnmetered(token string) bool
}

// Handler serves POST /v1/install over a Service.
type Handler struct {
	svc Service
}

// New wires the handler to the use case.
func New(svc Service) *Handler { return &Handler{svc: svc} }

// request is the /v1/install input shape (§3). Unknown fields are dropped by the
// default decoder (not declared → ignored), never errored.
type request struct {
	Fingerprint string `json:"fingerprint"`
	Client      string `json:"client"`
}

// body is the success entity (§3): the plaintext token (returned ONLY here),
// the monthly quota, and the RFC3339 next-month reset.
type body struct {
	Token        string `json:"token"`
	MonthlyQuota int64  `json:"monthlyQuota"`
	ResetAt      string `json:"resetAt"`
}

// ServeHTTP guards POST, derives the IP key, runs the PoW gate first, decodes the
// body, then issues. Every reject renders a distinct apierr; a gate reject also
// audits (ip_key + gate + code only — never the fingerprint plaintext, GW-INV-20).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	ctx := r.Context()
	ipKey := clientip.Key(clientip.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For")))

	// Operator god-mode: a device that already holds a whitelisted token may present
	// it as a bearer to re-mint freely. When unmetered we skip the PoW gate AND every
	// Sybil gate (passed into Issue). Empty/non-whitelisted bearer → false → the full
	// normal path runs unchanged (dormant zero-cost when no token is whitelisted).
	unmetered := h.svc.IsUnmetered(response.Bearer(r))

	// PoW gate first — cheapest flood defense, before any body read (§3). Dormant
	// modes admit with no header read inside the app, so this is zero-cost off.
	if !unmetered {
		if ae := h.svc.PoWGate(ctx, r.Header.Get("X-PoW"), ipKey); ae != nil {
			appinstall.AuditReject(ctx, ipKey, "pow", ae.Code)
			response.WriteError(w, ae)
			return
		}
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}

	// NewRequest trims+truncates so the Sybil bucket key and the stored row derive
	// from the identical post-truncate string (domain owns the normalization).
	res, gate, ae := h.svc.Issue(ctx, dominstall.NewRequest(req.Fingerprint, req.Client), ipKey, unmetered)
	if ae != nil {
		// A tripped gate is an audited security event; an internal fault (GateNone)
		// renders INTERNAL without an audit line.
		if gate != dominstall.GateNone {
			appinstall.AuditReject(ctx, ipKey, gate, ae.Code)
		}
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, body{
		Token:        res.Token,
		MonthlyQuota: res.MonthlyQuota,
		ResetAt:      res.ResetAt.Format(time.RFC3339),
	})
}
