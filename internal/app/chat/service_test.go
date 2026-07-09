package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
)

// okAuth is an authenticated active install.
func okAuth() fakeAuth { return fakeAuth{id: "inst1", status: dominstall.StatusActive, found: true} }

func TestStreaming_RelayAndSettleOnUsage(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: "data: {\"choices\":[]}\ndata: {\"usage\":{\"total_tokens\":33}}\ndata: [DONE]\n"}
	svc, wg := build(Deps{
		Auth: okAuth(), Quota: q, Upstream: up,
		RL: &fakeRL{allow: true}, Throttle: fakeThrottle{}, Metrics: mx,
	})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodStreamBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 {
		t.Fatalf("status: %d", sink.statusCode())
	}
	if ct := sink.header("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %q", ct)
	}
	if !strings.Contains(sink.bodyString(), "[DONE]") {
		t.Fatalf("frames not relayed: %q", sink.bodyString())
	}
	if got := q.settles(); len(got) != 1 || got[0] != 33 {
		t.Fatalf("expected settle to actual 33, got %v", got)
	}
	if q.rollbacks() != 0 {
		t.Fatalf("no rollback on success, got %d", q.rollbacks())
	}
}

func TestNonStream_RelayAndSettle(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"id":"x","usage":{"total_tokens":12}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 || sink.header("Content-Type") != "application/json" {
		t.Fatalf("status=%d ct=%q", sink.statusCode(), sink.header("Content-Type"))
	}
	if got := q.settles(); len(got) != 1 || got[0] != 12 {
		t.Fatalf("expected settle 12, got %v", got)
	}
}

func TestNonStream_NoUsageSettlesFullEst(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"id":"x"}`} // no usage object.
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	got := q.settles()
	if len(got) != 1 || got[0] != q.reserved {
		t.Fatalf("absent usage must settle full est %d, got %v", q.reserved, got)
	}
}

func TestPreOutputFailure_RollsBackAll(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{aerr: apierr.ErrUpstreamError}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != apierr.ErrUpstreamError.Status {
		t.Fatalf("status: %d", sink.statusCode())
	}
	if q.rollbacks() != 1 {
		t.Fatalf("pre-output failure must roll back budget, got %d", q.rollbacks())
	}
	if len(q.settles()) != 0 {
		t.Fatalf("no settle on pre-output failure, got %v", q.settles())
	}
}

func TestClientDisconnectMidStream_KeepsCount(t *testing.T) {
	// A sink whose Write fails simulates a client disconnect mid-stream. Output
	// already started, so it must SETTLE (count kept), never roll back.
	q := &fakeQuota{}
	up := &fakeUpstream{body: "data: {\"choices\":[]}\ndata: more\n"} // no usage frame before "disconnect".
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := &disconnectSink{fakeSink: newFakeSink(), failAfter: 1}
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodStreamBody)}, sink.appSink())
	wg.Wait()

	if q.rollbacks() != 0 {
		t.Fatalf("mid-stream disconnect must NOT roll back (count kept), got %d rollbacks", q.rollbacks())
	}
	got := q.settles()
	if len(got) != 1 || got[0] != q.reserved {
		t.Fatalf("disconnect before usage must settle full est, got %v", got)
	}
}

func TestBreakerOpen_ShedsWithoutSlot(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{breakerOpen: true}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != apierr.ErrUpstreamBusy.Status {
		t.Fatalf("status: %d", sink.statusCode())
	}
	if q.rollbacks() != 1 {
		t.Fatalf("breaker-open must roll back reservation, got %d", q.rollbacks())
	}
	// No slot was taken: all N slots remain free.
	if len(svc.sem) != 0 {
		t.Fatalf("breaker-shed must not occupy an N_global slot, sem len=%d", len(svc.sem))
	}
}

func TestQueueWaitTimeout_BusyVsClientCancel(t *testing.T) {
	cfg := testCfg()
	cfg.NGlobalConcurrency = 1
	cfg.QueueWait = 30 * time.Millisecond

	// --- timeout → UPSTREAM_BUSY ---
	block := make(chan struct{})
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"total_tokens":1}}`, block: block}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})

	// Occupy the single slot with a blocked request.
	hold := newFakeSink()
	var holdWG sync.WaitGroup
	holdWG.Add(1)
	go func() {
		defer holdWG.Done()
		svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, hold)
	}()
	waitForSlot(t, svc, 1)

	// Second request must time out queuing → UPSTREAM_BUSY, reservation rolled back.
	busy := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, busy)
	if busy.statusCode() != apierr.ErrUpstreamBusy.Status {
		t.Fatalf("queue timeout status: %d", busy.statusCode())
	}

	// --- client cancel → 499 (no body, no busy) ---
	cancelSink := newFakeSink()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before queueing.
	svc.Handle(ctx, HandleInput{Token: "t", Body: []byte(goodBody)}, cancelSink)
	if cancelSink.statusCode() != 0 || cancelSink.bodyString() != "" {
		t.Fatalf("client-cancel must write nothing: status=%d body=%q", cancelSink.statusCode(), cancelSink.bodyString())
	}

	close(block)
	holdWG.Wait()
	wg.Wait()

	// Both the timed-out and the canceled request rolled back; the held one settled.
	if q.rollbacks() < 2 {
		t.Fatalf("queue-timeout + client-cancel should each roll back, got %d", q.rollbacks())
	}
}

func TestNGreaterThanOne_400(t *testing.T) {
	q := &fakeQuota{}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	body := `{"model":"x","messages":[{"role":"user","content":"a"}],"n":3}`
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(body)}, sink)
	wg.Wait()
	if sink.statusCode() != 400 {
		t.Fatalf("n>1 must 400, got %d", sink.statusCode())
	}
	if len(q.settles()) != 0 || q.rollbacks() != 0 {
		t.Fatalf("n>1 rejected before reserve; no accounting")
	}
}

func TestDangerFieldsStripped_ToolsPassed(t *testing.T) {
	q := &fakeQuota{}
	// Capture the marshaled payload by failing upstream and inspecting nothing —
	// instead assert at the domain level via a spy upstream that records payload.
	spy := &payloadSpy{}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: spy, RL: &fakeRL{allow: true}})
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}],"top_p":0.5,"logit_bias":{"1":2}}`
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(body)}, sink)
	wg.Wait()
	p := string(spy.payload)
	if !strings.Contains(p, `"tools"`) {
		t.Fatalf("tools must pass through: %s", p)
	}
	for _, banned := range []string{"top_p", "logit_bias"} {
		if strings.Contains(p, banned) {
			t.Fatalf("danger field %q leaked upstream: %s", banned, p)
		}
	}
}

func TestSettleErrorObservability_B2(t *testing.T) {
	q := &fakeQuota{settleErr: errBoom}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: `{"usage":{"total_tokens":5}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if mx.settleFailures() != 1 {
		t.Fatalf("a failed settle must be COUNTED (B2), got %d", mx.settleFailures())
	}
}

func TestBannedAndAuthFailures(t *testing.T) {
	cases := []struct {
		name   string
		auth   fakeAuth
		token  string
		status int
	}{
		{"no token", okAuth(), "", apierr.ErrInvalidToken.Status},
		{"not found", fakeAuth{found: false}, "t", apierr.ErrInvalidToken.Status},
		{"lookup error", fakeAuth{err: errBoom}, "t", 500},
		{"banned", fakeAuth{id: "i", status: dominstall.StatusBanned, found: true}, "t", apierr.ErrAccountBanned.Status},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQuota{}
			svc, wg := build(Deps{Auth: c.auth, Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{Token: c.token, Body: []byte(goodBody)}, sink)
			wg.Wait()
			if sink.statusCode() != c.status {
				t.Fatalf("%s: status %d want %d", c.name, sink.statusCode(), c.status)
			}
			if len(q.settles()) != 0 {
				t.Fatalf("auth reject must not reserve/settle")
			}
		})
	}
}

func TestRateLimited_AndDiskDegrade(t *testing.T) {
	// rate limited
	q := &fakeQuota{}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: false}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if sink.statusCode() != apierr.ErrRateLimited.Status {
		t.Fatalf("rate-limit status: %d", sink.statusCode())
	}

	// disk degrade sheds before reserve
	q2 := &fakeQuota{}
	svc2, wg2 := build(Deps{Auth: okAuth(), Quota: q2, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}, Disk: fakeDisk{degraded: true}})
	sink2 := newFakeSink()
	svc2.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink2)
	wg2.Wait()
	if sink2.statusCode() != apierr.ErrDiskLow.Status {
		t.Fatalf("disk-low status: %d", sink2.statusCode())
	}
	if q2.reserved != 0 {
		t.Fatalf("disk degrade must shed BEFORE reserve")
	}
}

func TestReserveDenial_Surfaces(t *testing.T) {
	q := &fakeQuota{reserveErr: apierr.ErrBudgetExhausted}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if sink.statusCode() != apierr.ErrBudgetExhausted.Status {
		t.Fatalf("reserve denial status: %d", sink.statusCode())
	}
}

func TestBodyError_400(t *testing.T) {
	svc, wg := build(Deps{Auth: okAuth(), Quota: &fakeQuota{}, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", BodyError: errBoom}, sink)
	wg.Wait()
	if sink.statusCode() != 400 {
		t.Fatalf("body error must 400, got %d", sink.statusCode())
	}
}

func TestInputTokenCapZero_DisablesGateAndReachesReserve(t *testing.T) {
	// INPUT_TOKEN_CAP=0 disables the input gate: a prompt whose estimate far
	// exceeds any plausible nonzero cap must pass through to Reserve (the model's
	// own context limit judges instead). InstallDailyTokenCap stays 0 in testCfg
	// so the 4b pre-reserve gate is disabled too.
	cfg := testCfg()
	cfg.InputTokenCap = 0
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"total_tokens":7}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})

	// ~60k chars → promptEst ≈ 72k (runes ×1.2), far above e.g. a 16384 cap.
	body := `{"model":"x","messages":[{"role":"user","content":"` + strings.Repeat("a", 60_000) + `"}]}`
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(body)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 {
		t.Fatalf("cap=0 must disable the input gate, got status %d body=%q", sink.statusCode(), sink.bodyString())
	}
	// Reserve WAS reached, with est = promptEst + maxTok (the estimate still
	// feeds the reservation even when the gate is off).
	if q.reserved <= 60_000 {
		t.Fatalf("Reserve not reached with the full estimate: reserved=%d", q.reserved)
	}
	if got := q.settles(); len(got) != 1 {
		t.Fatalf("success path must settle once, got %v", got)
	}
}

func TestEstOverInstallDailyTokenCap_400BeforeReserve(t *testing.T) {
	// est = promptEst + clamped max_tokens. With MaxTokensCap=1000 the clamp
	// yields 1000 even for a tiny prompt, so est > InstallDailyTokenCap=500 —
	// such a request can NEVER succeed and must 400 BEFORE any reservation.
	cfg := testCfg()
	cfg.InstallDailyTokenCap = 500 // < MaxTokensCap (1000) ⇒ est always over.
	q := &fakeQuota{}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 400 {
		t.Fatalf("est over install-day cap must 400, got %d", sink.statusCode())
	}
	if !strings.Contains(sink.bodyString(), "request exceeds the per-install daily token capacity") {
		t.Fatalf("wrong 4b message: %q", sink.bodyString())
	}
	if q.reserved != 0 || len(q.settles()) != 0 || q.rollbacks() != 0 {
		t.Fatalf("4b gate must reject BEFORE Reserve: reserved=%d settles=%v rollbacks=%d",
			q.reserved, q.settles(), q.rollbacks())
	}
}

func TestUpstreamRejected_400RollsBackAndMarksRejected(t *testing.T) {
	// A normalized upstream rejection (ADR-011 amendment) surfaces as 400
	// UPSTREAM_REJECTED with the coarse details.reason, rolls the reservation
	// back (pre-output failure), and is metered under the "rejected" label —
	// never "error" (an oversized-prompt burst must not read as an outage).
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{aerr: apierr.UpstreamRejected(apierr.RejectedContextLength)}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{Token: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 400 {
		t.Fatalf("upstream rejection must surface 400, got %d", sink.statusCode())
	}
	body := sink.bodyString()
	if !strings.Contains(body, apierr.CodeUpstreamRejected) || !strings.Contains(body, apierr.RejectedContextLength) {
		t.Fatalf("envelope must carry UPSTREAM_REJECTED + reason enum, got %q", body)
	}
	if q.rollbacks() != 1 {
		t.Fatalf("pre-output rejection must roll back the reservation, got %d rollbacks", q.rollbacks())
	}
	if len(q.settles()) != 0 {
		t.Fatalf("no settle on a rejection, got %v", q.settles())
	}
	if got := mx.upstreamCount("rejected"); got != 1 {
		t.Fatalf("metric label: upstream{rejected}=%d want 1", got)
	}
	if got := mx.upstreamCount("error"); got != 0 {
		t.Fatalf("a rejection must not count as upstream{error}, got %d", got)
	}
}

// --- helpers ---

// waitForSlot spins until n slots are occupied (the held request acquired one).
func waitForSlot(t *testing.T, s *Service, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.sem) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("slot never occupied")
}
