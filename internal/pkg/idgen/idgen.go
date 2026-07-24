// Package idgen mints opaque, prefixed public identifiers.
//
// 不可预测是核心：install_id 与 request_id 都是对外暴露的随机标识。
// 所有熵均来自 crypto/rand。
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

const (
	// installEntropyBytes is 16 so collisions are effectively impossible; the regenerate-on-conflict
	// retry still lives in the install store (slice 5), which is the only layer
	// that can observe a UNIQUE violation. idgen just mints wide ids.
	installEntropyBytes = 16
	requestEntropyBytes = 8 // request_id is a per-request trace handle, not a secret.
	mediaEntropyBytes   = 16
)

// InstallID returns a fresh installation id: "ins_" + 32 hex chars (16 bytes).
func InstallID() string {
	return "ins_" + hex.EncodeToString(randBytes(installEntropyBytes))
}

// RequestID returns a fresh request trace id: "req_" + 16 hex chars (8 bytes).
func RequestID() string {
	return "req_" + hex.EncodeToString(randBytes(requestEntropyBytes))
}

// MediaUploadID returns a private staging identifier accepted by mediafs only.
func MediaUploadID() string { return "mup_" + hex.EncodeToString(randBytes(mediaEntropyBytes)) }

// MediaLeaseID returns an opaque completion capability, intentionally independent of the upload
// id and source SHA so neither can be inferred from the other.
func MediaLeaseID() string { return "mls_" + hex.EncodeToString(randBytes(mediaEntropyBytes)) }

// MediaFetchToken returns a provider-only 256-bit capability. Its plaintext is never written to
// SQLite or returned by the public upload API; persistence stores only its SHA-256 hash.
func MediaFetchToken() string { return hex.EncodeToString(randBytes(32)) }

// randBytes fills n cryptographically-random bytes.
//
// Go 1.24+ crypto/rand.Read never returns an error; per the stdlib idiom we
// still panic on the impossible error rather than silently mint a weak id.
func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	return b
}
