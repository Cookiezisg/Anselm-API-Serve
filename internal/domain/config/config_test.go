package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// validBase returns a Config that passes ValidateSemantics + the default memory
// budget — the fixture every override/semantic test mutates from.
func validBase() Config {
	return Config{
		DeepSeekAPIKeys: []string{"sk-test"},
		DeepSeekBaseURL: "https://api.deepseek.com",
		ModelAllowlist:  []string{"deepseek-chat"},
		DefaultModel:    "deepseek-chat",

		MonthlyQuota:         5000,
		GlobalDailyBudget:    1_000_000,
		InstallDailyTokenCap: 100_000,
		MaxTokensCap:         4096,
		InputTokenCap:        16384,
		MaxMessages:          256,
		MaxMessageChars:      131072,
		NGlobalConcurrency:   8,
		RatePerMin:           20,
		DailySublimit:        0,
		InstallPerIPHour:     10,

		InstallPowMode:       PowModeOff,
		InstallPowDifficulty: 20,

		TokenAnomalyRPM:          0,
		TokenThrottleFactor:      4,
		TokenThrottleCooldownSec: 300,

		UpstreamHeaderTimeout: 60 * time.Second,
		QueueWait:             1500 * time.Millisecond,

		GoMemLimitMiB:      768,
		SQLiteCacheKiB:     32768,
		SQLiteMmapBytes:    256 * 1024 * 1024,
		SQLiteAutockptPage: 4000,
		ReadPoolMaxConns:   4,
		MemBudgetMiB:       2048,
		MemSafetyMarginMiB: 400,

		DiskMinMB:      500,
		DiskMinPercent: 5,

		ResetTZ:       "Asia/Shanghai",
		Location:      time.UTC,
		ListenAddr:    "127.0.0.1:8080",
		AdminAddr:     "127.0.0.1:9090",
		DashboardAddr: "127.0.0.1:8081",
		LogLevel:      "info",
		DBPath:        "anselm-gateway.db",
	}
}

func TestValidBaseIsValid(t *testing.T) {
	c := validBase()
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("validBase failed ValidateSemantics: %v", err)
	}
	if adv, err := c.ValidateMemoryBudget(); adv || err != nil {
		t.Fatalf("validBase memory budget: advisory=%v err=%v", adv, err)
	}
}

// --- ValidateSemantics rules ---

func TestSemanticsInstallCapVsGlobalBudget(t *testing.T) {
	c := validBase()
	c.InstallDailyTokenCap = c.GlobalDailyBudget + 1
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "INSTALL_DAILY_TOKEN_CAP") {
		t.Fatalf("want install-cap>budget error, got %v", err)
	}
}

func TestSemanticsInputPlusMaxVsInstallCap(t *testing.T) {
	c := validBase()
	// Make per-request worst-case exceed the install daily sub-cap.
	c.InstallDailyTokenCap = c.InputTokenCap + c.MaxTokensCap - 1
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "must be <= INSTALL_DAILY_TOKEN_CAP") {
		t.Fatalf("want input+max>install-cap error, got %v", err)
	}
}

func TestSemanticsInputPlusMaxAtBoundPasses(t *testing.T) {
	c := validBase()
	// Exactly at the bound: INPUT + MAX == INSTALL_DAILY_TOKEN_CAP is allowed
	// (the rule is <=, not <) — the day's first request can just fit.
	c.InstallDailyTokenCap = c.InputTokenCap + c.MaxTokensCap
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("input+max == install-cap should pass, got %v", err)
	}
}

func TestSemanticsInputCapZeroDegradesToMaxTokensRule(t *testing.T) {
	// INPUT_TOKEN_CAP=0 disables the input gate: rule ② degrades to
	// MAX_TOKENS_CAP <= INSTALL_DAILY_TOKEN_CAP(输入闸禁用时只约束输出分量).
	c := validBase()
	c.InputTokenCap = 0
	c.MaxTokensCap = 4096
	c.InstallDailyTokenCap = 4096 // == is allowed
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("input-cap=0 with max<=daily should pass, got %v", err)
	}
	c.InstallDailyTokenCap = 4095 // max > daily → reject, naming MAX_TOKENS_CAP
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "MAX_TOKENS_CAP") ||
		!strings.Contains(err.Error(), "when INPUT_TOKEN_CAP=0") {
		t.Fatalf("want degraded-rule error naming MAX_TOKENS_CAP, got %v", err)
	}
}

func TestSemanticsInputCapZeroSkipsCombinedRule(t *testing.T) {
	// With the input gate disabled the combined INPUT+MAX bound must NOT apply: a
	// daily cap that would fail the old combined rule (16384+4096) still passes.
	c := validBase()
	c.InputTokenCap = 0
	c.MaxTokensCap = 4096
	c.InstallDailyTokenCap = 5000
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("combined rule must be skipped at input-cap=0, got %v", err)
	}
}

func TestSemanticsPowSecretRequired(t *testing.T) {
	for _, mode := range []string{PowModeShadow, PowModeEnforce} {
		c := validBase()
		c.InstallPowMode = mode
		c.InstallPowSecret = nil
		err := c.ValidateSemantics()
		if err == nil || !strings.Contains(err.Error(), "CONFIG_POW_SECRET_REQUIRED") {
			t.Fatalf("mode=%s want pow-secret error, got %v", mode, err)
		}
		// With a secret it passes.
		c.InstallPowSecret = []byte("hmac-key")
		if err := c.ValidateSemantics(); err != nil {
			t.Fatalf("mode=%s with secret should pass, got %v", mode, err)
		}
	}
}

func TestSemanticsPowOffNeedsNoSecret(t *testing.T) {
	c := validBase()
	c.InstallPowMode = PowModeOff
	c.InstallPowSecret = nil
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("off mode needs no secret, got %v", err)
	}
}

// --- Memory budget (PERF-2) ---

func TestWorstCaseMemoryFormula(t *testing.T) {
	c := validBase()
	// 768 + (32768/1024)*(1+4) + 256 = 768 + 160 + 256 = 1184
	if got := c.WorstCaseMemoryMiB(); got != 1184 {
		t.Fatalf("worst-case = %d, want 1184", got)
	}
}

func TestMemoryBudgetPass(t *testing.T) {
	c := validBase()
	adv, err := c.ValidateMemoryBudget()
	if adv || err != nil {
		t.Fatalf("default should pass: advisory=%v err=%v", adv, err)
	}
}

func TestMemoryBudgetFailFastWhenGoMemLimitSet(t *testing.T) {
	c := validBase()
	c.SQLiteCacheKiB = 512 * 1024 // 512 MiB/conn → blows the budget
	adv, err := c.ValidateMemoryBudget()
	if adv {
		t.Fatalf("with GOMEMLIMIT>0 overflow must fail-fast, not advise")
	}
	var memErr *ErrMemoryBudget
	if !errors.As(err, &memErr) {
		t.Fatalf("want *ErrMemoryBudget, got %v", err)
	}
	if !strings.Contains(err.Error(), "PERF-2 memory budget exceeded") {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestMemoryBudgetAdvisoryWhenGoMemLimitZero(t *testing.T) {
	c := validBase()
	c.GoMemLimitMiB = 0
	c.SQLiteCacheKiB = 512 * 1024 // over budget but heap unbounded
	adv, err := c.ValidateMemoryBudget()
	if !adv || err != nil {
		t.Fatalf("GOMEMLIMIT=0 overflow → advisory only: advisory=%v err=%v", adv, err)
	}
}

// --- ApplyOverrides: bounds (override path parity with env Max* consts) ---

func TestApplyOverrideBoundsRejectedPerKey(t *testing.T) {
	cases := []struct {
		key, val, want string
	}{
		{"MONTHLY_QUOTA", "0", "MONTHLY_QUOTA must be >= 1"},
		{"MONTHLY_QUOTA", "1000000001", "MONTHLY_QUOTA must be <= 1000000000"},
		{"GLOBAL_DAILY_BUDGET_TOKENS", "0", "GLOBAL_DAILY_BUDGET_TOKENS must be >= 1"},
		{"MAX_TOKENS_CAP", "0", "MAX_TOKENS_CAP must be >= 1"},
		{"MAX_TOKENS_CAP", "1000001", "MAX_TOKENS_CAP must be <= 1000000"},
		{"MAX_MESSAGES", "0", "MAX_MESSAGES must be >= 1"},
		{"MAX_MESSAGES", "100001", "MAX_MESSAGES must be <= 100000"},
		{"RATE_PER_MIN", "-1", "RATE_PER_MIN must be >= 0"},
		{"RATE_PER_MIN", "10000001", "RATE_PER_MIN must be <= 10000000"},
		{"INSTALL_POW_DIFFICULTY", "0", "INSTALL_POW_DIFFICULTY must be >= 1"},
		{"INSTALL_POW_DIFFICULTY", "33", "INSTALL_POW_DIFFICULTY must be <= 32"},
		{"DISK_MIN_PERCENT", "101", "DISK_MIN_PERCENT must be <= 100"},
		{"TOKEN_THROTTLE_FACTOR", "0", "TOKEN_THROTTLE_FACTOR must be >= 1"},
		{"QUEUE_WAIT_MS", "60001", "QUEUE_WAIT_MS must be <= 60000"},
		{"UPSTREAM_HEADER_TIMEOUT_SEC", "0", "UPSTREAM_HEADER_TIMEOUT_SEC must be >= 1"},
	}
	for _, tc := range cases {
		_, err := ApplyOverrides(validBase(), map[string]string{tc.key: tc.val})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: want %q, got %v", tc.key, tc.val, tc.want, err)
		}
	}
}

func TestApplyOverrideBoundsParityWithConsts(t *testing.T) {
	// The override ceilings ARE the exported Max* consts (env-load shares them).
	if Specs() == nil {
		t.Fatal("Specs empty")
	}
	byKey := specByKey()
	if byKey["MONTHLY_QUOTA"].Max != MaxMonthlyQuota {
		t.Fatalf("MONTHLY_QUOTA max %d != const %d", byKey["MONTHLY_QUOTA"].Max, MaxMonthlyQuota)
	}
	if byKey["MAX_MESSAGE_CHARS"].Max != int64(MaxMessageChars) {
		t.Fatalf("MAX_MESSAGE_CHARS max mismatch")
	}
}

func TestApplyOverrideValidInt(t *testing.T) {
	got, err := ApplyOverrides(validBase(), map[string]string{"MONTHLY_QUOTA": "9999"})
	if err != nil {
		t.Fatalf("valid override err: %v", err)
	}
	if got.MonthlyQuota != 9999 {
		t.Fatalf("MonthlyQuota = %d, want 9999", got.MonthlyQuota)
	}
}

func TestApplyOverrideInvalidIntFormat(t *testing.T) {
	_, err := ApplyOverrides(validBase(), map[string]string{"MONTHLY_QUOTA": "abc"})
	if err == nil || !strings.Contains(err.Error(), "invalid int") {
		t.Fatalf("want invalid-int error, got %v", err)
	}
}

func TestApplyOverrideInputTokenCapZeroAccepted(t *testing.T) {
	// 0 = disable the input estimate gate (spec floor is 0, not 1) — must be
	// accepted and land as 0; semantics degrade to the output-only rule.
	got, err := ApplyOverrides(validBase(), map[string]string{"INPUT_TOKEN_CAP": "0"})
	if err != nil {
		t.Fatalf("INPUT_TOKEN_CAP=0 should be accepted, got %v", err)
	}
	if got.InputTokenCap != 0 {
		t.Fatalf("InputTokenCap = %d, want 0", got.InputTokenCap)
	}
}

func TestApplyOverrideMaxBodyBytesValid(t *testing.T) {
	got, err := ApplyOverrides(validBase(), map[string]string{"MAX_BODY_BYTES": "4194304"})
	if err != nil {
		t.Fatalf("MAX_BODY_BYTES=4194304 should be accepted, got %v", err)
	}
	if got.MaxBodyBytes != 4194304 {
		t.Fatalf("MaxBodyBytes = %d, want 4194304", got.MaxBodyBytes)
	}
}

func TestApplyOverrideMaxBodyBytesBoundsRejected(t *testing.T) {
	// Floor 4KiB (install/dashboard bodies must fit); ceiling 8MiB (PERF-2 box
	// bound) — one byte past either side is rejected by name.
	cases := []struct{ val, want string }{
		{"4095", "MAX_BODY_BYTES must be >= 4096"},
		{"8388609", "MAX_BODY_BYTES must be <= 8388608"},
	}
	for _, tc := range cases {
		_, err := ApplyOverrides(validBase(), map[string]string{"MAX_BODY_BYTES": tc.val})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("MAX_BODY_BYTES=%s: want %q, got %v", tc.val, tc.want, err)
		}
	}
}

func TestApplyOverrideModelAllowlist(t *testing.T) {
	got, err := ApplyOverrides(validBase(), map[string]string{"MODEL_ALLOWLIST": " a , b ,"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got.ModelAllowlist) != 2 || got.DefaultModel != "a" {
		t.Fatalf("allowlist=%v default=%q", got.ModelAllowlist, got.DefaultModel)
	}
}

func TestApplyOverrideModelAllowlistEmptyRejected(t *testing.T) {
	_, err := ApplyOverrides(validBase(), map[string]string{"MODEL_ALLOWLIST": " , ,"})
	if err == nil || !strings.Contains(err.Error(), "MODEL_ALLOWLIST must not be empty") {
		t.Fatalf("want empty-allowlist error, got %v", err)
	}
}

// --- ApplyOverrides: rejection by name (secret / startup-hard / unknown) ---

func TestApplyOverrideRejectsStartupHardByName(t *testing.T) {
	for _, key := range []string{"GOMEMLIMIT_MIB", "SQLITE_CACHE_KIB", "READ_POOL_MAX_CONNS", "SQLITE_MMAP_MB", "RESET_TZ", "LISTEN_ADDR", "ADMIN_ADDR", "DASHBOARD_ADDR", "GATEWAY_DB_PATH"} {
		_, err := ApplyOverrides(validBase(), map[string]string{key: "123"})
		if err == nil || !strings.Contains(err.Error(), "startup hard-constraint") || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: want named startup-hard rejection, got %v", key, err)
		}
	}
}

func TestApplyOverrideRejectsSecretByName(t *testing.T) {
	for _, key := range []string{"DEEPSEEK_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD"} {
		_, err := ApplyOverrides(validBase(), map[string]string{key: "x"})
		if err == nil || !strings.Contains(err.Error(), "unknown or secret") || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: want named secret/unknown rejection, got %v", key, err)
		}
	}
}

func TestApplyOverrideRejectsUnknownByName(t *testing.T) {
	_, err := ApplyOverrides(validBase(), map[string]string{"NOPE_NOT_A_KEY": "1"})
	if err == nil || !strings.Contains(err.Error(), "NOPE_NOT_A_KEY") || !strings.Contains(err.Error(), "unknown or secret") {
		t.Fatalf("want named unknown rejection, got %v", err)
	}
}

// --- ApplyOverrides: PoW hot-flip needs base-inherited secret ---

func TestApplyOverridePowFlipWithoutSecretRejected(t *testing.T) {
	base := validBase() // off + no secret
	_, err := ApplyOverrides(base, map[string]string{"INSTALL_POW_MODE": "enforce"})
	if err == nil || !strings.Contains(err.Error(), "CONFIG_POW_SECRET_REQUIRED") {
		t.Fatalf("flip to enforce w/o secret must be rejected, got %v", err)
	}
}

func TestApplyOverridePowFlipWithBaseSecretOK(t *testing.T) {
	base := validBase()
	base.InstallPowSecret = []byte("hmac-key") // inherited from env
	got, err := ApplyOverrides(base, map[string]string{"INSTALL_POW_MODE": "shadow"})
	if err != nil {
		t.Fatalf("flip with base secret should pass, got %v", err)
	}
	if got.InstallPowMode != PowModeShadow {
		t.Fatalf("mode=%q want shadow", got.InstallPowMode)
	}
}

func TestApplyOverridePowModeInvalidValue(t *testing.T) {
	_, err := ApplyOverrides(validBase(), map[string]string{"INSTALL_POW_MODE": "enabled"})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("want invalid-mode error, got %v", err)
	}
}

// --- ApplyOverrides: cross-field semantics re-run on the batch ---

func TestApplyOverrideReRunsSemantics(t *testing.T) {
	// Individually valid, but together they break install-cap <= global-budget.
	_, err := ApplyOverrides(validBase(), map[string]string{
		"GLOBAL_DAILY_BUDGET_TOKENS": "50000",
		"INSTALL_DAILY_TOKEN_CAP":    "60000",
	})
	if err == nil || !strings.Contains(err.Error(), "INSTALL_DAILY_TOKEN_CAP") {
		t.Fatalf("batch must re-run semantics, got %v", err)
	}
}

// --- ApplyOverrides: all-or-nothing (no partial state on failure) ---

func TestApplyOverrideAllOrNothing(t *testing.T) {
	base := validBase()
	// First key valid, second key out of bounds → whole batch must fail and the
	// returned Config must be the zero value (caller keeps base).
	got, err := ApplyOverrides(base, map[string]string{
		"MONTHLY_QUOTA":  "123",
		"MAX_TOKENS_CAP": "9999999", // > max → reject
	})
	if err == nil {
		t.Fatal("want error from out-of-bounds key")
	}
	// On error the returned Config must be the zero value (no partial apply): the
	// valid first key (MonthlyQuota=123) must NOT have landed.
	if got.MonthlyQuota != 0 || got.ModelAllowlist != nil {
		t.Fatalf("on error must return zero Config, got partial: %+v", got)
	}
	// Base must be untouched.
	if base.MonthlyQuota != 5000 {
		t.Fatalf("base mutated: MonthlyQuota=%d", base.MonthlyQuota)
	}
}

func TestApplyOverrideDoesNotMutateBaseOnSuccess(t *testing.T) {
	base := validBase()
	_, err := ApplyOverrides(base, map[string]string{"MONTHLY_QUOTA": "777"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base.MonthlyQuota != 5000 {
		t.Fatalf("base mutated on success: %d", base.MonthlyQuota)
	}
}

func TestApplyOverrideEmptyIsNoop(t *testing.T) {
	base := validBase()
	got, err := ApplyOverrides(base, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.MonthlyQuota != base.MonthlyQuota {
		t.Fatalf("empty override changed config")
	}
}

// --- Dump / tier descriptor table ---

func TestDumpSurfacesAllSpecsNoSecrets(t *testing.T) {
	c := validBase()
	items := c.Dump()
	if len(items) != len(Specs()) {
		t.Fatalf("Dump len %d != Specs len %d", len(items), len(Specs()))
	}
	for _, it := range items {
		switch it.Key {
		case "DEEPSEEK_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD":
			t.Fatalf("secret %q leaked into Dump", it.Key)
		}
	}
}

func TestDumpEditableAndRestartFlags(t *testing.T) {
	c := validBase()
	byKey := make(map[string]DumpItem)
	for _, it := range c.Dump() {
		byKey[it.Key] = it
	}
	if !byKey["MONTHLY_QUOTA"].Editable || byKey["MONTHLY_QUOTA"].RestartRequired {
		t.Fatalf("MONTHLY_QUOTA should be editable, not restart")
	}
	// N_GLOBAL_CONCURRENCY: editable yet restart-effective.
	ng := byKey["N_GLOBAL_CONCURRENCY"]
	if !ng.Editable || !ng.RestartRequired {
		t.Fatalf("N_GLOBAL_CONCURRENCY want editable+restart, got %+v", ng)
	}
	// Startup-hard: read-only + restart.
	gm := byKey["GOMEMLIMIT_MIB"]
	if gm.Editable || !gm.RestartRequired {
		t.Fatalf("GOMEMLIMIT_MIB want read-only+restart, got %+v", gm)
	}
}

func TestDumpBoundedHintsMatchApply(t *testing.T) {
	c := validBase()
	byKey := make(map[string]DumpItem)
	for _, it := range c.Dump() {
		byKey[it.Key] = it
	}
	mq := byKey["MONTHLY_QUOTA"]
	if !mq.Bounded || mq.Min != 1 || mq.Max != MaxMonthlyQuota {
		t.Fatalf("MONTHLY_QUOTA hints wrong: %+v", mq)
	}
	// MODEL_ALLOWLIST is a string list — not bounded.
	if byKey["MODEL_ALLOWLIST"].Bounded {
		t.Fatalf("MODEL_ALLOWLIST should not be bounded")
	}
}

func TestSpecMaxBodyBytesFlags(t *testing.T) {
	// MAX_BODY_BYTES: runtime-hot (editable/persisted/validated hot) yet
	// RestartRequired — like N_GLOBAL_CONCURRENCY the middleware chain's
	// MaxBytesReader bound is assembled once at boot. Bounds = [4KiB, 8MiB].
	s, ok := specByKey()["MAX_BODY_BYTES"]
	if !ok {
		t.Fatal("MAX_BODY_BYTES missing from Specs")
	}
	if s.Tier != TierRuntimeHot || !s.RestartRequired || !s.Bounded {
		t.Fatalf("MAX_BODY_BYTES spec flags wrong: tier=%v restart=%v bounded=%v", s.Tier, s.RestartRequired, s.Bounded)
	}
	if s.Min != MinBodyBytes || s.Max != MaxBodyBytesCeiling {
		t.Fatalf("MAX_BODY_BYTES bounds = [%d,%d], want [%d,%d]", s.Min, s.Max, MinBodyBytes, MaxBodyBytesCeiling)
	}
	// Dump surfaces the same hints (editable + restart + bounds) and the live value.
	c := validBase()
	c.MaxBodyBytes = 262144
	for _, it := range c.Dump() {
		if it.Key != "MAX_BODY_BYTES" {
			continue
		}
		if !it.Editable || !it.RestartRequired || !it.Bounded || it.Min != MinBodyBytes || it.Max != MaxBodyBytesCeiling {
			t.Fatalf("MAX_BODY_BYTES Dump hints wrong: %+v", it)
		}
		if it.Value != "262144" {
			t.Fatalf("MAX_BODY_BYTES Dump value = %q, want 262144", it.Value)
		}
		return
	}
	t.Fatal("MAX_BODY_BYTES missing from Dump")
}

func TestDumpReflectsLiveValue(t *testing.T) {
	c := validBase()
	c.MonthlyQuota = 4242
	for _, it := range c.Dump() {
		if it.Key == "MONTHLY_QUOTA" && it.Value != "4242" {
			t.Fatalf("Dump value %q != 4242", it.Value)
		}
	}
}

// --- Bound helpers (shared by env-load + override) ---

func TestBoundHelpers(t *testing.T) {
	if err := BoundInt("x", 5, 1, 10); err != nil {
		t.Fatalf("in-range should pass: %v", err)
	}
	if err := BoundInt("x", 0, 1, 10); err == nil || !strings.Contains(err.Error(), ">=") {
		t.Fatalf("under floor: %v", err)
	}
	if err := BoundInt64("y", 11, 1, 10); err == nil || !strings.Contains(err.Error(), "<=") {
		t.Fatalf("over ceiling: %v", err)
	}
}

func TestValidatePowMode(t *testing.T) {
	for _, m := range []string{PowModeOff, PowModeShadow, PowModeEnforce} {
		if err := ValidatePowMode(m); err != nil {
			t.Fatalf("%s should be valid: %v", m, err)
		}
	}
	if err := ValidatePowMode("bogus"); err == nil {
		t.Fatal("bogus mode should be rejected")
	}
}
