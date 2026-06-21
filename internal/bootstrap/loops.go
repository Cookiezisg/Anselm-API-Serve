package bootstrap

// loops.go holds the two infra collaborators bootstrap must construct itself —
// the M2 token-anomaly throttle (no infra slice built one) and the cached
// upstream readiness prober — plus the background loop bodies the lifecycle runs.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/ratelimit"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
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

// upstreamProber holds the last successful upstream probe time so /readyz reads a
// CACHED result — never a live DeepSeek call per request (a per-request probe
// could stall behind a struggling upstream). The background loop refreshes it.
type upstreamProber struct {
	client  upstream.Client
	baseURL string
	lastOK  atomic.Int64 // unix-nanos of the last success (0 = never)
}

func newUpstreamProber(client upstream.Client, baseURL string) *upstreamProber {
	return &upstreamProber{client: client, baseURL: baseURL}
}

// LastOK reports the last success + its age (health.UpstreamProbe).
func (p *upstreamProber) LastOK() (bool, time.Duration) {
	ns := p.lastOK.Load()
	if ns == 0 {
		return false, 0
	}
	return true, time.Since(time.Unix(0, ns))
}

// probe does a cheap connectivity check against the upstream base URL. A TCP/TLS
// reachable endpoint (any HTTP status) counts as OK — we are probing reachability,
// not a real completion (which would spend budget). The breaker-open state alone
// already gates the chat path; this only feeds /readyz.
func (p *upstreamProber) probe(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	p.lastOK.Store(time.Now().UnixNano())
}
