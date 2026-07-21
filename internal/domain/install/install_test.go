package install

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashFingerprintIsSHA256Hex(t *testing.T) {
	fingerprint := "fingerprint-abc123"
	want := sha256.Sum256([]byte(fingerprint))
	got := HashFingerprint(fingerprint)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("HashFingerprint = %q, want hex sha256", got)
	}
	if len(got) != 64 {
		t.Fatalf("hex sha256 must be 64 chars, got %d", len(got))
	}
	if got == fingerprint {
		t.Fatal("hash equals plaintext")
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
