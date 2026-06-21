package dashboard

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCreateGetSlideDestroy(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	s := NewSessionStore(time.Hour, clock)
	sess := s.Create("admin")
	if sess.ID == "" || sess.CSRF == "" || sess.ID == sess.CSRF {
		t.Fatalf("session must have distinct non-empty id+csrf: %+v", sess)
	}

	// Advance within TTL: get slides expiry.
	now = time.Unix(1000+1800, 0)
	got, ok := s.get(sess.ID)
	if !ok {
		t.Fatal("session should still be valid mid-TTL")
	}
	if !got.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("get must slide expiry to now+ttl, got %v", got.Expires)
	}

	// Destroy invalidates.
	s.Destroy(sess.ID)
	if _, ok := s.get(sess.ID); ok {
		t.Fatal("destroyed session must not resolve")
	}
}

func TestSessionExpiresAndSweeps(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSessionStore(time.Minute, func() time.Time { return now })
	sess := s.Create("admin")
	now = now.Add(2 * time.Minute)
	if _, ok := s.get(sess.ID); ok {
		t.Fatal("expired session must not resolve")
	}
	// A second create then sweep past TTL clears it.
	s2 := s.Create("admin")
	now = now.Add(2 * time.Minute)
	s.Sweep()
	if _, ok := s.get(s2.ID); ok {
		t.Fatal("sweep must drop expired sessions")
	}
}

// TestLoginLimiterBurstPrecise (B15 core): the (max+1)-th attempt is already
// locked — folding check-and-reserve into one hold makes lockout burst-precise.
func TestLoginLimiterBurstPrecise(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLoginLimiter(func() time.Time { return now })
	// First LoginMaxFailures attempts proceed (each reserves a failure slot).
	for i := 0; i < LoginMaxFailures; i++ {
		if !l.Attempt("ip1") {
			t.Fatalf("attempt %d should proceed", i)
		}
	}
	// The next attempt is locked out.
	if l.Attempt("ip1") {
		t.Fatal("attempt past threshold must be locked")
	}
	if !l.Locked("ip1") {
		t.Fatal("ip must report locked")
	}
	if l.RetryAfter("ip1") < 1 {
		t.Fatal("RetryAfter must be >=1 while locked")
	}
}

func TestLoginLimiterConcurrentNoOvershoot(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLoginLimiter(func() time.Time { return now })
	var proceeded int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Attempt("ip") {
				atomic.AddInt64(&proceeded, 1)
			}
		}()
	}
	wg.Wait()
	// At most LoginMaxFailures concurrent tries may proceed before lockout arms.
	if proceeded > int64(LoginMaxFailures) {
		t.Fatalf("burst overshoot: %d proceeded, want <= %d", proceeded, LoginMaxFailures)
	}
}

func TestLoginLimiterSuccessClears(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLoginLimiter(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		l.Attempt("ip")
	}
	l.Success("ip")
	if l.Locked("ip") {
		t.Fatal("success must clear the failure state")
	}
	// Fresh budget after success.
	for i := 0; i < LoginMaxFailures; i++ {
		if !l.Attempt("ip") {
			t.Fatalf("post-success attempt %d should proceed", i)
		}
	}
}

func TestLoginLimiterDecayResetsStreak(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLoginLimiter(func() time.Time { return now })
	for i := 0; i < LoginMaxFailures; i++ {
		l.Attempt("ip")
	}
	if !l.Locked("ip") {
		t.Fatal("should be locked after threshold")
	}
	// Decay: a try arriving more than one base window after the last failure resets
	// the streak so a legitimate late retry proceeds (not stranded at the ceiling).
	now = now.Add(time.Hour)
	if !l.Attempt("ip") {
		t.Fatal("after long idle the streak should reset and a fresh try proceeds")
	}
	if l.Locked("ip") {
		t.Fatal("a single fresh failure after decay must not re-lock")
	}
}
