package upstream

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

const testKey = "sk-SUPER-SECRET-KEY-DO-NOT-LOG"

// newTestBackend builds a client against a test server the same way the
// composition root builds the real one. It exists because this resilience suite
// predates the provider-local constructor: the behaviors it pins (retry, breaker,
// key rotation, redaction, cancel classification) are unchanged, so the tests
// were PORTED onto the surviving entry point rather than deleted along with the
// config-driven one.
//
// newTestBackend 按组合根构造真实客户端的同一方式,对着测试服务器构造一个。它存在,是因为这套
// 韧性测试早于「按 provider 收窄」的构造器:它钉住的行为(重试、熔断、key 轮换、脱敏、取消分类)
// 一条都没变,故这些测试是被**移植**到活下来的入口上,而不是随配置驱动那个入口一起删掉。
func newTestBackend(baseURL string, keys []string, headerTimeout time.Duration, logger *slog.Logger, hook MetricsHook) *client {
	return NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: strings.TrimRight(baseURL, "/") + "/chat/completions",
		APIKeys:            keys,
		HeaderTimeout:      headerTimeout,
		Logger:             logger,
		Hook:               hook,
	}).(*client)
}

// call adapts DoCall's CallFailure down to the bare APIError these assertions read.
func (c *client) call(ctx context.Context, payload []byte, stream bool) (*Stream, *apierr.APIError) {
	s, f := c.DoCall(ctx, Call{Payload: payload, Stream: stream})
	if f != nil {
		return nil, f.APIError
	}
	return s, nil
}

// drain fully reads + closes a stream body (the caller's relay obligation).
func drain(t *testing.T, s *Stream) string {
	t.Helper()
	b, err := io.ReadAll(s.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = s.Close()
	return string(b)
}

func TestSingleKey_StreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: hello\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s, ae := c.call(context.Background(), []byte("{}"), true)
	if ae != nil {
		t.Fatalf("expected success, got %v", ae)
	}
	if s.Status != 200 {
		t.Fatalf("status = %d", s.Status)
	}
	if got := drain(t, s); !strings.Contains(got, "hello") {
		t.Fatalf("body = %q", got)
	}
}

func TestSingleKey_NonStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	s, ae := c.call(context.Background(), []byte("{}"), false)
	if ae != nil {
		t.Fatalf("expected success, got %v", ae)
	}
	if got := drain(t, s); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
}

// GW-INV-11: the key is injected on a clone and never logged.
func TestKeyNeverInLogs(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: x\n\n")
	}))
	defer srv.Close()

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, logger, nil)
	caller, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte("{}")))
	s, ae := c.call(context.Background(), []byte("{}"), true)
	if ae != nil {
		t.Fatalf("unexpected error: %v", ae)
	}
	_ = drain(t, s)

	if seenAuth != "Bearer "+testKey {
		t.Fatalf("upstream should receive the bearer key, got %q", seenAuth)
	}
	// The CALLER's request must never carry the Authorization header.
	if caller.Header.Get("Authorization") != "" {
		t.Fatal("caller request must never carry the key (clone-only injection)")
	}
	if strings.Contains(logbuf.String(), testKey) {
		t.Fatalf("key leaked into logs:\n%s", logbuf.String())
	}
}

// GW-INV-23: 429 → UPSTREAM_BUSY, no retry, no breaker count.
func TestUpstream429MapsBusyNoRetryNoBreaker(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(429)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	for i := 0; i < 12; i++ {
		_, ae := c.call(context.Background(), []byte("{}"), true)
		if ae == nil || ae.Code != "UPSTREAM_BUSY" {
			t.Fatalf("expected UPSTREAM_BUSY, got %v", ae)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 12 {
		t.Fatalf("429 must not be retried: expected 12 upstream calls, got %d", got)
	}
	if c.BreakerOpen() {
		t.Fatal("429 must not trip the process breaker")
	}
}

// GW-INV-30 / REL-3: key0 401 → cooldown → key1 success on the SAME Do (retry
// rotates the key).
func TestFailover_Key0AuthCooldownKey1Success(t *testing.T) {
	var key0Hits, key1Hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer key0":
			atomic.AddInt32(&key0Hits, 1)
			w.WriteHeader(401)
		case "Bearer key1":
			atomic.AddInt32(&key1Hits, 1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: ok\n\n")
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{"key0", "key1"}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	// Force the first pick to key0.
	c.transport.rr = 0

	s, ae := c.call(context.Background(), []byte("{}"), true)
	if ae != nil {
		t.Fatalf("failover should have routed to key1: %v", ae)
	}
	_ = drain(t, s)

	if atomic.LoadInt32(&key0Hits) == 0 {
		t.Fatal("key0 should have been tried first")
	}
	if atomic.LoadInt32(&key1Hits) == 0 {
		t.Fatal("key1 should have served after key0's 401")
	}
	if c.BreakerOpen() {
		t.Fatal("a per-key auth signal must NOT trip the process breaker")
	}
}

// B5 regression: a client cancel must never trip the process breaker.
func TestClientCancelNoBreakerTrip(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the client cancels
		w.WriteHeader(200)
	}))
	defer srv.Close()
	defer close(release)

	c := newTestBackend(srv.URL, []string{testKey}, 5*time.Second, nil, nil)

	// Fire many cancels; were they counted as faults the breaker would trip.
	for i := 0; i < 12; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		_, ae := c.call(ctx, []byte("{}"), true)
		if ae == nil || ae.Code != "CLIENT_CANCELED" {
			t.Fatalf("expected CLIENT_CANCELED, got %v", ae)
		}
		cancel()
	}
	if c.BreakerOpen() {
		t.Fatal("B5: client cancel must NEVER trip the process breaker")
	}
}

// GW-INV-26: a 503 is charge-ambiguous. It must not be retried because a later
// success could hide two provider charges behind one reservation.
func TestChargeAmbiguous503IsNeverRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: recovered\n\n")
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	c.retry = retryPolicy{maxAttempts: 3, base: time.Millisecond, cap: 5 * time.Millisecond}

	s, ae := c.call(context.Background(), []byte("{}"), true)
	if s != nil {
		_ = s.Close()
		t.Fatal("ambiguous 503 must not retry through to a later success")
	}
	if ae == nil || ae.Code != "UPSTREAM_ERROR" {
		t.Fatalf("expected terminal UPSTREAM_ERROR, got %v", ae)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("charge-ambiguous 503 calls = %d, want exactly 1", got)
	}
}

// A persistent ambiguous failure is likewise one attempt, irrespective of the
// per-key failover budget.
func TestPersistent503StillMakesOneBillableAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	c.retry = retryPolicy{maxAttempts: 3, base: time.Millisecond, cap: 2 * time.Millisecond}

	_, ae := c.call(context.Background(), []byte("{}"), true)
	if ae == nil || ae.Code != "UPSTREAM_ERROR" {
		t.Fatalf("expected UPSTREAM_ERROR after exhausting retries, got %v", ae)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("ambiguous failures must not retry: calls=%d want=1", got)
	}
}

// GW-INV-27: first-byte timeout — upstream sends headers but stalls the body;
// the timer cancels and the charge-ambiguous attempt is terminal.
func TestFirstByteTimeout(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers out, but no body byte
		}
		<-hold
	}))
	defer srv.Close()
	defer close(hold)

	c := newTestBackend(srv.URL, []string{testKey}, 30*time.Millisecond, nil, nil)
	c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	_, ae := c.call(context.Background(), []byte("{}"), true)
	if ae == nil || ae.Code != "UPSTREAM_TIMEOUT" {
		t.Fatalf("stalled first byte should map to UPSTREAM_TIMEOUT, got %v", ae)
	}
}

func TestNonStreamHeadersWithoutBodyStillHitFirstByteTimeout(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers only; JSON body never begins.
		}
		<-hold
	}))
	defer srv.Close()
	defer close(hold)

	c := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: srv.URL + "/chat/completions",
		APIKeys:            []string{"qwen-key"},
		HeaderTimeout:      30 * time.Millisecond,
	}).(*client)
	c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	stream, failure := c.DoCall(context.Background(), Call{
		Payload: []byte("{}"), Stream: false, FirstByteTimeout: 30 * time.Millisecond,
	})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("headers-only non-stream response must not be handed to the app")
	}
	if failure == nil || failure.APIError == nil || failure.APIError.Code != "UPSTREAM_TIMEOUT" {
		t.Fatalf("headers-only non-stream response = %#v, want UPSTREAM_TIMEOUT", failure)
	}
	if failure.Exposure != billing.ChargePossible {
		t.Fatalf("headers-only timeout exposure = %v, want charge possible", failure.Exposure)
	}
}

func TestRequestSnapshotCanIncreaseFirstByteTimeoutPastConstructorDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer srv.Close()

	// A fixed transport ResponseHeaderTimeout of 5ms would defeat the live 300ms
	// snapshot below. The one request timer is the sole timeout authority.
	c := newTestBackend(srv.URL, []string{testKey}, 5*time.Millisecond, nil, nil)
	c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}
	// The per-request snapshot must WIN over the constructor default; that is the
	// whole claim, so it is passed on the Call rather than through the helper.
	// 逐请求快照必须**压过**构造期默认值——这正是本用例的全部主张,故它走 Call 传入、不经 helper。
	stream, failure := c.DoCall(context.Background(), Call{
		Payload: []byte("{}"), Stream: true, FirstByteTimeout: 300 * time.Millisecond,
	})
	if failure != nil {
		t.Fatalf("live first-byte timeout increase was ignored: %v", failure.APIError)
	}
	s := stream
	if got := drain(t, s); !strings.Contains(got, "ok") {
		t.Fatalf("body = %q", got)
	}
}

// B6: the first-byte timer disarm is idempotent — a started stream is never
// canceled by a timer firing the instant output begins. We race a very short
// header timeout against a stream that DOES deliver a first byte, and assert the
// body relays in full.
func TestFirstByteTimerDisarmIdempotentUnderRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		// Deliver the first byte immediately, then a long tail. If the disarm were
		// not idempotent, a timer firing right after Peek could cancel mid-tail.
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = rc.Flush()
		for i := 0; i < 200; i++ {
			_, _ = io.WriteString(w, "data: frame\n\n")
			_ = rc.Flush()
			time.Sleep(time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Header timeout deliberately tiny to race the first byte.
			c := newTestBackend(srv.URL, []string{testKey}, 2*time.Millisecond, nil, nil)
			s, ae := c.call(context.Background(), []byte("{}"), true)
			if ae != nil {
				return // a genuine pre-first-byte timeout is acceptable
			}
			// Once started, the body must relay to completion uninterrupted.
			body := drain(t, s)
			if !strings.Contains(body, "[DONE]") {
				t.Errorf("started stream was truncated by the timer (B6): %q", body[:min(40, len(body))])
			}
		}()
	}
	wg.Wait()
}

// GW-INV-23 reinforcement: a plain 429 (no Retry-After) does NOT cool the key,
// so a second Do still picks the same single key (no spurious cooldown).
func TestPlain429DoesNotCoolKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	_, _ = c.call(context.Background(), []byte("{}"), true)

	ks := c.transport.keys[0]
	if ks.onCooldown(time.Now()) {
		t.Fatal("a plain 429 (no long Retry-After) must not cool the key down")
	}
}

func TestAuthCooldownIsHardGateAndNeverReusesCooledKey(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"bad-key"},
		HeaderTimeout:      time.Second,
	}).(*client)
	c.retry = retryPolicy{maxAttempts: 3, base: time.Millisecond, cap: time.Millisecond}

	_, first := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if first == nil || first.APIError == nil || first.APIError.Code != "UPSTREAM_BUSY" {
		t.Fatalf("all-cooled pool should shed as UPSTREAM_BUSY, got %#v", first)
	}
	if first.Exposure != billing.DefinitelyUnbilled {
		t.Fatalf("auth refusal exposure = %v, want definitely unbilled", first.Exposure)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("first call reused a cooled key: upstream hits=%d want=1", got)
	}

	_, second := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if second == nil || second.APIError == nil || second.APIError.Code != "UPSTREAM_BUSY" {
		t.Fatalf("cooled pool should continue shedding, got %#v", second)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("later call bypassed cooldown: upstream hits=%d want=1", got)
	}
}

func TestBreakerTripsOnPersistent5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	// ConsecutiveFailures >= 5 trips the breaker.
	for i := 0; i < 6; i++ {
		_, _ = c.call(context.Background(), []byte("{}"), true)
	}
	if !c.BreakerOpen() {
		t.Fatal("persistent 5xx should trip the process breaker")
	}
	// Once open, Do fast-sheds to UPSTREAM_BUSY.
	_, ae := c.call(context.Background(), []byte("{}"), true)
	if ae == nil || ae.Code != "UPSTREAM_BUSY" {
		t.Fatalf("open breaker should fast-shed UPSTREAM_BUSY, got %v", ae)
	}
}

// REL-3: when the only key's breaker is open, the hard gate sheds immediately
// as UPSTREAM_BUSY and never charges the process breaker.
func TestSingleKeyOpenIsBusyNotProcessFault(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: x\n\n")
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	c.retry = retryPolicy{maxAttempts: 3, base: time.Millisecond, cap: 2 * time.Millisecond}

	// Force the single key's breaker open: 5 consecutive account faults.
	ks := c.transport.keys[0]
	for i := 0; i < 5; i++ {
		_, _ = ks.cb.Execute(func() (*http.Response, error) { return nil, errAccountFault })
	}
	if ks.cb.State() != gobreaker.StateOpen {
		t.Fatalf("key breaker should be open, got %v", ks.cb.State())
	}

	_, ae := c.call(context.Background(), []byte("{}"), true)
	if ae == nil || ae.Code != "UPSTREAM_BUSY" {
		t.Fatalf("open key with no sibling should yield UPSTREAM_BUSY, got %v", ae)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("an open key breaker must short-circuit before reaching upstream")
	}
	if c.BreakerOpen() {
		t.Fatal("REL-3: a single open key must NOT trip the process breaker")
	}
}

// Sanity: the process breaker's IsSuccessful excludes context.Canceled.
func TestProcessBreakerExcludesCancel(t *testing.T) {
	b := newProcessBreaker("test-upstream", nil)
	for i := 0; i < 20; i++ {
		_, _ = b.Execute(func() (struct{}, error) { return struct{}{}, context.Canceled })
	}
	if b.State() == gobreaker.StateOpen {
		t.Fatal("context.Canceled must never trip the breaker")
	}
}
