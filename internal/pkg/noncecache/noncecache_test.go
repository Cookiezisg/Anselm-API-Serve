package noncecache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// frozen clock helper: tests must not depend on wall time.
func clockAt(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestUseOnce_TrueThenFalse(t *testing.T) {
	c := New(time.Minute)
	if !c.UseOnce("ch") {
		t.Fatal("first UseOnce must return true")
	}
	if c.UseOnce("ch") {
		t.Fatal("replay within TTL must return false")
	}
	if c.UseOnce("ch") {
		t.Fatal("further replay must stay false")
	}
}

func TestUseOnce_TTLExpiry(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := New(100 * time.Millisecond)
	c.SetNow(clockAt(&now))

	if !c.UseOnce("ch") {
		t.Fatal("first use true")
	}
	now = now.Add(50 * time.Millisecond) // still within TTL
	if c.UseOnce("ch") {
		t.Fatal("replay before expiry must be false")
	}
	now = now.Add(100 * time.Millisecond) // past expiry (150ms > 100ms)
	if !c.UseOnce("ch") {
		t.Fatal("expired prior entry must be treated as fresh -> true")
	}
}

func TestLRUEviction_UnderCap(t *testing.T) {
	now := time.Unix(2_000, 0)
	c := NewWithMax(time.Hour, 2) // tiny cap, long TTL so eviction is LRU not TTL
	c.SetNow(clockAt(&now))

	c.UseOnce("a")
	c.UseOnce("b")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	c.UseOnce("c") // overflow -> evict LRU ("a")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len after overflow = %d, want 2", got)
	}
	// "a" was evicted, so it is seen as fresh again.
	if !c.UseOnce("a") {
		t.Fatal("evicted challenge should be admitted as fresh")
	}
	// "c" is still live -> replay.
	if c.UseOnce("c") {
		t.Fatal("live challenge c must be a replay")
	}
}

func TestNonPositiveMax_FallsBackToDefault(t *testing.T) {
	if c := NewWithMax(time.Minute, 0); c.max != DefaultMax {
		t.Fatalf("max = %d, want DefaultMax %d", c.max, DefaultMax)
	}
}

// One winner: under concurrent UseOnce for the same challenge, exactly one call
// returns true.
func TestUseOnce_ConcurrentOneWinner(t *testing.T) {
	c := New(time.Minute)
	const goroutines = 64
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if c.UseOnce("same") {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

// Concurrent distinct challenges under -race: no data race, cache stays bounded.
func TestUseOnce_ConcurrentDistinct_Bounded(t *testing.T) {
	c := NewWithMax(time.Minute, 128)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				c.UseOnce(string(rune('A'+base)) + string(rune(i%256)))
			}
		}(w)
	}
	wg.Wait()
	if got := c.Len(); got > 128 {
		t.Fatalf("Len = %d exceeds cap 128", got)
	}
}
