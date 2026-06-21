package chat

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// forward sends the sanitized body upstream and relays the response. All
// pre-output failures funnel through ONE deferred rollback (REL-5): it fires iff
// outputStarted is still false. The breaker + bounded retry live entirely inside
// upstream.Do (pre-output), so produced output is never retried/double-billed;
// the monthly count is KEPT once output starts (anti-stream-abuse §7.2).
func (s *Service) forward(ctx context.Context, sink Sink, payload []byte, stream bool, cfg *config.Config, resv *domquota.Reservation) {
	outputStarted := false
	defer func() {
		if !outputStarted {
			s.rollback(ctx, resv)
		}
	}()

	st, aerr := s.upstream.Do(ctx, payload, stream, cfg)
	if aerr != nil {
		// Pre-output failure (breaker open / retry exhausted / 4xx-5xx / connect /
		// client-cancel-as-499). Roll back via the defer, write the normalized error.
		// A client-cancel (CLIENT_CANCELED-class) has no body to write but the
		// upstream port already normalized it; just surface it.
		s.markUpstreamForErr(aerr)
		writeErr(sink, aerr)
		return
	}

	// First byte (stream) / 2xx (non-stream) is ready → output begins; disarm the
	// rollback. The use case owns the single Stream.Close (releases body + the
	// per-request upstream context + first-byte timer).
	outputStarted = true
	defer func() { _ = st.Close() }()

	s.mx.Upstream("success")
	if stream {
		s.streamThrough(ctx, sink, st, resv)
		return
	}
	s.nonStreamThrough(ctx, sink, st, resv)
}

// streamThrough relays SSE frames after the first byte arrived, emitting 200 +
// the whitelisted gateway headers, settling from the include_usage final frame.
// Output has started: count is KEPT even on a mid-stream disconnect (防断流刷量).
// The per-frame write deadline is rolled via the sink (NOT a global timeout).
func (s *Service) streamThrough(ctx context.Context, sink Sink, body io.Reader, resv *domquota.Reservation) {
	s.writeStreamHeaders(sink, resv)
	sink.WriteHeader(200)

	var actual int64 = -1
	settled := false
	settle := func() {
		if settled {
			return
		}
		settled = true
		a := actual
		if a < 0 {
			a = resv.Reserved // disconnected/absent usage frame → full est (conservative).
		}
		s.settle(ctx, resv, a)
	}
	defer settle()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		sink.SetWriteDeadline(s.clock.Now().Add(frameWriteDeadline))
		if u := domchat.ParseUsageLine(line); u >= 0 {
			actual = u
		}
		if _, err := sink.Write(line); err != nil {
			return // client gone; settle via defer with current actual/est.
		}
		if _, err := sink.Write(newline); err != nil {
			return
		}
		sink.Flush()
		if domchat.IsDONE(line) {
			break
		}
	}
	// scanner.Err() is intentionally not surfaced for settlement: stream end
	// (graceful or aborted) always settles via the defer.
}

var newline = []byte("\n")

// nonStreamThrough relays a non-streaming JSON response, settling from its usage
// object. Output is already committed (2xx). A read failure after the commit
// cannot be retried/rolled back without double-billing risk, so we settle full
// est (honest, conservative) and surface a normalized error.
func (s *Service) nonStreamThrough(ctx context.Context, sink Sink, body io.Reader, resv *domquota.Reservation) {
	data, err := io.ReadAll(io.LimitReader(body, nonStreamBodyLimit))
	if err != nil {
		s.settle(ctx, resv, resv.Reserved)
		s.mx.Upstream("error")
		writeErr(sink, apierr.ErrUpstreamError)
		return
	}
	actual := domchat.ParseUsageBody(data)
	if actual < 0 {
		actual = resv.Reserved
	}
	s.settle(ctx, resv, actual)

	s.writeStreamHeaders(sink, resv)
	sink.SetHeader("Content-Type", "application/json")
	sink.WriteHeader(200)
	_, _ = sink.Write(data)
}

// writeStreamHeaders writes ONLY the whitelisted gateway headers (§4.3): the
// SSE content type + the quota hint headers. The upstream body/headers/key never
// pass through.
func (s *Service) writeStreamHeaders(sink Sink, resv *domquota.Reservation) {
	cfg := s.cfg.Load()
	sink.SetHeader("Content-Type", "text/event-stream")
	sink.SetHeader("Cache-Control", "no-cache")
	sink.SetHeader("X-Quota-Limit", strconv.FormatInt(cfg.MonthlyQuota, 10))
	sink.SetHeader("X-Quota-Reset", domquota.MonthResetAt(resv.Period, cfg.Location).Format(time.RFC3339))
}

// markUpstreamForErr maps a normalized upstream apierr to its metric label.
func (s *Service) markUpstreamForErr(ae *apierr.APIError) {
	switch ae.Code {
	case apierr.ErrUpstreamBusy.Code:
		s.mx.Upstream("busy")
	case apierr.ErrUpstreamTimeout.Code:
		s.mx.Upstream("timeout")
	default:
		s.mx.Upstream("error")
	}
}
