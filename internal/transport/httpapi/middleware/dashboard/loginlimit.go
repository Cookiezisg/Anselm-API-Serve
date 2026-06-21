package dashboard

import (
	"sync"
	"time"
)

// Login brute-force defense: per-IP consecutive-failure count with an escalating
// lockout window. After LoginMaxFailures consecutive failures an IP is locked for
// a window that grows with the overflow (capped), so credential stuffing is
// throttled hard while a legitimate fat-finger recovers quickly. A success clears
// the counter; so does idleness — a failure arriving more than one base-lockout
// window after the previous one resets the streak first, so a shared-NAT IP that
// was hammered does not strand legitimate users at the escalated ceiling. The map
// is bounded by Sweep (expired entries dropped) so a distinct-IP flood cannot grow
// it without limit.
const (
	LoginMaxFailures = 5                // failures before lockout kicks in
	loginBaseLockout = 30 * time.Second // first lockout window
	loginMaxLockout  = 15 * time.Minute // ceiling on the escalating window
	loginEntryTTL    = 1 * time.Hour    // drop stale per-IP entries after this idle
)

type loginEntry struct {
	failures   int
	lockedTill time.Time
	lastSeen   time.Time
	lastFail   time.Time
}

// LoginLimiter is the per-IP login-attempt gate.
type LoginLimiter struct {
	mu  sync.Mutex
	m   map[string]*loginEntry
	now func() time.Time
}

// NewLoginLimiter builds a limiter. now is the injectable clock (nil ⇒ time.Now).
func NewLoginLimiter(now func() time.Time) *LoginLimiter {
	if now == nil {
		now = time.Now
	}
	return &LoginLimiter{m: make(map[string]*loginEntry), now: now}
}

// Attempt atomically gates ONE login try: under a single lock hold it both checks
// the current lockout AND pre-reserves a failure slot (counting this try as a
// failure up-front), arming/escalating the lockout the instant the threshold is
// crossed. Returns whether the IP may proceed (not locked out). A successful login
// then calls Success() to clear the reservation; a failed one leaves it.
//
// B15: the caller MUST decode+validate the body BEFORE calling Attempt, so a
// malformed body never reaches here and thus never consumes a failure slot.
//
// Single hold (not allowed()+fail()): with two locks N concurrent tries could each
// pass an allowed() snapshot before any recorded its failure, briefly overshooting
// the threshold before lockout arms. Folding check-and-reserve into one critical
// section makes the lockout burst-precise.
func (l *LoginLimiter) Attempt(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	if e == nil {
		e = &loginEntry{}
		l.m[ip] = e
	}
	e.lastSeen = now
	if now.Before(e.lockedTill) {
		return false // locked: do not reserve another slot (counter frozen).
	}
	// Decay: a first try arriving more than one base window after the previous
	// failure is fresh — discard the prior streak before counting this one, so a
	// hammered shared-NAT IP does not re-escalate straight to the ceiling on a
	// single late legitimate fat-finger. Anchored to the base window so a genuine
	// sustained attacker still escalates while an idle IP recovers.
	if !e.lastFail.IsZero() && now.Sub(e.lastFail) > loginBaseLockout {
		e.failures = 0
	}
	e.failures++
	e.lastFail = now
	if e.failures >= LoginMaxFailures {
		over := e.failures - LoginMaxFailures
		if over > 16 {
			over = 16 // clamp so the shift can never overflow.
		}
		window := loginBaseLockout << uint(over) //nolint:gosec // over is clamped to [0,16]; no overflow
		if window <= 0 || window > loginMaxLockout {
			window = loginMaxLockout
		}
		e.lockedTill = now.Add(window)
	}
	return true
}

// Locked reports whether ip is currently locked out (a pure read for the
// reject-audit log; Attempt is the gate that reserves a slot).
func (l *LoginLimiter) Locked(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[ip]
	if !ok {
		return false
	}
	return now.Before(e.lockedTill)
}

// RetryAfter returns the whole-second remainder of ip's current lockout (ceiled,
// so a sub-second remainder still reports 1s — never 0 while locked), or 0 when
// not locked. Drives the 429 Retry-After header + body retryAfterSec.
func (l *LoginLimiter) RetryAfter(ip string) int {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[ip]
	if !ok {
		return 0
	}
	rem := e.lockedTill.Sub(now)
	if rem <= 0 {
		return 0
	}
	secs := int((rem + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// Success clears any failure state for ip (a valid login resets the counter).
func (l *LoginLimiter) Success(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

// Sweep drops entries idle past the TTL (and not currently locked), bounding the
// map under a distinct-IP flood.
func (l *LoginLimiter) Sweep() {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.m {
		if now.Before(e.lockedTill) {
			continue
		}
		if now.Sub(e.lastSeen) > loginEntryTTL {
			delete(l.m, ip)
		}
	}
}
