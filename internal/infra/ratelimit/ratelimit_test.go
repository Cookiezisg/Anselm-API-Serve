package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// frozenClock is a controllable time source for deterministic refill tests.
type frozenClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *frozenClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *frozenClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAllow_BucketEmptiesThenRefills(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	rl := New(3)
	rl.SetNow(clk.now)

	for i := 0; i < 3; i++ {
		if !rl.Allow("a") {
			t.Fatalf("token %d should be allowed", i)
		}
	}
	if rl.Allow("a") {
		t.Fatal("4th request must be denied (bucket empty)")
	}

	// Refill: 3/min → one token back in 20s.
	clk.advance(20 * time.Second)
	if !rl.Allow("a") {
		t.Fatal("after 20s one token should have refilled")
	}
	if rl.Allow("a") {
		t.Fatal("only one token refilled; second must be denied")
	}
}

func TestAllow_DisabledLimiterAlwaysAllows(t *testing.T) {
	rl := New(0)
	for i := 0; i < 1000; i++ {
		if !rl.Allow("x") {
			t.Fatal("perMin<=0 must always allow")
		}
	}
	if rl.Len() != 0 {
		t.Fatal("disabled limiter must allocate no buckets")
	}
}

func TestAllow_PerKeyIndependent(t *testing.T) {
	rl := New(1)
	if !rl.Allow("a") || !rl.Allow("b") {
		t.Fatal("distinct keys have independent buckets")
	}
	if rl.Allow("a") || rl.Allow("b") {
		t.Fatal("each key's single token is spent")
	}
}

func TestEviction_BoundedAndHookFires(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]int{}
	rl := NewWithMax(10, 2)
	rl.SetOnEvict(func(k string) {
		mu.Lock()
		evicted[k]++
		mu.Unlock()
	})

	// Touch a, then b (a is now LRU). Touching c evicts a.
	rl.Allow("a")
	rl.Allow("b")
	rl.Allow("c")

	if rl.Len() != 2 {
		t.Fatalf("map must stay bounded at max=2, got %d", rl.Len())
	}
	mu.Lock()
	defer mu.Unlock()
	if evicted["a"] != 1 {
		t.Fatalf("LRU key 'a' should have been evicted once, got %v", evicted)
	}
}

func TestEviction_RecentUseProtectsKey(t *testing.T) {
	rl := NewWithMax(10, 2)
	rl.Allow("a")
	rl.Allow("b")
	rl.Allow("a") // a now MRU, b is LRU
	rl.Allow("c") // evicts b, not a

	if !hasKey(rl, "a") || !hasKey(rl, "c") {
		t.Fatal("recently-used 'a' and new 'c' must survive")
	}
	if hasKey(rl, "b") {
		t.Fatal("LRU 'b' should have been evicted")
	}
}

func hasKey(rl *RateLimiter, k string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	_, ok := rl.buckets[k]
	return ok
}

func TestSetKeyLimit_TightensSameBucket(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	rl := New(60) // 60/min → one token/sec
	rl.SetNow(clk.now)

	// Spend a couple tokens so the bucket is NOT full, proving the throttle does
	// not reset to a fresh full window.
	rl.Allow("k")
	rl.Allow("k")

	// Throttle to 2/min for 10 minutes. The same bucket is pulled down to <= 2.
	rl.SetKeyLimit("k", 2, clk.now().Add(10*time.Minute))

	// Drain whatever <=2 tokens remain.
	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.Allow("k") {
			allowed++
		}
	}
	if allowed > 2 {
		t.Fatalf("throttled bucket must cap at 2, allowed %d", allowed)
	}

	// Refill is now at the throttled 2/min (one token per 30s), NOT 60/min.
	clk.advance(15 * time.Second)
	if rl.Allow("k") {
		t.Fatal("at 2/min only half a token refills in 15s; must deny")
	}
	clk.advance(15 * time.Second)
	if !rl.Allow("k") {
		t.Fatal("after a full 30s one throttled token should refill")
	}
}

func TestSetKeyLimit_NeverRelaxes(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	rl := New(2)
	rl.SetNow(clk.now)

	// A larger-than-global override is inert (only ever tightens).
	rl.SetKeyLimit("k", 100, clk.now().Add(time.Hour))
	rl.Allow("k")
	rl.Allow("k")
	if rl.Allow("k") {
		t.Fatal("override larger than global must not relax the 2/min cap")
	}
}

func TestSetKeyLimit_ExpiresRevertsToGlobal(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	rl := New(60)
	rl.SetNow(clk.now)

	rl.SetKeyLimit("k", 1, clk.now().Add(time.Minute))
	// Override active: drain to the throttle ceiling.
	for rl.Allow("k") {
	}

	// Past expiry, refill reverts to the global 60/min (one token/sec).
	clk.advance(2 * time.Minute)
	if !rl.Allow("k") {
		t.Fatal("after override expiry the global rate should refill")
	}
}

func TestSetKeyLimit_OnDisabledLimiterIsNoop(t *testing.T) {
	rl := New(0)
	rl.SetKeyLimit("k", 1, time.Now().Add(time.Hour))
	if rl.Len() != 0 {
		t.Fatal("throttle on a disabled limiter must create no bucket")
	}
	if !rl.Allow("k") {
		t.Fatal("disabled limiter still always allows")
	}
}

func TestConcurrentAllowRace(t *testing.T) {
	rl := New(1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%5))
			for j := 0; j < 200; j++ {
				rl.Allow(key)
				if j%20 == 0 {
					rl.SetKeyLimit(key, 10, time.Now().Add(time.Second))
				}
			}
		}(i)
	}
	wg.Wait()
}
