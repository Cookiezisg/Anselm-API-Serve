package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

type observedRequest struct {
	path string
	auth string
}

// Two clients are two ACCOUNTS, whether or not they are two vendors. The
// gateway wires one provider today, so the realistic second client is a second
// credential against the same vendor — and that is precisely the case where a
// shared transport, breaker or key pool would be invisible until one account's
// outage started shedding the other account's traffic.
//
// 两个 client 就是两个**账号**,不论它们是不是两家厂商。本网关今天只接一家,故现实中的第二个
// client 是**同一家的第二把凭证**——而那恰恰是「共享 transport / 熔断器 / key 池」最难被发现的
// 场景:直到一个账号的故障开始甩掉另一个账号的流量为止。
func TestBackendConstructionFreezesEndpointKeysAndIdentity(t *testing.T) {
	primarySeen := make(chan observedRequest, 8)
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primarySeen <- observedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"account":"primary"}`)
	}))
	defer primaryServer.Close()

	secondarySeen := make(chan observedRequest, 8)
	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondarySeen <- observedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"account":"secondary"}`)
	}))
	defer secondaryServer.Close()

	primaryKeys := []string{"  primary-key  "}
	primaryOpts := Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: primaryServer.URL + "/primary/chat/completions",
		APIKeys:            primaryKeys,
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	primary := NewBackend(primaryOpts).(*client)
	secondary := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: secondaryServer.URL + "/chat/completions",
		APIKeys:            []string{"secondary-key"},
		HeaderTimeout:      2 * time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)

	// Mutating the caller-owned options and slice after construction must not
	// alter any wire or resilience fact held by the client.
	primaryOpts.Backend = BackendID("mutated")
	primaryOpts.ChatCompletionsURL = secondaryServer.URL + "/wrong"
	primaryOpts.HeaderTimeout = 10 * time.Second
	primaryKeys[0] = "mutated-key"

	if primary.backend != BackendQwen || secondary.backend != BackendQwen {
		t.Fatalf("backend identity was not frozen: primary=%q secondary=%q", primary.backend, secondary.backend)
	}
	if primary.timeout != time.Second || secondary.timeout != 2*time.Second {
		t.Fatalf("header timeouts were not frozen: primary=%v secondary=%v", primary.timeout, secondary.timeout)
	}
	if primary.transport == secondary.transport {
		t.Fatal("accounts must not share a transport")
	}
	if primary.breaker == secondary.breaker {
		t.Fatal("accounts must not share a process breaker")
	}
	if primary.transport.keys[0].cb == secondary.transport.keys[0].cb {
		t.Fatal("accounts must not share per-key breakers")
	}

	primaryStream, primaryErr := primary.DoCall(context.Background(), Call{Payload: []byte(`{"model":"a"}`)})
	if primaryErr != nil {
		t.Fatalf("primary call failed: %v", primaryErr)
	}
	if got := drain(t, primaryStream); got != `{"account":"primary"}` {
		t.Fatalf("primary response = %q", got)
	}

	secondaryStream, secondaryErr := secondary.DoCall(context.Background(), Call{Payload: []byte(`{"model":"b"}`)})
	if secondaryErr != nil {
		t.Fatalf("secondary call failed: %v", secondaryErr)
	}
	if got := drain(t, secondaryStream); got != `{"account":"secondary"}` {
		t.Fatalf("secondary response = %q", got)
	}

	if len(primarySeen) != 1 {
		t.Fatalf("primary server calls = %d, want 1", len(primarySeen))
	}
	if got := <-primarySeen; got != (observedRequest{path: "/primary/chat/completions", auth: "Bearer primary-key"}) {
		t.Fatalf("primary request = %#v", got)
	}
	if len(secondarySeen) != 1 {
		t.Fatalf("secondary server calls = %d, want 1", len(secondarySeen))
	}
	if got := <-secondarySeen; got != (observedRequest{path: "/chat/completions", auth: "Bearer secondary-key"}) {
		t.Fatalf("secondary request = %#v", got)
	}
}

func TestBackendProcessBreakersAreIndependent(t *testing.T) {
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failingServer.Close()

	var healthyCalls atomic.Int32
	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer healthyServer.Close()

	failing := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: failingServer.URL + "/v1/chat/completions",
		APIKeys:            []string{"failing-key"},
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)
	healthy := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: healthyServer.URL + "/chat/completions",
		APIKeys:            []string{"healthy-key"},
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)
	failing.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}
	healthy.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	for i := 0; i < 6; i++ {
		_, _ = failing.DoCall(context.Background(), Call{Payload: []byte("{}")})
	}
	if !failing.BreakerOpen() {
		t.Fatal("persistent faults should open the faulting account's breaker")
	}
	if healthy.BreakerOpen() {
		t.Fatal("a healthy account's breaker must be independent of another account's health")
	}

	stream, ae := healthy.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if ae != nil {
		t.Fatalf("healthy account was contaminated by the failing account's breaker: %v", ae)
	}
	_ = drain(t, stream)
	if got := healthyCalls.Load(); got != 1 {
		t.Fatalf("healthy backend calls = %d, want 1", got)
	}
}

func TestNilEmptyAndBlankKeyPoolsFailClosed(t *testing.T) {
	var upstreamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cases := []struct {
		name string
		keys []string
	}{
		{name: "nil", keys: nil},
		{name: "empty", keys: []string{}},
		{name: "blank", keys: []string{""}},
		{name: "whitespace", keys: []string{" ", "\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/backend", func(t *testing.T) {
			c := NewBackend(Options{
				Backend:            BackendQwen,
				ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
				APIKeys:            tc.keys,
				HeaderTimeout:      time.Second,
			}).(*client)
			if len(c.transport.keys) != 0 {
				t.Fatalf("usable key count = %d, want 0", len(c.transport.keys))
			}
			stream, ae := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
			assertConfigurationFailure(t, stream, ae)
			if c.BreakerOpen() {
				t.Fatal("an empty key pool must not charge the process breaker")
			}
		})

	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("invalid key pools reached upstream %d times", got)
	}
}

func TestEmptyTransportRoundTripReturnsErrorInsteadOfPanicking(t *testing.T) {
	transport := newRedactingTransport(nil, nil, nil, BackendQwen)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("an empty transport must not return a response")
	}
	if !errors.Is(err, errNoUsableAPIKey) {
		t.Fatalf("RoundTrip error = %v, want %v", err, errNoUsableAPIKey)
	}
}

func TestInvalidEndpointsFailClosedBeforeBreaker(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"relative":       "/v1/chat/completions",
		"wrong scheme":   "ftp://example.com/v1/chat/completions",
		"remote HTTP":    "http://example.com/v1/chat/completions",
		"localhost HTTP": "http://localhost:18080/v1/chat/completions",
		"userinfo":       "https://user:pass@example.com/v1/chat/completions",
		"query":          "https://example.com/v1/chat/completions?key=secret",
		"fragment":       "https://example.com/v1/chat/completions#secret",
	}
	for name, endpoint := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewBackend(Options{
				Backend:            BackendQwen,
				ChatCompletionsURL: endpoint,
				APIKeys:            []string{"qwen-key"},
				HeaderTimeout:      time.Second,
			}).(*client)
			stream, ae := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
			assertConfigurationFailure(t, stream, ae)
			if c.BreakerOpen() {
				t.Fatal("an invalid endpoint must not charge the process breaker")
			}
		})
	}
}

func TestEndpointTransportPolicyAllowsHTTPSAndLoopbackHTTP(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"remote HTTPS": "https://example.com/v1/chat/completions",
		"IPv4 HTTP":    "http://127.0.0.1:18080/v1/chat/completions",
		"IPv6 HTTP":    "http://[::1]:18080/v1/chat/completions",
	}
	for name, endpoint := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewBackend(Options{
				Backend:            BackendQwen,
				ChatCompletionsURL: endpoint,
				APIKeys:            []string{"qwen-key"},
				HeaderTimeout:      time.Second,
			}).(*client)
			if c.endpoint != endpoint {
				t.Fatalf("normalized endpoint = %q, want %q", c.endpoint, endpoint)
			}
		})
	}
}

func TestBlankKeysAreSkippedWhenUsableKeysRemain(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"", "  usable-key  ", "\t"},
		HeaderTimeout:      time.Second,
	}).(*client)
	if len(c.transport.keys) != 1 || c.transport.keys[0].id != 1 {
		t.Fatalf("usable key pool = %#v, want only original index 1", c.transport.keys)
	}
	stream, ae := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if ae != nil {
		t.Fatalf("call with one usable key failed: %v", ae)
	}
	_ = drain(t, stream)
	if auth != "Bearer usable-key" {
		t.Fatalf("Authorization = %q, want trimmed usable key", auth)
	}
}

func TestQwenArrayRejectionThroughDoCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `[{"error":{"code":400,"message":"Invalid max_tokens for this model.","status":"INVALID_ARGUMENT"}}]`)
	}))
	defer srv.Close()

	c := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"qwen-key"},
		HeaderTimeout:      time.Second,
	}).(*client)
	stream, ae := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("a rejected call must not return a stream")
	}
	if ae == nil || ae.APIError == nil || ae.APIError.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("Qwen rejection = %v, want %s", ae, apierr.CodeUpstreamRejected)
	}
	if got := ae.APIError.Details["reason"]; got != apierr.RejectedMaxTokens {
		t.Fatalf("rejection reason = %v, want %s", got, apierr.RejectedMaxTokens)
	}
	if ae.Exposure != billing.DefinitelyUnbilled {
		t.Fatalf("rejection exposure = %v, want definitely unbilled", ae.Exposure)
	}
	if calls.Load() != 1 {
		t.Fatalf("Qwen rejection calls = %d, want 1", calls.Load())
	}
	if c.BreakerOpen() {
		t.Fatal("a Qwen request rejection must not charge its process breaker")
	}
}

func TestBackendFailureExposureIsIndependentOfNormalizedCode(t *testing.T) {
	tests := []struct {
		name string
		code int
		want billing.ChargeExposure
	}{
		{name: "request rejection", code: http.StatusBadRequest, want: billing.DefinitelyUnbilled},
		{name: "key auth refusal", code: http.StatusUnauthorized, want: billing.DefinitelyUnbilled},
		{name: "account balance refusal", code: http.StatusPaymentRequired, want: billing.DefinitelyUnbilled},
		{name: "rate limit refusal", code: http.StatusTooManyRequests, want: billing.DefinitelyUnbilled},
		{name: "provider failure", code: http.StatusInternalServerError, want: billing.ChargePossible},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			c := NewBackend(Options{
				Backend:            BackendQwen,
				ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
				APIKeys:            []string{"qwen-key"},
				HeaderTimeout:      time.Second,
			}).(*client)
			c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

			stream, failure := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
			if stream != nil {
				_ = stream.Close()
				t.Fatal("failed request returned a stream")
			}
			if failure == nil || failure.APIError == nil {
				t.Fatalf("status %d returned no normalized failure", tc.code)
			}
			if failure.Exposure != tc.want {
				t.Fatalf("status %d exposure = %v, want %v", tc.code, failure.Exposure, tc.want)
			}
		})
	}
}

func TestBackendNeverFollowsRedirectOrLeaksKeyToLocation(t *testing.T) {
	var targetCalls atomic.Int32
	var targetAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		targetAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	var originAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Authorization")
		w.Header().Set("Location", target.URL+"/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	c := NewBackend(Options{
		Backend:            BackendQwen,
		ChatCompletionsURL: origin.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"redirect-secret"},
		HeaderTimeout:      time.Second,
	}).(*client)
	c.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	stream, failure := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("redirect response must not become a success stream")
	}
	if failure == nil || failure.APIError == nil || failure.Exposure != billing.DefinitelyUnbilled {
		t.Fatalf("redirect failure = %#v, want normalized definitely-unbilled refusal", failure)
	}
	if originAuth != "Bearer redirect-secret" {
		t.Fatalf("configured origin auth = %q", originAuth)
	}
	if got := targetCalls.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests; key isolation requires zero", got)
	}
	if v := targetAuth.Load(); v != nil {
		t.Fatalf("redirect target received Authorization %q", v)
	}
}

func assertConfigurationFailure(t *testing.T, stream *Stream, failure *CallFailure) {
	t.Helper()
	if stream != nil {
		_ = stream.Close()
		t.Fatal("configuration failure must not return a stream")
	}
	if failure == nil || failure.APIError == nil || failure.APIError.Code != apierr.ErrUpstreamError.Code {
		t.Fatalf("configuration failure = %v, want %s", failure, apierr.ErrUpstreamError.Code)
	}
	if failure.Exposure != billing.DefinitelyUnbilled {
		t.Fatalf("configuration exposure = %v, want definitely unbilled", failure.Exposure)
	}
}
