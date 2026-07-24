// Package media defines the gateway's short-lived, install-bound media capability records.
// Original bytes are deliberately absent: SQLite carries only lifecycle/integrity metadata while
// a private file store owns staged bytes. A completion may name only a high-entropy Lease ID; it
// never receives a path, provider URL, source SHA-derived handle, or fetch secret.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	UploadOpen      = "open"
	UploadCompleted = "completed"
	UploadAborted   = "aborted"
	UploadExpired   = "expired"

	LeaseActive  = "active"
	LeaseExpired = "expired"
	LeaseDeleted = "deleted"
)

// Upload is a resumable, private staging object. ExpectedSHA256 pins the exact byte sequence
// before the first chunk arrives; received bytes are a monotonic progress counter, not a trust
// decision (completion verifies the digest from the staged file).
type Upload struct {
	ID             string
	InstallID      string
	ExpectedSHA256 string
	MIMEType       string
	TotalBytes     int64
	ReceivedBytes  int64
	State          string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// Lease is the opaque capability used by a completion. FetchTokenHash is the SHA-256 hash of a
// separately-generated high-entropy provider-fetch token; plaintext tokens are never persisted.
type Lease struct {
	ID             string
	InstallID      string
	UploadID       string
	SHA256         string
	MIMEType       string
	SizeBytes      int64
	FetchTokenHash string
	State          string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	DeletedAt      *time.Time
}

func ValidSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func ValidUploadState(v string) bool {
	switch v {
	case UploadOpen, UploadCompleted, UploadAborted, UploadExpired:
		return true
	default:
		return false
	}
}

func ValidLeaseState(v string) bool {
	switch v {
	case LeaseActive, LeaseExpired, LeaseDeleted:
		return true
	default:
		return false
	}
}

// HashSecret returns the only persistable representation of a media fetch capability.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}
