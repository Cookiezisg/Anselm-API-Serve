package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// FetchSigner derives the private, provider-facing credential from durable lease
// facts. Unlike a random token kept only in process memory, it remains valid
// across a gateway restart while SQLite still stores only a one-way hash.
type FetchSigner interface {
	Token(leaseID, installID string, expiresAt time.Time) string
	Verify(token, leaseID, installID string, expiresAt time.Time) bool
}

// HMACSigner is deliberately local to the media use case: signing a provider
// fetch URL is media policy, not a generic HTTP/session primitive.
type HMACSigner struct{ secret []byte }

func NewHMACSigner(secret []byte) (*HMACSigner, error) {
	if len(secret) < 32 {
		return nil, errors.New("media signer secret must contain at least 32 bytes")
	}
	return &HMACSigner{secret: append([]byte(nil), secret...)}, nil
}

func (s *HMACSigner) Token(leaseID, installID string, expiresAt time.Time) string {
	expires := expiresAt.UTC().Unix()
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(fetchTokenPayload(leaseID, installID, expires)))
	return strconv.FormatInt(expires, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *HMACSigner) Verify(token, leaseID, installID string, expiresAt time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expires != expiresAt.UTC().Unix() {
		return false
	}
	want := s.Token(leaseID, installID, expiresAt)
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

func fetchTokenPayload(leaseID, installID string, expires int64) string {
	return "anselm-media-fetch-v1\x00" + leaseID + "\x00" + installID + "\x00" + strconv.FormatInt(expires, 10)
}
