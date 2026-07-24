package bootstrap

// loops.go holds the two infra collaborators bootstrap must construct itself —
// the M2 token-anomaly throttle (no infra slice built one) and the cached
// upstream readiness prober — plus the background loop bodies the lifecycle runs.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/ratelimit"
)

// --- M2 per-token anomaly auto-throttle (chat.Throttle) -----------------------

// tokenThrottle implements chat.Throttle: it tracks each install's sliding
// per-minute request count and, when it crosses TOKEN_ANOMALY_RPM, tightens the
// SAME rate-limiter bucket (SetKeyLimit) for a cooldown so the abuse bites on the
// next Allow — it NEVER rejects the current request (spec-behaviors §1.3). It is
// dormant (Observe returns false, zero work) when TOKEN_ANOMALY_RPM=0. Bounded by
// opportunistic prune so distinct-install churn cannot grow the map unbounded.
type tokenThrottle struct {
	cfg *configprovider.Provider
	rl  *ratelimit.RateLimiter
	mx  *metrics.Metrics

	mu     sync.Mutex
	counts map[string]*rpmWindow
}

// rpmWindow is one install's current-minute counter + the throttle-until stamp.
type rpmWindow struct {
	minute int64 // unix minute the count belongs to
	n      int
	until  time.Time // active throttle expiry (zero = not throttled)
}

func newTokenThrottle(cfg *configprovider.Provider, rl *ratelimit.RateLimiter, mx *metrics.Metrics) *tokenThrottle {
	return &tokenThrottle{cfg: cfg, rl: rl, mx: mx, counts: make(map[string]*rpmWindow)}
}

// Observe meters one request for installID and, on an anomaly, tightens its
// bucket. Returns true when an anomaly was (re)triggered so the use case sets the
// audit flag. Dormant when the trigger RPM is 0.
func (t *tokenThrottle) Observe(installID string) bool {
	cfg := t.cfg.Load()
	rpm := cfg.TokenAnomalyRPM
	if rpm <= 0 {
		return false // whole auto-throttle path off — zero work.
	}

	now := time.Now()
	minute := now.Unix() / 60

	t.mu.Lock()
	defer t.mu.Unlock()

	w := t.counts[installID]
	if w == nil {
		w = &rpmWindow{minute: minute}
		t.counts[installID] = w
		t.pruneLocked(now)
	}
	if w.minute != minute {
		w.minute, w.n = minute, 0
	}
	w.n++

	if w.n < rpm {
		return false
	}
	// Anomaly: tighten the SAME bucket to RATE_PER_MIN/FACTOR for the cooldown.
	factor := cfg.TokenThrottleFactor
	if factor < 1 {
		factor = 1
	}
	tightened := cfg.RatePerMin / factor
	if tightened < 1 {
		tightened = 1
	}
	until := now.Add(time.Duration(cfg.TokenThrottleCooldownSec) * time.Second)
	w.until = until
	t.rl.SetKeyLimit(installID, tightened, until)
	t.mx.TokenThrottled.Inc()
	return true
}

// TokensThrottledNow counts installs currently under an active throttle, sweeping
// expired stamps (the count auto-decays). Feeds the dashboard gauge + OBS-4 alert.
func (t *tokenThrottle) TokensThrottledNow() int {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, w := range t.counts {
		if !w.until.IsZero() && now.Before(w.until) {
			n++
		}
	}
	t.mx.TokensThrottledNow.Set(float64(n))
	return n
}

// pruneLocked drops idle windows so distinct-install churn can't grow the map.
func (t *tokenThrottle) pruneLocked(now time.Time) {
	if len(t.counts) < 8192 {
		return
	}
	cutoff := now.Unix()/60 - 5
	for k, w := range t.counts {
		if w.minute < cutoff && (w.until.IsZero() || now.After(w.until)) {
			delete(t.counts, k)
		}
	}
}

var _ appchat.Throttle = (*tokenThrottle)(nil)

// --- cached upstream readiness prober (health.UpstreamProbe) ------------------

// upstreamProber holds the last time EVERY configured provider authenticated and
// advertised its pinned model. /readyz reads this cached aggregate — never a live
// provider call per request (which could stall behind a struggling upstream).
// Qwen is deliberately absent from targets when its optional key is not
// configured, so a text-only deployment remains ready while media fails with the
// explicit MULTIMODAL_UNAVAILABLE contract.
type upstreamProber struct {
	targets []upstreamProbeTarget
	client  *http.Client
	lastOK  atomic.Int64 // unix-nanos of the last all-target success (0 = never)
}

type upstreamProbeTarget struct {
	modelsURL string
	modelID   string
	apiKeys   []string
}

func newUpstreamProber(cfg config.Config) *upstreamProber {
	targets := []upstreamProbeTarget{{
		modelsURL: strings.TrimRight(cfg.DeepSeekBaseURL, "/") + "/models",
		modelID:   cfg.TextUpstreamModel,
		apiKeys:   append([]string(nil), cfg.DeepSeekAPIKeys...),
	}}
	if len(cfg.QwenAPIKeys) > 0 {
		targets = append(targets, upstreamProbeTarget{
			modelsURL: strings.TrimRight(cfg.QwenBaseURL, "/") + "/models",
			modelID:   cfg.MultimodalUpstreamModel,
			apiKeys:   append([]string(nil), cfg.QwenAPIKeys...),
		})
	}
	return &upstreamProber{
		targets: targets,
		client: &http.Client{
			// Auth is injected below. Never follow a provider-controlled Location:
			// redirecting would attach neither an intentional endpoint nor a safe
			// deployment-readiness meaning, and could disclose credentials.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// LastOK reports the last success + its age (health.UpstreamProbe).
func (p *upstreamProber) LastOK() (bool, time.Duration) {
	ns := p.lastOK.Load()
	if ns == 0 {
		return false, 0
	}
	return true, time.Since(time.Unix(0, ns))
}

// probe calls each provider's no-completion /models endpoint with its configured
// credentials. Success requires a 2xx response whose bounded JSON list contains
// the exact pinned model. This catches bad/expired keys, wrong compatibility base
// URLs, and unavailable model IDs without spending inference tokens. Multiple
// keys are alternatives within one provider; every configured provider must pass.
func (p *upstreamProber) probe(ctx context.Context) {
	if p == nil || p.client == nil || len(p.targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, target := range p.targets {
		if !p.probeTarget(ctx, target) {
			return
		}
	}
	p.lastOK.Store(time.Now().UnixNano())
}

func (p *upstreamProber) probeTarget(ctx context.Context, target upstreamProbeTarget) bool {
	for _, rawKey := range target.apiKeys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.modelsURL, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Accept", "application/json")
		// Match the completion client's key hygiene: the caller-owned request never
		// carries the credential. A one-call transport clone injects it at the last
		// possible moment, and redirects remain disabled on the copied client.
		client := *p.client
		base := client.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		client.Transport = probeBearerTransport{base: base, apiKey: key}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		ok := responseAdvertisesModel(resp, target.modelID)
		_ = resp.Body.Close()
		if ok {
			return true
		}
	}
	return false
}

// probeBearerTransport keeps readiness credentials out of the request object
// owned by probeTarget. This mirrors infra/upstream's redacting transport while
// keeping the health prober independent from completion breaker/key state.
type probeBearerTransport struct {
	base   http.RoundTripper
	apiKey string
}

func (t probeBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return t.base.RoundTrip(clone)
}

func responseAdvertisesModel(resp *http.Response, modelID string) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return false
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil {
		return false
	}
	for _, model := range list.Data {
		// Google's OpenAI-compatible /models endpoint advertises IDs as
		// "models/<name>", while its chat/completions endpoint accepts the
		// unprefixed model name. Preserve the configured public/provider name and
		// normalize only this discovery representation.
		advertisedID := strings.TrimPrefix(model.ID, "models/")
		if advertisedID == modelID {
			return true
		}
	}
	return false
}
