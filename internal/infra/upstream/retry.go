package upstream

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/sony/gobreaker/v2"
)

// retryPolicy bounds the connect→first-byte retry window (REL-1, GW-INV-26).
// Only transient pre-output faults (connect fail / TLS reset / 502·503·504, and
// a per-key signal the next attempt can route around) are retried — NEVER 429,
// NEVER after output starts (that would double-bill / double-produce).
type retryPolicy struct {
	maxAttempts int           // total attempts incl. the first
	base        time.Duration // initial backoff
	cap         time.Duration // backoff ceiling
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{maxAttempts: 3, base: 200 * time.Millisecond, cap: 3 * time.Second}
}

// backoff returns the sleep before retry `attempt` (1-based for the wait after
// the first failed attempt): exponential base*2^(n-1) capped, then full jitter
// in [0, d]. Full jitter (vs equal jitter) maximally decorrelates retries; using
// the concurrency-safe top-level rand keeps same-tick retries from sharing a
// per-request seed under a thundering herd.
func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.base << (attempt - 1)
	if d > p.cap || d <= 0 {
		d = p.cap
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) //nolint:gosec // backoff jitter, not security
}

// parseRetryAfter parses a Retry-After header (delta-seconds or HTTP-date) into
// a positive duration relative to now, or 0 when absent/invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// newProcessBreaker builds the PROCESS-wide breaker wrapping all upstream calls
// (REL-2). Only genuine faults (5xx/timeout/connect, surfaced as one failed
// attempt-set per request) are recorded; 429, client cancel, and post-output
// disconnects are not. Open → immediate UPSTREAM_BUSY. IsSuccessful excludes
// context.Canceled belt-and-suspenders so a client cancel never trips it.
func newProcessBreaker(onStateChange func(from, to gobreaker.State)) *gobreaker.CircuitBreaker[struct{}] {
	return gobreaker.NewCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        "deepseek-upstream",
		MaxRequests: 1,
		Interval:    30 * time.Second,
		Timeout:     10 * time.Second,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			if c.ConsecutiveFailures >= 5 {
				return true
			}
			return c.Requests >= 10 && float64(c.TotalFailures)/float64(c.Requests) > 0.5
		},
		IsSuccessful: func(err error) bool {
			// Only errUpstreamFault (genuine fault) is recorded as a failure. A nil
			// error is success; client-cancel is explicitly excluded.
			if err == nil {
				return true
			}
			return isClientCancel(err)
		},
		OnStateChange: func(_ string, from, to gobreaker.State) {
			if onStateChange != nil {
				onStateChange(from, to)
			}
		},
	})
}
