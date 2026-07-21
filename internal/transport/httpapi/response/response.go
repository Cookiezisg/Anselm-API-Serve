// Package response renders the §5.2 wire contract (ADR-003): a BARE entity on
// success (no envelope) and the {"error":{"code","message"[,"details"]}} envelope
// on failure. It imports domain/apierr ONLY — never app, never infra/bootstrap —
// so the renderer is the single home of the wire shape every handler reuses. A
// non-*APIError normalizes to 500 INTERNAL (§4.3: never leak an internal detail /
// upstream body / key). The Retry-After header is sourced from the apierr's
// details so header and body stay in lockstep.
package response

import (
	"encoding/json"
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// envelope/errorBody are the §5.2 error wire shape. Success is a BARE entity, so
// there is deliberately no success wrapper here.
type envelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteJSON renders v as a bare JSON entity at status — no envelope (the success
// contract). Sets Content-Type before WriteHeader so the header lands.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError renders err as the error envelope. An *apierr.APIError carries its
// own status/code/message/details (and a Retry-After when its details hold
// retryAfterSec); anything else normalizes to 500 INTERNAL so an internal fault
// never leaks detail. The single error-rendering path for every handler.
func WriteError(w http.ResponseWriter, err error) {
	ae, ok := err.(*apierr.APIError)
	if !ok || ae == nil {
		ae = apierr.Internal()
	}
	writeAPIError(w, ae)
}

// WriteErrorWith renders an explicit status/code/message envelope (no details).
// For the handful of call sites that synthesize an error inline (e.g. a 405 from
// the router guard) without an apierr sentinel.
func WriteErrorWith(w http.ResponseWriter, status int, code, message string) {
	writeAPIError(w, apierr.NewError(status, code, message))
}

// writeAPIError is the shared envelope writer: it sets the Retry-After header
// from the apierr's details (so the SPA can act without parsing the message),
// then the content type, status, and body — in that order so headers commit
// before the status line.
func writeAPIError(w http.ResponseWriter, ae *apierr.APIError) {
	if _, hdr, ok := ae.RetryAfterSeconds(); ok {
		w.Header().Set("Retry-After", hdr)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(envelope{Error: errorBody{
		Code:    ae.Code,
		Message: ae.Message,
		Details: ae.Details,
	}})
}
