package upstream

import (
	"context"
	"errors"
	"net"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// faultClass is the SINGLE typed classification of one connect→first-byte
// outcome, computed in ONE place (classify) and shared by every path (connect
// error, non-2xx, stream-peek failure, backoff cancel). Collapsing it here is
// the construction that makes the B5/B3 DoS-amplifier bug impossible: a
// client-cancel or a 429 can never be misfiled as a breaker fault because no
// other site re-derives the class (ADR-011).
//
// Each class carries the four orthogonal decisions a caller needs:
// - apiErr: normalized client-facing error (never the upstream body)
// - retryable: retry pre-output? (only definitely-unbilled per-key signals)
// - breakerFlt: count toward the PROCESS-wide breaker? (only genuine faults)
// - exposure: may the provider have charged? (independent of fault health)
type faultClass int

const (
	classOK               faultClass = iota // success — output started
	classClientCancel                       // parent ctx canceled — 499, non-fault, non-retry
	classBusy                               // 429 / key-open — UPSTREAM_BUSY, non-fault, non-retry
	classKeyAuth                            // 401/403 per-key signal — retry (failover), non-fault
	classKeyOpen                            // a key's breaker open — retry (failover), non-fault
	classTimeout                            // connect→first-byte timeout — 504, fault, no retry (charge ambiguous)
	classUpstream                           // 5xx/connect/transport — 502, fault, no retry (charge ambiguous)
	classUpstreamFinal                      // non-retryable ambiguous status (e.g. 500) — 502, fault
	classUpstreamRefused                    // other explicit 3xx/4xx — 502, fault, definitely unbilled
	classUpstreamRejected                   // 400/413/422 request rejection — 400, NON-fault, non-retry
)

// outcome is the resolved decision tuple for a classified attempt.
type outcome struct {
	class      faultClass
	apiErr     *apierr.APIError
	retryable  bool
	breakerFlt bool
	exposure   billing.ChargeExposure
}

// resolve turns a class into its decision tuple. retryAfter from upstream is
// attached by the caller, not here, to keep this a pure class→policy map.
func resolve(c faultClass) outcome {
	switch c {
	case classClientCancel:
		// Parent request canceled/disconnected: not an upstream fault, not retry,
		// audit-only. 499 is non-standard but internal; the transport renders it.
		return outcome{class: c, apiErr: apierr.NewError(499, "CLIENT_CANCELED", "client canceled the request"), retryable: false, breakerFlt: false, exposure: billing.ChargePossible}
	case classBusy:
		return outcome{class: c, apiErr: apierr.ErrUpstreamBusy, retryable: false, breakerFlt: false, exposure: billing.DefinitelyUnbilled}
	case classKeyAuth:
		// Account-level key signal: retry so failover routes to a sibling key (the
		// transport already cooled this one down), but NOT a process-breaker fault.
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: true, breakerFlt: false, exposure: billing.DefinitelyUnbilled}
	case classKeyOpen:
		// A single key's breaker is open: retryable (next attempt rotates keys),
		// never charges the process breaker (one dead key must not trip upstream).
		return outcome{class: c, apiErr: apierr.ErrUpstreamBusy, retryable: true, breakerFlt: false, exposure: billing.DefinitelyUnbilled}
	case classTimeout:
		return outcome{class: c, apiErr: apierr.ErrUpstreamTimeout, retryable: false, breakerFlt: true, exposure: billing.ChargePossible}
	case classUpstream:
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: false, breakerFlt: true, exposure: billing.ChargePossible}
	case classUpstreamFinal:
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: false, breakerFlt: true, exposure: billing.ChargePossible}
	case classUpstreamRefused:
		return outcome{class: c, apiErr: apierr.ErrUpstreamError, retryable: false, breakerFlt: true, exposure: billing.DefinitelyUnbilled}
	case classUpstreamRejected:
		// Reached only via a caller that lost the reason (defensive); the real
		// path is resolveRejected, which attaches the parsed coarse reason.
		return resolveRejected(apierr.RejectedInvalid)
	default:
		return outcome{class: classOK, exposure: billing.ChargePossible}
	}
}

// resolveRejected builds the outcome for an upstream request-rejection (HTTP
// 400/413/422 before any output): the CLIENT must change the request, so it is
// terminal (no retry) and — the ADR-011 amendment — NOT a process-breaker fault:
// a stream of oversized prompts must never open the breaker and black-hole every
// other user. Normalized to 400 UPSTREAM_REJECTED carrying only the coarse
// machine reason; the upstream body/text never passes through (GW-INV-11).
func resolveRejected(reason string) outcome {
	return outcome{class: classUpstreamRejected, apiErr: apierr.UpstreamRejected(reason), retryable: false, breakerFlt: false, exposure: billing.DefinitelyUnbilled}
}

// classifyConnectErr maps a transport-level error from client.Do into a class.
// parentCtx is the CALLER's request context: a cancel originating there is a
// client cancel (non-fault), distinct from the first-byte timer firing (which
// sets timedOut and is a genuine upstream timeout). Order matters: client-cancel
// is checked BEFORE timeout so a parent cancel is never misread as a 504.
func classifyConnectErr(err error, parentCtx context.Context, timedOut bool) faultClass {
	if errors.Is(err, errAllKeysGated) || errors.Is(err, errNoUsableAPIKey) {
		return classBusy
	}
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
// 400/413/422 is a request rejection (client-caused — non-fault, relayed as 400
// UPSTREAM_REJECTED); 502/503/504 are charge-ambiguous terminal faults; other
// explicit 3xx/4xx refusals (for example 402) are terminal but definitely
// unbilled; remaining statuses (for example 500) are terminal ambiguous faults.
func classifyStatus(code int) faultClass {
	switch code {
	case 429:
		return classBusy
	case 401, 403:
		return classKeyAuth
	case 400, 413, 422:
		return classUpstreamRejected
	case 504:
		return classTimeout
	case 502, 503:
		return classUpstream
	default:
		if code >= 300 && code < 500 {
			return classUpstreamRefused
		}
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
