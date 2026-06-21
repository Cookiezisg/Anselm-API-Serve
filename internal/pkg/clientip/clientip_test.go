package clientip

import (
	"strings"
	"testing"
)

// TestClientIPTrustBoundary pins GW-INV-16: XFF is honored only for a loopback
// direct peer (the Caddy hop), and only its rightmost segment; a non-loopback peer
// ignores XFF entirely.
func TestClientIPTrustBoundary(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"loopback trusts rightmost xff", "127.0.0.1:54321", "1.1.1.1, 9.9.9.9", "9.9.9.9"},
		{"loopback single xff", "127.0.0.1:54321", "9.9.9.9", "9.9.9.9"},
		{"ipv6 loopback trusts rightmost", "[::1]:5000", "9.9.9.9, 8.8.8.8", "8.8.8.8"},
		{"loopback empty xff falls back", "127.0.0.1:54321", "", "127.0.0.1:54321"},
		{"loopback blank rightmost falls back", "127.0.0.1:54321", "1.1.1.1, ", "127.0.0.1:54321"},
		{"non-loopback ignores xff", "203.0.113.7:60000", "1.1.1.1, 9.9.9.9", "203.0.113.7:60000"},
		{"non-loopback ignores single xff", "203.0.113.7:60000", "9.9.9.9", "203.0.113.7:60000"},
		{"unparseable peer ignores xff", "not-an-addr", "9.9.9.9", "not-an-addr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(tc.remoteAddr, tc.xff); got != tc.want {
				t.Fatalf("ClientIP(%q,%q)=%q want %q", tc.remoteAddr, tc.xff, got, tc.want)
			}
		})
	}
}

// TestKeyCollapsesAndHashes pins the cardinality collapse (IPv6 → /64, IPv4 full)
// and that the output is a stable hex SHA-256 (no plaintext address surfaces).
func TestKeyCollapsesAndHashes(t *testing.T) {
	// IPv6 addresses sharing a /64 collapse to the same key; differing /64 differ.
	a := Key("[2001:db8:abcd:1234::1]:443")
	b := Key("[2001:db8:abcd:1234:ffff:ffff:ffff:ffff]:80")
	c := Key("[2001:db8:abcd:9999::1]:443")
	if a != b {
		t.Fatalf("same /64 should share key: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("different /64 should differ: both %q", a)
	}

	// IPv4 keys on the full address; port is stripped.
	if Key("1.2.3.4:1000") != Key("1.2.3.4:2000") {
		t.Fatal("IPv4 key must ignore port")
	}
	if Key("1.2.3.4") == Key("1.2.3.5") {
		t.Fatal("distinct IPv4 must differ")
	}

	// Output is hex SHA-256: 64 lowercase hex chars, never the plaintext.
	k := Key("1.2.3.4")
	if len(k) != 64 {
		t.Fatalf("key len=%d want 64", len(k))
	}
	if strings.ContainsAny(k, ".:") || strings.Contains(k, "1.2.3.4") {
		t.Fatalf("key leaks plaintext: %q", k)
	}
}

// FuzzIPKey hardens the per-IP Sybil defense's address parsing. ClientIP + Key face
// attacker-controlled RemoteAddr and X-Forwarded-For (the gateway is public-facing
// behind Caddy). Invariants for ANY input: never panic, and — critically — XFF is
// honored ONLY when the direct peer is loopback. A non-loopback peer's XFF must be
// ignored entirely, else an attacker rotates XFF to dodge the per-IP limit.
func FuzzIPKey(f *testing.F) {
	f.Add("127.0.0.1:54321", "1.1.1.1, 9.9.9.9")
	f.Add("203.0.113.7:60000", "1.1.1.1, 9.9.9.9")
	f.Add("[::1]:5000", "9.9.9.9, 8.8.8.8")
	f.Add("[2001:db8:abcd:1234::1]:443", "")
	f.Add("not-an-addr", "garbage,,,")
	f.Add("", "")
	f.Add("127.0.0.1:1", strings.Repeat("1.2.3.4,", 1000))
	f.Add("\x00\x01:bad", "\x00")
	// Hostile seed from the legacy corpus: a spoofed left XFF the gateway must ignore.
	f.Add("203.0.113.7:60000", "1.1.1.1, 2.2.2.2, 9.9.9.9")

	f.Fuzz(func(t *testing.T, remoteAddr, xff string) {
		// Must not panic for any input.
		peer := ClientIP(remoteAddr, xff)
		_ = Key(peer)
		_ = Key(remoteAddr)

		// 🔴 Trust boundary: a NON-loopback peer must NEVER let XFF change the
		// derived peer — the result must be RemoteAddr verbatim, regardless of XFF.
		if !isLoopback(remoteAddr) && peer != remoteAddr {
			t.Fatalf("non-loopback peer trusted XFF: remoteAddr=%q xff=%q → peer=%q",
				remoteAddr, xff, peer)
		}
	})
}
