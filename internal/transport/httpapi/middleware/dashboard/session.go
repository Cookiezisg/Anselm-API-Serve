// Package dashboard holds the loopback admin backend's auth/session middleware:
// a server-side session store (256-bit crypto/rand token, 12h SLIDING TTL),
// requireSession (B14: re-issues the cookie on every pass so the browser learns
// the slide), requireCSRF (double-submit constant-time compare on state-changing
// POSTs), a per-IP escalating login limiter (B15: a malformed body must NOT
// consume a failure slot), and securityHeaders. It is transport-layer: it imports
// pkg + transport peers + net/http ONLY (no infra/app), so the architecture gate
// holds while the auth stack stays self-contained and unit-testable.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

// SessionTTL is the absolute idle lifetime of a dashboard session; each request
// that passes RequireSession slides it forward (rolling renewal). Unused past the
// TTL ⇒ expired + swept.
const SessionTTL = 12 * time.Hour

// CookieName is the dashboard-scoped session cookie. CSRFHeader is the
// double-submit header a state-changing POST must echo.
const (
	CookieName = "anselm_dash"
	CSRFHeader = "X-CSRF-Token"
)

// Session is one logged-in session: an unguessable id (the cookie value), a bound
// CSRF token (double-submit defense), the operator handle (the audit actor), and
// an absolute expiry that slides on use.
type Session struct {
	ID      string
	CSRF    string
	User    string
	Expires time.Time
}

// SessionStore is the server-side, in-memory session table. Tokens live ONLY here
// (never in the cookie beyond the opaque id), so logout / TTL expiry truly
// invalidates them. RWMutex-guarded for concurrent reads vs occasional writes.
type SessionStore struct {
	mu   sync.RWMutex
	byID map[string]*Session
	ttl  time.Duration
	now  func() time.Time
}

// NewSessionStore builds a store with the given TTL (<=0 ⇒ SessionTTL). now is the
// injectable clock (tests drive expiry); nil ⇒ time.Now.
func NewSessionStore(ttl time.Duration, now func() time.Time) *SessionStore {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	if now == nil {
		now = time.Now
	}
	return &SessionStore{byID: make(map[string]*Session), ttl: ttl, now: now}
}

// randToken returns a 256-bit crypto/rand hex token (session id + CSRF token).
//
// A crypto/rand failure is never swallowed: a silently zeroed token would be a
// predictable/forgeable id or CSRF secret (an auth bypass), so we panic rather
// than ever mint a weak value. rand.Read only fails on a broken entropy source —
// an unrecoverable condition we must not serve under.
func randToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Create mints a fresh session for user with a new id + CSRF token and TTL.
func (s *SessionStore) Create(user string) *Session {
	sess := &Session{ID: randToken(), CSRF: randToken(), User: user, Expires: s.now().Add(s.ttl)}
	s.mu.Lock()
	s.byID[sess.ID] = sess
	s.mu.Unlock()
	return sess
}

// get returns the live session for id, sliding its expiry forward, or (nil,false)
// when absent/expired. An expired hit is swept inline.
func (s *SessionStore) get(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if now.After(sess.Expires) {
		delete(s.byID, id)
		return nil, false
	}
	sess.Expires = now.Add(s.ttl) // sliding renewal: active sessions never expire mid-work.
	return sess, true
}

// Destroy invalidates a session id (logout). Idempotent.
func (s *SessionStore) Destroy(id string) {
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
}

// Sweep drops all expired sessions so the map cannot grow with abandoned entries.
func (s *SessionStore) Sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.byID {
		if now.After(sess.Expires) {
			delete(s.byID, id)
		}
	}
}

// ctxKey scopes the session stashed by RequireSession into the request context.
type ctxKey string

const sessionCtxKey ctxKey = "dash_session"

// SessionFrom returns the session RequireSession stashed in ctx, or nil.
func SessionFrom(ctx context.Context) *Session {
	if v, ok := ctx.Value(sessionCtxKey).(*Session); ok {
		return v
	}
	return nil
}

func withSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey, sess)
}

// ConstantTimeEq compares two strings without leaking length/content timing.
func ConstantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
