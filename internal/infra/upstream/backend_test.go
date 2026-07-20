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

func TestBackendConstructionFreezesEndpointKeysAndIdentity(t *testing.T) {
	deepSeekSeen := make(chan observedRequest, 8)
	deepSeekServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deepSeekSeen <- observedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"backend":"deepseek"}`)
	}))
	defer deepSeekServer.Close()

	kimiSeen := make(chan observedRequest, 8)
	kimiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kimiSeen <- observedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"backend":"kimi"}`)
	}))
	defer kimiServer.Close()

	deepSeekKeys := []string{"  deepseek-key  "}
	deepSeekOpts := Options{
		Backend:            BackendDeepSeek,
		ChatCompletionsURL: deepSeekServer.URL + "/deepseek/chat/completions",
		APIKeys:            deepSeekKeys,
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	deepSeek := NewBackend(deepSeekOpts).(*client)
	kimi := NewBackend(Options{
		Backend:            BackendKimi,
		ChatCompletionsURL: kimiServer.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"kimi-key"},
		HeaderTimeout:      2 * time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)

	// Mutating the caller-owned options and slice after construction must not
	// alter any wire or resilience fact held by the client.
	deepSeekOpts.Backend = BackendKimi
	deepSeekOpts.ChatCompletionsURL = kimiServer.URL + "/wrong"
	deepSeekOpts.HeaderTimeout = 10 * time.Second
	deepSeekKeys[0] = "mutated-key"

	if deepSeek.backend != BackendDeepSeek || kimi.backend != BackendKimi {
		t.Fatalf("backend identities were not frozen: deepseek=%q kimi=%q", deepSeek.backend, kimi.backend)
	}
	if deepSeek.timeout != time.Second || kimi.timeout != 2*time.Second {
		t.Fatalf("header timeouts were not frozen: deepseek=%v kimi=%v", deepSeek.timeout, kimi.timeout)
	}
	if deepSeek.transport == kimi.transport {
		t.Fatal("backends must not share a transport")
	}
	if deepSeek.breaker == kimi.breaker {
		t.Fatal("backends must not share a process breaker")
	}
	if deepSeek.transport.keys[0].cb == kimi.transport.keys[0].cb {
		t.Fatal("backends must not share per-key breakers")
	}

	deepSeekStream, deepSeekErr := deepSeek.DoCall(context.Background(), Call{Payload: []byte(`{"model":"deepseek-chat"}`)})
	if deepSeekErr != nil {
		t.Fatalf("deepseek call failed: %v", deepSeekErr)
	}
	if got := drain(t, deepSeekStream); got != `{"backend":"deepseek"}` {
		t.Fatalf("deepseek response = %q", got)
	}

	kimiStream, kimiErr := kimi.DoCall(context.Background(), Call{Payload: []byte(`{"model":"kimi"}`)})
	if kimiErr != nil {
		t.Fatalf("kimi call failed: %v", kimiErr)
	}
	if got := drain(t, kimiStream); got != `{"backend":"kimi"}` {
		t.Fatalf("kimi response = %q", got)
	}

	if len(deepSeekSeen) != 1 {
		t.Fatalf("DeepSeek server calls = %d, want 1", len(deepSeekSeen))
	}
	if got := <-deepSeekSeen; got != (observedRequest{path: "/deepseek/chat/completions", auth: "Bearer deepseek-key"}) {
		t.Fatalf("deepseek request = %#v", got)
	}
	if len(kimiSeen) != 1 {
		t.Fatalf("Kimi server calls = %d, want 1", len(kimiSeen))
	}
	if got := <-kimiSeen; got != (observedRequest{path: "/v1beta/openai/chat/completions", auth: "Bearer kimi-key"}) {
		t.Fatalf("kimi request = %#v", got)
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

	deepSeek := NewBackend(Options{
		Backend:            BackendDeepSeek,
		ChatCompletionsURL: failingServer.URL + "/v1/chat/completions",
		APIKeys:            []string{"deepseek-key"},
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)
	kimi := NewBackend(Options{
		Backend:            BackendKimi,
		ChatCompletionsURL: healthyServer.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"kimi-key"},
		HeaderTimeout:      time.Second,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*client)
	deepSeek.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}
	kimi.retry = retryPolicy{maxAttempts: 1, base: time.Millisecond, cap: time.Millisecond}

	for i := 0; i < 6; i++ {
		_, _ = deepSeek.DoCall(context.Background(), Call{Payload: []byte("{}")})
	}
	if !deepSeek.BreakerOpen() {
		t.Fatal("persistent DeepSeek faults should open only the DeepSeek breaker")
	}
	if kimi.BreakerOpen() {
		t.Fatal("the Kimi breaker must be independent of DeepSeek health")
	}

	stream, ae := kimi.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if ae != nil {
		t.Fatalf("healthy Kimi backend was contaminated by DeepSeek breaker: %v", ae)
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
				Backend:            BackendKimi,
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

		t.Run(tc.name+"/legacy", func(t *testing.T) {
			c := New(tc.keys, time.Second, nil, nil).(*client)
			stream, ae := c.Do(context.Background(), []byte("{}"), false, testCfg(srv.URL, time.Second))
			assertLegacyConfigurationFailure(t, stream, ae)
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
	transport := newRedactingTransport(nil, nil, nil, BackendKimi)
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
				Backend:            BackendKimi,
				ChatCompletionsURL: endpoint,
				APIKeys:            []string{"kimi-key"},
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
				Backend:            BackendKimi,
				ChatCompletionsURL: endpoint,
				APIKeys:            []string{"kimi-key"},
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
		Backend:            BackendKimi,
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

func TestKimiArrayRejectionThroughDoCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `[{"error":{"code":400,"message":"Invalid max_tokens for this model.","status":"INVALID_ARGUMENT"}}]`)
	}))
	defer srv.Close()

	c := NewBackend(Options{
		Backend:            BackendKimi,
		ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
		APIKeys:            []string{"kimi-key"},
		HeaderTimeout:      time.Second,
	}).(*client)
	stream, ae := c.DoCall(context.Background(), Call{Payload: []byte("{}")})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("a rejected call must not return a stream")
	}
	if ae == nil || ae.APIError == nil || ae.APIError.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("Kimi rejection = %v, want %s", ae, apierr.CodeUpstreamRejected)
	}
	if got := ae.APIError.Details["reason"]; got != apierr.RejectedMaxTokens {
		t.Fatalf("rejection reason = %v, want %s", got, apierr.RejectedMaxTokens)
	}
	if ae.Exposure != billing.DefinitelyUnbilled {
		t.Fatalf("rejection exposure = %v, want definitely unbilled", ae.Exposure)
	}
	if calls.Load() != 1 {
		t.Fatalf("Kimi rejection calls = %d, want 1", calls.Load())
	}
	if c.BreakerOpen() {
		t.Fatal("a Kimi request rejection must not charge its process breaker")
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
				Backend:            BackendKimi,
				ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions",
				APIKeys:            []string{"kimi-key"},
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
		Backend:            BackendKimi,
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

func assertLegacyConfigurationFailure(t *testing.T, stream *Stream, ae *apierr.APIError) {
	t.Helper()
	if stream != nil {
		_ = stream.Close()
		t.Fatal("configuration failure must not return a stream")
	}
	if ae == nil || ae.Code != apierr.ErrUpstreamError.Code {
		t.Fatalf("configuration failure = %v, want %s", ae, apierr.ErrUpstreamError.Code)
	}
}
