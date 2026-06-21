// Package ratesample computes a recent-window QPS and error-rate server-side,
// independent of who reads it.
//
// WHY (B16): the legacy design kept ONE mutable sampler that every /api/overview
// poll overwrote by diffing cumulative counters against the last poll's snapshot.
// Two concurrent pollers stole each other's "last" point, halving/spiking the dt
// and corrupting QPS. Here observation (Observe) and reading (Snapshot) are fully
// decoupled: requests feed a fixed ring of per-second buckets, and any number of
// concurrent Snapshot callers derive the same window view without mutating state
// that another reader depends on.
package ratesample

import (
	"sync"
	"time"
)

// Sampler is a sliding-window request/error rate over a fixed number of 1-second
// buckets. Safe for concurrent Observe and Snapshot.
type Sampler struct {
	mu        sync.Mutex
	windowSec int
	reqs      []int64 // ring of per-second request counts
	errs      []int64 // ring of per-second error counts
	lastSec   int64   // unix second of the most recently advanced bucket
	now       func() time.Time
}

// Snapshot is the derived recent-window view.
type Snapshot struct {
	QPS       float64 `json:"qps"`
	ErrorRate float64 `json:"errorRate"` // errored requests / sec over the window
	WindowSec int     `json:"windowSec"`
}

// Option configures a Sampler.
type Option func(*Sampler)

// WithClock injects a clock for deterministic tests.
func WithClock(now func() time.Time) Option {
	return func(s *Sampler) {
		if now != nil {
			s.now = now
		}
	}
}

// New builds a Sampler over a windowSec-wide sliding window. windowSec < 1 is
// clamped to 1.
func New(windowSec int, opts ...Option) *Sampler {
	if windowSec < 1 {
		windowSec = 1
	}
	s := &Sampler{
		windowSec: windowSec,
		reqs:      make([]int64, windowSec),
		errs:      make([]int64, windowSec),
		now:       time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	s.lastSec = s.now().Unix()
	return s
}

// advance rolls the ring forward to nowSec, zeroing any buckets that fall out of
// the window. Caller holds s.mu. Returns the index for nowSec.
func (s *Sampler) advance(nowSec int64) int {
	delta := nowSec - s.lastSec
	if delta < 0 {
		// Clock moved backwards; treat as same second (defensive).
		delta = 0
		nowSec = s.lastSec
	}
	if delta >= int64(s.windowSec) {
		// Whole window elapsed: every bucket is stale.
		for i := range s.reqs {
			s.reqs[i] = 0
			s.errs[i] = 0
		}
	} else {
		// Clear the buckets we are stepping into.
		for i := int64(1); i <= delta; i++ {
			idx := int((s.lastSec + i) % int64(s.windowSec))
			s.reqs[idx] = 0
			s.errs[idx] = 0
		}
	}
	s.lastSec = nowSec
	return int(nowSec % int64(s.windowSec))
}

// Observe records one request with the given HTTP status. Call once per request
// from the metrics middleware. status >= 500 counts as an error.
func (s *Sampler) Observe(status int) {
	nowSec := s.now().Unix()
	s.mu.Lock()
	idx := s.advance(nowSec)
	s.reqs[idx]++
	if status >= 500 {
		s.errs[idx]++
	}
	s.mu.Unlock()
}

// Snapshot returns the current sliding-window rates. Concurrent callers each get
// a correct, independent view (B16 invariant).
func (s *Sampler) Snapshot() Snapshot {
	nowSec := s.now().Unix()
	s.mu.Lock()
	s.advance(nowSec)
	var totReq, totErr int64
	for i := range s.reqs {
		totReq += s.reqs[i]
		totErr += s.errs[i]
	}
	s.mu.Unlock()

	w := float64(s.windowSec)
	return Snapshot{
		QPS:       float64(totReq) / w,
		ErrorRate: float64(totErr) / w,
		WindowSec: s.windowSec,
	}
}
