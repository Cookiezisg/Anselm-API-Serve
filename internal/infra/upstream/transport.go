package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"
)

// Sentinel errors classify upstream outcomes across the transport/retry layers.
// They never reach the client verbatim — classify normalizes them to apierr.
var (
	// errAccountFault marks a non-2xx the transport attributed to the chosen
	// key's account health (counted toward that key's breaker). Internal only.
	errAccountFault = errors.New("upstream account-level fault")
	// errKeyOpen means the chosen key's per-key breaker is open. Treated as a
	// retryable upstream signal that may rotate to a sibling key next attempt.
	errKeyOpen = errors.New("upstream key breaker open")
	// errAllKeysGated means every configured key is in an explicit cooldown or
	// has an open breaker. No request was sent, so it is definitely unbilled and
	// must be shed rather than bypassing those health gates.
	errAllKeysGated = errors.New("all upstream api keys are gated")
	// errNoUsableAPIKey is a defense-in-depth guard for direct RoundTrip use.
	// Normal client calls fail closed before reaching the transport.
	errNoUsableAPIKey = errors.New("upstream api key pool has no usable keys")
)

// redactingTransport injects the upstream key at the last possible moment, on a
// CLONE of the request, so the caller's *http.Request never carries it (no panic
// dump / log can leak it; GW-INV-11). It also owns multi-key failover (REL-3):
// it holds every configured key with per-key health (a gobreaker + a cooldown).
// RoundTrip picks a healthy key and injects it; an account-level signal cools
// that key and rotates to a sibling. Single-key config behaves identically.
//
// Failover only changes WHICH key is used; it never adds an in-flight slot, so
// N_global is unaffected by rotation. Key material is never logged — only
// key_index.
type redactingTransport struct {
	backend BackendID
	base    http.RoundTripper
	keys    []*keyState
	rr      uint64 // round-robin starting cursor for fair spread across healthy keys
	mu      sync.Mutex
	log     *slog.Logger
	hook    MetricsHook
}

// keyState carries one upstream key plus its independent health gate.
type keyState struct {
	id     int // stable index for audit — NEVER the key material
	apiKey string
	cb     *gobreaker.CircuitBreaker[*http.Response]

	// cooldownUntil is a hard account-level pause for this key (401/403, or a 429
	// carrying a long Retry-After). It complements the breaker: the breaker tracks
	// a sliding window of consecutive faults; the cooldown honors an upstream's
	// explicit "this key is unusable until T".
	mu            sync.Mutex
	cooldownUntil time.Time
}

func (k *keyState) onCooldown(now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return now.Before(k.cooldownUntil)
}

func (k *keyState) setCooldown(until time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if until.After(k.cooldownUntil) {
		k.cooldownUntil = until
	}
}

// newRedactingTransport builds the provider-local transport. It deliberately
// leaves ResponseHeaderTimeout unset: the request-snapshot first-byte timer in
// client.go is the single dynamic connect→header→first-byte deadline. A fixed
// transport timeout would make a hot increase of UPSTREAM_HEADER_TIMEOUT_SEC
// ineffective while still adding no protection the context timer lacks.
func newRedactingTransport(apiKeys []string, logger *slog.Logger, hook MetricsHook, backend BackendID) *redactingTransport {
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if logger == nil {
		logger = slog.Default()
	}
	t := &redactingTransport{backend: backend, base: base, log: logger, hook: hook}
	for i, rawKey := range apiKeys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		//nolint:bodyclose // newKeyBreaker returns a breaker, not a response; the body is closed by Stream.Close / drainAndClose
		t.keys = append(t.keys, &keyState{id: i, apiKey: key, cb: newKeyBreaker(backend, i, logger)})
	}
	return t
}

// newKeyBreaker builds a per-key breaker that trips on a sustained burst of
// account-level faults (only those are recorded as failures; 429 and client
// cancel are not). half-open single probe to recover. IsSuccessful excludes
// context.Canceled so a client cancel can never trip even a per-key breaker.
func newKeyBreaker(backend BackendID, id int, logger *slog.Logger) *gobreaker.CircuitBreaker[*http.Response] {
	//nolint:bodyclose // generic breaker over *http.Response; the actual body is closed by Stream.Close / drainAndClose
	return gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        string(backend) + "-key",
		MaxRequests: 1,
		Interval:    30 * time.Second,
		Timeout:     20 * time.Second,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			if c.ConsecutiveFailures >= 5 {
				return true
			}
			return c.Requests >= 10 && float64(c.TotalFailures)/float64(c.Requests) > 0.5
		},
		IsSuccessful: func(err error) bool {
			// Client cancel is never a key fault (ADR-011). errAccountFault and any
			// genuine transport error ARE faults. A nil error is success.
			if err == nil {
				return true
			}
			return errors.Is(err, context.Canceled)
		},
		OnStateChange: func(_ string, from, to gobreaker.State) {
			// Audit by index only — never key material.
			logger.Warn("upstream_key_breaker_state_change", "event", "key_switch",
				"backend", backend, "key_index", id, "from", from.String(), "to", to.String())
		},
	})
}

// pickKey returns a key that is neither in cooldown nor open, scanning from a
// rotating cursor for fair spread. When every key is gated it returns nil: a
// cooldown/breaker is a hard isolation boundary, never a selection preference.
func (t *redactingTransport) pickKey(now time.Time) *keyState {
	n := uint64(len(t.keys))
	if n == 0 {
		return nil
	}
	t.mu.Lock()
	start := t.rr
	t.rr++
	t.mu.Unlock()

	for i := uint64(0); i < n; i++ {
		ks := t.keys[(start+i)%n]
		if ks.onCooldown(now) || ks.cb.State() == gobreaker.StateOpen {
			continue
		}
		return ks
	}
	return nil
}

// RoundTrip selects a healthy key, injects it on a clone, and routes the call
// through that key's breaker. A returned transport error or an account-level
// non-2xx counts as a key failure; 2xx and plain 429 count as success so 429
// never trips a key offline.
func (t *redactingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	now := time.Now()
	if len(t.keys) == 0 {
		return nil, errNoUsableAPIKey
	}
	ks := t.pickKey(now)
	if ks == nil {
		return nil, errAllKeysGated
	}

	resp, err := ks.cb.Execute(func() (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.Header.Set("Authorization", "Bearer "+ks.apiKey)
		r, e := t.base.RoundTrip(clone)
		if e != nil {
			return nil, e // transport/connect failure → key fault (unless cancel)
		}
		if t.accountLevelFault(ks, r, now) {
			// Return resp AND an error so the breaker records a failure while the
			// caller still gets the response to classify/normalize.
			return r, errAccountFault
		}
		return r, nil
	})

	// The key's breaker is open: surface a retryable signal so the next attempt
	// rotates to a sibling key (NOT a process-breaker fault, REL-3).
	if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
		return nil, errKeyOpen
	}
	// errAccountFault is internal bookkeeping; the caller only needs the resp.
	if err == errAccountFault {
		return resp, nil
	}
	return resp, err
}

// accountLevelFault decides whether a non-2xx indicates THIS key is unhealthy at
// the account level (vs a transient/global busy). On a hard signal it cools the
// key down and audits by index.
//
// 401/403 → 10-min cooldown + key switch; 5xx → counted toward the breaker; a
// plain 429 → not a key fault (global busy), but a 429 with Retry-After > 5s is
// a per-key rate pause (cooldown by that delta).
func (t *redactingTransport) accountLevelFault(ks *keyState, r *http.Response, now time.Time) bool {
	switch {
	case r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden:
		ks.setCooldown(now.Add(10 * time.Minute))
		t.log.Warn("upstream_key_cooldown", "event", "key_switch",
			"backend", t.backend, "key_index", ks.id, "reason", "auth", "upstream_status", r.StatusCode, "cooldown", "10m")
		if t.hook != nil {
			t.hook.KeyCooldown()
		}
		return true
	case r.StatusCode == http.StatusTooManyRequests:
		if d := parseRetryAfter(r.Header.Get("Retry-After"), now); d > 5*time.Second {
			ks.setCooldown(now.Add(d))
			t.log.Warn("upstream_key_cooldown", "event", "key_switch",
				"backend", t.backend, "key_index", ks.id, "reason", "retry_after", "upstream_status", 429, "cooldown", d.String())
			if t.hook != nil {
				t.hook.KeyCooldown()
			}
		}
		return false
	case r.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// atomicBool is a tiny set-once flag used by the first-byte timer disarm (B6).
type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) set()      { b.v.Store(true) }
func (b *atomicBool) get() bool { return b.v.Load() }

// takeArmed atomically transitions armed true→false, reporting whether THIS
// caller won the transition. It is the heart of the B6 idempotent disarm: the
// first-byte AfterFunc and the success-path disarm both race to take it, and the
// winner alone decides whether the stream is canceled — so a timer firing the
// instant output begins can never cancel an already-started stream.
func (b *atomicBool) takeArmed() bool { return b.v.CompareAndSwap(true, false) }
