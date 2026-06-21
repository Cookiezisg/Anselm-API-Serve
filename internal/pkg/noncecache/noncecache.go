// Package noncecache provides the PoW nonce-once replay defense: a bounded,
// TTL'd set of already-consumed challenge identities. The first UseOnce within
// the TTL window admits a solution; any later submission for the same challenge
// is a replay and is rejected — without this a single valid solve could be
// replayed unboundedly inside the freshness window, amortizing the PoW cost to
// zero (GW-INV-20 family, nonce-once via bounded LRU+TTL).
package noncecache

import (
	"container/list"
	"sync"
	"time"
)

// DefaultMax bounds the consumed-challenge set so a flood of distinct challenges
// cannot grow the cache without limit. An entry is inserted only on a fully
// verified solve, so reaching this many live entries implies a very high solve
// rate; LRU eviction then drops the oldest, whose TTL is anyway about to lapse.
const DefaultMax = 65536

// Cache is a bounded, TTL'd set of consumed challenges. It keys on the challenge
// string itself (a challenge is single-use), so the first accepted solution
// consumes it and any later submission for that challenge is a replay. TTL
// matches the challenge freshness window, after which the timestamp check
// rejects the challenge anyway, so the entry is free to evict.
type Cache struct {
	mu      sync.Mutex
	ll      *list.List               // recency order: Front=newest, Back=oldest
	entries map[string]*list.Element // challenge -> node (Value is *entry)
	max     int
	ttl     time.Duration
	now     func() time.Time
}

type entry struct {
	challenge string
	expireAt  time.Time
}

// New builds a Cache holding consumed challenges for ttl, capped at DefaultMax.
func New(ttl time.Duration) *Cache {
	return NewWithMax(ttl, DefaultMax)
}

// NewWithMax is New with an explicit entry-count ceiling (tests assert eviction
// with a tiny max). A non-positive max falls back to DefaultMax.
func NewWithMax(ttl time.Duration, max int) *Cache {
	if max <= 0 {
		max = DefaultMax
	}
	return &Cache{
		ll:      list.New(),
		entries: make(map[string]*list.Element, max),
		max:     max,
		ttl:     ttl,
		now:     time.Now,
	}
}

// UseOnce atomically checks whether challenge has already been consumed and, if
// not, marks it consumed. It returns true on the first use (admit the PoW) and
// false on any replay within the TTL (reject). An expired prior entry is treated
// as never-seen — an expired challenge would already have failed the freshness
// check elsewhere, so a same-string collision after expiry is itself rejected
// upstream.
func (c *Cache) UseOnce(challenge string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if el, ok := c.entries[challenge]; ok {
		e := el.Value.(*entry)
		if now.Before(e.expireAt) {
			// Live prior use -> replay. Refresh recency so a hammered replay
			// stays hot (a known-replay until its TTL lapses).
			c.ll.MoveToFront(el)
			return false
		}
		// Stale prior entry: re-stamp it as a fresh consumption in place.
		e.expireAt = now.Add(c.ttl)
		c.ll.MoveToFront(el)
		return true
	}

	e := &entry{challenge: challenge, expireAt: now.Add(c.ttl)}
	c.entries[challenge] = c.ll.PushFront(e)
	c.evictLocked(now)
	return true
}

// evictLocked drops expired entries from the LRU tail first, then (if still over
// capacity) the least-recently-used ones until within max. Caller holds c.mu.
func (c *Cache) evictLocked(now time.Time) {
	// Opportunistic TTL sweep from the tail (oldest, least-recently-touched):
	// expired entries cluster there.
	for {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*entry)
		if now.Before(e.expireAt) {
			break // tail still live -> no more cheap expiry reclaim
		}
		c.ll.Remove(back)
		delete(c.entries, e.challenge)
	}
	// Hard cap backstop: a burst of valid solves still overflowing drops LRU.
	for len(c.entries) > c.max {
		back := c.ll.Back()
		if back == nil {
			return
		}
		e := back.Value.(*entry)
		c.ll.Remove(back)
		delete(c.entries, e.challenge)
	}
}

// SetNow overrides the clock source (tests only — frozen/controlled time). Taken
// under the cache mutex so it races safely with concurrent UseOnce. Production
// always uses time.Now (the construction default).
func (c *Cache) SetNow(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Len reports the current number of tracked entries (test/observability hook).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
