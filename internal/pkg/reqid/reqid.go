// Package reqid mints and sanitizes X-Request-ID values.
//
// Pure helper: no http.Handler lives here. The Recover/request-id middleware
// (transport/middleware) calls Mint/Sanitize, binds the result onto the
// request-scoped logger, and echoes it on the response.
package reqid

import (
	"crypto/rand"
	"encoding/hex"
)

// MaxLen bounds a client-supplied request id. A longer header is rejected
// rather than truncated: silently shortening could collide distinct ids and a
// mid-token cut buys nothing once we already cap length.
const MaxLen = 64

// Mint returns a fresh random request id (16 lowercase hex chars). crypto/rand
// is used so ids are unpredictable, not merely unique.
func Mint() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Sanitize reuses a client-supplied X-Request-ID when it is safe, otherwise
// mints a new one. Safe means non-empty, within MaxLen, and drawn only from
// [a-zA-Z0-9_-] — the whitelist blocks log injection (newlines/control chars)
// and line bloat from a hostile header.
func Sanitize(incoming string) string {
	if clean := clean(incoming); clean != "" {
		return clean
	}
	return Mint()
}

func clean(s string) string {
	if s == "" || len(s) > MaxLen {
		return ""
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return ""
		}
	}
	return s
}
