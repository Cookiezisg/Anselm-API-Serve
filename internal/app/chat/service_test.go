package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
)

func ptrInt64(v int64) *int64 { return &v }

// okAuth is an authenticated active install.
func okAuth() fakeAuth { return fakeAuth{id: "inst1", status: dominstall.StatusActive, found: true} }

func TestStreaming_RelayAndSettleOnUsage(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: "data: {\"choices\":[]}\ndata: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":13,\"total_tokens\":33}}\ndata: [DONE]\n"}
	svc, wg := build(Deps{
		Auth: okAuth(), Quota: q, Upstream: up,
		RL: &fakeRL{allow: true}, Throttle: fakeThrottle{}, Metrics: mx,
	})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink)
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
	want := int64(20*140_000 + 13*280_000)
	if got := q.settles(); len(got) != 1 || got[0] != want {
		t.Fatalf("expected provider-priced settle %d, got %v", want, got)
	}
	if q.rollbacks() != 0 {
		t.Fatalf("no rollback on success, got %d", q.rollbacks())
	}
	if got := mx.upstreamCount(billing.ProviderDeepSeek, "success"); got != 1 {
		t.Fatalf("valid stream outcome success=%d want 1", got)
	}
}

func TestStreamingNegativeUsageCannotBeClampedIntoARefund(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{body: "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":-1,\"total_tokens\":100}}\ndata: [DONE]\n"}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, newFakeSink())
	wg.Wait()
	if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("malformed stream usage must retain full quote %d, got %v", q.reservedPUSD, got)
	}
}

func TestNonStream_RelayAndSettle(t *testing.T) {
	q := &fakeQuota{}
	const upstreamBody = `{"id":"x","usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
	up := &fakeUpstream{body: upstreamBody}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 || sink.header("Content-Type") != "application/json" {
		t.Fatalf("status=%d ct=%q", sink.statusCode(), sink.header("Content-Type"))
	}
	if got := sink.bodyString(); got != upstreamBody {
		t.Fatalf("valid model-free object must pass byte-for-byte: got=%q want=%q", got, upstreamBody)
	}
	want := int64(7*140_000 + 5*280_000)
	if got := q.settles(); len(got) != 1 || got[0] != want {
		t.Fatalf("expected provider-priced settle %d, got %v", want, got)
	}
}

func TestNonStream_NoUsageSettlesFullEst(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"id":"x"}`} // no usage object.
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	got := q.settles()
	if len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("absent usage must settle full quote %d, got %v", q.reservedPUSD, got)
	}
}

func TestNonStream_OversizedUpstreamBodyFailsClosedAndKeepsQuote(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{body: strings.Repeat("x", nonStreamBodyLimit+1)}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if sink.statusCode() != apierr.ErrUpstreamError.Status {
		t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
	}
	if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("oversized ambiguous body must retain quote: reserved=%d settles=%v", q.reservedPUSD, got)
	}
}

func TestNonStreamMalformedOrNonObjectBodyFailsClosedBefore200(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "truncated object", body: `{"model":"deepseek-v4-flash"`},
		{name: "array", body: `["qwen3.7-plus"]`},
		{name: "scalar", body: `"deepseek-v4-flash"`},
		{name: "null", body: `null`},
		{name: "trailing garbage", body: `{"model":"deepseek-v4-flash"} provider-debug`},
		{name: "second value", body: `{"model":"deepseek-v4-flash"}{"model":"qwen3.7-plus"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			mx := newFakeMetrics()
			up := &fakeUpstream{body: tc.body}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
			wg.Wait()

			if sink.statusCode() != apierr.ErrUpstreamError.Status {
				t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
			}
			if sink.header("Content-Type") != "application/json" || !strings.Contains(sink.bodyString(), `"code":"UPSTREAM_ERROR"`) {
				t.Fatalf("want normalized JSON 502 envelope, headers/body=%q/%q", sink.header("Content-Type"), sink.bodyString())
			}
			for _, providerModel := range []string{billing.DeepSeekV4Flash, billing.Qwen37Plus, "provider-debug"} {
				if strings.Contains(sink.bodyString(), providerModel) {
					t.Fatalf("provider payload leaked through normalized failure: %q", sink.bodyString())
				}
			}
			if q.rollbacks() != 0 {
				t.Fatalf("provider 2xx exposure must never rollback, got %d", q.rollbacks())
			}
			if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
				t.Fatalf("malformed body must retain full quote %d, got %v", q.reservedPUSD, got)
			}
			if got := mx.upstreamCount(billing.ProviderDeepSeek, "error"); got != 1 {
				t.Fatalf("malformed body outcome error=%d want 1", got)
			}
			if got := mx.upstreamCount(billing.ProviderDeepSeek, "success"); got != 0 {
				t.Fatalf("malformed body must not count success, got %d", got)
			}
		})
	}
}

func TestStreamingMalformedDataFrameIsSuppressedAndKeepsFullQuote(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: ": keep-alive\n" +
		`data: {"model":"deepseek-v4-flash","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n" +
		`data: ["deepseek-v4-flash"]` + "\n" +
		`data: {"model":"deepseek-v4-flash","choices":[{"delta":{"content":"must-not-pass"}}]}` + "\n" +
		"data: [DONE]\n"}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 || sink.header("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status/content-type=%d/%q", sink.statusCode(), sink.header("Content-Type"))
	}
	want := ":\n" +
		`data: {"model":"anselm-auto","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n"
	if got := sink.bodyString(); got != want {
		t.Fatalf("malformed frame or tail crossed public boundary\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(sink.bodyString(), billing.DeepSeekV4Flash) || strings.Contains(sink.bodyString(), "must-not-pass") {
		t.Fatalf("provider-owned bytes leaked: %q", sink.bodyString())
	}
	if q.rollbacks() != 0 {
		t.Fatalf("stream output/exposure must not rollback, got %d", q.rollbacks())
	}
	if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("malformed frame must override earlier usage and retain full quote %d, got %v", q.reservedPUSD, got)
	}
	if mx.upstreamCount(billing.ProviderDeepSeek, "error") != 1 || mx.upstreamCount(billing.ProviderDeepSeek, "success") != 0 {
		t.Fatalf("malformed frame outcomes: error=%d success=%d", mx.upstreamCount(billing.ProviderDeepSeek, "error"), mx.upstreamCount(billing.ProviderDeepSeek, "success"))
	}
}

func TestStreamingOversizedFrameIsNeverPartiallyRelayedAndKeepsFullQuote(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: `data: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n" +
		"data: {\"model\":\"" + billing.DeepSeekV4Flash + `","padding":"` + strings.Repeat("x", 1024*1024) + `"}` + "\n"}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink)
	wg.Wait()

	if got := sink.bodyString(); got != `data: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n" {
		t.Fatalf("oversized frame was partially relayed: len=%d tail=%q", len(got), got[max(0, len(got)-64):])
	}
	if strings.Contains(sink.bodyString(), billing.DeepSeekV4Flash) {
		t.Fatalf("oversized provider model leaked: %q", sink.bodyString())
	}
	if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("scanner failure must retain full quote %d, got %v", q.reservedPUSD, got)
	}
	if mx.upstreamCount(billing.ProviderDeepSeek, "error") != 1 || mx.upstreamCount(billing.ProviderDeepSeek, "success") != 0 {
		t.Fatalf("oversized frame outcomes: error=%d success=%d", mx.upstreamCount(billing.ProviderDeepSeek, "error"), mx.upstreamCount(billing.ProviderDeepSeek, "success"))
	}
}

func TestAmbiguousOpenFailuresKeepFullReservation(t *testing.T) {
	for _, aerr := range []*apierr.APIError{
		apierr.ErrUpstreamError,
		apierr.ErrUpstreamTimeout,
		apierr.NewError(499, "CLIENT_CANCELED", "client canceled the request"),
	} {
		t.Run(aerr.Code, func(t *testing.T) {
			q := &fakeQuota{}
			up := &fakeUpstream{aerr: aerr}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
			wg.Wait()

			if sink.statusCode() != aerr.Status {
				t.Fatalf("status: %d want %d", sink.statusCode(), aerr.Status)
			}
			if q.rollbacks() != 0 {
				t.Fatalf("ambiguous Open outcome must not rollback, got %d", q.rollbacks())
			}
			if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
				t.Fatalf("ambiguous Open outcome must keep full quote %d, got %v", q.reservedPUSD, got)
			}
		})
	}
}

func TestExplicitBusyFromOpenIsDefinitelyUnbilledAndRollsBack(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{aerr: apierr.ErrUpstreamBusy, exposure: billing.DefinitelyUnbilled}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, newFakeSink())
	wg.Wait()
	if q.rollbacks() != 1 || len(q.settles()) != 0 {
		t.Fatalf("explicit busy must rollback only: rollbacks=%d settles=%v", q.rollbacks(), q.settles())
	}
}

func TestDefinitelyUnbilledExposureRollsBackEvenWithGenericErrorCode(t *testing.T) {
	// A provider 401/402 is normalized to UPSTREAM_ERROR for the client but is an
	// explicit pre-generation refusal for accounting. This regression proves the
	// app consumes the independent exposure fact instead of guessing from Code.
	q := &fakeQuota{}
	up := &fakeUpstream{aerr: apierr.ErrUpstreamError, exposure: billing.DefinitelyUnbilled}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, newFakeSink())
	wg.Wait()
	if q.rollbacks() != 1 || len(q.settles()) != 0 {
		t.Fatalf("explicit refusal must rollback only: rollbacks=%d settles=%v", q.rollbacks(), q.settles())
	}
}

func TestClientDisconnectMidStream_KeepsCount(t *testing.T) {
	// A sink whose Write fails simulates a client disconnect mid-stream. Output
	// already started, so it must SETTLE (count kept), never roll back.
	q := &fakeQuota{}
	up := &fakeUpstream{body: "data: {\"choices\":[]}\ndata: more\n"} // no usage frame before "disconnect".
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := &disconnectSink{fakeSink: newFakeSink(), failAfter: 1}
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink.appSink())
	wg.Wait()

	if q.rollbacks() != 0 {
		t.Fatalf("mid-stream disconnect must NOT roll back (count kept), got %d rollbacks", q.rollbacks())
	}
	got := q.settles()
	if len(got) != 1 || got[0] != q.reservedPUSD {
		t.Fatalf("disconnect before usage must settle full est, got %v", got)
	}
}

func TestStreamingUsageBeforeDONECannotRefundOnEOFOrDisconnect(t *testing.T) {
	usageLine := `data: {"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n"
	for _, tc := range []struct {
		name string
		body string
		sink Sink
	}{
		{name: "clean EOF", body: usageLine, sink: newFakeSink()},
		{name: "client disconnect", body: usageLine + `data: {"choices":[]}` + "\n" + "data: [DONE]\n",
			sink: (&disconnectSink{fakeSink: newFakeSink(), failAfter: 2}).appSink()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			mx := newFakeMetrics()
			up := &fakeUpstream{body: tc.body}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, tc.sink)
			wg.Wait()

			if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
				t.Fatalf("non-terminal usage must retain full quote %d, got %v", q.reservedPUSD, got)
			}
			if got := mx.upstreamCount(billing.ProviderDeepSeek, "error"); got != 1 {
				t.Fatalf("incomplete stream outcome error=%d want 1", got)
			}
		})
	}
}

func TestStreamingDONEReadBeforeClientWriteFailureAllowsUsageSettle(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{body: `data: {"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n" +
		"data: [DONE]\n"}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	// The usage line and its newline succeed; writing the already-read DONE line
	// fails on the third Write.
	sink := &disconnectSink{fakeSink: newFakeSink(), failAfter: 2}
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink.appSink())
	wg.Wait()

	want := int64(10*140_000 + 2*280_000)
	if got := q.settles(); len(got) != 1 || got[0] != want {
		t.Fatalf("terminal usage settle=%v want %d", got, want)
	}
	if got := mx.upstreamCount(billing.ProviderDeepSeek, "success"); got != 1 {
		t.Fatalf("fully read provider stream outcome success=%d want 1", got)
	}
}

func TestBreakerOpen_ShedsWithoutSlot(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{breakers: map[billing.Provider]bool{billing.ProviderDeepSeek: true}}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
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
		svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, hold)
	}()
	waitForSlot(t, svc, 1)

	// Second request must time out queuing → UPSTREAM_BUSY, reservation rolled back.
	busy := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, busy)
	if busy.statusCode() != apierr.ErrUpstreamBusy.Status {
		t.Fatalf("queue timeout status: %d", busy.statusCode())
	}

	// --- client cancel → 499 (no body, no busy) ---
	cancelSink := newFakeSink()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before queueing.
	svc.Handle(ctx, HandleInput{InstallID: "t", Body: []byte(goodBody)}, cancelSink)
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
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
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
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()
	raw, err := json.Marshal(spy.request)
	if err != nil {
		t.Fatalf("marshal captured request: %v", err)
	}
	p := string(raw)
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
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if mx.settleFailures() != 1 {
		t.Fatalf("a failed settle must be COUNTED (B2), got %d", mx.settleFailures())
	}
}

func TestBannedAndAuthFailures(t *testing.T) {
	cases := []struct {
		name      string
		auth      fakeAuth
		installID string
		status    int
	}{
		{"no install id", okAuth(), "", apierr.ErrInvalidInstall.Status},
		{"not found", fakeAuth{found: false}, "t", apierr.ErrInvalidInstall.Status},
		{"lookup error", fakeAuth{err: errBoom}, "t", 500},
		{"banned", fakeAuth{id: "i", status: dominstall.StatusBanned, found: true}, "t", apierr.ErrAccountBanned.Status},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQuota{}
			svc, wg := build(Deps{Auth: c.auth, Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: c.installID, Body: []byte(goodBody)}, sink)
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
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if sink.statusCode() != apierr.ErrRateLimited.Status {
		t.Fatalf("rate-limit status: %d", sink.statusCode())
	}

	// disk degrade sheds before reserve
	q2 := &fakeQuota{}
	svc2, wg2 := build(Deps{Auth: okAuth(), Quota: q2, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}, Disk: fakeDisk{degraded: true}})
	sink2 := newFakeSink()
	svc2.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink2)
	wg2.Wait()
	if sink2.statusCode() != apierr.ErrDiskLow.Status {
		t.Fatalf("disk-low status: %d", sink2.statusCode())
	}
	if q2.reservedPUSD != 0 {
		t.Fatalf("disk degrade must shed BEFORE reserve")
	}
}

func TestReserveDenial_Surfaces(t *testing.T) {
	q := &fakeQuota{reserveErr: apierr.ErrBudgetExhausted}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	if sink.statusCode() != apierr.ErrBudgetExhausted.Status {
		t.Fatalf("reserve denial status: %d", sink.statusCode())
	}
}

func TestBodyError_400(t *testing.T) {
	svc, wg := build(Deps{Auth: okAuth(), Quota: &fakeQuota{}, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", BodyError: errBoom}, sink)
	wg.Wait()
	if sink.statusCode() != 400 {
		t.Fatalf("body error must 400, got %d", sink.statusCode())
	}
}

func TestBodyTooLarge_413DistinctFromContext(t *testing.T) {
	svc, wg := build(Deps{Auth: okAuth(), Quota: &fakeQuota{}, Upstream: &fakeUpstream{}, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", BodyTooLarge: true, BodyError: errBoom}, sink)
	wg.Wait()
	if sink.statusCode() != 413 || !strings.Contains(sink.bodyString(), "REQUEST_BODY_TOO_LARGE") {
		t.Fatalf("body cap must be precise: status=%d body=%s", sink.statusCode(), sink.bodyString())
	}
}

func TestDeepSeekOverLimitByteEstimateIsClampedForReserveAndForwarded(t *testing.T) {
	req, derr := domchat.DecodeInbound([]byte(goodBody))
	if derr != nil {
		t.Fatal(derr)
	}
	plan, ae := billingPlan(
		billing.ProviderDeepSeek, billing.DeepSeekV4Flash, req,
		billing.DeepSeekInputLimit+500_000, 16_384,
	)
	if ae != nil {
		t.Fatalf("accounting estimate must not reject: %v", ae)
	}
	if plan.PromptQuote != billing.DeepSeekInputLimit {
		t.Fatalf("prompt quote=%d want model limit=%d", plan.PromptQuote, billing.DeepSeekInputLimit)
	}
}

func TestLegacyInputTokenCapDoesNotGateAndRequestReachesReserve(t *testing.T) {
	// INPUT_TOKEN_CAP is compatibility-only: even a tiny nonzero legacy value
	// must not turn the conservative accounting estimate into an admission
	// decision. The provider owns the real context limit.
	cfg := testCfg()
	cfg.InputTokenCap = 1
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})

	// ~60k chars is far above the deliberately tiny legacy value.
	body := `{"model":"x","messages":[{"role":"user","content":"` + strings.Repeat("a", 60_000) + `"}]}`
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()

	if sink.statusCode() != 200 {
		t.Fatalf("legacy input cap must not gate, got status %d body=%q", sink.statusCode(), sink.bodyString())
	}
	// Reserve still receives the prompt quote: removing admission does not make
	// forwarded prompt/tool bytes free.
	if q.plan.PromptQuote <= 60_000 {
		t.Fatalf("Reserve not reached with the full prompt quote: plan=%+v", q.plan)
	}
	if got := q.settles(); len(got) != 1 {
		t.Fatalf("success path must settle once, got %v", got)
	}
}

func TestUpstreamRejected_400RollsBackAndMarksRejected(t *testing.T) {
	// A normalized upstream rejection (ADR-011 amendment) surfaces as 400
	// UPSTREAM_REJECTED with the coarse details.reason, rolls the reservation
	// back (pre-output failure), and is metered under the "rejected" label —
	// never "error" (an oversized-prompt burst must not read as an outage).
	q := &fakeQuota{}
	mx := newFakeMetrics()
	up := &fakeUpstream{
		aerr:     apierr.UpstreamRejected(apierr.RejectedContextLength),
		exposure: billing.DefinitelyUnbilled,
	}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
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
	if got := mx.upstreamCount(billing.ProviderDeepSeek, "rejected"); got != 1 {
		t.Fatalf("metric label: upstream{rejected}=%d want 1", got)
	}
	if got := mx.upstreamCount(billing.ProviderDeepSeek, "error"); got != 0 {
		t.Fatalf("a rejection must not count as upstream{error}, got %d", got)
	}
}

func TestDeterministicProviderRoutingIgnoresClientModel(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	mediaBody := `{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]},` +
		`{"role":"assistant","content":"seen"},` +
		`{"role":"user","content":"last turn is text"}` +
		`]}`

	tests := []struct {
		name         string
		body         string
		wantProvider billing.Provider
		wantModel    string
	}{
		{"text", `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hello"}]}`,
			billing.ProviderDeepSeek, billing.DeepSeekV4Flash},
		{"media anywhere in history", mediaBody, billing.ProviderQwen, billing.Qwen37Plus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			up := &fakeUpstream{body: `{"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(tc.body)}, sink)
			wg.Wait()
			if sink.statusCode() != 200 {
				t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
			}
			calls := up.callSnapshot()
			if len(calls) != 1 || calls[0].Provider != tc.wantProvider || calls[0].Model != tc.wantModel {
				t.Fatalf("calls=%+v, want one %s/%s", calls, tc.wantProvider, tc.wantModel)
			}
			if calls[0].Timeout != testCfg().UpstreamHeaderTimeout {
				t.Fatalf("first-byte timeout=%s want request snapshot %s", calls[0].Timeout, testCfg().UpstreamHeaderTimeout)
			}
			if q.plan.Provider != tc.wantProvider || q.plan.Model != tc.wantModel {
				t.Fatalf("reserved plan=%+v", q.plan)
			}
		})
	}
}

func TestClientFacingModelIsPublicAcrossProvidersAndResponseModes(t *testing.T) {
	const publicModel = "anselm-client"
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})

	requestBody := func(media, stream bool) string {
		streamJSON := "false"
		if stream {
			streamJSON = "true"
		}
		if media {
			return `{"model":"client-cannot-pick","stream":` + streamJSON + `,"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
		}
		return `{"model":"client-cannot-pick","stream":` + streamJSON + `,"messages":[{"role":"user","content":"hello"}]}`
	}

	tests := []struct {
		name          string
		media         bool
		stream        bool
		provider      billing.Provider
		upstreamModel string
	}{
		{name: "DeepSeek non-stream", provider: billing.ProviderDeepSeek, upstreamModel: billing.DeepSeekV4Flash},
		{name: "DeepSeek stream", stream: true, provider: billing.ProviderDeepSeek, upstreamModel: billing.DeepSeekV4Flash},
		{name: "Qwen non-stream", media: true, provider: billing.ProviderQwen, upstreamModel: billing.Qwen37Plus},
		{name: "Qwen stream", media: true, stream: true, provider: billing.ProviderQwen, upstreamModel: billing.Qwen37Plus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamBody, wantClientBody string
			if tc.stream {
				upstreamBody = `data: {"id":"chunk-1", "model" : "` + tc.upstreamModel + `","choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}` + "\n" +
					`data: {"model":"` + tc.upstreamModel + `","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n" +
					"data: [DONE]\n"
				wantClientBody = `data: {"id":"chunk-1", "model" : "` + publicModel + `","choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}` + "\n" +
					`data: {"model":"` + publicModel + `","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n" +
					"data: [DONE]\n"
			} else {
				upstreamBody = `{"id":"cmpl-1", "model" : "` + tc.upstreamModel + `","choices":[{"message":{"tool_calls":[{"id":"call_1"}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
				wantClientBody = `{"id":"cmpl-1", "model" : "` + publicModel + `","choices":[{"message":{"tool_calls":[{"id":"call_1"}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
			}

			cfg := testCfg()
			cfg.PublicModelID = publicModel
			q := &fakeQuota{}
			up := &fakeUpstream{body: upstreamBody}
			svc, wg := build(Deps{
				Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true},
				Config: fakeConfig{c: cfg},
			})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(requestBody(tc.media, tc.stream))}, sink)
			wg.Wait()

			if sink.statusCode() != 200 {
				t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
			}
			if got := sink.bodyString(); got != wantClientBody {
				t.Fatalf("client response changed outside model rewrite\n got: %s\nwant: %s", got, wantClientBody)
			}
			calls := up.callSnapshot()
			if len(calls) != 1 || calls[0].Provider != tc.provider || calls[0].Model != tc.upstreamModel {
				t.Fatalf("upstream calls=%+v want one %s/%s", calls, tc.provider, tc.upstreamModel)
			}
		})
	}
}

func TestQwenUnavailableIsExplicitAndNeverFallsBack(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
	q := &fakeQuota{}
	up := &fakeUpstream{available: map[billing.Provider]bool{
		billing.ProviderDeepSeek: true,
		billing.ProviderQwen:     false,
	}, body: `{}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()

	if sink.statusCode() != 503 || !strings.Contains(sink.bodyString(), "MULTIMODAL_UNAVAILABLE") {
		t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
	}
	if q.reservedPUSD != 0 || len(up.callSnapshot()) != 0 {
		t.Fatalf("unavailable route must reject before reserve/open: reserved=%d calls=%+v", q.reservedPUSD, up.callSnapshot())
	}

	// The absent Qwen credential is isolated: text remains on DeepSeek.
	textQ := &fakeQuota{}
	textSvc, textWG := build(Deps{Auth: okAuth(), Quota: textQ, Upstream: up, RL: &fakeRL{allow: true}})
	textSink := newFakeSink()
	textSvc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, textSink)
	textWG.Wait()
	if textSink.statusCode() != 200 {
		t.Fatalf("text must remain available, status=%d body=%s", textSink.statusCode(), textSink.bodyString())
	}
	calls := up.callSnapshot()
	if len(calls) != 1 || calls[0].Provider != billing.ProviderDeepSeek {
		t.Fatalf("text route calls=%+v", calls)
	}
}

func TestAudioIsExplicitlyUnavailableBeforeReserveOrRouting(t *testing.T) {
	// Audio is intentionally a valid protocol part, but the current deterministic table has no
	// audio upstream. This must never silently become a Qwen request or consume a wallet slot.
	wav := base64.StdEncoding.EncodeToString([]byte("RIFF\x04\x00\x00\x00WAVEfmt "))
	body := `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"wav"}}]}]}`
	q := &fakeQuota{}
	up := &fakeUpstream{}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()

	if sink.statusCode() != 503 || !strings.Contains(sink.bodyString(), "AUDIO_UNAVAILABLE") {
		t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
	}
	if q.reservedPUSD != 0 || len(up.callSnapshot()) != 0 {
		t.Fatalf("audio must reject before reserve/open: reserved=%d calls=%+v", q.reservedPUSD, up.callSnapshot())
	}
}

func TestProviderBreakersAreIsolated(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	mediaBody := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
	tests := []struct {
		name        string
		body        string
		openBreaker billing.Provider
		wantRoute   billing.Provider
	}{
		{"DeepSeek open does not block Qwen", mediaBody, billing.ProviderDeepSeek, billing.ProviderQwen},
		{"Qwen open does not block DeepSeek", goodBody, billing.ProviderQwen, billing.ProviderDeepSeek},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			up := &fakeUpstream{
				breakers: map[billing.Provider]bool{tc.openBreaker: true},
				body:     `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(tc.body)}, sink)
			wg.Wait()
			if sink.statusCode() != 200 {
				t.Fatalf("status=%d body=%s", sink.statusCode(), sink.bodyString())
			}
			calls := up.callSnapshot()
			if len(calls) != 1 || calls[0].Provider != tc.wantRoute {
				t.Fatalf("calls=%+v want=%s", calls, tc.wantRoute)
			}
		})
	}
}

func TestQwenFailureAndBreakerNeverFallbackToDeepSeek(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
	for _, tc := range []struct {
		name         string
		up           *fakeUpstream
		wantRollback bool
	}{
		{"open failure", &fakeUpstream{aerr: apierr.ErrUpstreamError}, false},
		{"breaker open", &fakeUpstream{breakers: map[billing.Provider]bool{billing.ProviderQwen: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: tc.up, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
			wg.Wait()
			for _, call := range tc.up.callSnapshot() {
				if call.Provider != billing.ProviderQwen {
					t.Fatalf("multimodal request fell back: calls=%+v", tc.up.callSnapshot())
				}
			}
			if got := q.rollbacks(); (got == 1) != tc.wantRollback {
				t.Fatalf("rollbacks=%d wantRollback=%v", got, tc.wantRollback)
			}
			if tc.wantRollback && len(q.settles()) != 0 {
				t.Fatalf("definitely-unbilled failure must not settle: %v", q.settles())
			}
			if !tc.wantRollback {
				if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
					t.Fatalf("ambiguous failure must retain quote: %v", got)
				}
			}
		})
	}
}

func TestStructuredUsagePricingAndStreamSnapshotsUseMax(t *testing.T) {
	t.Run("DeepSeek cache dimensions", func(t *testing.T) {
		q := &fakeQuota{}
		up := &fakeUpstream{body: `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":6}}`}
		svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
		sink := newFakeSink()
		svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
		wg.Wait()
		want := int64(4*2_800 + 6*140_000 + 2*280_000)
		if got := q.settles(); len(got) != 1 || got[0] != want {
			t.Fatalf("settles=%v want=%d", got, want)
		}
	})

	t.Run("stream cumulative snapshots take max not sum", func(t *testing.T) {
		q := &fakeQuota{}
		up := &fakeUpstream{body: "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n" +
			"data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n" +
			"data: [DONE]\n"}
		svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
		sink := newFakeSink()
		svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodStreamBody)}, sink)
		wg.Wait()
		want := int64(12*140_000 + 3*280_000)
		if got := q.settles(); len(got) != 1 || got[0] != want {
			t.Fatalf("snapshots were summed or mispriced: settles=%v want=%d", got, want)
		}
	})

	t.Run("contradictory usage keeps reservation", func(t *testing.T) {
		q := &fakeQuota{}
		up := &fakeUpstream{body: `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":99}}`}
		svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
		svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, newFakeSink())
		wg.Wait()
		if got := q.settles(); len(got) != 1 || got[0] != q.reservedPUSD {
			t.Fatalf("malformed usage must retain full reservation: reserved=%d got=%v", q.reservedPUSD, got)
		}
	})
}

func TestQwenHardQuoteForImages(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	tests := []struct {
		name      string
		body      string
		wantClass billing.InputClass
		inputRate int64
	}{
		{"image standard", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`, billing.InputStandard, 1_600_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuota{}
			up := &fakeUpstream{body: `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}})
			sink := newFakeSink()
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(tc.body)}, sink)
			wg.Wait()
			if q.plan.Provider != billing.ProviderQwen || q.plan.PromptQuote != billing.Qwen37InputLimit || q.plan.OutputQuote != billing.Qwen37OutputLimit || q.plan.InputClass != tc.wantClass {
				t.Fatalf("Qwen hard plan=%+v", q.plan)
			}
			wantCost := 10*tc.inputRate + 2*int64(1_600_000)
			if got := q.settles(); len(got) != 1 || got[0] != wantCost {
				t.Fatalf("settles=%v want=%d", got, wantCost)
			}
		})
	}
}

func TestBillingDriftSettlesTruthAndEmitsMetricWarn(t *testing.T) {
	q := &fakeQuota{}
	mx := newFakeMetrics()
	logger := &fakeLogger{}
	cfg := testCfg()
	cfg.MaxTokensCap = 1
	up := &fakeUpstream{body: `{"usage":{"prompt_tokens":100,"completion_tokens":100,"total_tokens":200}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Metrics: mx, Logger: logger, Config: fakeConfig{c: cfg}})
	sink := newFakeSink()
	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()
	want := int64(100*140_000 + 100*280_000)
	if got := q.settles(); len(got) != 1 || got[0] != want || got[0] <= q.reservedPUSD {
		t.Fatalf("actual top-up must settle truth: reserved=%d got=%v want=%d", q.reservedPUSD, got, want)
	}
	if mx.driftCount(billing.ProviderDeepSeek) != 1 || logger.warningCount() != 1 {
		t.Fatalf("drift observability: metric=%d warns=%d", mx.driftCount(billing.ProviderDeepSeek), logger.warningCount())
	}
}

func TestPayloadMaxTokensBoundedForWireAndQuote(t *testing.T) {
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	tests := []struct {
		name      string
		body      string
		wantWire  *int64
		wantQuote int64
	}{
		{"DeepSeek absent max_tokens explicitly capped on wire and quote", `{"messages":[{"role":"user","content":"hi"}]}`, ptrInt64(billing.DeepSeekOutputLimit), billing.DeepSeekOutputLimit},
		{"DeepSeek high client max_tokens capped", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":999999}`, ptrInt64(billing.DeepSeekOutputLimit), billing.DeepSeekOutputLimit},
		{"DeepSeek lower client max_tokens respected", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, ptrInt64(1), 1},
		{"Qwen high client max_tokens capped on wire", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}],"max_tokens":999999}`, ptrInt64(billing.Qwen37OutputLimit), billing.Qwen37OutputLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.MaxTokensCap = 1_000_000
			q := &fakeQuota{}
			up := &fakeUpstream{}
			svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})
			svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(tc.body)}, newFakeSink())
			wg.Wait()
			calls := up.callSnapshot()
			if len(calls) != 1 {
				t.Fatalf("calls=%+v", calls)
			}
			if (calls[0].Request.MaxTokens == nil) != (tc.wantWire == nil) {
				t.Fatalf("wire max_tokens=%v want %v", calls[0].Request.MaxTokens, tc.wantWire)
			}
			if tc.wantWire != nil && *calls[0].Request.MaxTokens != *tc.wantWire {
				t.Fatalf("wire max_tokens=%v want %d", calls[0].Request.MaxTokens, *tc.wantWire)
			}
			if q.plan.OutputQuote != tc.wantQuote {
				t.Fatalf("plan=%+v want quote=%d", q.plan, tc.wantQuote)
			}
		})
	}
}

func TestResponseHeadersReuseEntryConfigSnapshot(t *testing.T) {
	cfgSource := &countingConfig{c: testCfg()}
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: cfgSource})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(goodBody)}, sink)
	wg.Wait()
	// One startup read fixes semaphore capacity; exactly one entry read owns the
	// whole request, including response headers.
	if got := cfgSource.count(); got != 2 {
		t.Fatalf("config loads=%d want 2 (startup + one request snapshot)", got)
	}
}

func TestQwenPromptEstimateBeyondModelLimitStillReachesUpstream(t *testing.T) {
	cfg := testCfg()
	cfg.InputTokenCap = 1 // compatibility-only; must have no execution effect.
	cfg.MaxMessageChars = 2_000_000
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("a", int(billing.Qwen37InputLimit)) +
		`"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()
	calls := len(up.callSnapshot())
	if sink.statusCode() != 200 || q.reservedPUSD == 0 || calls != 1 {
		t.Fatalf("conservative estimate must not gate: status=%d reserved=%d call_count=%d", sink.statusCode(), q.reservedPUSD, calls)
	}
}

func TestMultimodalAccountingEstimateDoesNotTokenizeBase64TransportText(t *testing.T) {
	cfg := testCfg()
	cfg.InputTokenCap = 1 // proves this legacy setting is irrelevant to admission.
	cfg.MaxMediaDecodedBytes = 128 * 1024
	png := append([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}, make([]byte, 64*1024)...)
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`
	q := &fakeQuota{}
	up := &fakeUpstream{body: `{"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`}
	svc, wg := build(Deps{Auth: okAuth(), Quota: q, Upstream: up, RL: &fakeRL{allow: true}, Config: fakeConfig{c: cfg}})
	sink := newFakeSink()
	svc.Handle(context.Background(), HandleInput{InstallID: "t", Body: []byte(body)}, sink)
	wg.Wait()
	if sink.statusCode() != 200 || len(up.callSnapshot()) != 1 {
		t.Fatalf("media transport encoding was treated as text tokens: status=%d body=%s calls=%d", sink.statusCode(), sink.bodyString(), len(up.callSnapshot()))
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
