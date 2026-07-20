package chat

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// forward sends the sanitized body upstream and relays the response. Queue and
// breaker fast-path failures never reach this method and are known-unbilled.
// Open returns a separately classified billing-exposure fact: local shedding
// and explicit provider refusals prove no charge, while timeout/transport/5xx/
// client-cancel outcomes are ambiguous and conservatively keep the full quote.
// The app never guesses exposure from the client-facing error code.
func (s *Service) forward(ctx context.Context, sink Sink, provider billing.Provider, model string, req domchat.CompletionRequest, cfg *config.Config, resv *domquota.Reservation) {
	accountingExposure := false
	rollbackArmed := true
	defer func() {
		if rollbackArmed && !accountingExposure {
			s.rollback(ctx, resv)
		}
	}()

	st, failure := s.upstream.Open(ctx, provider, model, req, cfg.UpstreamHeaderTimeout)
	if failure != nil {
		aerr := failure.APIError
		if aerr == nil {
			aerr = apierr.ErrUpstreamError
		}
		accountingExposure = failure.Exposure.MayHaveCharged()
		if accountingExposure {
			rollbackArmed = false
			s.settle(ctx, resv, resv.ReservedPUSD)
		}
		// Definitely-unbilled results remain rollback-armed; every ambiguous result
		// above has already retained the full reservation.
		s.markUpstreamForErr(provider, aerr)
		writeErr(sink, aerr)
		return
	}
	if st == nil {
		// A broken adapter returned no normalized error and no stream after Open.
		// The provider outcome is unknowable, so keep the quote and fail closed.
		accountingExposure = true
		rollbackArmed = false
		s.settle(ctx, resv, resv.ReservedPUSD)
		s.mx.Upstream(provider, "error")
		writeErr(sink, apierr.ErrUpstreamError)
		return
	}

	// First body byte is available from a provider 2xx, so billing exposure is
	// established and rollback is disarmed. Streaming commits immediately;
	// non-streaming still buffers + validates its whole JSON object before 200.
	// The use case owns the single Stream.Close (body/context/timer release).
	accountingExposure = true
	rollbackArmed = false
	defer func() { _ = st.Close() }()

	if req.Stream {
		s.streamThrough(ctx, sink, provider, st, cfg, resv)
		return
	}
	s.nonStreamThrough(ctx, sink, provider, st, cfg, resv)
}

// streamThrough relays SSE frames after the first byte arrived, emitting 200 +
// the whitelisted gateway headers, settling from the include_usage final frame.
// Blank separators, normalized comments, and exact [DONE] pass; every other data
// payload must be one complete JSON object before it can cross the public wire.
// Usage is refundable only after the gateway has fully read a valid terminal
// [DONE]: an earlier cumulative snapshot is not proof of the provider's final
// charge. Malformed data, client disconnect, EOF, or read failure before DONE
// therefore retain the full quote. Output has started, so no later 502 can
// replace the 200. The per-frame write deadline rolls via the sink.
func (s *Service) streamThrough(ctx context.Context, sink Sink, provider billing.Provider, body io.Reader, cfg *config.Config, resv *domquota.Reservation) {
	s.writeStreamHeaders(sink, cfg, resv)
	sink.WriteHeader(200)

	var usage billing.Usage
	outcome := "error"
	terminal := false
	settled := false
	settle := func() {
		if settled {
			return
		}
		settled = true
		if !terminal {
			usage = usage.MergeSnapshot(billing.Usage{Malformed: true})
		}
		s.settleFromUsage(ctx, resv, usage)
	}
	defer func() {
		settle()
		s.mx.Upstream(provider, outcome)
	}()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		sink.SetWriteDeadline(s.clock.Now().Add(frameWriteDeadline))
		clientLine, valid := rewriteClientModelSSELine(line, cfg.PublicModelID)
		if !valid {
			outcome = "error"
			usage = usage.MergeSnapshot(billing.Usage{Malformed: true})
			return
		}
		usage = usage.MergeSnapshot(domchat.ParseUsageSnapshotLine(line))
		if domchat.IsDONE(line) {
			// Reading the terminal sentinel is the authority boundary. If writing
			// that already-read sentinel to a disconnected client fails, the
			// provider stream is still complete and cumulative usage may settle.
			terminal = true
			outcome = "success"
		}
		if _, err := sink.Write(clientLine); err != nil {
			return
		}
		if _, err := sink.Write(newline); err != nil {
			return
		}
		sink.Flush()
		if terminal {
			break
		}
	}
	// A scanner failure means a frame could not be bounded/read completely. No
	// partial bytes crossed the boundary, and uncertain usage keeps the full quote.
	if scanner.Err() != nil {
		usage = usage.MergeSnapshot(billing.Usage{Malformed: true})
	}
}

var newline = []byte("\n")

// nonStreamThrough buffers and validates the complete upstream body before
// committing 200. The public success boundary accepts exactly one JSON object;
// malformed/non-object/trailing-value bodies are suppressed, normalized to 502,
// and retain the full reservation. A valid object's top-level model values are
// rewritten before usage-based settlement and relay.
func (s *Service) nonStreamThrough(ctx context.Context, sink Sink, provider billing.Provider, body io.Reader, cfg *config.Config, resv *domquota.Reservation) {
	data, err := io.ReadAll(io.LimitReader(body, nonStreamBodyLimit+1))
	if err != nil || len(data) > nonStreamBodyLimit {
		s.mx.Upstream(provider, "error")
		s.settle(ctx, resv, resv.ReservedPUSD)
		writeErr(sink, apierr.ErrUpstreamError)
		return
	}
	data, valid := rewriteClientModelJSON(data, cfg.PublicModelID)
	if !valid {
		s.mx.Upstream(provider, "error")
		s.settle(ctx, resv, resv.ReservedPUSD)
		writeErr(sink, apierr.ErrUpstreamError)
		return
	}
	s.mx.Upstream(provider, "success")
	s.settleFromUsage(ctx, resv, domchat.ParseUsageSnapshotBody(data))

	s.writeStreamHeaders(sink, cfg, resv)
	sink.SetHeader("Content-Type", "application/json")
	sink.WriteHeader(200)
	_, _ = sink.Write(data)
}

// writeStreamHeaders writes ONLY the whitelisted gateway headers (§4.3): the
// SSE content type + the quota hint headers. The upstream body/headers/key never
// pass through.
func (s *Service) writeStreamHeaders(sink Sink, cfg *config.Config, resv *domquota.Reservation) {
	sink.SetHeader("Content-Type", "text/event-stream")
	sink.SetHeader("Cache-Control", "no-cache")
	sink.SetHeader("X-Quota-Limit", strconv.FormatInt(cfg.MonthlyQuota, 10))
	sink.SetHeader("X-Quota-Reset", domquota.MonthResetAt(resv.Period, cfg.Location).Format(time.RFC3339))
}

// markUpstreamForErr maps a normalized upstream apierr to its metric label.
// "rejected" (client-caused upstream 4xx) is split from "error" so a burst of
// oversized prompts is never misread as an upstream outage on the dashboard.
func (s *Service) markUpstreamForErr(provider billing.Provider, ae *apierr.APIError) {
	switch ae.Code {
	case apierr.ErrUpstreamBusy.Code:
		s.mx.Upstream(provider, "busy")
	case apierr.ErrUpstreamTimeout.Code:
		s.mx.Upstream(provider, "timeout")
	case apierr.CodeUpstreamRejected:
		s.mx.Upstream(provider, "rejected")
	default:
		s.mx.Upstream(provider, "error")
	}
}

// settleFromUsage converts the provider's cumulative token vector through the
// frozen rate card. Missing, contradictory, or unpriceable usage keeps the full
// reservation: an uncertain provider charge is never refunded. If authoritative
// cost exceeds the quote, truth wins — settle the top-up and emit a dedicated
// low-cardinality signal plus a secret-safe WARN.
func (s *Service) settleFromUsage(ctx context.Context, resv *domquota.Reservation, usage billing.Usage) {
	actualPUSD := resv.ReservedPUSD
	if cost, ok, err := resv.Plan.Cost(usage); err == nil && ok {
		actualPUSD = cost
	}
	if actualPUSD > resv.ReservedPUSD {
		s.mx.BillingDrift(resv.Plan.Provider)
		s.log.Warn(ctx, "billing_drift",
			"event", "billing_actual_exceeded_reservation",
			"request_id", resv.RequestID,
			"install_id", resv.InstallID,
			"provider", string(resv.Plan.Provider),
			"reserved_pusd", resv.ReservedPUSD,
			"actual_pusd", actualPUSD)
	}
	s.settle(ctx, resv, actualPUSD)
}
