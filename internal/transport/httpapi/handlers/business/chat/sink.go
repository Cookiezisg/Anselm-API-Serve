package chat

import (
	"net/http"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
)

// httpSink adapts http.ResponseWriter + http.ResponseController into the
// app/chat.Sink so the use case drives the response without importing net/http.
// The controller carries SetWriteDeadline + Flush (which a bare ResponseWriter
// may not expose under HTTP/2); a per-frame deadline (NOT a global timeout)
// keeps a long stream alive while bounding a wedged client.
type httpSink struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// newSink builds the adapter. http.NewResponseController degrades gracefully:
// SetWriteDeadline/Flush become no-ops (returning ErrNotSupported, which we
// ignore) if the underlying writer doesn't support them.
func newSink(w http.ResponseWriter) appchat.Sink {
	return &httpSink{w: w, rc: http.NewResponseController(w)}
}

func (s *httpSink) SetHeader(key, value string) { s.w.Header().Set(key, value) }
func (s *httpSink) WriteHeader(status int)      { s.w.WriteHeader(status) }
func (s *httpSink) Write(p []byte) (int, error) { return s.w.Write(p) }

// Flush pushes buffered frames for real-time SSE delivery; an unsupported writer
// is a no-op (the error is intentionally dropped — best-effort flush).
func (s *httpSink) Flush() { _ = s.rc.Flush() }

// SetWriteDeadline rolls the per-frame deadline; unsupported → no-op.
func (s *httpSink) SetWriteDeadline(t time.Time) { _ = s.rc.SetWriteDeadline(t) }
