// Package challenge is the thin HTTP handler for GET /v1/install/challenge: it
// mints a fresh stateless PoW challenge for a client to solve before POST
// /v1/install. All PoW math (mint/difficulty/required) lives in app/install; this
// handler only guards the method and renders the bare {challenge,difficulty,
// required} entity (§4). It answers even when PoW is dormant (required=false) so
// the surface exists to probe; /install simply never verifies in a dormant mode.
package challenge

import (
	"context"
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Service is the app/install surface the handler drives (just the mint). An
// interface so the handler unit-tests against a stub; the concrete service
// satisfies it structurally.
type Service interface {
	MintChallenge(ctx context.Context) (challenge string, difficulty int, required bool, err error)
}

// Handler serves GET /v1/install/challenge over a Service.
type Handler struct {
	svc Service
}

// New wires the handler to the use case.
func New(svc Service) *Handler { return &Handler{svc: svc} }

// body is the success entity (§4): the challenge token, the difficulty target,
// and whether the client MUST attach an X-PoW (true only in enforce mode).
type body struct {
	Challenge  string `json:"challenge"`
	Difficulty int    `json:"difficulty"`
	Required   bool   `json:"required"`
}

// ServeHTTP guards GET and renders a freshly minted challenge. A mint failure
// (e.g. a clamped-but-still-rejected difficulty) renders INTERNAL — never the
// underlying error detail.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	ch, difficulty, required, err := h.svc.MintChallenge(r.Context())
	if err != nil {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	response.WriteJSON(w, http.StatusOK, body{
		Challenge:  ch,
		Difficulty: difficulty,
		Required:   required,
	})
}
