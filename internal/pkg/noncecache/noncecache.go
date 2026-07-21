// Package noncecache provides bounded, TTL'd replay-once sets for PoW challenges
// and signed device-proof request ids. The first UseOnce within the TTL window
// admits a value; later submissions are rejected.
package noncecache

import (
	"container/list"
	"sync"
	"time"
)

// DefaultMax bounds a consumed-value set so distinct valid submissions cannot
// grow memory without limit. Normal caches evict LRU; security-sensitive callers
// use NewFailClosed so a live entry is never forgotten.
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
	// failClosed rejects a never-seen value while all slots hold live entries.
	// Device proofs use this mode so memory pressure can deny traffic but can
	// never turn a consumed proof back into an admissible replay.
	failClosed bool
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

// NewFailClosed builds a bounded cache that never evicts a live entry. Once all
// slots are live, new values are denied until expiry frees capacity. This mode is
// for callers where accepting an evicted value would violate a security invariant.
func NewFailClosed(ttl time.Duration, max int) *Cache {
	c := NewWithMax(ttl, max)
	c.failClosed = true
	return c
}

// UseOnce atomically checks whether challenge has already been consumed and, if
// not, marks it consumed. It returns true on the first use and
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

	c.evictExpiredLocked(now)
	if c.failClosed && len(c.entries) >= c.max {
		// A replay refreshes LRU recency, so an expired entry can sit ahead of
		// a live tail entry. At capacity, do the bounded full sweep before
		// denying admission; this preserves fail-closed safety without keeping
		// reclaimable slots stranded until another request happens to touch them.
		c.evictAllExpiredLocked(now)
		if len(c.entries) >= c.max {
			return false
		}
	}
	e := &entry{challenge: challenge, expireAt: now.Add(c.ttl)}
	c.entries[challenge] = c.ll.PushFront(e)
	if !c.failClosed {
		c.evictOverflowLocked()
	}
	return true
}

// evictAllExpiredLocked reclaims every expired entry. The O(max) scan is used
// only by fail-closed caches at capacity, where avoiding a false saturation is
// worth the bounded work. Caller holds c.mu.
func (c *Cache) evictAllExpiredLocked(now time.Time) {
	for el := c.ll.Back(); el != nil; {
		previous := el.Prev()
		e := el.Value.(*entry)
		if !now.Before(e.expireAt) {
			c.ll.Remove(el)
			delete(c.entries, e.challenge)
		}
		el = previous
	}
}

// evictExpiredLocked drops expired entries from the LRU tail. Caller holds c.mu.
func (c *Cache) evictExpiredLocked(now time.Time) {
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
}

// evictOverflowLocked drops live LRU entries until within max. It is used only
// by the original PoW mode; fail-closed proof caches never call it.
func (c *Cache) evictOverflowLocked() {
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
