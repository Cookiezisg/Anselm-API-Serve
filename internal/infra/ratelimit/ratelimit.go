// Package ratelimit is the in-memory per-install minute token bucket: a bounded
// LRU of per-key buckets (so distinct-key churn can never grow memory without
// limit) with an eviction hook for churn detection, plus the M2 token-anomaly
// tighten-only override (SetKeyLimit) that slows ONE bucket in place.
//
// Restart-reset is harmless: the durable daily sub-limit + global daily budget
// are the real hard guardrails; the minute bucket is only the cheap first soft
// gate. The override (SetKeyLimit) must tighten the SAME bucket — never hand a
// throttled key a fresh full window — so the accumulated count carries over.
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// defaultMax bounds the bucket map so a flood of distinct keys cannot grow it
// without limit. It must be set ABOVE the expected active-install count: it is
// the "concurrently active keys" ceiling, not a safety cap. Below the active
// count, healthy keys get evicted then rebuilt with a full bucket, which is
// equivalent to relaxing the minute limit — hence every eviction fires onEvict
// so churn is observable, and the durable daily limits are the true guardrail.
const defaultMax = 16384

// RateLimiter is a bounded-LRU set of per-key minute token buckets. The hot
// path (Allow) is O(1): a map lookup + a list move-to-front. All state is under
// one mutex, so SetKeyLimit / SetRate / SetNow race safely with Allow.
type RateLimiter struct {
	mu      sync.Mutex
	ll      *list.List               // LRU order: Front=most-recent, Back=least-recent
	buckets map[string]*list.Element // key → list node (node Value is *entry)
	max     int
	perMin  int
	now     func() time.Time

	// onEvict, if set, fires once per LRU eviction so the caller can emit a
	// churn-detection metric/WARN. Called WITH mu held — it must be cheap and
	// non-reentrant (a counter Inc / a rate-limited log), never call back in.
	onEvict func(key string)
}

// entry is one key's sliding minute bucket.
type entry struct {
	key    string
	tokens float64
	last   time.Time

	// Per-key tighten-only override (M2 token-anomaly throttle): while ovrUntil
	// is in the future AND ovrLimit is strictly smaller than the global perMin,
	// this bucket refills + caps at ovrLimit on the SAME accumulated tokens.
	// Switching to the stricter rate must tighten the existing window, never reset
	// it to a full bucket (which would momentarily relax the very key being
	// slowed). Expiry auto-reverts to perMin on the next refill — no timer.
	ovrLimit int
	ovrUntil time.Time
}

// effLimit is the per-minute cap in force for this bucket at now: the per-key
// override only when active AND strictly smaller than perMin, else perMin. An
// override can only ever tighten — a larger ovrLimit is ignored.
func (e *entry) effLimit(perMin int, now time.Time) int {
	if e.ovrLimit > 0 && now.Before(e.ovrUntil) && e.ovrLimit < perMin {
		return e.ovrLimit
	}
	return perMin
}

// New caps each key at perMin requests/minute (linear refill) and retains at
// most defaultMax distinct keys (LRU eviction). perMin<=0 disables the limiter
// (Allow always allows).
func New(perMin int) *RateLimiter { return NewWithMax(perMin, defaultMax) }

// NewWithMax is New with an explicit key-count ceiling (tests assert eviction
// with a tiny max).
func NewWithMax(perMin, max int) *RateLimiter {
	if max <= 0 {
		max = defaultMax
	}
	return &RateLimiter{
		ll:      list.New(),
		buckets: make(map[string]*list.Element, max),
		max:     max,
		perMin:  perMin,
		now:     time.Now,
	}
}

// Allow consumes one token for key, returning false when the bucket is empty.
// perMin<=0 short-circuits to true with no allocation/lock (limiter disabled).
func (rl *RateLimiter) Allow(key string) bool {
	if rl.perMin <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	e := rl.touchLocked(key, now)

	// Effective cap = global perMin, tightened to a smaller active override. The
	// same bucket's accumulated tokens carry over so a throttle tightens the
	// existing window rather than resetting it.
	limit := e.effLimit(rl.perMin, now)

	// Linear refill: limit tokens per 60s, clamped to the CURRENT effective cap.
	// Clamping to the (possibly smaller) override pulls a key that was sitting on
	// a full global bucket straight down to the throttle ceiling, so throttling
	// bites on the next request rather than a minute later.
	elapsed := now.Sub(e.last).Seconds()
	e.tokens += elapsed * (float64(limit) / 60.0)
	if e.tokens > float64(limit) {
		e.tokens = float64(limit)
	}
	e.last = now

	if e.tokens < 1 {
		return false
	}
	e.tokens--
	return true
}

// SetKeyLimit installs a per-key tighten-only override: until `until`, key's
// bucket refills + caps at `limit` (a smaller per-minute rate) on the SAME
// sliding bucket (accumulated tokens are NOT reset). This is the M2 throttle
// primitive — it MUST stay on this one bucket so the count is continuous; a
// stricter SECOND limiter would give the abuser a brand-new (initially full)
// window, momentarily relaxing exactly the key being slowed. A larger-than-
// global or expired override is inert (effLimit only ever tightens).
func (rl *RateLimiter) SetKeyLimit(key string, limit int, until time.Time) {
	if rl.perMin <= 0 {
		return // limiter disabled; nothing to tighten (Allow always allows)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	el, ok := rl.buckets[key]
	if !ok {
		// Create the bucket if absent so the throttle binds even to a key whose
		// bucket was just evicted/never seen — start it at the (smaller) throttle
		// ceiling, never the full global count, so the first post-set request is
		// already metered at the tightened rate.
		start := float64(limit)
		if limit > rl.perMin {
			start = float64(rl.perMin)
		}
		e := &entry{key: key, tokens: start, last: now, ovrLimit: limit, ovrUntil: until}
		rl.buckets[key] = rl.ll.PushFront(e)
		rl.evictLocked()
		return
	}
	rl.ll.MoveToFront(el)
	e := el.Value.(*entry)
	e.ovrLimit = limit
	e.ovrUntil = until
	// Pull a key sitting on a full global bucket down to the throttle ceiling now
	// so the tightening is felt on the next request. Only ever lowers tokens.
	if limit > 0 && limit < rl.perMin && e.tokens > float64(limit) {
		e.tokens = float64(limit)
	}
}

// touchLocked returns key's entry, creating it (LRU front + evict) on miss or
// moving it to the front on hit. Caller holds mu.
func (rl *RateLimiter) touchLocked(key string, now time.Time) *entry {
	if el, ok := rl.buckets[key]; ok {
		rl.ll.MoveToFront(el)
		return el.Value.(*entry)
	}
	e := &entry{key: key, tokens: float64(rl.perMin), last: now}
	rl.buckets[key] = rl.ll.PushFront(e)
	rl.evictLocked()
	return e
}

// SetOnEvict installs the LRU-eviction observer (churn detection). Wire it once
// at construction, before the limiter is shared across goroutines.
func (rl *RateLimiter) SetOnEvict(f func(key string)) { rl.onEvict = f }

// SetRate hot-swaps the global per-minute cap (RATE_PER_MIN hot-reload). Taken
// under mu so it races safely with Allow; existing buckets keep their tokens and
// refill at the new rate from here on. perMin<=0 disables (Allow allows).
func (rl *RateLimiter) SetRate(perMin int) {
	rl.mu.Lock()
	rl.perMin = perMin
	rl.mu.Unlock()
}

// SetNow overrides the clock (tests only). Taken under mu so it races safely
// with concurrent Allow. Production uses the time.Now construction default.
func (rl *RateLimiter) SetNow(now func() time.Time) {
	rl.mu.Lock()
	rl.now = now
	rl.mu.Unlock()
}

// evictLocked drops least-recently-used keys until the map is within max,
// firing onEvict per drop. Caller holds mu.
func (rl *RateLimiter) evictLocked() {
	for len(rl.buckets) > rl.max {
		back := rl.ll.Back()
		if back == nil {
			return
		}
		evicted := back.Value.(*entry).key
		rl.ll.Remove(back)
		delete(rl.buckets, evicted)
		// An evicted-then-recreated bucket is full again, so sustained eviction =
		// the minute cap being skirted; surface it for churn detection.
		if rl.onEvict != nil {
			rl.onEvict(evicted)
		}
	}
}

// Len reports the current number of tracked keys (test/observability hook).
func (rl *RateLimiter) Len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.buckets)
}
