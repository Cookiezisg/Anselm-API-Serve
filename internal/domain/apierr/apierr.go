// Package apierr is the single source of truth for the gateway's structured
// error type and every stable wire code. It is pure domain: zero net/http,
// zero DB. The transport layer (transport/httpapi/response) renders an
// *APIError into the {"error":{"code","message"[,"details"]}} envelope; the
// type itself carries no rendering. Keeping wire codes here — the innermost
// layer — lets domain/app/infra/transport all import them with no reverse dep.
//
// Wire codes are an external contract (SPA, Caddy, operators depend on the
// exact values), so their status/code/message are spec, not arbitrary.
package apierr

import "strconv"

// APIError is a typed gateway error carrying both an HTTP status and a stable
// UPPER_SNAKE wire code. Details is omitempty and carries a machine-actionable
// field for the rare case that needs one (currently only LOGIN_LOCKED's
// retryAfterSec). Secrets MUST NOT enter Message (GW-INV-11/17).
type APIError struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Error reports code + message; never includes Details (which may be logged).
func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// NewError builds an APIError without details.
func NewError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// NewErrorWithDetails builds an APIError carrying a structured details map,
// e.g. the dashboard login lockout's retryAfterSec.
func NewErrorWithDetails(status int, code, message string, details map[string]any) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Details: details}
}

// LoginLocked returns the dashboard per-IP backoff error carrying retryAfterSec
// (delta-seconds). The transport layer also sets the Retry-After header from
// this value; the details field lets the SPA act without parsing the message.
func LoginLocked(retryAfterSec int) *APIError {
	return NewErrorWithDetails(StatusLoginLocked, "LOGIN_LOCKED",
		"too many attempts, retry later",
		map[string]any{"retryAfterSec": retryAfterSec})
}

// HTTP status constants for the wire codes. Kept as untyped ints (not
// net/http.Status*) so this package never imports net/http and stays pure
// domain; the values are the spec.
const (
	statusBadRequest          = 400
	statusNotFound            = 404
	statusUnprocessableEntity = 422
	statusRequestTooLarge     = 413
	statusUnauthorized        = 401
	statusPaymentRequired     = 402
	statusForbidden           = 403
	statusConflict            = 409
	statusTooManyRequests     = 429
	statusBadGateway          = 502
	statusServiceUnavailable  = 503
	statusGatewayTimeout      = 504
	statusInternalServerError = 500

	// StatusLoginLocked is exported for the dashboard backoff helper.
	StatusLoginLocked = statusTooManyRequests
)

// Sentinel wire codes — the §5.2 status/code table, verbatim. Messages are
// client-safe (no upstream body, no key). These are the machine contract;
// clients branch on Code (UPPER_SNAKE), never on an OpenAI-style type/param.
var (
	// ErrInvalidInstall — the public install id is absent or unknown.
	ErrInvalidInstall = NewError(statusUnauthorized, "INVALID_INSTALL", "missing or invalid install id")
	// ErrDeviceProofRequired — request has no device-bound proof.
	ErrDeviceProofRequired = NewError(statusUnauthorized, "DEVICE_PROOF_REQUIRED", "device proof is required")
	// ErrDeviceProofInvalid — signature or signed request material is invalid.
	ErrDeviceProofInvalid = NewError(statusUnauthorized, "DEVICE_PROOF_INVALID", "device proof is invalid")
	// ErrDeviceProofNonceInvalid — challenge is forged, expired, or from a prior process.
	ErrDeviceProofNonceInvalid = NewError(statusUnauthorized, "DEVICE_PROOF_NONCE_INVALID", "device proof challenge expired; fetch a fresh challenge")
	// ErrDeviceProofReplayed — the signed request id was already consumed.
	ErrDeviceProofReplayed = NewError(statusConflict, "DEVICE_PROOF_REPLAYED", "device proof was already used")
	// ErrAccountBanned — this install has been disabled.
	ErrAccountBanned = NewError(statusForbidden, "ACCOUNT_BANNED", "this install has been disabled")
	// ErrRateLimited — per-minute rate or daily sublimit exceeded.
	ErrRateLimited = NewError(statusTooManyRequests, "RATE_LIMITED", "rate or daily sub-limit exceeded")
	// ErrRequestBodyTooLarge — the exact HTTP request body crossed the
	// configured memory-safety envelope. This is deliberately distinct from a
	// model context rejection.
	ErrRequestBodyTooLarge = NewError(statusRequestTooLarge, "REQUEST_BODY_TOO_LARGE", "request body exceeds the configured size limit")
	// ErrQuotaExhausted — monthly free-tier quota exhausted.
	ErrQuotaExhausted = NewError(statusTooManyRequests, "QUOTA_EXHAUSTED", "monthly free-tier quota exhausted")
	// ErrUpstreamBusy — upstream capacity busy (queue full/timeout, breaker open,
	// upstream 429). Its own class so it is never retried/breakered (GW-INV-23).
	ErrUpstreamBusy = NewError(statusTooManyRequests, "UPSTREAM_BUSY", "upstream capacity is busy, retry shortly")
	// ErrBudgetExhausted — daily free-tier budget reached. Note 402, not 429.
	ErrBudgetExhausted = NewError(statusPaymentRequired, "BUDGET_EXHAUSTED", "daily service budget reached, try again tomorrow")
	// ErrBadRequest — invalid request body.
	ErrBadRequest = NewError(statusBadRequest, "BAD_REQUEST", "invalid request body")
	// ErrUpstreamError — upstream model provider error.
	ErrUpstreamError = NewError(statusBadGateway, "UPSTREAM_ERROR", "upstream model provider error")
	// ErrUpstreamTimeout — upstream model provider timeout.
	ErrUpstreamTimeout = NewError(statusGatewayTimeout, "UPSTREAM_TIMEOUT", "upstream model provider timeout")
	// ErrMultimodalUnavailable — this deployment has no configured multimodal
	// provider credential. It is deliberately distinct from UPSTREAM_BUSY: retrying
	// cannot help until the operator enables the visual provider, and text remains available.
	ErrMultimodalUnavailable = NewError(statusServiceUnavailable, "MULTIMODAL_UNAVAILABLE", "multimodal input is unavailable on this deployment")
	// ErrAudioUnavailable — audio is a valid public content part, but the current fixed routing
	// table deliberately has no audio-capable upstream. It is distinct from malformed input and from
	// a missing visual-provider credential, so clients can preserve their attachment and retry after an upgrade.
	ErrAudioUnavailable = NewError(statusServiceUnavailable, "AUDIO_UNAVAILABLE", "audio input is not available on this deployment")
	// ErrMediaUnavailable means durable media ingress has not been explicitly
	// configured on this gateway. It is not a malformed attachment and retrying
	// the same bytes cannot repair it.
	ErrMediaUnavailable = NewError(statusServiceUnavailable, "MEDIA_UNAVAILABLE", "durable media upload is not enabled on this deployment")
	// Media upload failures intentionally distinguish client-fixable lifecycle
	// mistakes from a verified object whose bytes/digest do not match.
	ErrMediaUploadInvalid   = NewError(statusBadRequest, "MEDIA_UPLOAD_INVALID", "invalid media upload request")
	ErrMediaUploadNotFound  = NewError(statusNotFound, "MEDIA_UPLOAD_NOT_FOUND", "media upload was not found")
	ErrMediaUploadConflict  = NewError(statusConflict, "MEDIA_UPLOAD_CONFLICT", "media upload is not writable in its current state")
	ErrMediaIntegrityFailed = NewError(statusUnprocessableEntity, "MEDIA_INTEGRITY_FAILED", "media upload failed integrity verification")

	// /install reject paths use DISTINCT codes for audit separation (GW-INV-20):

	// ErrInstallRateLimited — per-IP install rate exceeded (INSTALL_PER_IP_HOUR).
	ErrInstallRateLimited = NewError(statusTooManyRequests, "INSTALL_RATE_LIMITED", "too many installs from this address, retry later")
	// ErrInstallCapReached — global daily coarse cap reached (INSTALL_GLOBAL_DAILY_CAP).
	ErrInstallCapReached = NewError(statusTooManyRequests, "INSTALL_CAP_REACHED", "install issuance is temporarily at capacity, retry later")
	// ErrInstallFPLimited — per-fingerprint daily limit / cooldown window.
	ErrInstallFPLimited = NewError(statusTooManyRequests, "INSTALL_FP_LIMITED", "too many installs for this client, retry later")
	// ErrInstallPoWRequired — enforce mode, missing X-PoW (403: re-fetch challenge,
	// do not back off and retry).
	ErrInstallPoWRequired = NewError(statusForbidden, "INSTALL_POW_REQUIRED", "proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW")
	// ErrInstallPoWInvalid — HMAC forgery / expired challenge / difficulty miss /
	// nonce reuse.
	ErrInstallPoWInvalid = NewError(statusForbidden, "INSTALL_POW_INVALID", "invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry")

	// ErrDiskLow — REL-6 low-disk read-only degradation (GW-INV-29).
	ErrDiskLow = NewError(statusServiceUnavailable, "DISK_LOW", "service temporarily read-only: low disk space")
	// ErrQuotaResetBusy — a dashboard-wide quota reset must wait until all
	// current-month reservations have reached a terminal ledger state.
	ErrQuotaResetBusy = NewError(statusConflict, "QUOTA_RESET_BUSY", "quota reset is waiting for active requests to settle")
)

// CodeUpstreamRejected is the wire code for an upstream-originated request
// rejection; exported as a const so callers branch without a magic string.
const CodeUpstreamRejected = "UPSTREAM_REJECTED"

// Coarse machine reasons an UPSTREAM_REJECTED carries in details.reason — a
// CLOSED enum derived by the upstream layer from the provider's 4xx error shape.
// Upstream TEXT never passes through (GW-INV-11); only one of these values does.
const (
	// RejectedContextLength — input (+ max_tokens) exceeds the model's context window.
	RejectedContextLength = "context_length"
	// RejectedMaxTokens — max_tokens outside the model's accepted range.
	RejectedMaxTokens = "max_tokens"
	// RejectedInvalid — any other request-shaped upstream rejection.
	RejectedInvalid = "invalid_request"
)

// UpstreamRejected — the upstream model provider rejected the request as invalid
// (e.g. context length exceeded). 400: the CLIENT must change the request, so it
// is neither retried nor a breaker fault (ADR-011). The message is fixed and the
// details carry only the coarse reason enum — never the upstream body (GW-INV-11).
func UpstreamRejected(reason string) *APIError {
	return NewErrorWithDetails(statusBadRequest, CodeUpstreamRejected,
		"upstream rejected the request: reduce input size or max_tokens, or fix request parameters",
		map[string]any{"reason": reason})
}

// Internal is the normalization target for any non-*APIError reaching the
// transport renderer — never leak internal detail/upstream body/key (§4.3).
// The renderer constructs this; defined here so the contract has one home.
func Internal() *APIError {
	return NewError(statusInternalServerError, "INTERNAL", "internal error")
}

// retryAfterString formats a delta-seconds value for the Retry-After header.
// Exposed for the transport layer so the header and details stay in lockstep.
func retryAfterString(sec int) string { return strconv.Itoa(sec) }

// RetryAfterSeconds returns the retryAfterSec from an APIError's Details (if
// present) plus its string form for the Retry-After header. ok is false when
// the error carries no such field. Lets transport render the header without
// re-deriving the value.
func (e *APIError) RetryAfterSeconds() (sec int, header string, ok bool) {
	if e == nil || e.Details == nil {
		return 0, "", false
	}
	v, present := e.Details["retryAfterSec"]
	if !present {
		return 0, "", false
	}
	n, isInt := v.(int)
	if !isInt {
		return 0, "", false
	}
	return n, retryAfterString(n), true
}
