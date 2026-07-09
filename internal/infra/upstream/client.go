// Package upstream is the DeepSeek client + resilience: the redacting multi-key
// transport (per-key failover, GW-INV-11/30), the bounded connect→first-byte
// retry (GW-INV-26), the per-attempt first-byte timer (GW-INV-27), the process-
// wide circuit breaker (REL-2), and the single typed fault classification that
// excludes client-cancel and 429 by construction (ADR-011, GW-INV-23).
//
// The Client interface is the port app/chat (slice 7) consumes: Do runs one
// request end to connect→first-byte and returns a *Stream whose Body the caller
// relays (SSE frames + usage parsing). Non-2xx upstream responses are normalized
// to *apierr.APIError — the upstream body/headers/key NEVER pass through. The
// one nuance: a request rejection (400/413/422) parses the bounded error body
// solely to derive the coarse details.reason enum for 400 UPSTREAM_REJECTED
// (non-fault, non-retry per ADR-011); the upstream text itself is still discarded.
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
	"strings"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

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
	KeyCooldown()                 // a key was cooled down (401/403 or long Retry-After)
	BreakerStateChange(open bool) // process breaker flipped (open=true on Open)
}

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

// Client is the upstream port app/chat consumes.
type Client interface {
	// Do runs one request through the process breaker + bounded retry to
	// connect→first-byte. On success it returns a *Stream the caller relays and
	// must Close; on failure a normalized *apierr.APIError (never the upstream
	// body). A client-cancel surfaces as 499 CLIENT_CANCELED (non-fault); a 429 /
	// breaker-open as UPSTREAM_BUSY (non-fault, non-retry).
	Do(ctx context.Context, payload []byte, stream bool, cfg *config.Config) (*Stream, *apierr.APIError)
	// BreakerOpen reports whether the process-wide breaker is currently open
	// (drives the OBS-4 alert; lets a caller fast-shed before taking a slot).
	BreakerOpen() bool
}

// client is the concrete Client.
type client struct {
	transport *redactingTransport
	http      *http.Client
	breaker   *gobreaker.CircuitBreaker[struct{}]
	retry     retryPolicy
	hook      MetricsHook
	now       func() time.Time
}

// New builds a Client for the configured keys. respHeaderTimeout seeds the
// transport's connect→header bound; the per-attempt first-byte timer reads the
// LIVE cfg.UpstreamHeaderTimeout on each Do. logger/hook may be nil.
func New(apiKeys []string, respHeaderTimeout time.Duration, logger *slog.Logger, hook MetricsHook) Client {
	if logger == nil {
		logger = slog.Default()
	}
	t := newRedactingTransport(apiKeys, respHeaderTimeout, logger, hook)
	c := &client{
		transport: t,
		http:      &http.Client{Transport: t}, // NO client.Timeout: it would truncate long streams
		retry:     defaultRetryPolicy(),
		hook:      hook,
		now:       time.Now,
	}
	c.breaker = newProcessBreaker(func(from, to gobreaker.State) {
		logger.Warn("upstream_breaker_state_change", "event", "upstream_breaker",
			"from", from.String(), "to", to.String())
		if hook != nil {
			hook.BreakerStateChange(to == gobreaker.StateOpen)
		}
	})
	return c
}

// BreakerOpen reports whether the process breaker is open.
func (c *client) BreakerOpen() bool { return c.breaker.State() == gobreaker.StateOpen }

// Do drives connect→first-byte through the process breaker (REL-2). The inner
// func runs the bounded retry loop (REL-1) and succeeds only once the first byte
// has arrived, so the breaker records exactly one outcome per request (retry-
// exhausted = one failure; 429 / client-cancel / post-output never counted).
func (c *client) Do(ctx context.Context, payload []byte, stream bool, cfg *config.Config) (*Stream, *apierr.APIError) {
	// Fast-path shed: an already-open breaker → immediate UPSTREAM_BUSY without
	// even attempting (the caller must not have taken an N_global slot yet).
	if c.breaker.State() == gobreaker.StateOpen {
		return nil, apierr.ErrUpstreamBusy
	}

	var (
		out      *Stream
		finalErr *apierr.APIError
	)
	_, berr := c.breaker.Execute(func() (struct{}, error) {
		s, o := c.attempt(ctx, payload, stream, cfg)
		if o.class == classOK {
			out = s
			return struct{}{}, nil
		}
		finalErr = o.apiErr
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
		return nil, apierr.ErrUpstreamBusy
	}
	if finalErr != nil {
		return nil, finalErr
	}
	return nil, apierr.ErrUpstreamError
}

// attempt runs the bounded retry loop for connect→first-byte. It returns the
// resolved outcome of the last attempt; only a retryable, non-final outcome with
// budget left loops. The same payload is reused unchanged across attempts.
func (c *client) attempt(ctx context.Context, payload []byte, stream bool, cfg *config.Config) (*Stream, outcome) {
	var last outcome
	for n := 1; n <= c.retry.maxAttempts; n++ {
		s, o, retryAfter := c.tryOnce(ctx, payload, stream, cfg)
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
			// Parent canceled during backoff: a client cancel, NOT a fault (the
			// B5/B3 site). Resolve as client-cancel so the breaker never counts it.
			return nil, resolve(classClientCancel)
		}
	}
	return nil, last
}

// tryOnce performs ONE connect→first-byte attempt: build a fresh upstream
// request, bound connect→first-byte with an idempotently-disarmed timer (B6),
// and on a 2xx peek the first stream byte. It returns the resolved outcome plus
// any upstream Retry-After hint for the caller's backoff math.
func (c *client) tryOnce(ctx context.Context, payload []byte, stream bool, cfg *config.Config) (*Stream, outcome, time.Duration) {
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
	fbTimer := time.AfterFunc(cfg.UpstreamHeaderTimeout, func() {
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
		cfg.DeepSeekBaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		cleanup()
		return nil, resolve(classUpstream), 0
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
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
			reason := rejectionReason(resp.Body)
			_ = resp.Body.Close()
			cleanup()
			return nil, resolveRejected(reason), ra
		}
		_ = drainAndClose(resp.Body)
		cleanup()
		return nil, resolve(cls), ra
	}

	if !stream {
		// 2xx headers = header phase succeeded; the bounded body read happens in
		// the caller (nonStream). Hand off ONLY if we won the disarm CAS — else the
		// first-byte timer already canceled upCtx and the body read would fail (B6).
		if !disarm() {
			_ = drainAndClose(resp.Body)
			cancelUp()
			return nil, resolve(classTimeout), 0
		}
		return &Stream{Body: resp.Body, Status: http.StatusOK, resp: resp, cancel: cancelUp}, outcome{class: classOK}, 0
	}

	// Streaming: peek the first byte to gate the 200. If upstream sent headers but
	// stalls, the timer cancels upCtx and this Peek fails → a retryable pre-output
	// timeout. Disarm the INSTANT Peek returns, before classifying, closing the
	// narrow race where the timer fires between a good Peek and the handoff.
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
		// retryable pre-output timeout so the retry loop reattempts (B6: a RETURNED
		// stream is never timer-cancelable). Vanishingly rare in prod (60s timeout).
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
func rejectionReason(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, 4*1024))
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := strings.ToLower(env.Error.Message)
	switch {
	case strings.Contains(msg, "context length"):
		return apierr.RejectedContextLength
	case strings.Contains(msg, "max_tokens"):
		return apierr.RejectedMaxTokens
	default:
		return apierr.RejectedInvalid
	}
}
