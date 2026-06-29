package install

import "time"

// IssueParams is the fully-resolved input to one atomic issuance: the normalized
// request, the precomputed window keys, and the per-gate enable+ceiling values.
// It is a pure domain value shared by the app port and its infra implementation
// (so infra never imports app). The app resolves these from a config snapshot (a
// disabled gate has Enabled false so the store never touches its table — dormant
// zero-cost); the token hash + install id are supplied so domain owns hashing and
// idgen owns id minting.
type IssueParams struct {
	InstallID   string
	TokenSHA256 string
	Fingerprint string // plaintext for the installs row; "" → NULL.
	Client      string // "" → NULL.
	Now         time.Time

	IPGate     IPGate
	GlobalGate GlobalGate
	FPGate     FPGate
}

// IPGate is the always-on per-IP hourly bucket (INSTALL_PER_IP_HOUR, min 1 —
// never disabled). Key is the hashed, /64-collapsed client IP.
type IPGate struct {
	Key        string
	WindowHour string // "2006-01-02T15" in RESET_TZ.
	Max        int
}

// GlobalGate is the global daily coarse valve (INSTALL_GLOBAL_DAILY_CAP, 0=off).
// Enabled false short-circuits it before any DB work.
type GlobalGate struct {
	Enabled   bool
	WindowDay string // "2006-01-02" in RESET_TZ.
	Cap       int64
}

// FPGate is the per-fingerprint daily cap + cooldown (INSTALL_PER_FP_DAILY /
// INSTALL_PER_FP_COOLDOWN_SEC, both 0=off). Enabled false (or an empty
// fingerprint) short-circuits it before any DB work and writes no fp row.
type FPGate struct {
	Enabled   bool
	FPSHA256  string
	WindowDay string
	// Cap is the daily ceiling; when the daily-cap sub-gate is off the app passes a
	// non-binding sentinel so `count < Cap` is never the binding predicate.
	Cap int64
	// Cutoff is the cooldown boundary: bump only when the stored last_at < Cutoff.
	// When the cooldown is off the app sets it far in the future so it never blocks.
	Cutoff time.Time
}

// IssueResult reports the outcome of an issuance. Admitted true means the installs
// row committed; otherwise Gate names the tripped guardrail (for the audit + the
// wire-code mapping).
type IssueResult struct {
	Admitted bool
	Gate     Gate
}

// Gate identifies which guardrail tripped, for the unsampled WARN audit field and
// the apierr mapping. The values are the spec's audit `gate` labels (GW-INV-20).
type Gate string

const (
	// GateNone — admitted (no gate tripped).
	GateNone Gate = ""
	// GateIP — per-IP hourly limit.
	GateIP Gate = "ip"
	// GateGlobal — global daily coarse cap.
	GateGlobal Gate = "global"
	// GateFP — per-fingerprint daily cap / cooldown.
	GateFP Gate = "fp"
)
