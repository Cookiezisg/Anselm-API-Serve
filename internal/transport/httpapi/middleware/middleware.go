// Package middleware holds the cross-cutting HTTP wrappers every endpoint shares:
// Recover (mint/echo X-Request-ID, attach a scoped logger to ctx, recover panics
// to 500 INTERNAL without leaking a dump), DenyCORS (default-deny cross-origin),
// and MaxBody (cap the request body). They import app/transport peers + pkg +
// domain/apierr — never infra/bootstrap. Each is a plain http.Handler wrapper so
// they chain in any order.
package middleware

import (
	"log/slog"
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/reqid"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Recover is the entrypoint middleware. It (1) establishes a request id —
// reusing a safe client-supplied X-Request-ID, else minting one — (2) binds it
// onto a request-scoped slog logger carried in ctx (logx.From downstream carries
// it on every line), (3) echoes it on the response, and (4) installs a top-level
// panic handler that logs ONLY the id + a category (never a raw dump that could
// embed the Authorization header / upstream key, §4.3) and writes 500 INTERNAL.
//
// onPanic (may be nil) is bumped once per caught panic so a panic is a numeric
// signal (gateway_panics_total), not only a log line. It runs under recover, so
// it must be cheap and never panic.
func Recover(next http.Handler, onPanic func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := reqid.Sanitize(r.Header.Get("X-Request-ID"))
		logger := logx.From(r.Context()).With("rid", rid)
		r = r.WithContext(logx.Into(r.Context(), logger))
		w.Header().Set("X-Request-ID", rid)

		defer func() {
			if rec := recover(); rec != nil {
				// Deliberately do NOT log `rec` — it may embed a request carrying the
				// Authorization header / key. Only the id (on the logger) + a category.
				logger.LogAttrs(r.Context(), slog.LevelError, "panic_recovered",
					slog.String("category", "handler_panic"))
				if onPanic != nil {
					onPanic()
				}
				response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// DenyCORS sets a default-deny CORS posture: the gateway is called by a localhost
// sidecar / Caddy, never a browser (§4.3). A cross-origin preflight (OPTIONS with
// an Origin) is rejected 403; a non-preflight cross-origin request likewise gets
// 403 so a browser fetch can never reach an endpoint. Same-origin / no-Origin
// (the normal sidecar path) passes through.
func DenyCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			response.WriteErrorWith(w, http.StatusForbidden, "FORBIDDEN", "cross-origin requests are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MaxBody wraps the request body in a MaxBytesReader so an oversized body is
// capped before any handler reads it (amplification/DoS defense, §5.3). The
// handler's io.ReadAll then surfaces the over-cap as a read error → 400.
func MaxBody(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}
