package upstream

import (
	"context"
	"errors"
	"net"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// faultClass is the SINGLE typed classification of one connect→first-byte
// outcome, computed in ONE place (classify) and shared by every path (connect
// error, non-2xx, stream-peek failure, backoff cancel). Collapsing it here is
// the construction that makes the B5/B3 DoS-amplifier bug impossible: a
// client-cancel or a 429 can never be misfiled as a breaker fault because no
// other site re-derives the class (ADR-011).
//
// Each class carries the three orthogonal decisions a caller needs:
//   - apiErr:     normalized client-facing error (never the upstream body)
//   - retryable:  retry pre-output? (only transient faults + per-key signals)
//   - breakerFlt: count toward the PROCESS-wide breaker? (only genuine faults)
type faultClass int

const (
	classOK            faultClass = iota // success — output started
	classClientCancel                    // parent ctx canceled — 499, non-fault, non-retry
	classBusy                            // 429 / key-open — UPSTREAM_BUSY, non-fault, non-retry
	classKeyAuth                         // 401/403 per-key signal — retry (failover), non-fault
	classKeyOpen                         // a key's breaker open — retry (failover), non-fault
	classTimeout                         // connect→first-byte timeout — 504, fault, retry
	classUpstream                        // 5xx/connect/transport — 502, fault, retry
	classUpstreamFinal                   // non-retryable upstream non-2xx (e.g. 400) — 502, fault
)

// outcome is the resolved decision triple for a classified attempt.
type outcome struct {
	class      faultClass
	apiErr     *apierr.APIError
	retryable  bool
	breakerFlt bool
}

// resolve turns a class into its decision triple. retryAfter from upstream is
// attached by the caller, not here, to keep this a pure class→policy map.
func resolve(c faultClass) outcome {
	switch c {
	case classClientCancel:
		// Parent request canceled/disconnected: not an upstream fault, not retry,
		// audit-only. 499 is non-standard but internal; the transport renders it.
		return outcome{class: c, apiErr: apierr.NewError(499, "CLIENT_CANCELED", "client canceled the request"), retryable: false, breakerFlt: false}
	case classBusy:
		return outcome{class: c, apiErr: apierr.ErrUpstreamBusy, retryable: false, breakerFlt: false}
	case classKeyAuth:
		// Account-level key signal: retry so failover routes to a sibling key (the
		// transport already cooled this one down), but NOT a process-breaker fault.
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: true, breakerFlt: false}
	case classKeyOpen:
		// A single key's breaker is open: retryable (next attempt rotates keys),
		// never charges the process breaker (one dead key must not trip upstream).
		return outcome{class: c, apiErr: apierr.ErrUpstreamBusy, retryable: true, breakerFlt: false}
	case classTimeout:
		return outcome{class: c, apiErr: apierr.ErrUpstreamTimeout, retryable: true, breakerFlt: true}
	case classUpstream:
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: true, breakerFlt: true}
	case classUpstreamFinal:
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: false, breakerFlt: true}
	default:
		return outcome{class: classOK}
	}
}

// classifyConnectErr maps a transport-level error from client.Do into a class.
// parentCtx is the CALLER's request context: a cancel originating there is a
// client cancel (non-fault), distinct from the first-byte timer firing (which
// sets timedOut and is a genuine upstream timeout). Order matters: client-cancel
// is checked BEFORE timeout so a parent cancel is never misread as a 504.
func classifyConnectErr(err error, parentCtx context.Context, timedOut bool) faultClass {
	if errors.Is(err, errKeyOpen) {
		return classKeyOpen
	}
	// Client cancel/disconnect: the parent ctx is done AND the first-byte timer
	// did not fire. This is the B5/B3 guard — never a breaker fault.
	if !timedOut && parentCtx.Err() != nil && errors.Is(err, context.Canceled) {
		return classClientCancel
	}
	if timedOut || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return classTimeout
	}
	return classUpstream
}

// classifyStatus maps an upstream non-2xx status (received before any output)
// into a class. 429 is its own non-fault class; 401/403 is a per-key signal;
// 502/503/504 are retryable faults; anything else is a terminal upstream fault.
func classifyStatus(code int) faultClass {
	switch code {
	case 429:
		return classBusy
	case 401, 403:
		return classKeyAuth
	case 504:
		return classTimeout
	case 502, 503:
		return classUpstream
	default:
		return classUpstreamFinal
	}
}

// classifyPeekErr maps a stream first-byte Peek failure into a class. A parent
// cancel is a client cancel; the first-byte timer firing (timedOut) or any other
// read failure is a retryable upstream timeout/fault.
func classifyPeekErr(err error, parentCtx context.Context, timedOut bool) faultClass {
	if !timedOut && parentCtx.Err() != nil && errors.Is(err, context.Canceled) {
		return classClientCancel
	}
	if timedOut || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return classTimeout
	}
	return classUpstream
}

// isTimeout reports whether err is a net timeout (i/o deadline, dial timeout).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
