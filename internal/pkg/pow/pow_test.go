package pow

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"
)

var testSecret = []byte("test-pow-secret-do-not-use-in-prod")

// stubNonce is a deterministic NonceUser: first UseOnce(challenge) -> true, any
// later call for the same challenge -> false. No clock, no TTL — the spec puts
// nonce-once LAST so tests must control exactly when it fires.
type stubNonce struct{ seen map[string]bool }

func newStubNonce() *stubNonce { return &stubNonce{seen: map[string]bool{}} }

func (s *stubNonce) UseOnce(c string) bool {
	if s.seen[c] {
		return false
	}
	s.seen[c] = true
	return true
}

// solve brute-forces a nonce meeting difficulty for the given challenge. Kept low
// (diff<=12) so tests stay fast and deterministic.
func solve(t *testing.T, challenge string, difficulty int) string {
	t.Helper()
	for i := 0; i < 1<<24; i++ {
		nonce := strconv.Itoa(i)
		h := sha256.Sum256([]byte(challenge + "." + nonce))
		if leadingZeroBits(h[:]) >= difficulty {
			return nonce
		}
	}
	t.Fatalf("no nonce found for difficulty %d", difficulty)
	return ""
}

func TestMintVerifyRoundtrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const diff = 4
	ch, err := Mint(testSecret, diff, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	nonce := solve(t, ch, diff)
	if err := Verify(testSecret, ch, nonce, diff, now, newStubNonce()); err != nil {
		t.Fatalf("Verify of fresh valid solve: %v", err)
	}
}

func TestForgedHMACRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ch, _ := Mint(testSecret, 1, now)
	// Verify under a DIFFERENT secret: tag will not authenticate.
	err := Verify([]byte("other-secret"), ch, "0", 1, now, newStubNonce())
	if !errors.Is(err, ErrForged) {
		t.Fatalf("want ErrForged, got %v", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	issued := time.Unix(1_700_000_000, 0)
	ch, _ := Mint(testSecret, 1, issued)
	// Past expiry.
	if err := Verify(testSecret, ch, "0", 1, issued.Add(ChallengeTTL+time.Second), newStubNonce()); !errors.Is(err, ErrExpired) {
		t.Fatalf("past: want ErrExpired, got %v", err)
	}
	// Future-dated beyond slack (clock skew / forgery class).
	if err := Verify(testSecret, ch, "0", 1, issued.Add(-(ChallengeTTL + time.Second)), newStubNonce()); !errors.Is(err, ErrExpired) {
		t.Fatalf("future: want ErrExpired, got %v", err)
	}
}

func TestInsufficientDifficultyRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Mint at diff 1, but VERIFY demands a high difficulty an arbitrary nonce
	// won't meet.
	ch, _ := Mint(testSecret, 1, now)
	err := Verify(testSecret, ch, "this-nonce-is-not-a-solution", 24, now, newStubNonce())
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
}

func TestReplayRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const diff = 4
	ch, _ := Mint(testSecret, diff, now)
	nonce := solve(t, ch, diff)
	nc := newStubNonce()
	if err := Verify(testSecret, ch, nonce, diff, now, nc); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	// Second submission of the same challenge is a replay.
	if err := Verify(testSecret, ch, nonce, diff, now, nc); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: want ErrReplayed, got %v", err)
	}
}

// TestVerifyOrderForgedExpiredFailsOnHMAC pins the verify ORDER: a challenge that
// is BOTH forged AND expired must fail on HMAC (ErrForged) first — the cheapest
// forgery defense runs before the freshness check.
func TestVerifyOrderForgedExpiredFailsOnHMAC(t *testing.T) {
	issued := time.Unix(1_700_000_000, 0)
	ch, _ := Mint(testSecret, 1, issued)
	verifyAt := issued.Add(ChallengeTTL + time.Hour) // also expired
	err := Verify([]byte("wrong-secret"), ch, "0", 1, verifyAt, newStubNonce())
	if !errors.Is(err, ErrForged) {
		t.Fatalf("forged+expired must fail on HMAC first; want ErrForged, got %v", err)
	}
}

// TestNonceNotBurnedOnEarlierFailure asserts the single-use mark is LAST: a solve
// that fails difficulty must NOT consume the challenge, so a later correct solve
// of the SAME challenge still succeeds.
func TestNonceNotBurnedOnEarlierFailure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const diff = 4
	ch, _ := Mint(testSecret, diff, now)
	nc := newStubNonce()
	// Fails on difficulty (bad nonce) -> must not touch nonce cache.
	if err := Verify(testSecret, ch, "bad-nonce", 24, now, nc); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
	// Correct solve of the same challenge still admits.
	nonce := solve(t, ch, diff)
	if err := Verify(testSecret, ch, nonce, diff, now, nc); err != nil {
		t.Fatalf("challenge wrongly burned by earlier failure: %v", err)
	}
}

func TestMalformedRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []string{
		"",                 // empty
		"!!!not base64!!!", // bad base64url
		base64.RawURLEncoding.EncodeToString([]byte("short")), // wrong length
	}
	for _, ch := range cases {
		if err := Verify(testSecret, ch, "0", 1, now, newStubNonce()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("challenge %q: want ErrMalformed, got %v", ch, err)
		}
	}
}

func TestBadDifficultyRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, d := range []int{0, -1, MaxDifficulty + 1, 100} {
		if _, err := Mint(testSecret, d, now); !errors.Is(err, ErrBadDifficulty) {
			t.Fatalf("Mint diff=%d: want ErrBadDifficulty, got %v", d, err)
		}
		if err := Verify(testSecret, "x", "0", d, now, newStubNonce()); !errors.Is(err, ErrBadDifficulty) {
			t.Fatalf("Verify diff=%d: want ErrBadDifficulty, got %v", d, err)
		}
	}
}

// TestConstantTimeCompareUsed is a behavioral proxy for the constant-time tag
// compare: a tag differing only in its LAST byte must still be rejected (a
// byte-wise early-exit compare on a matching prefix would also reject, so this is
// a sanity guard that the truncated-tag compare path is exercised, paired with
// the source using subtle.ConstantTimeCompare).
func TestConstantTimeCompareUsed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ch, _ := Mint(testSecret, 1, now)
	raw, _ := base64.RawURLEncoding.DecodeString(ch)
	raw[len(raw)-1] ^= 0x01 // flip last tag bit
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if err := Verify(testSecret, tampered, "0", 1, now, newStubNonce()); !errors.Is(err, ErrForged) {
		t.Fatalf("tampered tag: want ErrForged, got %v", err)
	}
}

func TestParseHeader(t *testing.T) {
	tests := []struct {
		in        string
		wantCh    string
		wantNonce string
		wantOK    bool
	}{
		{"abc.123", "abc", "123", true},
		{"  abc.123  ", "abc", "123", true}, // trimmed
		{"abc.12.3", "abc", "12.3", true},   // split on FIRST dot; nonce keeps rest
		{"", "", "", false},
		{".123", "", "", false}, // empty challenge
		{"abc.", "", "", false}, // empty nonce
		{"abc", "", "", false},  // no separator
	}
	for _, tc := range tests {
		ch, nonce, ok := ParseHeader(tc.in)
		if ok != tc.wantOK || ch != tc.wantCh || nonce != tc.wantNonce {
			t.Fatalf("ParseHeader(%q)=(%q,%q,%v), want (%q,%q,%v)",
				tc.in, ch, nonce, ok, tc.wantCh, tc.wantNonce, tc.wantOK)
		}
	}
}

// TestLeadingZeroBits spot-checks the bit counter at byte boundaries.
func TestLeadingZeroBits(t *testing.T) {
	tests := []struct {
		b    []byte
		want int
	}{
		{[]byte{0xFF}, 0},
		{[]byte{0x80}, 0},
		{[]byte{0x40}, 1},
		{[]byte{0x01}, 7},
		{[]byte{0x00}, 8},
		{[]byte{0x00, 0x80}, 8},
		{[]byte{0x00, 0x40}, 9},
		{[]byte{0x00, 0x00}, 16},
	}
	for _, tc := range tests {
		if got := leadingZeroBits(tc.b); got != tc.want {
			t.Fatalf("leadingZeroBits(% x)=%d, want %d", tc.b, got, tc.want)
		}
	}
}

// FuzzVerify asserts Verify never panics on arbitrary inputs and always returns a
// non-nil error for garbage (a fuzz-found challenge can never authenticate without
// the secret).
func FuzzVerify(f *testing.F) {
	f.Add("abc.def", "nonce", 1, int64(1_700_000_000))
	f.Add("", "", 0, int64(0))
	f.Add("!!!", "x", 33, int64(-1))
	nc := newStubNonce()
	f.Fuzz(func(t *testing.T, challenge, nonce string, difficulty int, unixSec int64) {
		// Just must not panic. A nil error would imply a fuzzed challenge
		// authenticated under testSecret, which is cryptographically impossible.
		err := Verify(testSecret, challenge, nonce, difficulty, time.Unix(unixSec, 0), nc)
		if err == nil {
			t.Fatalf("fuzz produced a valid solve: ch=%q nonce=%q diff=%d", challenge, nonce, difficulty)
		}
	})
}
