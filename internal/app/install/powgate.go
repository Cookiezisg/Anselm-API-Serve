package install

import (
	"context"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/pow"
)

// PoW outcome labels. Each /v1/install request increments EXACTLY ONE of these so
// `sum by(result)` is exact (B12 fix: shadow gets disjoint terminal labels, never
// double-counting a `missing`/`failed` outcome alongside `shadow_pass`).
const (
	powResultMissing  = "missing"        // enforce: no X-PoW presented.
	powResultVerified = "verified"       // a valid solve (enforce or shadow).
	powResultFailed   = "failed"         // enforce: present but invalid.
	powShadowMissing  = "shadow_missing" // shadow: no X-PoW (admit, observe gap).
	powShadowInvalid  = "shadow_invalid" // shadow: present but invalid (admit + WARN).
)

// PoWGate is the M2 PoW enforcement point for POST /v1/install. It returns nil to
// ADMIT and a 403 *apierr.APIError to REJECT. The whole body short-circuits when
// the mode is not exactly shadow/enforce, so a dormant config (off / "" /
// any-other-value) reads no header, emits no metric, and is byte-for-byte
// unchanged. powHeader is the raw X-PoW header value (the transport reads it; the
// app stays HTTP-free).
//
// - off/dormant: admit immediately.
// - enforce: missing → INSTALL_POW_REQUIRED; present-invalid → INSTALL_POW_INVALID.
// - shadow: verify if present; on miss/failure admit (disjoint shadow label) + WARN on a bad solve.
func (s *Service) PoWGate(ctx context.Context, powHeader, ipKey string) *apierr.APIError {
	cfg := s.cfg.Load()
	mode := cfg.InstallPowMode
	// Dormant unless EXPLICITLY shadow/enforce: off, "" (zero-value Config), and any
	// other value all admit with no header read and no metric.
	if mode != config.PowModeShadow && mode != config.PowModeEnforce {
		return nil
	}

	hdr := strings.TrimSpace(powHeader)
	if hdr == "" {
		if mode == config.PowModeEnforce {
			s.powMetric(powResultMissing)
			return apierr.ErrInstallPoWRequired
		}
		// shadow: observe the gap, admit (disjoint label — exactly one per request).
		s.powMetric(powShadowMissing)
		return nil
	}

	if s.verifyPoW(cfg, hdr) {
		s.powMetric(powResultVerified)
		return nil
	}

	// Present but invalid.
	if mode == config.PowModeEnforce {
		s.powMetric(powResultFailed)
		return apierr.ErrInstallPoWInvalid
	}
	// shadow: a bad PoW still passes, but WARN so ops sees broken clients during
	// rollout. Zero-body: only ip_key + outcome, never the challenge/nonce content.
	s.powMetric(powShadowInvalid)
	logx.From(ctx).Warn("security_event", "event", "install_pow_shadow",
		"endpoint", "/v1/install", "ip_key", ipKey, "outcome", "shadow_pass_invalid")
	return nil
}

// verifyPoW validates an X-PoW header against the live config via pkg/pow: parse →
// HMAC authenticity → freshness → difficulty → nonce-once (consumed LAST, only on
// an otherwise-valid solve). Returns true only when ALL pass.
func (s *Service) verifyPoW(cfg *config.Config, hdr string) bool {
	challenge, nonce, ok := pow.ParseHeader(hdr)
	if !ok {
		return false
	}
	err := pow.Verify(cfg.InstallPowSecret, challenge, nonce, cfg.InstallPowDifficulty, s.now(), s.nonces)
	return err == nil
}

// MintChallenge serves the data for GET /v1/install/challenge: a fresh stateless
// PoW challenge plus the difficulty and whether a client MUST attach an X-PoW
// (required only in enforce mode). When dormant it still answers (required=false)
// so the surface exists for a client to probe, but /install never verifies. The
// effective secret is the env secret when present, else a fixed dormant
// placeholder (never the signing key on an active verification, which config
// fail-fast guarantees has a real secret).
func (s *Service) MintChallenge(ctx context.Context) (challenge string, difficulty int, required bool, err error) {
	cfg := s.cfg.Load()
	ch, err := pow.Mint(s.powSecret(cfg), s.powDifficulty(cfg), s.now())
	if err != nil {
		return "", 0, false, err
	}
	return ch, s.powDifficulty(cfg), cfg.InstallPowMode == config.PowModeEnforce, nil
}

// powDifficulty clamps the configured difficulty into pkg/pow's accepted range so
// a dormant zero-value Config (difficulty 0) can still mint a probe challenge
// instead of erroring on ErrBadDifficulty.
func (s *Service) powDifficulty(cfg *config.Config) int {
	d := cfg.InstallPowDifficulty
	if d < pow.MinDifficulty {
		return pow.MinDifficulty
	}
	if d > pow.MaxDifficulty {
		return pow.MaxDifficulty
	}
	return d
}

// powDormantKey is the placeholder HMAC key used ONLY when minting a probe
// challenge in a dormant mode (no real secret, no verification path). An active
// shadow/enforce mode always carries a real env secret (config fail-fast), so this
// is never the signing key on an enforced verification. Never logged.
var powDormantKey = []byte("anselm-pow-dormant")

// powSecret returns the effective HMAC signing secret: the env secret when set,
// else the dormant placeholder.
func (s *Service) powSecret(cfg *config.Config) []byte {
	if len(cfg.InstallPowSecret) > 0 {
		return cfg.InstallPowSecret
	}
	return powDormantKey
}

// powMetric increments gateway_install_pow_total{result} when wired.
func (s *Service) powMetric(result string) {
	if s.mxPoW != nil {
		s.mxPoW.Inc(result)
	}
}
