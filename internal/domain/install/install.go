// Package install is the PURE install-identity domain: the Install entity, its
// Status enum, the request/result value shapes, and the SHA-256 hashing contract
// for tokens and fingerprints. It has ZERO I/O — no os, no database/sql, no
// net/http. Storage (installstore), config wiring, and the Sybil/PoW gate
// orchestration are the app+infra layers' job; this package only owns the types
// and the irreversible-hashing rule both the issuance and auth paths share.
//
// 红线(GW-INV-12/20):token 只在签发瞬间返回,服务端只存 SHA-256;fingerprint
// 只作风控键、以 SHA-256 落库,绝不入库/入日志明文,且绝不作合并/去重键。
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Status is the install account state. Modeled as a typed enum (not bare
// strings) so a status comparison cannot silently typo against a literal and the
// only two legal values live in one place. The DB column is TEXT defaulting
// 'active'; these constants are its exact wire values.
//
// 安装态用具名枚举(非裸字符串):合法值集中一处,比较不会拼错。
type Status string

const (
	// StatusActive is a usable install (the issuance default).
	StatusActive Status = "active"
	// StatusBanned is a disabled install — auth resolves it but the caller
	// rejects with ACCOUNT_BANNED.
	StatusBanned Status = "banned"
)

// Input bounds (OWASP-API4): an over-long fingerprint/client string is truncated
// BEFORE hashing/storage so an attacker cannot bloat a rate-bucket key or the
// installs row. Truncation precedes hashing so the gate key and the stored value
// derive from the identical post-truncate string.
const (
	// MaxFingerprintLen caps the fingerprint at 256 chars before hashing/storage.
	MaxFingerprintLen = 256
	// MaxClientLen caps the client tag at 128 chars before storage.
	MaxClientLen = 128
)

// Install is one issued install identity. The token is NEVER held here — only its
// SHA-256 ever persists. Fingerprint/Client are observation-only metadata
// (Fingerprint is stored as given on the installs row for risk review, but it is
// never a merge/dedup key; the per-fp Sybil bucket keys on HashFingerprint).
type Install struct {
	ID          string
	TokenSHA256 string
	Fingerprint string // plaintext on the installs row (risk obs only); "" → NULL.
	Client      string // client tag; "" → NULL.
	Status      Status
}

// Request is the normalized /v1/install input: a trimmed+truncated fingerprint
// and client tag. Both are optional (an empty fingerprint is never a Sybil bucket
// key — see the per-fp gate). NewRequest is the single normalization point so the
// gate key and the persisted row always derive from the same string.
type Request struct {
	Fingerprint string
	Client      string
}

// NewRequest trims and length-bounds the raw inputs. Truncation MUST precede
// hashing (a different post-truncate value would key a different bucket than the
// row's stored fingerprint), so all consumers go through here.
func NewRequest(fingerprint, client string) Request {
	fp := strings.TrimSpace(fingerprint)
	if len(fp) > MaxFingerprintLen {
		fp = fp[:MaxFingerprintLen]
	}
	cl := strings.TrimSpace(client)
	if len(cl) > MaxClientLen {
		cl = cl[:MaxClientLen]
	}
	return Request{Fingerprint: fp, Client: cl}
}

// HashToken returns the hex SHA-256 of a token — the only form that ever
// persists (installs.token_sha256) and the lookup key on auth. A high-entropy
// random token needs no salt/slow-hash; SHA-256 is the irreversible store-and-
// compare contract shared by issuance and LookupInstall.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashFingerprint returns the hex SHA-256 of a fingerprint — the per-fp Sybil
// bucket key (install_fp_rate.fp_sha256). PRIVACY red-line (GW-INV-20): the
// fingerprint is an uncontrolled external string used only as a risk key; it is
// stored ONLY as this hash, never logged in plaintext. An empty fingerprint is
// never hashed into a key (the caller skips the per-fp gate entirely).
func HashFingerprint(fp string) string {
	sum := sha256.Sum256([]byte(fp))
	return hex.EncodeToString(sum[:])
}
