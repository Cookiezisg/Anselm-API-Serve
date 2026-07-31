// Package upstream provides provider-neutral OpenAI-compatible HTTP clients and
// resilience: the redacting multi-key transport (per-key failover,
// GW-INV-11/30), the bounded connect→first-byte retry (GW-INV-26), the
// per-attempt first-byte timer (GW-INV-27), provider-local process breakers
// (REL-2), and typed fault classification that excludes client-cancel and 429
// by construction (ADR-011, GW-INV-23).
//
// A Client owns exactly one construction-time endpoint, key pool, transport and
// breaker. DoCall runs one request to connect→first-byte and returns a *Stream
// whose Body the caller relays (SSE frames + usage parsing). Non-2xx responses
// are normalized to *apierr.APIError — upstream body/headers/key NEVER pass
// through. A request rejection (400/413/422) parses only a bounded error body to
// derive the coarse details.reason enum for 400 UPSTREAM_REJECTED (non-fault,
// non-retry per ADR-011); provider-controlled text is still discarded.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/pkg/secureurl"
)

// BackendID is the low-cardinality identity of an upstream account.
// It is deliberately separate from a model id: breakers, key pools and metrics
// are isolated per account, while models remain request targets within one
// account. Callers should use the declared constants and must never populate
// this value from untrusted request JSON.
type BackendID string

const BackendQwen BackendID = "qwen"

// Options freezes everything that belongs to one upstream account. In
// particular, URL/key/breaker state may never be selected from a client request
// or a different backend's health state.
type Options struct {
	Backend            BackendID
	ChatCompletionsURL string
	APIKeys            []string
	HeaderTimeout      time.Duration
	Logger             *slog.Logger
	Hook               MetricsHook
}

// Call contains the only request-varying wire facts the resilience engine
// needs. Payload has already been provider-encoded and sanitized by the caller.
type Call struct {
	Payload          []byte
	Stream           bool
	FirstByteTimeout time.Duration
}

// CallFailure keeps the normalized client error and the independent billing
// fact produced by the connect→first-byte engine. Callers must never infer
// charge exposure from an API error code: the same UPSTREAM_ERROR envelope can
// represent either an explicit, definitely-unbilled refusal or an ambiguous
// transport failure after the provider may have accepted the request.
type CallFailure struct {
	APIError *apierr.APIError
	Exposure billing.ChargeExposure
}

func callFailure(ae *apierr.APIError, exposure billing.ChargeExposure) *CallFailure {
	if ae == nil {
		ae = apierr.ErrUpstreamError
	}
	return &CallFailure{APIError: ae, Exposure: exposure}
}

// errUpstreamFault marks a terminal transient upstream fault (retry exhausted)
// so the process breaker records exactly ONE failure per request attempt-set.
// Internal — the client sees the normalized apierr instead.
var errUpstreamFault = errors.New("upstream fault")

// isClientCancel reports whether err is the breaker-excluded client-cancel
// sentinel (so the process breaker's IsSuccessful never counts it).
func isClientCancel(err error) bool { return errors.Is(err, context.Canceled) }

// MetricsHook is the optional observability port. All methods may be called
// concurrently and must be cheap + non-reentrant. nil disables every hook (the
// hot path stays allocation-free). Kept tiny so upstream needn't import the
// metrics package (which would couple infra→infra unnecessarily).
type MetricsHook interface {
	KeyCooldown()                          // a key was cooled down (401/403 or long Retry-After)
	BreakerStateChange(state BreakerState) // process breaker changed state
	CallLatency(elapsed time.Duration)     // complete connect→first-byte attempt-set
}

// BreakerState is the stable numeric state exposed to metrics. It decouples the
// bootstrap adapter from gobreaker's type while preserving all three states.
type BreakerState uint8

const (
	BreakerClosed BreakerState = iota
	BreakerHalfOpen
	BreakerOpen
)

// Stream is the result of a successful connect→first-byte: the upstream body the
// caller relays, plus the status it should emit (always 200 on the success path)
// and a cancel that frees the per-request upstream context + first-byte timer.
// The caller MUST call Close exactly once (it closes the body and cancels).
//
// Body is a *bufio.Reader for the streaming case (the first byte is already
// peeked, so the 200 is gated on real output) and the raw body otherwise. The
// caller parses usage from the frames; upstream never inspects the payload.
type Stream struct {
	Body   io.Reader
	Status int
	resp   *http.Response
	cancel context.CancelFunc
}

// Read makes *Stream satisfy io.Reader (and thus app/chat.UpstreamStream) by
// delegating to the relayed body — so a tiny bootstrap adapter can return the
// concrete *Stream as the app's infra-free UpstreamStream port without app ever
// importing this package.
func (s *Stream) Read(p []byte) (int, error) { return s.Body.Read(p) }

// Close releases the stream's body and per-request context. Idempotent-safe to
// call once; the caller owns the single Close on the success path.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.resp != nil {
		err = s.resp.Body.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	return err
}

// BackendClient is the narrow provider-local client returned by NewBackend. Its
// type surface cannot accept a runtime config or an alternate URL: the endpoint
// and the key pool are frozen at construction, so one provider's credential can
// never travel to another provider's address.
//
// BackendClient 是 NewBackend 返回的、按 provider 收窄的客户端。它的类型面**收不下**运行时
// 配置或另一个 URL:端点与 key 池在构造期就冻住,故一家的凭证永远走不到另一家的地址上。
type BackendClient interface {
	// DoCall opens a request against this client's construction-time endpoint.
	// A client represents exactly one provider-local key pool + breaker.
	DoCall(ctx context.Context, call Call) (*Stream, *CallFailure)
	// BreakerOpen reports whether this client's breaker is currently open
	// (drives the OBS-4 alert; lets a caller fast-shed before taking a slot).
	BreakerOpen() bool
}

// client is the concrete BackendClient.
type client struct {
	backend   BackendID
	endpoint  string
	timeout   time.Duration
	transport *redactingTransport
	http      *http.Client
	breaker   *gobreaker.CircuitBreaker[struct{}]
	retry     retryPolicy
	hook      MetricsHook
	now       func() time.Time
}

// NewBackend builds one provider-local client. It intentionally returns no
// shared registry: constructing two clients creates two fully independent key
// pools, transports and process breakers. An invalid URL is collapsed to an
// unusable endpoint and normalizes to UPSTREAM_ERROR at call time; configuration
// validation remains the composition root's responsibility.
func NewBackend(opts Options) BackendClient {
	return newClient(opts)
}

func newClient(opts Options) *client {
	backend := opts.Backend
	if backend == "" {
		backend = BackendQwen
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	endpoint := normalizeEndpoint(opts.ChatCompletionsURL)
	t := newRedactingTransport(opts.APIKeys, logger, opts.Hook, backend)
	c := &client{
		backend:   backend,
		endpoint:  endpoint,
		timeout:   opts.HeaderTimeout,
		transport: t,
		http: &http.Client{
			Transport: t, // NO client.Timeout: it would truncate long streams.
			// A provider endpoint is immutable. Following Location would both violate
			// that invariant and let our auth-injecting transport attach the key to a
			// different host. Return the 3xx for normal, secret-safe classification.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		retry: defaultRetryPolicy(),
		hook:  opts.Hook,
		now:   time.Now,
	}
	c.breaker = newProcessBreaker(string(backend)+"-upstream", func(from, to gobreaker.State) {
		logger.Warn("upstream_breaker_state_change", "event", "upstream_breaker",
			"backend", backend, "from", from.String(), "to", to.String())
		if opts.Hook != nil {
			state := BreakerClosed
			switch to {
			case gobreaker.StateHalfOpen:
				state = BreakerHalfOpen
			case gobreaker.StateOpen:
				state = BreakerOpen
			}
			opts.Hook.BreakerStateChange(state)
		}
	})
	return c
}

// normalizeEndpoint accepts credential-safe absolute endpoints: HTTPS, or plain
// HTTP only when the host is explicitly loopback for local development/tests.
// Returning the empty string gives both constructors one fail-closed sentinel
// and prevents malformed/insecure configuration from reaching a provider key or
// being charged to the provider's health breaker.
func normalizeEndpoint(raw string) string {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !secureurl.AllowsCredentialTransport(u) {
		return ""
	}
	return endpoint
}

// BreakerOpen reports whether the process breaker is open.
func (c *client) BreakerOpen() bool {
	return c != nil && c.breaker != nil && c.breaker.State() == gobreaker.StateOpen
}

// DoCall is the provider-neutral entry point. URL, auth material and breaker
// identity are construction-time facts; only the sanitized body, stream flag and
// request-snapshot first-byte timeout vary per call.
func (c *client) DoCall(ctx context.Context, call Call) (*Stream, *CallFailure) {
	if c == nil {
		return nil, callFailure(apierr.ErrUpstreamError, billing.DefinitelyUnbilled)
	}
	timeout := call.FirstByteTimeout
	if timeout <= 0 {
		timeout = c.timeout
	}
	return c.do(ctx, call.Payload, call.Stream, timeout, c.endpoint)
}

func (c *client) do(ctx context.Context, payload []byte, stream bool, firstByteTimeout time.Duration, endpoint string) (*Stream, *CallFailure) {
	// Both exported entry points converge here. Fail closed before touching a
	// breaker or RoundTripper so invalid construction and empty/blank key pools
	// are configuration faults, not provider health events.
	if c == nil || endpoint == "" || c.transport == nil || len(c.transport.keys) == 0 || c.http == nil || c.breaker == nil {
		return nil, callFailure(apierr.ErrUpstreamError, billing.DefinitelyUnbilled)
	}
	started := time.Now()
	if c.hook != nil {
		defer func() { c.hook.CallLatency(time.Since(started)) }()
	}
	// Fast-path shed: an already-open breaker → immediate UPSTREAM_BUSY without
	// even attempting (the caller must not have taken an N_global slot yet).
	if c.breaker.State() == gobreaker.StateOpen {
		return nil, callFailure(apierr.ErrUpstreamBusy, billing.DefinitelyUnbilled)
	}

	var (
		out          *Stream
		finalFailure *CallFailure
	)
	_, berr := c.breaker.Execute(func() (struct{}, error) {
		s, o := c.attempt(ctx, payload, stream, firstByteTimeout, endpoint)
		if o.class == classOK {
			out = s
			return struct{}{}, nil
		}
		finalFailure = callFailure(o.apiErr, o.exposure)
		if o.breakerFlt {
			// One process-breaker failure for the whole attempt-set.
			return struct{}{}, errUpstreamFault
		}
		// Non-fault terminal (429 busy, client cancel, key signal exhausted):
		// success from the breaker's perspective so it never trips on these.
		return struct{}{}, nil
	})

	if out != nil {
		return out, nil
	}
	// The breaker was open between the fast-path check and Execute, or every
	// attempt failed. ErrOpenState/ErrTooManyRequests → UPSTREAM_BUSY.
	if berr == gobreaker.ErrOpenState || berr == gobreaker.ErrTooManyRequests {
		return nil, callFailure(apierr.ErrUpstreamBusy, billing.DefinitelyUnbilled)
	}
	if finalFailure != nil {
		return nil, finalFailure
	}
	return nil, callFailure(apierr.ErrUpstreamError, billing.ChargePossible)
}

// attempt runs the bounded failover loop for connect→first-byte. It returns the
// resolved outcome of the last attempt; only a definitely-unbilled per-key
// signal with budget left loops. Charge-ambiguous outcomes are terminal so one
// reservation can never hide multiple possible provider charges.
func (c *client) attempt(ctx context.Context, payload []byte, stream bool, firstByteTimeout time.Duration, endpoint string) (*Stream, outcome) {
	var last outcome
	for n := 1; n <= c.retry.maxAttempts; n++ {
		s, o, retryAfter := c.tryOnce(ctx, payload, stream, firstByteTimeout, endpoint)
		if o.class == classOK {
			return s, o
		}
		last = o
		if !o.retryable || n == c.retry.maxAttempts {
			break
		}
		// Honor an upstream Retry-After when longer than our backoff; else
		// exponential backoff + full jitter. Capped either way.
		wait := c.retry.backoff(n)
		if retryAfter > wait {
			wait = retryAfter
		}
		if wait > c.retry.cap {
			wait = c.retry.cap
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			// Parent canceled between definitely-unbilled key failover attempts:
			// surface client-cancel but preserve the proof that no provider charge
			// was possible. It remains a non-fault for the process breaker.
			canceled := resolve(classClientCancel)
			canceled.exposure = billing.DefinitelyUnbilled
			return nil, canceled
		}
	}
	return nil, last
}

// tryOnce performs ONE connect→first-byte attempt: build a fresh upstream
// request, bound connect→first-byte with an idempotently-disarmed timer (B6),
// and on a 2xx peek the first stream byte. It returns the resolved outcome plus
// any upstream Retry-After hint for the caller's backoff math.
func (c *client) tryOnce(ctx context.Context, payload []byte, stream bool, firstByteTimeout time.Duration, endpoint string) (*Stream, outcome, time.Duration) {
	upCtx, cancelUp := context.WithCancel(ctx)

	// First-byte timer (GW-INV-27): bounds connect→header→first-byte so an
	// upstream that connects but never replies cannot pin a slot / virtually
	// occupy budget. armed makes the disarm IDEMPOTENT (B6): the AfterFunc cancels
	// ONLY while still armed, and every success path clears armed BEFORE the timer
	// can fire — so the timer can never cancel an already-started stream even if
	// it fires concurrently with output starting.
	timedOut := &atomicBool{}
	var armed atomicBool
	armed.set()
	if firstByteTimeout <= 0 {
		firstByteTimeout = 60 * time.Second
	}
	fbTimer := time.AfterFunc(firstByteTimeout, func() {
		// Only the racer that wins the armed true→false transition acts. If the
		// success path already disarmed, this CAS fails and the timer is a no-op —
		// so it can never cancel an already-started stream (B6).
		if !armed.takeArmed() {
			return
		}
		timedOut.set()
		cancelUp()
	})
	// disarm takes the armed flag and stops the timer; it returns whether WE won
	// the armed true→false CAS. won==true ⇒ the AfterFunc's own CAS will fail, so
	// the timer can NEVER cancel upCtx — safe to hand off the stream. won==false ⇒
	// the timer already won the CAS and WILL cancelUp, so a stream handed off now
	// would read on a doomed context (B6): the caller must NOT hand it off.
	// Keying the decision on the CAS result (not the separate timedOut flag) closes
	// the gap between the timer winning the CAS and it setting timedOut. Idempotent.
	disarm := func() bool {
		won := armed.takeArmed()
		fbTimer.Stop()
		return won
	}
	// On any non-success path we release upCtx + timer ourselves; on success the
	// caller owns upCtx via Stream.cancel + body Close.
	cleanup := func() {
		disarm()
		cancelUp()
	}

	upReq, err := http.NewRequestWithContext(upCtx, http.MethodPost,
		endpoint, bytes.NewReader(payload))
	if err != nil {
		cleanup()
		return nil, resolve(classUpstream), 0
	}
	upReq.Header.Set("Content-Type", "application/json")
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	// Authorization is injected by the redacting transport on a clone, never here.

	resp, err := c.http.Do(upReq)
	if err != nil {
		cleanup()
		return nil, resolve(classifyConnectErr(err, ctx, timedOut.get())), 0
	}

	// Non-2xx BEFORE any output: classify + normalize. Never pass the upstream
	// body/headers through (GW-INV-11). A request rejection (400/413/422) is the
	// one class that PARSES the (bounded) error body — only to derive the coarse
	// reason enum for UPSTREAM_REJECTED; the text itself is discarded.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ra := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
		cls := classifyStatus(resp.StatusCode)
		if cls == classUpstreamRejected {
			reason, providerMsg := rejectionReason(resp.Body)
			// Operator diagnostics ONLY — the caller still receives nothing but the coarse enum
			// (GW-INV-11 is about what we FORWARD, not what we can see in our own journal). Without
			// this line an upstream rejection is a black box: the 2026-07-25 media-fetch failure took
			// an hour of live probing to attribute because the gateway kept the provider's one-line
			// explanation to itself.
			// 只作运维诊断——调用方拿到的仍只有粗粒度枚举(GW-INV-11 管的是**转发**什么,不是我们自己的
			// 日志里能看什么)。没有这一行,上游拒绝就是黑盒:2026-07-25 的媒体拉取故障折腾了一小时线上
			// 探测才定位,因为网关把 provider 那一句解释留给了自己。
			slog.Warn("upstream_rejected",
				"backend", string(c.backend), "status", resp.StatusCode,
				"reason", reason, "provider_message", providerMsg)
			_ = resp.Body.Close()
			cleanup()
			return nil, resolveRejected(reason), ra
		}
		_ = drainAndClose(resp.Body)
		cleanup()
		return nil, resolve(cls), ra
	}

	// Both response modes must produce a first body byte before handoff. A 2xx
	// header alone is not progress: without this shared Peek, a non-stream server
	// could flush headers then pin N_global forever in the app's body ReadAll. The
	// request-snapshot timer therefore gates connect→header→first byte for both
	// JSON and SSE. Disarm the instant Peek returns to close the B6 race.
	br := bufio.NewReader(resp.Body)
	_, peekErr := br.Peek(1)
	won := disarm()
	if peekErr != nil {
		_ = resp.Body.Close()
		cancelUp()
		return nil, resolve(classifyPeekErr(peekErr, ctx, timedOut.get())), 0
	}
	if !won {
		// Good first byte, but the timer won the disarm CAS in the post-Peek gap and
		// WILL cancel upCtx → handing off this stream would truncate it. Resolve as a
		// terminal charge-ambiguous timeout (B6: a RETURNED stream is never timer-
		// cancelable). Vanishingly rare in production (60s default timeout).
		_ = resp.Body.Close()
		cancelUp()
		return nil, resolve(classTimeout), 0
	}
	return &Stream{Body: br, Status: http.StatusOK, resp: resp, cancel: cancelUp}, outcome{class: classOK}, 0
}

// drainAndClose drains a bounded amount of a discarded body then closes it, so
// the connection can be reused. Bounded so a hostile upstream cannot make us
// read forever.
func drainAndClose(rc io.ReadCloser) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4*1024))
	return rc.Close()
}

// rejectionReason maps a BOUNDED slice of an upstream 4xx rejection body onto
// the closed apierr.Rejected* reason enum. It reads at most 4KiB, tolerates any
// malformed body, and never returns upstream text (GW-INV-11) — only the enum.
// The provider's OpenAI-style shape is {"error":{"message":...}}; the context-
// overflow message reads "This model's maximum context length is N tokens..."
// and the max_tokens range error names "max_tokens" — matched case-insensitively.
// The second return is the provider's own (bounded, truncated) explanation — for the local journal
// only, never for the caller. 第二个返回值是 provider 自己的解释(有界、截断)——只进本地日志,绝不给调用方。
func rejectionReason(body io.Reader) (string, string) {
	raw, _ := io.ReadAll(io.LimitReader(body, 4*1024))
	type errorEnvelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Message == "" {
		// Some compatibility endpoints return a one-element error array. Tolerate
		// that envelope without ever forwarding its provider-controlled text.
		var list []errorEnvelope
		if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
			env = list[0]
		}
	}
	providerMsg := env.Error.Message
	if len(providerMsg) > 300 {
		providerMsg = providerMsg[:300]
	}
	msg := strings.ToLower(env.Error.Message)
	switch {
	case strings.Contains(msg, "context length"),
		strings.Contains(msg, "context window"),
		strings.Contains(msg, "input too large"),
		strings.Contains(msg, "too many input tokens"),
		strings.Contains(msg, "maximum input"):
		return apierr.RejectedContextLength, providerMsg
	case strings.Contains(msg, "max_tokens"):
		return apierr.RejectedMaxTokens, providerMsg
	default:
		return apierr.RejectedInvalid, providerMsg
	}
}
