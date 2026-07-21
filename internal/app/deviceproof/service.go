// Package deviceproof owns the gateway's device-bound request authentication.
// A public install id identifies an account; possession of its Ed25519 private
// key authenticates each concrete HTTP request. No reusable bearer secret exists.
package deviceproof

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

const (
	nonceTTL        = 5 * time.Minute
	proofSkew       = 90 * time.Second
	maxProofBytes   = 4096
	maxInstallID    = 128
	maxPublicKeyB64 = 128
)

var b64 = base64.RawURLEncoding

// KeyResolver resolves the public key bound to an install id.
type KeyResolver interface {
	PublicKey(ctx context.Context, installID string) ([]byte, bool, error)
}

// ReplayGuard atomically admits a request id once within the proof window.
type ReplayGuard interface{ UseOnce(string) bool }

// Service issues authenticated short-lived nonces and verifies signed requests.
type Service struct {
	keys   KeyResolver
	replay ReplayGuard
	secret [32]byte
	now    func() time.Time
}

// New builds a verifier. The nonce secret is intentionally process-local: after
// a restart clients simply fetch a fresh challenge, while no deploy secret exists.
func New(keys KeyResolver, replay ReplayGuard) *Service {
	s := &Service{keys: keys, replay: replay, now: time.Now}
	if _, err := rand.Read(s.secret[:]); err != nil {
		panic("deviceproof: crypto/rand failed: " + err.Error())
	}
	return s
}

// Challenge is a server-authenticated nonce and its expiry instant.
type Challenge struct {
	Nonce     string
	ExpiresAt time.Time
}

// IssueChallenge returns a nonce reusable for signed requests until expiry.
// Replay protection is per proof jti, not per nonce, avoiding a preflight per call.
func (s *Service) IssueChallenge() Challenge {
	now := s.now().UTC()
	raw := make([]byte, 8+16)
	binary.BigEndian.PutUint64(raw[:8], uint64(now.Unix()))
	if _, err := rand.Read(raw[8:]); err != nil {
		panic("deviceproof: crypto/rand failed: " + err.Error())
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write(raw)
	raw = append(raw, mac.Sum(nil)...)
	return Challenge{Nonce: b64.EncodeToString(raw), ExpiresAt: now.Add(nonceTTL)}
}

// Request is the exact request material covered by a proof.
type Request struct {
	Method string
	Target string
	Body   []byte
}

// Registration is a verified install-registration identity.
type Registration struct {
	PublicKey  []byte
	Thumbprint string
}

type payload struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid"`
	Issued  int64  `json:"iat"`
	ID      string `json:"jti"`
	Nonce   string `json:"nonce"`
	Method  string `json:"htm"`
	Target  string `json:"htu"`
	Body    string `json:"bh"`
}

// VerifyRegistration verifies a request against the supplied Ed25519 public key.
func (s *Service) VerifyRegistration(req Request, encodedKey, compact string) (Registration, *apierr.APIError) {
	if encodedKey == "" || len(encodedKey) > maxPublicKeyB64 {
		return Registration{}, apierr.ErrDeviceProofInvalid
	}
	key, err := b64.DecodeString(encodedKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Registration{}, apierr.ErrDeviceProofInvalid
	}
	thumb := Thumbprint(key)
	if ae := s.verify(req, thumb, ed25519.PublicKey(key), compact); ae != nil {
		return Registration{}, ae
	}
	return Registration{PublicKey: append([]byte(nil), key...), Thumbprint: thumb}, nil
}

// VerifyInstall verifies a request using the key registered for installID.
func (s *Service) VerifyInstall(ctx context.Context, req Request, installID, compact string) *apierr.APIError {
	if installID == "" || compact == "" {
		return apierr.ErrDeviceProofRequired
	}
	if len(installID) > maxInstallID {
		return apierr.ErrInvalidInstall
	}
	if s.keys == nil {
		return apierr.Internal()
	}
	key, found, err := s.keys.PublicKey(ctx, installID)
	if err != nil {
		return apierr.Internal()
	}
	if !found || len(key) != ed25519.PublicKeySize {
		return apierr.ErrInvalidInstall
	}
	return s.verify(req, installID, ed25519.PublicKey(key), compact)
}

func (s *Service) verify(req Request, expectedID string, key ed25519.PublicKey, compact string) *apierr.APIError {
	if len(compact) > maxProofBytes {
		return apierr.ErrDeviceProofInvalid
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return apierr.ErrDeviceProofInvalid
	}
	rawPayload, err := b64.DecodeString(parts[0])
	if err != nil {
		return apierr.ErrDeviceProofInvalid
	}
	var p payload
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return apierr.ErrDeviceProofInvalid
	}
	sig, err := b64.DecodeString(parts[1])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]), sig) {
		return apierr.ErrDeviceProofInvalid
	}
	bh := sha256.Sum256(req.Body)
	now := s.now().UTC()
	issued := time.Unix(p.Issued, 0)
	if p.Version != 1 || p.KeyID != expectedID || p.ID == "" || len(p.ID) > 128 ||
		p.Method != req.Method || p.Target != req.Target || p.Body != b64.EncodeToString(bh[:]) ||
		issued.Before(now.Add(-proofSkew)) || issued.After(now.Add(proofSkew)) {
		return apierr.ErrDeviceProofInvalid
	}
	if !s.validNonce(p.Nonce, now) {
		return apierr.ErrDeviceProofNonceInvalid
	}
	if s.replay == nil || !s.replay.UseOnce(expectedID+":"+p.ID) {
		return apierr.ErrDeviceProofReplayed
	}
	return nil
}

func (s *Service) validNonce(encoded string, now time.Time) bool {
	raw, err := b64.DecodeString(encoded)
	if err != nil || len(raw) != 8+16+sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write(raw[:24])
	if !hmac.Equal(raw[24:], mac.Sum(nil)) {
		return false
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(raw[:8])), 0)
	return !issued.After(now.Add(proofSkew)) && now.Before(issued.Add(nonceTTL))
}

// Thumbprint is the stable public identifier of an Ed25519 key.
func Thumbprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return b64.EncodeToString(sum[:])
}
