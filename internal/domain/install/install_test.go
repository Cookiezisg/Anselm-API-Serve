package install

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashTokenIsSHA256Hex(t *testing.T) {
	tok := "gwk_abc123"
	want := sha256.Sum256([]byte(tok))
	got := HashToken(tok)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("HashToken = %q, want hex sha256", got)
	}
	if len(got) != 64 {
		t.Fatalf("hex sha256 must be 64 chars, got %d", len(got))
	}
	if got == tok {
		t.Fatal("hash equals plaintext")
	}
}

func TestHashFingerprintMatchesHashToken(t *testing.T) {
	// Both are plain SHA-256 hex; a fingerprint and a token with the same bytes
	// hash identically (no salt) — the irreversible-store contract.
	s := "same-bytes"
	if HashFingerprint(s) != HashToken(s) {
		t.Fatal("fingerprint and token hashing must both be plain SHA-256 hex")
	}
}

func TestNewRequestTrimsAndTruncates(t *testing.T) {
	longFP := strings.Repeat("f", MaxFingerprintLen+50)
	longCl := strings.Repeat("c", MaxClientLen+50)
	r := NewRequest("  "+longFP+"  ", "  "+longCl+"  ")
	if len(r.Fingerprint) != MaxFingerprintLen {
		t.Fatalf("fingerprint len = %d, want %d", len(r.Fingerprint), MaxFingerprintLen)
	}
	if len(r.Client) != MaxClientLen {
		t.Fatalf("client len = %d, want %d", len(r.Client), MaxClientLen)
	}
}

func TestNewRequestEmpty(t *testing.T) {
	r := NewRequest("   ", "")
	if r.Fingerprint != "" || r.Client != "" {
		t.Fatalf("blank inputs must normalize to empty: %+v", r)
	}
}

func TestStatusValues(t *testing.T) {
	if StatusActive != "active" || StatusBanned != "banned" {
		t.Fatalf("status wire values drifted: %q %q", StatusActive, StatusBanned)
	}
}
