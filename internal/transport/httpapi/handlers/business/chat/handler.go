// Package chat is the thin HTTP handler for POST /v1/chat/completions. It owns
// only the net/http concerns: method guard, bearer extraction, IP-key derivation,
// body read, and adapting http.ResponseWriter + http.NewResponseController into
// the app/chat.Sink. All policy (the gate order + the reserve→forward→settle
// saga) lives in app/chat — the handler just wires the adapter and calls Handle.
package chat

import (
	"io"
	"net/http"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/pkg/clientip"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// bodyLimit defensively re-caps the body even when MaxBody middleware already
// wrapped it (the handler may be mounted standalone). The bound is the domain's
// single contract constant (§5.3).
const bodyLimit = domchat.BodyDecodeLimit

// Handler serves POST /v1/chat/completions over an app/chat.Service.
type Handler struct {
	svc *appchat.Service
}

// New wires the handler to the use case.
func New(svc *appchat.Service) *Handler { return &Handler{svc: svc} }

// ServeHTTP guards the method, extracts the bearer + IP key, reads the (capped)
// body, adapts the writer into a Sink, and delegates to the use case. The use
// case writes every outcome through the Sink — the handler renders nothing else.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}

	// Read the body here (not in the use case) so app/chat stays net/http-free.
	// A read error (incl. MaxBytesReader over-cap) is surfaced as BodyError; the
	// use case turns it into 400 at the body gate, in cheapest-first order.
	body, bodyErr := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))

	in := appchat.HandleInput{
		Token:     response.Bearer(r),
		Body:      body,
		BodyError: bodyErr,
		IPKey:     clientip.Key(clientip.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))),
		RequestID: w.Header().Get("X-Request-ID"), // set by Recover middleware.
	}

	sink := newSink(w)
	h.svc.Handle(r.Context(), in, sink)
}
