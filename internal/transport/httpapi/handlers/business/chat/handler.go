// Package chat is the thin HTTP handler for POST /v1/chat/completions. It owns
// only the net/http concerns: method guard, install-id extraction, IP-key derivation,
// body read, and adapting http.ResponseWriter + http.NewResponseController into
// the app/chat.Sink. All policy (the gate order + the reserve→forward→settle
// saga) lives in app/chat — the handler just wires the adapter and calls Handle.
package chat

import (
	"io"
	"net/http"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/pkg/clientip"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Handler serves POST /v1/chat/completions over an app/chat.Service. bodyLimit
// defensively re-caps the body even when MaxBody middleware already wrapped it
// (the handler may be mounted standalone); the router passes the same config
// MAX_BODY_BYTES bound it gives the chain, so the two can never disagree.
type Handler struct {
	svc       *appchat.Service
	bodyLimit int64
}

// New wires the handler to the use case. bodyLimit <= 0 falls back to the
// domain contract default (domchat.BodyDecodeLimit, 256KiB, §5.3).
func New(svc *appchat.Service, bodyLimit int64) *Handler {
	if bodyLimit <= 0 {
		bodyLimit = domchat.BodyDecodeLimit
	}
	return &Handler{svc: svc, bodyLimit: bodyLimit}
}

// ServeHTTP guards the method, extracts the public install id + IP key, reads the (capped)
// body, adapts the writer into a Sink, and delegates to the use case. The use
// case writes every outcome through the Sink — the handler renders nothing else.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}

	// Cheapest-first, pre-buffer: a request with NO install id is 401 before
	// we buffer a single body byte — otherwise an unauthenticated flood forces
	// up to bodyLimit of memory per connection before the use case's auth gate
	// runs (the buffering happens here, ahead of svc.Handle). Same wire result
	// as the use case's own empty-install gate (401 INVALID_INSTALL), just earlier.
	installID := r.Header.Get(proofhttp.HeaderInstallID)
	if installID == "" {
		response.WriteError(w, apierr.ErrInvalidInstall)
		return
	}

	// Read the body here (not in the use case) so app/chat stays net/http-free.
	// A read error (incl. MaxBytesReader over-cap) is surfaced as BodyError; the
	// use case turns it into 400 at the body gate, in cheapest-first order.
	body, bodyErr := io.ReadAll(http.MaxBytesReader(w, r.Body, h.bodyLimit))

	in := appchat.HandleInput{
		InstallID: installID,
		Body:      body,
		BodyError: bodyErr,
		IPKey:     clientip.Key(clientip.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))),
		RequestID: w.Header().Get("X-Request-ID"), // set by Recover middleware.
	}

	sink := newSink(w)
	h.svc.Handle(r.Context(), in, sink)
}
