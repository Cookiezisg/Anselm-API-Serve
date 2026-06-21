package dashboard

import (
	"sync"
	"time"
)

// AuditEvent is one dashboard-surfaced security/audit record. It carries ONLY
// safe, low-cardinality facts (action / target install_id / operator-supplied
// reason / outcome / actor) — never a token, key, prompt, or raw IP. install_id
// is the opaque ins_<hex> handle, safe to surface.
//
// Seq is a process-monotonic write ordinal stamped at record time. It is the
// STABLE keyset pagination key: "load more" asks for events with Seq < cursor, so
// a concurrent new write (which only ever gets a HIGHER seq) can never shift a
// page boundary and skip/duplicate a row — unlike an offset over a moving ring.
type AuditEvent struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Action  string    `json:"action"`  // "ban" / "unban" / "config_update" / "export"
	Target  string    `json:"target"`  // opaque install_id or config key set (never a secret)
	Reason  string    `json:"reason"`  // operator-supplied note (bounded), never a secret
	Outcome string    `json:"outcome"` // "ok" / "not_found" / "error" / "interrupted"
	Actor   string    `json:"actor"`   // the configured admin user (audit origin)
}

// auditRing is a fixed-capacity, in-process ring of recent dashboard audit events
// — the queryable view BELOW the durable journald audit log. Bounded so it can
// never grow without limit; oldest events overwrite. Mutex-guarded for concurrent
// writers (handlers) vs the reader (GET /api/audit).
type auditRing struct {
	mu   sync.Mutex
	buf  []AuditEvent
	next int
	full bool
	seq  int64 // last assigned monotonic write ordinal
	now  func() time.Time
}

func newAuditRing(capacity int, now func() time.Time) *auditRing {
	if capacity <= 0 {
		capacity = 512
	}
	if now == nil {
		now = time.Now
	}
	return &auditRing{buf: make([]AuditEvent, capacity), now: now}
}

// record appends one event (overwriting the oldest when full). At + Seq are
// stamped here so callers cannot backdate or forge ordering. Concurrent-safe.
func (a *auditRing) record(e AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e.At = a.now().UTC()
	a.seq++
	e.Seq = a.seq
	a.buf[a.next] = e
	a.next = (a.next + 1) % len(a.buf)
	if a.next == 0 {
		a.full = true
	}
}

// page returns up to limit events, newest first, strictly OLDER than beforeSeq
// (keyset). beforeSeq<=0 means "from the newest". It also returns nextSeq (the
// seq of the last row returned, the next page's cursor) and whether more older
// events remain. The monotonic seq makes paging stable under concurrent writes.
func (a *auditRing) page(beforeSeq int64, limit int) (events []AuditEvent, nextSeq int64, more bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	total := len(a.buf)
	if !a.full {
		total = a.next
	}
	n := a.next
	out := make([]AuditEvent, 0, limit)
	for i := 0; i < total; i++ {
		idx := n - 1 - i
		for idx < 0 {
			idx += len(a.buf)
		}
		ev := a.buf[idx]
		if ev.Seq == 0 {
			continue // empty slot (defensive)
		}
		if beforeSeq > 0 && ev.Seq >= beforeSeq {
			continue
		}
		if len(out) == limit {
			// One more qualifying event exists beyond this page → older remain.
			return out, nextSeq, true
		}
		out = append(out, ev)
		nextSeq = ev.Seq
	}
	return out, nextSeq, false
}
