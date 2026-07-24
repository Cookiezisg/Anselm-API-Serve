// Package router assembles the single public business HTTP handler: the Go 1.22
// method+path ServeMux route table (install / challenge / chat / quota / models /
// healthz) wrapped by the shared middleware chain exactly as the gateway serves
// it. It is the ONE source of truth for the :8080 stack so main and the e2e
// harness exercise IDENTICAL routing + middleware ordering — no hand-copied twin
// that can drift (测试装配漂移红线). Each business route (NOT /healthz) is wrapped
// for HTTP RED (OBS-2) under a fixed low-cardinality label via the Wrapper port —
// *infra/metrics.Metrics satisfies it structurally, so the router stays infra-free
// (the metrics adapter is injected, never imported here).
package router

import (
	"net/http"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appdeviceproof "github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	appmedia "github.com/sunweilin/anselm/gateway/internal/app/media"
	appmodel "github.com/sunweilin/anselm/gateway/internal/app/model"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/challenge"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/chat"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/healthz"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/install"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/media"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/models"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/quota"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware"
)

// The body cap (amplification/DoS defense, §5.3) is applied once at the chain so
// a handler never reads more than it. The bound comes from config MAX_BODY_BYTES
// via Deps (snapshotted at boot — restart-effective); an unset Deps value falls
// back to the domain contract default (domchat.BodyDecodeLimit, 256KiB).

// Wrapper is the metrics RED port: it wraps one route handler with the HTTP RED
// instrumentation under a fixed low-cardinality label. *infra/metrics.Metrics
// satisfies it structurally; declaring it as an interface keeps the router
// testable with a no-op wrapper that doesn't pull a real registry, and a nil
// Wrapper means "no RED" (each route is mounted bare).
type Wrapper interface {
	Wrap(label string, h http.Handler) http.Handler
}

// PanicCounter is bumped once per recovered panic (gateway_panics_total). nil → no
// count (the recover still renders 500). Satisfied by the metrics bundle's Panics
// counter (its Inc method).
type PanicCounter interface {
	Inc()
}

// Deps are the assembled use-case services + the metrics surface the router wires.
// They are the concrete app services (transport may depend on app); the router is
// the single place that names them and adapts each into its thin handler, so the
// label↔route↔use-case wiring lives in exactly one file.
type Deps struct {
	Install *appinstall.Service // POST /v1/install + GET /v1/install/challenge + auth
	Proof   *appdeviceproof.Service
	Chat    *appchat.Service  // POST /v1/chat/completions
	Quota   *appquota.Service // GET /v1/quota
	Models  *appmodel.Catalog // GET /v1/models
	// Media is nil while MEDIA_ENABLED=false; routes remain proof-gated and
	// return MEDIA_UNAVAILABLE rather than silently accepting an unusable upload.
	Media              *appmedia.Service
	MediaChunkMaxBytes int64

	// Mx wraps each business route with HTTP RED; nil → routes mounted bare.
	Mx Wrapper
	// OnPanic counts a recovered panic; nil → recover still renders 500, no count.
	OnPanic PanicCounter

	// MaxBodyBytes is the chain-wide request-body cap (config MAX_BODY_BYTES,
	// snapshotted at boot). <=0 → domchat.BodyDecodeLimit (the 256KiB default).
	MaxBodyBytes int64
}

// BuildHandler assembles the business mux + the middleware chain exactly as main
// serves it. The middleware order is outer→inner: Recover (X-Request-ID + scoped
// logger + panic→counter) → DenyCORS → MaxBody(MAX_BODY_BYTES) → mux. Each business route
// is wrapped by Mx.Wrap(label, …) with a low-cardinality label; /healthz is NOT
// wrapped and never touches the DB (§8, GW-INV-13). It is a pure assembly function
// (no listeners, no DB open) so the e2e harness stands up the SAME stack via
// httptest instead of calling a handler directly and bypassing the chain.
//
// The install service doubles as the shared Authenticator for quota/models (one
// proof-verified install lookup, never a second status implementation, §2).
func BuildHandler(d Deps) http.Handler {
	limit := d.MaxBodyBytes
	if limit <= 0 {
		limit = domchat.BodyDecodeLimit
	}
	mediaHandler := media.New(d.Install, d.Media, d.MediaChunkMaxBytes)
	// The install service doubles as the shared Authenticator for quota/models.
	return assemble(routes{
		install:        install.New(d.Install, d.Proof),
		challenge:      challenge.New(d.Install),
		proofChallenge: proof.New(d.Proof),
		chat:           proof.Protect(d.Proof, chat.New(d.Chat, limit)),
		quota:          proof.Protect(d.Proof, quota.New(d.Install, d.Quota)),
		models:         proof.Protect(d.Proof, models.New(d.Install, d.Models)),
		mediaCreate:    proof.Protect(d.Proof, http.HandlerFunc(mediaHandler.Create)),
		mediaAppend:    proof.Protect(d.Proof, http.HandlerFunc(mediaHandler.Append)),
		mediaComplete:  proof.Protect(d.Proof, http.HandlerFunc(mediaHandler.Complete)),
		mediaFetch:     http.HandlerFunc(mediaHandler.Fetch),
		healthz:        healthz.New(),
	}, d.Mx, d.OnPanic, limit)
}

// routes is the pre-built per-route handler set. Splitting it from assemble lets
// the router test drive the EXACT production chain + routing with stub handlers,
// so the chain (Recover→DenyCORS→MaxBody→mux), the label↔path table, and the
// 404/405/CORS behavior are covered without standing up real app services.
type routes struct {
	install        http.Handler
	challenge      http.Handler
	proofChallenge http.Handler
	chat           http.Handler
	quota          http.Handler
	models         http.Handler
	mediaCreate    http.Handler
	mediaAppend    http.Handler
	mediaComplete  http.Handler
	mediaFetch     http.Handler
	healthz        http.Handler
}

// assemble mounts the route table and wraps it in the middleware chain — the ONE
// place the chain order and the RED labels live. Each business route is wrapped by
// mx.Wrap(label,…); /healthz is intentionally un-wrapped (§8).
func assemble(rt routes, mx Wrapper, onPanic PanicCounter, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = domchat.BodyDecodeLimit
	}
	mux := http.NewServeMux()

	wrap := func(label string, h http.Handler) http.Handler {
		if mx == nil {
			return h
		}
		return mx.Wrap(label, h)
	}

	mux.Handle("POST /v1/install", wrap("install", rt.install))
	mux.Handle("GET /v1/install/challenge", wrap("install_challenge", rt.challenge))
	mux.Handle("GET /v1/proof/challenge", wrap("proof_challenge", rt.proofChallenge))
	mux.Handle("POST /v1/chat/completions", wrap("chat_completions", rt.chat))
	mux.Handle("GET /v1/quota", wrap("quota", rt.quota))
	mux.Handle("GET /v1/models", wrap("models", rt.models))
	mux.Handle("POST /v1/media/uploads", wrap("media_create", rt.mediaCreate))
	mux.Handle("PUT /v1/media/uploads/{uploadId}", wrap("media_append", rt.mediaAppend))
	mux.Handle("POST /v1/media/uploads/{uploadId}/complete", wrap("media_complete", rt.mediaComplete))
	mux.Handle("GET /v1/media/leases/{leaseId}/content", wrap("media_fetch", rt.mediaFetch))
	// Liveness is the ONLY public health surface and is deliberately un-wrapped
	// (no RED label, §8): it must stay pure and never touch the DB (GW-INV-13).
	mux.Handle("GET /healthz", rt.healthz)

	var handler http.Handler = mux
	handler = middleware.MaxBody(handler, maxBodyBytes)
	handler = middleware.DenyCORS(handler)
	handler = middleware.Recover(handler, panicHook(onPanic))
	return handler
}

// panicHook adapts the optional PanicCounter into the func() the Recover
// middleware expects; nil counter → nil hook (recover still renders 500).
func panicHook(c PanicCounter) func() {
	if c == nil {
		return nil
	}
	return c.Inc
}
