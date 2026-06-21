package apierr

import "testing"

// GW-INV (stable wire codes): every sentinel's status/code/message is the
// external contract and must match spec-error-codes.md §2 verbatim. A drift
// here is a breaking change for every client, so assert exact bytes.
func TestSentinelsMatchSpecVerbatim(t *testing.T) {
	cases := []struct {
		name    string
		err     *APIError
		status  int
		code    string
		message string
	}{
		{"INVALID_TOKEN", ErrInvalidToken, 401, "INVALID_TOKEN", "missing or invalid install token"},
		{"ACCOUNT_BANNED", ErrAccountBanned, 403, "ACCOUNT_BANNED", "this install has been disabled"},
		{"RATE_LIMITED", ErrRateLimited, 429, "RATE_LIMITED", "rate or daily sub-limit exceeded"},
		{"QUOTA_EXHAUSTED", ErrQuotaExhausted, 429, "QUOTA_EXHAUSTED", "monthly free-tier quota exhausted"},
		{"UPSTREAM_BUSY", ErrUpstreamBusy, 429, "UPSTREAM_BUSY", "upstream capacity is busy, retry shortly"},
		{"BUDGET_EXHAUSTED", ErrBudgetExhausted, 402, "BUDGET_EXHAUSTED", "daily free-tier budget reached, try again tomorrow or use your own key"},
		{"BAD_REQUEST", ErrBadRequest, 400, "BAD_REQUEST", "invalid request body"},
		{"UPSTREAM_ERROR", ErrUpstreamError, 502, "UPSTREAM_ERROR", "upstream model provider error"},
		{"UPSTREAM_TIMEOUT", ErrUpstreamTimeout, 504, "UPSTREAM_TIMEOUT", "upstream model provider timeout"},
		{"INSTALL_RATE_LIMITED", ErrInstallRateLimited, 429, "INSTALL_RATE_LIMITED", "too many installs from this address, retry later"},
		{"INSTALL_CAP_REACHED", ErrInstallCapReached, 429, "INSTALL_CAP_REACHED", "install issuance is temporarily at capacity, retry later"},
		{"INSTALL_FP_LIMITED", ErrInstallFPLimited, 429, "INSTALL_FP_LIMITED", "too many installs for this client, retry later"},
		{"INSTALL_POW_REQUIRED", ErrInstallPoWRequired, 403, "INSTALL_POW_REQUIRED", "proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW"},
		{"INSTALL_POW_INVALID", ErrInstallPoWInvalid, 403, "INSTALL_POW_INVALID", "invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry"},
		{"DISK_LOW", ErrDiskLow, 503, "DISK_LOW", "service temporarily read-only: low disk space"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.Status != c.status {
				t.Errorf("status = %d, want %d", c.err.Status, c.status)
			}
			if c.err.Code != c.code {
				t.Errorf("code = %q, want %q", c.err.Code, c.code)
			}
			if c.err.Message != c.message {
				t.Errorf("message = %q, want %q", c.err.Message, c.message)
			}
			if c.err.Details != nil {
				t.Errorf("sentinel must carry no details, got %v", c.err.Details)
			}
		})
	}
}

func TestErrorMethodIsCodeColonMessage(t *testing.T) {
	got := ErrRateLimited.Error()
	want := "RATE_LIMITED: rate or daily sub-limit exceeded"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewErrorHasNoDetails(t *testing.T) {
	e := NewError(418, "TEAPOT", "short and stout")
	if e.Status != 418 || e.Code != "TEAPOT" || e.Message != "short and stout" {
		t.Fatalf("unexpected fields: %+v", e)
	}
	if e.Details != nil {
		t.Errorf("details should be nil, got %v", e.Details)
	}
}

func TestNewErrorWithDetailsCarriesMap(t *testing.T) {
	e := NewErrorWithDetails(429, "X", "y", map[string]any{"k": 1})
	if e.Details == nil || e.Details["k"] != 1 {
		t.Fatalf("details not carried: %+v", e.Details)
	}
}

// LOGIN_LOCKED is the one wire code carrying details (retryAfterSec) plus the
// Retry-After header; assert the shape transport relies on.
func TestLoginLockedDetailsAndRetryAfter(t *testing.T) {
	e := LoginLocked(30)
	if e.Status != 429 || e.Code != "LOGIN_LOCKED" || e.Message != "too many attempts, retry later" {
		t.Fatalf("unexpected fields: %+v", e)
	}
	if got := e.Details["retryAfterSec"]; got != 30 {
		t.Errorf("retryAfterSec = %v, want 30", got)
	}
	sec, header, ok := e.RetryAfterSeconds()
	if !ok || sec != 30 || header != "30" {
		t.Errorf("RetryAfterSeconds() = (%d,%q,%v), want (30,\"30\",true)", sec, header, ok)
	}
}

func TestRetryAfterSecondsAbsentWhenNoDetails(t *testing.T) {
	if _, _, ok := ErrRateLimited.RetryAfterSeconds(); ok {
		t.Error("sentinel without details should report ok=false")
	}
	var nilErr *APIError
	if _, _, ok := nilErr.RetryAfterSeconds(); ok {
		t.Error("nil receiver should report ok=false, not panic")
	}
}

// Non-APIError normalization target: transport renders any unknown error as
// this 500, leaking nothing (§4.3 / GW-INV-11).
func TestInternalFallbackShape(t *testing.T) {
	e := Internal()
	if e.Status != 500 || e.Code != "INTERNAL" || e.Message != "internal error" {
		t.Fatalf("unexpected internal fallback: %+v", e)
	}
}
