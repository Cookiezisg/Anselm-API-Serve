// Tests for the ADR-011 amendment: upstream request rejections (400/413/422)
// normalize to 400 UPSTREAM_REJECTED with a coarse details.reason enum, are
// terminal (no retry), and NEVER charge the process breaker — a stream of
// oversized prompts must not black-hole every other user. The upstream error
// text itself never passes through (GW-INV-11).
package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// classifyStatus is the single status→class map (ADR-011): 400/413/422 are
// request rejections; other explicit 3xx/4xx are terminal refusals, while 5xx
// statuses that are not retryable remain terminal ambiguous upstream faults.
func TestClassifyStatusTable(t *testing.T) {
	cases := []struct {
		code int
		want faultClass
	}{
		{429, classBusy},
		{401, classKeyAuth},
		{403, classKeyAuth},
		{400, classUpstreamRejected},
		{413, classUpstreamRejected},
		{422, classUpstreamRejected},
		{504, classTimeout},
		{502, classUpstream},
		{503, classUpstream},
		{402, classUpstreamRefused},
		{418, classUpstreamRefused},
		{500, classUpstreamFinal},
	}
	for _, tc := range cases {
		if got := classifyStatus(tc.code); got != tc.want {
			t.Errorf("classifyStatus(%d) = %d, want %d", tc.code, got, tc.want)
		}
	}
}

// rejectionReason maps a bounded slice of the upstream 4xx body onto the CLOSED
// apierr.Rejected* enum: tolerant of any malformed body, capped at 4KiB, and it
// never returns upstream text — only an enum value.
func TestRejectionReason(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "context overflow",
			body: `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 200069 tokens (198021 in the messages, 2048 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`,
			want: apierr.RejectedContextLength,
		},
		{
			name: "max_tokens range",
			body: `{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 8192]","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`,
			want: apierr.RejectedMaxTokens,
		},
		{
			name: "qwen array context overflow",
			body: `[{"error":{"code":400,"message":"The request exceeds this model's context length.","status":"INVALID_ARGUMENT"}}]`,
			want: apierr.RejectedContextLength,
		},
		{
			name: "qwen array max_tokens range",
			body: `[{"error":{"code":400,"message":"Invalid max_tokens for this model.","status":"INVALID_ARGUMENT"}}]`,
			want: apierr.RejectedMaxTokens,
		},
		{
			name: "qwen empty array",
			body: `[]`,
			want: apierr.RejectedInvalid,
		},
		{
			name: "case-insensitive match",
			body: `{"error":{"message":"This model's MAXIMUM CONTEXT LENGTH is 131072 tokens."}}`,
			want: apierr.RejectedContextLength,
		},
		{
			name: "provider input too large wording",
			body: `{"error":{"message":"input too large"}}`,
			want: apierr.RejectedContextLength,
		},
		{
			name: "garbage non-JSON body",
			body: "<html><body>upstream proxy said no</body></html>",
			want: apierr.RejectedInvalid,
		},
		{
			name: "empty body",
			body: "",
			want: apierr.RejectedInvalid,
		},
		{
			// > the 4KiB read bound: must truncate safely (no panic) and still
			// land on a valid enum value.
			name: "oversized body truncated",
			body: strings.Repeat("z", 8*1024),
			want: apierr.RejectedInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := rejectionReason(strings.NewReader(tc.body))
			if got != tc.want {
				t.Fatalf("rejectionReason = %q, want %q", got, tc.want)
			}
			switch got {
			case apierr.RejectedContextLength, apierr.RejectedMaxTokens, apierr.RejectedInvalid:
			default:
				t.Fatalf("rejectionReason returned a value outside the closed enum: %q", got)
			}
		})
	}
}

// Defensive resolve path + the real resolveRejected path: both are terminal
// (no retry), NON-fault (no breaker charge), and carry UPSTREAM_REJECTED. The
// reason-less resolve(classUpstreamRejected) falls back to invalid_request.
func TestResolveRejectedOutcome(t *testing.T) {
	defensive := resolve(classUpstreamRejected)
	real := resolveRejected(apierr.RejectedContextLength)
	for _, o := range []outcome{defensive, real} {
		if o.retryable {
			t.Error("a rejected outcome must not be retryable")
		}
		if o.breakerFlt {
			t.Error("a rejected outcome must not be a process-breaker fault (ADR-011)")
		}
		if o.apiErr == nil || o.apiErr.Code != apierr.CodeUpstreamRejected {
			t.Errorf("apiErr = %v, want code %q", o.apiErr, apierr.CodeUpstreamRejected)
		}
	}
	if got := defensive.apiErr.Details["reason"]; got != apierr.RejectedInvalid {
		t.Errorf("defensive resolve reason = %v, want %q", got, apierr.RejectedInvalid)
	}
	if got := real.apiErr.Details["reason"]; got != apierr.RejectedContextLength {
		t.Errorf("resolveRejected reason = %v, want %q", got, apierr.RejectedContextLength)
	}
}

// End-to-end through Client.Do: an OpenAI-compatible 400 body normalizes to 400
// UPSTREAM_REJECTED with details.reason=context_length, the server is hit
// EXACTLY once (no retry), and the upstream text never leaks (GW-INV-11).
func TestUpstreamRejected400EndToEnd(t *testing.T) {
	const rejectionBody = `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 200069 tokens (198021 in the messages, 2048 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, rejectionBody)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	_, ae := c.call(context.Background(), []byte("{}"), true)
	if ae == nil {
		t.Fatal("expected UPSTREAM_REJECTED, got success")
		return // unreachable; satisfies staticcheck SA5011 nil-flow analysis
	}
	if ae.Status != 400 {
		t.Fatalf("Status = %d, want 400", ae.Status)
	}
	if ae.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("Code = %q, want %q", ae.Code, apierr.CodeUpstreamRejected)
	}
	if got := ae.Details["reason"]; got != apierr.RejectedContextLength {
		t.Fatalf("Details[reason] = %v, want %q", got, apierr.RejectedContextLength)
	}
	// GW-INV-11: only the coarse enum passes through, never the upstream text.
	lower := strings.ToLower(ae.Message)
	if strings.Contains(ae.Message, "131072") || strings.Contains(lower, "maximum context length") {
		t.Fatalf("upstream error text leaked into the client message: %q", ae.Message)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a rejection must not be retried: expected exactly 1 upstream call, got %d", got)
	}
}

// ADR-011 amendment: persistent upstream 400 rejections must NEVER open the
// process breaker (contrast TestBreakerTripsOnPersistent5xx, where 5 consecutive
// 502s trip it). Every request keeps reaching upstream — no fast-shed.
func TestRejected400sDoNotTripBreaker(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 8192]"}}`)
	}))
	defer srv.Close()

	c := newTestBackend(srv.URL, []string{testKey}, time.Second, nil, nil)
	// Well past the breaker's 5-consecutive-failure threshold.
	for i := 0; i < 8; i++ {
		_, ae := c.call(context.Background(), []byte("{}"), true)
		if ae == nil || ae.Code != apierr.CodeUpstreamRejected {
			t.Fatalf("expected UPSTREAM_REJECTED, got %v", ae)
		}
		if got := ae.Details["reason"]; got != apierr.RejectedMaxTokens {
			t.Fatalf("Details[reason] = %v, want %q", got, apierr.RejectedMaxTokens)
		}
	}
	if c.BreakerOpen() {
		t.Fatal("upstream request rejections must NEVER trip the process breaker (ADR-011)")
	}
	if got := atomic.LoadInt32(&calls); got != 8 {
		t.Fatalf("rejections must not be retried or shed: expected 8 upstream calls, got %d", got)
	}
	// The breaker stayed closed, so a subsequent request still reaches upstream.
	_, _ = c.call(context.Background(), []byte("{}"), true)
	if got := atomic.LoadInt32(&calls); got != 9 {
		t.Fatalf("breaker must stay closed: the follow-up request should reach upstream (calls = %d, want 9)", got)
	}
}
