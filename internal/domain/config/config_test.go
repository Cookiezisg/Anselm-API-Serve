package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// validBase returns a Config that passes ValidateSemantics + the default memory
// budget — the fixture every override/semantic test mutates from.
func validBase() Config {
	return Config{
		QwenAPIKeys:             []string{"qwen-test"},
		QwenBaseURL:             "https://ws_test.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
		PublicModelID:           "anselm-auto",
		MultimodalUpstreamModel: billing.Qwen37Plus,

		MonthlyQuota:           5000,
		GlobalMonthlySpendPUSD: 420 * billing.PicoUSDPerUSD,
		MaxTokensCap:           4096,
		InputTokenCap:          16384,
		MaxMessages:            256,
		MaxMessageChars:        131072,
		MaxBodyBytes:           4 * 1024 * 1024,
		MaxMediaParts:          8,
		MaxMediaDecodedBytes:   3 * 1024 * 1024,
		NGlobalConcurrency:     8,
		RatePerMin:             0,
		DailySublimit:          0,
		InstallPerIPHour:       0,

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

func TestSemanticsMonthlySpendMustBePositive(t *testing.T) {
	c := validBase()
	c.GlobalMonthlySpendPUSD = 0
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "GLOBAL_MONTHLY_SPEND_MICRO_USD") {
		t.Fatalf("want monthly budget error, got %v", err)
	}
}

func TestSemanticsMultimodalQuoteVsMonthlyBudget(t *testing.T) {
	c := validBase()
	c.GlobalMonthlySpendPUSD = 200_000_000_000 // $0.20: above full text quote, below Qwen hard quote.
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "worst-case request quote") {
		t.Fatalf("want quote>monthly-budget error, got %v", err)
	}
}

func TestSemanticsMultimodalQuoteAtMonthlyBudgetBoundPasses(t *testing.T) {
	c := validBase()
	p, err := billing.NewPlan(billing.ProviderQwen, billing.Qwen37Plus,
		billing.InputStandard, billing.Qwen37InputLimit, billing.Qwen37OutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	c.GlobalMonthlySpendPUSD = p.ReservedPUSD
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("quote == monthly budget should pass, got %v", err)
	}
}

func TestSemanticsVideoRequiresExactI2VRateCard(t *testing.T) {
	c := validBase()
	c.VideoEnabled = true
	c.VideoUpstreamModel = billing.Wan27T2V
	c.VideoI2VUpstreamModel = "wan-not-an-i2v-card"
	c.DashScopeNativeBase = "https://ws-test.cn-beijing.maas.aliyuncs.com"
	c.VideoHandleKey = []byte("derived-handle-key")
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "VIDEO_I2V_UPSTREAM_MODEL") {
		t.Fatalf("want exact I2V rate-card error, got %v", err)
	}
}

func TestVideoI2VAvailabilityRequiresTheWholeVideoPathAndPricedModel(t *testing.T) {
	c := validBase()
	c.VideoEnabled = true
	c.VideoHandleKey = []byte("derived-handle-key")
	c.VideoI2VUpstreamModel = billing.Wan27I2V
	if !c.VideoI2VAvailable() {
		t.Fatal("complete I2V path must be advertised")
	}
	c.VideoI2VUpstreamModel = "unknown"
	if c.VideoI2VAvailable() {
		t.Fatal("an unpriced I2V model must not be advertised")
	}
	c.VideoI2VUpstreamModel = billing.Wan27I2V
	c.VideoHandleKey = nil
	if c.VideoI2VAvailable() {
		t.Fatal("I2V without a pollable video path must not be advertised")
	}
}

// The rate card is required UNCONDITIONALLY, because there is no longer any
// deployment shape in which the model goes unused. This replaces an older test
// that asserted the opposite — that a credential-less Qwen could not constrain
// startup — which described a text-only deployment that no longer exists.
//
// 费率卡是**无条件**必需的,因为再也不存在「那个模型用不上」的部署形态。它取代了一个断言相反
// 命题的旧测试(没有凭证的 Qwen 不该拖住启动)——那描述的是一个已经不存在的纯文本部署。
func TestSemanticsRequiresARateCardForTheRoutedModel(t *testing.T) {
	c := validBase()
	c.MultimodalUpstreamModel = "unpriced-model"
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "no exact rate card") {
		t.Fatalf("an unpriced routed model must fail startup, got %v", err)
	}
}

func TestSemanticsInputCapZeroAccepted(t *testing.T) {
	c := validBase()
	c.InputTokenCap = 0
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("input-cap=0 should defer text input bound to runtime, got %v", err)
	}
}

func TestSemanticsUnknownPricedModelFailsClosed(t *testing.T) {
	c := validBase()
	c.MultimodalUpstreamModel = "qwen-latest"
	err := c.ValidateSemantics()
	if err == nil || !strings.Contains(err.Error(), "no exact rate card") {
		t.Fatalf("unknown model must fail closed, got %v", err)
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
		{"GLOBAL_MONTHLY_SPEND_MICRO_USD", "0", "GLOBAL_MONTHLY_SPEND_MICRO_USD must be >= 1"},
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

func TestApplyOverridePublicModelID(t *testing.T) {
	got, err := ApplyOverrides(validBase(), map[string]string{"PUBLIC_MODEL_ID": " app-model "})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.PublicModelID != "app-model" {
		t.Fatalf("public=%q", got.PublicModelID)
	}
}

func TestApplyOverridePublicModelIDEmptyRejected(t *testing.T) {
	_, err := ApplyOverrides(validBase(), map[string]string{"PUBLIC_MODEL_ID": "   "})
	if err == nil || !strings.Contains(err.Error(), "PUBLIC_MODEL_ID must not be empty") {
		t.Fatalf("want empty-model error, got %v", err)
	}
}

func TestApplyOverridePublicModelIDRejectsUnsafeOrUnboundedValue(t *testing.T) {
	for _, id := range []string{"has space", "模型", strings.Repeat("a", MaxPublicModelIDBytes+1)} {
		_, err := ApplyOverrides(validBase(), map[string]string{"PUBLIC_MODEL_ID": id})
		if err == nil || !strings.Contains(err.Error(), "PUBLIC_MODEL_ID") {
			t.Fatalf("id=%q: expected named validation error, got %v", id, err)
		}
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
	for _, key := range []string{"DASHSCOPE_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD"} {
		_, err := ApplyOverrides(validBase(), map[string]string{key: "x"})
		if err == nil || !strings.Contains(err.Error(), "unknown or secret") || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: want named secret/unknown rejection, got %v", key, err)
		}
	}
}

func TestApplyOverrideRejectsUnknownByName(t *testing.T) {
	for _, key := range []string{"NOPE_NOT_A_KEY", "DASHSCOPE_API_KEY"} {
		_, err := ApplyOverrides(validBase(), map[string]string{key: "1"})
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "unknown or secret") {
			t.Fatalf("%s: want named unknown rejection, got %v", key, err)
		}
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
	// Individually valid, but together they make the fixed model quote larger
	// than the operator monthly budget.
	_, err := ApplyOverrides(validBase(), map[string]string{
		"GLOBAL_MONTHLY_SPEND_MICRO_USD": "100000",
		"MAX_TOKENS_CAP":                 "1000000",
	})
	if err == nil || !strings.Contains(err.Error(), "GLOBAL_MONTHLY_SPEND_MICRO_USD") {
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
	if got.MonthlyQuota != 0 || got.PublicModelID != "" {
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
		case "DASHSCOPE_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD":
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
	// PUBLIC_MODEL_ID is a string — not numerically bounded.
	if byKey["PUBLIC_MODEL_ID"].Bounded {
		t.Fatalf("PUBLIC_MODEL_ID should not be bounded")
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

// --- GATEWAY_MODE rationing mask (debug vs production) -----------------------

// hardened returns validBase with every rationing knob set to a DISTINCT non-zero
// production value, so a mask test can tell "was masked" from "was already zero"
// — the failure mode a fixture of zeros would hide completely.
//
// hardened = validBase + 每个配额旋钮都设成**互不相同的非零**生产值,于是掩码测试能分辨
// 「被掩掉了」与「本来就是 0」——一个全零 fixture 会把这个失败模式完全盖住。
func hardened() Config {
	c := validBase()
	c.MonthlyQuota = 5000
	c.GlobalMonthlySpendPUSD = 420 * billing.PicoUSDPerUSD
	c.RatePerMin = 8
	c.DailySublimit = 100
	c.TokenAnomalyRPM = 9
	c.ImageDailyLimit = 10
	c.SpeechDailyLimit = 50_000
	c.VideoDailyLimit = 10
	c.InstallPerIPHour = 5
	c.InstallGlobalDailyCap = 100
	c.InstallPerFPDaily = 3
	c.InstallPerFPCooldownSec = 3600
	c.InstallPowMode = PowModeEnforce
	c.InstallPowSecret = []byte("pow-secret")
	return c
}

func TestDebugModeOnlyMatchesTheExactLiteral(t *testing.T) {
	// The zero value and every near-miss must ENFORCE; only "debug" opens the gates.
	for _, mode := range []string{"", "production", "Debug", "DEBUG", "dbg", "debug "} {
		c := validBase()
		c.RuntimeMode = mode
		if c.DebugMode() {
			t.Fatalf("RuntimeMode %q must not enable the mask", mode)
		}
	}
	c := validBase()
	c.RuntimeMode = RuntimeModeDebug
	if !c.DebugMode() {
		t.Fatal("RuntimeMode debug must enable the mask")
	}
}

func TestEffectiveLimitsProductionIsIdentity(t *testing.T) {
	for _, mode := range []string{"", RuntimeModeProduction} {
		c := hardened()
		c.RuntimeMode = mode
		if got := c.EffectiveLimits(); got.RuntimeMode != mode ||
			got.MonthlyQuota != c.MonthlyQuota ||
			got.GlobalMonthlySpendPUSD != c.GlobalMonthlySpendPUSD ||
			got.RatePerMin != c.RatePerMin ||
			got.DailySublimit != c.DailySublimit ||
			got.ImageDailyLimit != c.ImageDailyLimit ||
			got.InstallPowMode != c.InstallPowMode {
			t.Fatalf("mode %q must be identity, got %+v", mode, got)
		}
	}
}

func TestEffectiveLimitsDebugOpensEveryRationingGate(t *testing.T) {
	c := hardened()
	c.RuntimeMode = RuntimeModeDebug
	m := c.EffectiveLimits()

	// Money gates raised to the registry ceilings (never denied).
	if m.MonthlyQuota != MaxMonthlyQuota {
		t.Fatalf("MonthlyQuota = %d, want ceiling %d", m.MonthlyQuota, MaxMonthlyQuota)
	}
	if want := MaxMonthlySpendMicroUSD * billing.PicoUSDPerMicroUSD; m.GlobalMonthlySpendPUSD != want {
		t.Fatalf("GlobalMonthlySpendPUSD = %d, want ceiling %d", m.GlobalMonthlySpendPUSD, want)
	}
	// Every 0-disables gate actually zeroed.
	zeroed := map[string]int64{
		"RatePerMin":              int64(m.RatePerMin),
		"DailySublimit":           m.DailySublimit,
		"TokenAnomalyRPM":         int64(m.TokenAnomalyRPM),
		"ImageDailyLimit":         m.ImageDailyLimit,
		"SpeechDailyLimit":        m.SpeechDailyLimit,
		"VideoDailyLimit":         m.VideoDailyLimit,
		"InstallPerIPHour":        int64(m.InstallPerIPHour),
		"InstallGlobalDailyCap":   m.InstallGlobalDailyCap,
		"InstallPerFPDaily":       m.InstallPerFPDaily,
		"InstallPerFPCooldownSec": int64(m.InstallPerFPCooldownSec),
	}
	for name, v := range zeroed {
		if v != 0 {
			t.Fatalf("%s = %d under debug, want 0 (disabled)", name, v)
		}
	}
	if m.InstallPowMode != PowModeOff {
		t.Fatalf("InstallPowMode = %q under debug, want off", m.InstallPowMode)
	}
	// Non-destructive: the receiver still carries the operator's real values, so
	// flipping back to production restores them with nothing to re-enter.
	if c.RatePerMin != 8 || c.MonthlyQuota != 5000 || c.InstallPowMode != PowModeEnforce {
		t.Fatalf("EffectiveLimits mutated the configured values: %+v", c)
	}
}

func TestEffectiveLimitsLeavesProcessProtectionAlone(t *testing.T) {
	// Debug must never be a route to OOM the box: shape/memory/concurrency caps
	// protect the PROCESS, they do not ration a user, so the mask must not touch them.
	c := hardened()
	c.RuntimeMode = RuntimeModeDebug
	m := c.EffectiveLimits()
	if m.MaxTokensCap != c.MaxTokensCap || m.MaxMessages != c.MaxMessages ||
		m.MaxMessageChars != c.MaxMessageChars || m.MaxMediaParts != c.MaxMediaParts ||
		m.MaxMediaDecodedBytes != c.MaxMediaDecodedBytes || m.MaxBodyBytes != c.MaxBodyBytes ||
		m.NGlobalConcurrency != c.NGlobalConcurrency || m.QueueWait != c.QueueWait ||
		m.DiskMinMB != c.DiskMinMB || m.GoMemLimitMiB != c.GoMemLimitMiB ||
		m.UpstreamHeaderTimeout != c.UpstreamHeaderTimeout {
		t.Fatalf("debug mask must not touch process-protection knobs: %+v", m)
	}
}

func TestEffectiveLimitsMaskStaysWithinRegistryBoundsAndSemantics(t *testing.T) {
	// The masked config must itself be a legal config: it is what every
	// enforcement site reads, and an out-of-bounds mask would be a guardrail
	// bypass wearing a config's clothes.
	c := hardened()
	c.RuntimeMode = RuntimeModeDebug
	m := c.EffectiveLimits()
	if err := m.ValidateSemantics(); err != nil {
		t.Fatalf("masked config must pass ValidateSemantics: %v", err)
	}
	for _, s := range Specs() {
		if !s.Bounded || s.Tier != TierRuntimeHot {
			continue
		}
		raw := s.get(&m)
		probe := m.Clone()
		if err := s.apply(probe, raw); err != nil {
			t.Fatalf("masked %s = %s is out of its own registry bounds: %v", s.Key, raw, err)
		}
	}
}

func TestValidateRuntimeModeClosedEnum(t *testing.T) {
	for _, ok := range []string{RuntimeModeDebug, RuntimeModeProduction} {
		if err := ValidateRuntimeMode(ok); err != nil {
			t.Fatalf("ValidateRuntimeMode(%q) = %v", ok, err)
		}
	}
	// Both near-miss directions are rejected: a "prod" that silently meant debug
	// would leave a public gateway wide open.
	for _, bad := range []string{"", "prod", "PRODUCTION", "dev", "off", "true"} {
		if err := ValidateRuntimeMode(bad); err == nil {
			t.Fatalf("ValidateRuntimeMode(%q) must reject", bad)
		}
	}
}

func TestApplyOverrideGatewayModeFlipsAndRejectsTypo(t *testing.T) {
	base := hardened()
	base.RuntimeMode = RuntimeModeDebug

	got, err := ApplyOverrides(base, map[string]string{"GATEWAY_MODE": "production"})
	if err != nil {
		t.Fatalf("flip to production: %v", err)
	}
	if got.RuntimeMode != RuntimeModeProduction {
		t.Fatalf("RuntimeMode = %q, want production", got.RuntimeMode)
	}
	// The flip alone must arm the configured values — nothing else was overridden.
	if got.RatePerMin != 8 || got.MonthlyQuota != 5000 {
		t.Fatalf("flip must preserve configured knobs, got rate=%d quota=%d", got.RatePerMin, got.MonthlyQuota)
	}

	if _, err := ApplyOverrides(base, map[string]string{"GATEWAY_MODE": "prod"}); err == nil {
		t.Fatal("typo GATEWAY_MODE must be rejected by name")
	}
}

func TestValidateSemanticsRejectsNonEmptyBadRuntimeMode(t *testing.T) {
	c := validBase()
	c.RuntimeMode = "prod"
	if err := c.ValidateSemantics(); err == nil {
		t.Fatal("non-empty invalid GATEWAY_MODE must fail semantics")
	}
	// The empty zero value stays legal (it is the enforcing default).
	c.RuntimeMode = ""
	if err := c.ValidateSemantics(); err != nil {
		t.Fatalf("empty RuntimeMode must stay valid: %v", err)
	}
}
