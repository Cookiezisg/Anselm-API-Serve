// Package healthz is the thin HTTP handler for GET /healthz: pure process
// liveness (GW-INV-13). It calls app/health.Live — a pure function that provably
// cannot reach any dependency — and writes the literal {"status":"ok"} bytes. It
// NEVER touches the DB/upstream (DB jitter must not trigger a restart storm), so
// it is the only health surface on the public mux; readiness is loopback-only.
// This route is deliberately NOT wrapped by mx.Wrap (§8).
package healthz

import (
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/app/health"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// okBody is the fixed liveness body. A constant (not a marshaled struct) so the
// response is byte-stable and the handler does no allocation per request.
const okBody = `{"status":"ok"}`

// Handler serves GET /healthz.
type Handler struct{}

// New builds the (stateless) liveness handler.
func New() *Handler { return &Handler{} }

// ServeHTTP writes liveness. Live() is consulted (always true) to keep the policy
// in app/health rather than hard-coding the verdict in transport; a false return
// would render INTERNAL, but Live never returns false by contract.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	if !health.Live() {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(okBody))
}
