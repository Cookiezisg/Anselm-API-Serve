// Package install is the thin HTTP handler for POST /v1/install: it owns only the
// net/http concerns — method guard, IP-key derivation, X-PoW header read, body
// decode — and delegates every policy decision to app/install. The PoW gate runs
// FIRST (cheapest flood defense, §3) before the body is even decoded; the Sybil
// gates + INSERT live atomically inside app/install.Issue. A gate reject emits the
// unsampled WARN audit (app's AuditReject) and renders its DISTINCT apierr code.
package install

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	appdeviceproof "github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/clientip"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// installBodyLimit caps the UNAUTHENTICATED /v1/install body independently of
// the chain-wide MAX_BODY_BYTES (which is sized for chat payloads): a legit
// install body is <1KiB (fingerprint ≤256 chars + client ≤128 post-truncate),
// so 8KiB is generous while denying an anonymous flood the chat-sized buffer.
const installBodyLimit int64 = 8 * 1024

// Service is the app/install surface the handler drives. Declared as an interface
// (not the concrete *appinstall.Service) so the handler is unit-testable with a
// stub; the concrete service satisfies it structurally.
type Service interface {
	PoWGate(ctx context.Context, powHeader, ipKey string) *apierr.APIError
	Issue(ctx context.Context, req dominstall.Request, publicKey []byte, thumbprint, ipKey string) (appinstall.IssueResultView, dominstall.Gate, *apierr.APIError)
}

// Handler serves POST /v1/install over a Service.
type Handler struct {
	svc   Service
	proof *appdeviceproof.Service
}

// New wires the handler to the use case.
func New(svc Service, proof *appdeviceproof.Service) *Handler {
	return &Handler{svc: svc, proof: proof}
}

// request is the /v1/install input shape (§3). Unknown fields are dropped by the
// default decoder (not declared → ignored), never errored.
type request struct {
	Fingerprint string `json:"fingerprint"`
	Client      string `json:"client"`
}

// body is the public success entity: install id, monthly quota, and the RFC3339
// next-month reset.
type body struct {
	InstallID    string `json:"installId"`
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

	// PoW gate first — cheapest flood defense, before any body read (§3). Dormant
	// modes admit with no header read inside the app, so this is zero-cost off.
	if ae := h.svc.PoWGate(ctx, r.Header.Get("X-PoW"), ipKey); ae != nil {
		appinstall.AuditReject(ctx, ipKey, "pow", ae.Code)
		response.WriteError(w, ae)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, installBodyLimit))
	if err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	var req request
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	reg, ae := proofhttp.Registration(h.proof, r, raw)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}

	// NewRequest trims+truncates so the Sybil bucket key and the stored row derive
	// from the identical post-truncate string (domain owns the normalization).
	res, gate, ae := h.svc.Issue(ctx, dominstall.NewRequest(req.Fingerprint, req.Client), reg.PublicKey, reg.Thumbprint, ipKey)
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
		InstallID:    res.InstallID,
		MonthlyQuota: res.MonthlyQuota,
		ResetAt:      res.ResetAt.Format(time.RFC3339),
	})
}
