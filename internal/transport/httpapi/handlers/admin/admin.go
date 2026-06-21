// Package admin holds the thin HTTP handlers for the loopback-only admin
// surface: /readyz (layered readiness JSON) and the metrics/pprof/expvar
// adapters mounted by router/admin. These never run on the public business mux
// — readiness leaks coarse dependency state and pprof/expvar leak runtime
// internals, so the whole surface is loopback-only (GW-INV-13/15). Binding to a
// loopback address is a bootstrap/listeners concern (slice 12); this layer only
// produces the handlers + JSON shape.
package admin

import (
	"context"
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/app/health"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// ReadyChecker is the app/health port the readyz handler reads. Declared as an
// interface (not the concrete *health.Checker) so tests can stub a fixed
// Readiness without wiring real DB/upstream/disk probes.
type ReadyChecker interface {
	Ready(ctx context.Context) health.Readiness
}

// readyResponse is the §9 bare-entity wire shape. Disk is surfaced alongside
// db/upstream (the layered readiness now carries it explicitly; db still
// reflects disk-low as degraded per the app policy, so monitoring sees both the
// shed and its cause).
type readyResponse struct {
	DB       string `json:"db"`
	Upstream string `json:"upstream"`
	Disk     string `json:"disk"`
}

// Ready serves GET /readyz: 200 when every component is healthy, else 503. The
// body never leaks a raw error — only the ok|degraded|down per-component states
// the app layer already collapsed to. Method other than GET is rejected so a
// stray POST cannot be mistaken for a probe.
func Ready(c ReadyChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
			return
		}
		rd := c.Ready(r.Context())
		status := http.StatusOK
		if !rd.OK {
			status = http.StatusServiceUnavailable
		}
		response.WriteJSON(w, status, readyResponse{
			DB:       rd.DB,
			Upstream: rd.Upstream,
			Disk:     rd.Disk,
		})
	}
}
