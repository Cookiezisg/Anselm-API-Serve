// Package idgen mints opaque, prefixed identifiers and the gateway API token.
//
// 不可逆/不可预测是核心:install_id 与 request_id 是对外暴露的随机标识,
// token 是仅在签发瞬间返回、服务端只存其 SHA-256 的凭证。所有熵均来自
// crypto/rand。
package idgen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
)

const (
	// installEntropyBytes is 16 (B13 FIX): the legacy 8-byte id made birthday
	// collisions plausible enough that an INSERT clash became a hard 500. 16
	// bytes makes a collision effectively impossible; the regenerate-on-conflict
	// retry still lives in the install store (slice 5), which is the only layer
	// that can observe a UNIQUE violation. idgen just mints wide ids.
	installEntropyBytes = 16
	requestEntropyBytes = 8  // request_id is a per-request trace handle, not a secret.
	tokenEntropyBytes   = 32 // 256-bit token, the actual bearer credential.
)

// InstallID returns a fresh installation id: "ins_" + 32 hex chars (16 bytes).
func InstallID() string {
	return "ins_" + hex.EncodeToString(randBytes(installEntropyBytes))
}

// RequestID returns a fresh request trace id: "req_" + 16 hex chars (8 bytes).
func RequestID() string {
	return "req_" + hex.EncodeToString(randBytes(requestEntropyBytes))
}

// Token returns a fresh API token: "gwk_" + base64url(32 bytes), no padding.
// Returned in full only at issuance; the store persists only its SHA-256.
func Token() string {
	return "gwk_" + base64.RawURLEncoding.EncodeToString(randBytes(tokenEntropyBytes))
}

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
