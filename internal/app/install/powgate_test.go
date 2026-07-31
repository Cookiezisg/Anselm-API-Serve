package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
	"github.com/sunweilin/anselm/gateway/internal/pkg/pow"
)

func powConfig(t *testing.T, mode string, secret []byte, difficulty int) *config.Config {
	t.Helper()
	loc, _ := time.LoadLocation("UTC")
	return &config.Config{
		Location:             loc,
		InstallPowMode:       mode,
		InstallPowSecret:     secret,
		InstallPowDifficulty: difficulty,
	}
}

// solve brute-forces a nonce whose SHA256(challenge"."nonce) meets the difficulty.
func solve(t *testing.T, challenge string, difficulty int) string {
	t.Helper()
	for i := 0; i < 50_000_000; i++ {
		nonce := itoaPow(i)
		if leadingZeros(challenge+"."+nonce) >= difficulty {
			return nonce
		}
	}
	t.Fatalf("no nonce found for difficulty %d", difficulty)
	return ""
}

// TestPoWDormantAdmitsNoMetric: off/""/unknown modes admit with no header read and
// no metric (byte-for-byte unchanged dormant path).
func TestPoWDormantAdmitsNoMetric(t *testing.T) {
	for _, mode := range []string{config.PowModeOff, "", "weird"} {
		mx := newLabelCounter()
		s := New(fakeCfg{powConfig(t, mode, nil, 0)}, &fakeStore{}, fakeNonces{}, nil, mx)
		if ae := s.PoWGate(context.Background(), "anything.here", "ipk"); ae != nil {
			t.Fatalf("mode %q: dormant must admit, got %v", mode, ae)
		}
		if mx.total() != 0 {
			t.Fatalf("mode %q: dormant must emit no metric, got %d", mode, mx.total())
		}
	}
}

// TestPoWEnforceMissing: enforce + no X-PoW → INSTALL_POW_REQUIRED, exactly one
// "missing" label.
func TestPoWEnforceMissing(t *testing.T) {
	mx := newLabelCounter()
	s := New(fakeCfg{powConfig(t, config.PowModeEnforce, []byte("secret"), 8)}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, mx)
	ae := s.PoWGate(context.Background(), "", "ipk")
	if ae == nil || ae.Code != "INSTALL_POW_REQUIRED" {
		t.Fatalf("enforce missing: %v want INSTALL_POW_REQUIRED", ae)
	}
	if mx.counts["missing"] != 1 || mx.total() != 1 {
		t.Fatalf("labels = %v want exactly {missing:1}", mx.counts)
	}
}

// TestPoWEnforceVerified: a valid solve admits with exactly one "verified" label.
func TestPoWEnforceVerified(t *testing.T) {
	secret := []byte("topsecret")
	difficulty := 8
	cfg := powConfig(t, config.PowModeEnforce, secret, difficulty)
	now := time.Now()
	mx := newLabelCounter()
	s := New(fakeCfg{cfg}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, mx)
	s.SetClock(func() time.Time { return now })

	ch, err := pow.Mint(secret, difficulty, now)
	if err != nil {
		t.Fatal(err)
	}
	nonce := solve(t, ch, difficulty)
	if ae := s.PoWGate(context.Background(), ch+"."+nonce, "ipk"); ae != nil {
		t.Fatalf("valid solve must admit, got %v", ae)
	}
	if mx.counts["verified"] != 1 || mx.total() != 1 {
		t.Fatalf("labels = %v want exactly {verified:1}", mx.counts)
	}
}

// TestPoWEnforceInvalid: a forged/garbage X-PoW → INSTALL_POW_INVALID, one "failed".
func TestPoWEnforceInvalid(t *testing.T) {
	mx := newLabelCounter()
	s := New(fakeCfg{powConfig(t, config.PowModeEnforce, []byte("secret"), 8)}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, mx)
	ae := s.PoWGate(context.Background(), "garbage.nonce", "ipk")
	if ae == nil || ae.Code != "INSTALL_POW_INVALID" {
		t.Fatalf("enforce invalid: %v want INSTALL_POW_INVALID", ae)
	}
	if mx.counts["failed"] != 1 || mx.total() != 1 {
		t.Fatalf("labels = %v want exactly {failed:1}", mx.counts)
	}
}

// TestPoWShadowDisjointLabels: shadow always admits, and every shadow outcome
// uses a DISJOINT label — never double-counting "missing"/"failed" alongside a
// shadow label. Exactly one label per request.
func TestPoWShadowDisjointLabels(t *testing.T) {
	secret := []byte("secret")
	cfg := powConfig(t, config.PowModeShadow, secret, 8)

	// shadow + missing → admit, exactly one "shadow_missing" (NOT "missing").
	mx := newLabelCounter()
	s := New(fakeCfg{cfg}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, mx)
	if ae := s.PoWGate(context.Background(), "", "ipk"); ae != nil {
		t.Fatalf("shadow missing must admit, got %v", ae)
	}
	if mx.total() != 1 || mx.counts["shadow_missing"] != 1 {
		t.Fatalf("shadow missing labels = %v want exactly {shadow_missing:1}", mx.counts)
	}
	if mx.counts["missing"] != 0 {
		t.Fatal(": shadow must NOT also emit the enforce 'missing' label")
	}

	// shadow + invalid → admit + WARN, exactly one "shadow_invalid" (NOT "failed").
	var buf bytes.Buffer
	logx.Init(&buf, "debug")
	mx2 := newLabelCounter()
	s2 := New(fakeCfg{cfg}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, mx2)
	if ae := s2.PoWGate(context.Background(), "garbage.nonce", "ipk-9"); ae != nil {
		t.Fatalf("shadow invalid must admit, got %v", ae)
	}
	if mx2.total() != 1 || mx2.counts["shadow_invalid"] != 1 {
		t.Fatalf("shadow invalid labels = %v want exactly {shadow_invalid:1}", mx2.counts)
	}
	if mx2.counts["failed"] != 0 {
		t.Fatal(": shadow must NOT also emit the enforce 'failed' label")
	}
	assertShadowWarn(t, &buf)
}

// TestPoWNonceOnce: a valid solve cannot be replayed within the TTL (nonce-once).
func TestPoWNonceOnce(t *testing.T) {
	secret := []byte("replay-secret")
	difficulty := 8
	cfg := powConfig(t, config.PowModeEnforce, secret, difficulty)
	now := time.Now()
	nc := noncecache.New(pow.ChallengeTTL)
	s := New(fakeCfg{cfg}, &fakeStore{}, nc, nil, newLabelCounter())
	s.SetClock(func() time.Time { return now })

	ch, _ := pow.Mint(secret, difficulty, now)
	nonce := solve(t, ch, difficulty)
	hdr := ch + "." + nonce
	if ae := s.PoWGate(context.Background(), hdr, "ipk"); ae != nil {
		t.Fatalf("first solve must admit, got %v", ae)
	}
	if ae := s.PoWGate(context.Background(), hdr, "ipk"); ae == nil || ae.Code != "INSTALL_POW_INVALID" {
		t.Fatalf("replay must reject INSTALL_POW_INVALID, got %v", ae)
	}
}

// TestMintChallengeDormantStillAnswers: dormant (off) still mints a probe
// challenge with required=false and a clamped difficulty (never ErrBadDifficulty).
func TestMintChallengeDormant(t *testing.T) {
	s := New(fakeCfg{powConfig(t, config.PowModeOff, nil, 0)}, &fakeStore{}, fakeNonces{}, nil, nil)
	ch, diff, required, err := s.MintChallenge(context.Background())
	if err != nil {
		t.Fatalf("dormant mint must not error: %v", err)
	}
	if ch == "" || required {
		t.Fatalf("dormant: ch=%q required=%v want non-empty + false", ch, required)
	}
	if diff < pow.MinDifficulty {
		t.Fatalf("difficulty %d below min %d", diff, pow.MinDifficulty)
	}
}

// TestMintChallengeEnforceRequired: enforce mints with required=true and the
// challenge verifies end-to-end through the gate.
func TestMintChallengeEnforceRoundTrip(t *testing.T) {
	secret := []byte("rt-secret")
	difficulty := 8
	cfg := powConfig(t, config.PowModeEnforce, secret, difficulty)
	now := time.Now()
	s := New(fakeCfg{cfg}, &fakeStore{}, noncecache.New(pow.ChallengeTTL), nil, newLabelCounter())
	s.SetClock(func() time.Time { return now })

	ch, diff, required, err := s.MintChallenge(context.Background())
	if err != nil || !required {
		t.Fatalf("enforce mint: required=%v err=%v", required, err)
	}
	nonce := solve(t, ch, diff)
	if ae := s.PoWGate(context.Background(), ch+"."+nonce, "ipk"); ae != nil {
		t.Fatalf("minted challenge must verify through the gate, got %v", ae)
	}
}

func assertShadowWarn(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	for _, ln := range strings.Split(buf.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) != nil {
			continue
		}
		if m["event"] == "install_pow_shadow" {
			if m["level"] != "WARN" {
				t.Fatalf("shadow event must be WARN, got %v", m["level"])
			}
			if m["outcome"] != "shadow_pass_invalid" {
				t.Fatalf("shadow outcome = %v want shadow_pass_invalid", m["outcome"])
			}
			return
		}
	}
	t.Fatalf("no install_pow_shadow WARN; logs=%s", buf.String())
}

// --- small local helpers for difficulty solving in tests ---

func leadingZeros(s string) int {
	h := sha256.Sum256([]byte(s))
	n := 0
	for _, b := range h {
		if b == 0 {
			n += 8
			continue
		}
		for mask := byte(0x80); mask != 0; mask >>= 1 {
			if b&mask != 0 {
				return n
			}
			n++
		}
		return n
	}
	return n
}

func itoaPow(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
