package ratesample

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a monotonic, settable clock for deterministic windows.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) set(sec int64)  { c.ns.Store(sec * int64(time.Second)) }

func newAt(sec int64, windowSec int) (*Sampler, *fakeClock) {
	c := &fakeClock{}
	c.set(sec)
	return New(windowSec, WithClock(c.now)), c
}

func TestQPSOverWindow(t *testing.T) {
	s, c := newAt(100, 10) // 10s window
	// 30 requests spread across 3 distinct seconds, all inside the window.
	for sec := int64(100); sec < 103; sec++ {
		c.set(sec)
		for i := 0; i < 10; i++ {
			s.Observe(200)
		}
	}
	c.set(105) // still within the 10s window of [96,105]
	got := s.Snapshot()
	if got.WindowSec != 10 {
		t.Fatalf("WindowSec = %d, want 10", got.WindowSec)
	}
	// 30 requests / 10s window = 3.0 QPS.
	if got.QPS != 3.0 {
		t.Fatalf("QPS = %v, want 3.0", got.QPS)
	}
	if got.ErrorRate != 0 {
		t.Fatalf("ErrorRate = %v, want 0", got.ErrorRate)
	}
}

func TestErrorRate(t *testing.T) {
	s, c := newAt(0, 10)
	c.set(1)
	for i := 0; i < 8; i++ {
		s.Observe(200)
	}
	for i := 0; i < 2; i++ {
		s.Observe(503) // >= 500 counts as error
	}
	s.Observe(404) // 4xx is NOT an error
	c.set(2)
	got := s.Snapshot()
	// 11 requests / 10s = 1.1 QPS; 2 errors / 10s = 0.2.
	if got.QPS != 1.1 {
		t.Fatalf("QPS = %v, want 1.1", got.QPS)
	}
	if got.ErrorRate != 0.2 {
		t.Fatalf("ErrorRate = %v, want 0.2", got.ErrorRate)
	}
}

func TestSlidingExpiry(t *testing.T) {
	s, c := newAt(0, 5) // 5s window
	c.set(0)
	for i := 0; i < 5; i++ {
		s.Observe(200)
	}
	// Advance past the window; the bucket at sec 0 must fall out.
	c.set(10)
	got := s.Snapshot()
	if got.QPS != 0 {
		t.Fatalf("QPS after expiry = %v, want 0", got.QPS)
	}
}

func TestPartialExpiry(t *testing.T) {
	s, c := newAt(0, 5)
	c.set(0)
	for i := 0; i < 5; i++ {
		s.Observe(200) // bucket sec 0
	}
	c.set(4)
	for i := 0; i < 5; i++ {
		s.Observe(200) // bucket sec 4
	}
	// At sec 5 the window is [1,5]: sec-0 bucket drops, sec-4 survives.
	c.set(5)
	got := s.Snapshot()
	if got.QPS != 1.0 { // 5 reqs / 5s
		t.Fatalf("QPS = %v, want 1.0", got.QPS)
	}
}

// TestConcurrentSnapshotsAgree is the B16 regression: two concurrent readers must
// both observe the same correct value. In the legacy design each poll mutated the
// shared "last" point, so concurrent readers stole each other's delta. Here reads
// never corrupt each other.
func TestConcurrentSnapshotsAgree(t *testing.T) {
	s, c := newAt(0, 10)
	c.set(1)
	for i := 0; i < 50; i++ {
		s.Observe(200)
	}
	c.set(2) // stop the clock so both readers see an identical window

	const readers = 16
	var wg sync.WaitGroup
	results := make([]Snapshot, readers)
	start := make(chan struct{})
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			<-start
			results[r] = s.Snapshot()
		}(r)
	}
	close(start)
	wg.Wait()

	want := Snapshot{QPS: 5.0, ErrorRate: 0, WindowSec: 10} // 50/10
	for r, got := range results {
		if got != want {
			t.Fatalf("reader %d got %+v, want %+v (B16: concurrent reads must agree)", r, got, want)
		}
	}
}

// TestConcurrentObserveAndSnapshot exercises the -race detector under mixed load
// with a concurrently advancing clock.
func TestConcurrentObserveAndSnapshot(t *testing.T) {
	c := &fakeClock{}
	c.set(0)
	s := New(10, WithClock(c.now))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Clock driver: advances until told to stop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sec := int64(0); ; sec++ {
			select {
			case <-stop:
				return
			default:
			}
			c.set(sec)
		}
	}()

	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 5000; i++ {
				s.Observe(200 + (i%3)*150) // mix of 200/350/500
			}
		}()
	}
	for r := 0; r < 4; r++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 5000; i++ {
				_ = s.Snapshot()
			}
		}()
	}

	workers.Wait() // all observers/readers done
	close(stop)    // now retire the clock driver
	wg.Wait()
}
