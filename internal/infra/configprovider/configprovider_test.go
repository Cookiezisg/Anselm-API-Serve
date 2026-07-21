package configprovider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// envMap builds a getenv closure over a map — hermetic, no process env.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// minimalEnv supplies the sole required secret plus explicit spend-wallet values
// that satisfy the compiled model quotes. PUBLIC_MODEL_ID is included so its
// default/override behavior remains visible in fixtures even though it is optional.
func minimalEnv() map[string]string {
	return map[string]string{
		"DEEPSEEK_API_KEY":              "sk-a,sk-b",
		"PUBLIC_MODEL_ID":               "anselm-auto",
		"GLOBAL_DAILY_SPEND_MICRO_USD":  "14000000",
		"INSTALL_DAILY_SPEND_MICRO_USD": "5600000",
	}
}

// fakeStore satisfies both settingsLoader and settingsStore for the provider tests.
type fakeStore struct {
	mu        sync.Mutex
	data      map[string]string
	loadErr   error
	persErr   error
	persisted []map[string]string // each PersistAll batch, in call order
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string]string{}} }

func (f *fakeStore) LoadAll(ctx context.Context) (map[string]string, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) PersistAll(ctx context.Context, overlay map[string]string) error {
	if f.persErr != nil {
		return f.persErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	batch := make(map[string]string, len(overlay))
	for k, v := range overlay {
		f.data[k] = v
		batch[k] = v
	}
	f.persisted = append(f.persisted, batch)
	return nil
}

func mustLoad(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	c, err := LoadBase(envMap(env))
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	return c
}

func TestLoadBaseDefaults(t *testing.T) {
	c := mustLoad(t, minimalEnv())

	if len(c.DeepSeekAPIKeys) != 2 || c.DeepSeekAPIKeys[0] != "sk-a" {
		t.Fatalf("DeepSeekAPIKeys = %v", c.DeepSeekAPIKeys)
	}
	if c.PublicModelID != "anselm-auto" {
		t.Fatalf("logical model = %q, want anselm-auto", c.PublicModelID)
	}
	if c.DeepSeekBaseURL != "https://api.deepseek.com" {
		t.Fatalf("DeepSeekBaseURL = %q", c.DeepSeekBaseURL)
	}
	// Spec defaults.
	if c.MonthlyQuota != 5000 || c.MaxTokensCap != 4096 || c.InputTokenCap != 16384 {
		t.Fatalf("numeric defaults wrong: quota=%d maxtok=%d input=%d", c.MonthlyQuota, c.MaxTokensCap, c.InputTokenCap)
	}
	if c.MaxMessages != 256 || c.MaxMessageChars != 131072 || c.NGlobalConcurrency != 8 {
		t.Fatalf("shape defaults wrong")
	}
	if c.InstallPowMode != config.PowModeOff || c.InstallPowSecretSource != "disabled" {
		t.Fatalf("pow defaults: mode=%q src=%q", c.InstallPowMode, c.InstallPowSecretSource)
	}
	if c.ResetTZ != "Asia/Shanghai" || c.Location == nil {
		t.Fatalf("tz default not resolved")
	}
	if c.ListenAddr != "127.0.0.1:8080" || c.AdminAddr != "127.0.0.1:9090" || c.DashboardAddr != "127.0.0.1:8081" {
		t.Fatalf("listener defaults wrong")
	}
	if c.UpstreamHeaderTimeout.Seconds() != 60 || c.QueueWait.Milliseconds() != 1500 {
		t.Fatalf("duration defaults wrong")
	}
}

func TestLoadBaseMaxBodyBytesDefault(t *testing.T) {
	// Unset MAX_BODY_BYTES keeps the historical 256KiB body contract.
	c := mustLoad(t, minimalEnv())
	if c.MaxBodyBytes != 262144 {
		t.Fatalf("MaxBodyBytes default = %d, want 262144", c.MaxBodyBytes)
	}
}

func TestLoadBaseMaxBodyBytesExplicit(t *testing.T) {
	env := minimalEnv()
	env["MAX_BODY_BYTES"] = "4194304"
	c := mustLoad(t, env)
	if c.MaxBodyBytes != 4194304 {
		t.Fatalf("MaxBodyBytes = %d, want 4194304", c.MaxBodyBytes)
	}
}

func TestLoadBaseInputTokenCapZeroDisables(t *testing.T) {
	// INPUT_TOKEN_CAP=0 disables the input estimate gate — must load without error
	// The text model's own context limit becomes the request-time guard; land 0.
	env := minimalEnv()
	env["INPUT_TOKEN_CAP"] = "0"
	c := mustLoad(t, env)
	if c.InputTokenCap != 0 {
		t.Fatalf("InputTokenCap = %d, want 0", c.InputTokenCap)
	}
}

func TestLoadBaseTrimsBaseURLAndKeys(t *testing.T) {
	env := minimalEnv()
	env["DEEPSEEK_BASE_URL"] = "https://x.example/"
	env["DEEPSEEK_API_KEY"] = " sk-1 , , sk-2 "
	c := mustLoad(t, env)
	if c.DeepSeekBaseURL != "https://x.example" {
		t.Fatalf("base url not trimmed: %q", c.DeepSeekBaseURL)
	}
	if len(c.DeepSeekAPIKeys) != 2 || c.DeepSeekAPIKeys[1] != "sk-2" {
		t.Fatalf("keys = %v", c.DeepSeekAPIKeys)
	}
}

func TestLoadBaseKimiKeyIsOptionalAndTrimmed(t *testing.T) {
	env := minimalEnv()
	c := mustLoad(t, env)
	if len(c.KimiAPIKeys) != 0 {
		t.Fatalf("unset Kimi key must keep multimodal disabled, got %d keys", len(c.KimiAPIKeys))
	}
	env["KIMI_API_KEY"] = " gem-a, ,gem-b "
	c = mustLoad(t, env)
	if len(c.KimiAPIKeys) != 2 || c.KimiAPIKeys[1] != "gem-b" {
		t.Fatalf("Kimi keys=%v", c.KimiAPIKeys)
	}
}

func TestLoadBaseFailFast(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr error // when non-nil, asserts errors.Is
		wantSub string
	}{
		{"missing key", func(m map[string]string) { delete(m, "DEEPSEEK_API_KEY") }, ErrDeepSeekKeyRequired, ""},
		{"blank key", func(m map[string]string) { m["DEEPSEEK_API_KEY"] = "  , " }, ErrDeepSeekKeyRequired, ""},
		{"base URL userinfo", func(m map[string]string) { m["KIMI_BASE_URL"] = "https://secret@example.com/v1beta/openai" }, nil, "absolute HTTPS base URL"},
		{"remote plaintext base URL", func(m map[string]string) { m["DEEPSEEK_BASE_URL"] = "http://api.deepseek.com" }, nil, "HTTP is allowed only for a literal loopback IP"},
		{"unsafe public model id", func(m map[string]string) { m["PUBLIC_MODEL_ID"] = "bad model" }, nil, "PUBLIC_MODEL_ID"},
		{"unknown priced model", func(m map[string]string) { m["TEXT_UPSTREAM_MODEL"] = "deepseek-latest" }, nil, "no exact rate card"},
		{"budget zero", func(m map[string]string) { m["GLOBAL_DAILY_SPEND_MICRO_USD"] = "0" }, nil, "GLOBAL_DAILY_SPEND_MICRO_USD must be >= 1"},
		{"cap zero", func(m map[string]string) { m["INSTALL_DAILY_SPEND_MICRO_USD"] = "0" }, nil, "INSTALL_DAILY_SPEND_MICRO_USD must be >= 1"},
		{"bad int", func(m map[string]string) { m["MONTHLY_QUOTA"] = "abc" }, nil, "invalid int"},
		{"over ceiling", func(m map[string]string) { m["MAX_TOKENS_CAP"] = "9999999" }, nil, "MAX_TOKENS_CAP must be <="},
		{"body cap under floor", func(m map[string]string) { m["MAX_BODY_BYTES"] = "4095" }, nil, "MAX_BODY_BYTES must be >= 4096"},
		{"body cap over ceiling", func(m map[string]string) { m["MAX_BODY_BYTES"] = "8388609" }, nil, "MAX_BODY_BYTES must be <= 8388608"},
		{"pow mode typo", func(m map[string]string) { m["INSTALL_POW_MODE"] = "enabled" }, nil, "INSTALL_POW_MODE"},
		{"pow secret required", func(m map[string]string) { m["INSTALL_POW_MODE"] = "enforce" }, nil, "CONFIG_POW_SECRET_REQUIRED"},
		{"cap exceeds budget", func(m map[string]string) {
			m["GLOBAL_DAILY_SPEND_MICRO_USD"] = "700000"
			m["INSTALL_DAILY_SPEND_MICRO_USD"] = "800000"
		}, nil, "must be <= GLOBAL_DAILY_SPEND_MICRO_USD"},
		{"per-req exceeds cap", func(m map[string]string) {
			m["KIMI_API_KEY"] = "gem-key"
			m["INSTALL_DAILY_SPEND_MICRO_USD"] = "100000"
		}, nil, "multimodal hard-limit quote"},
		{"dashboard half auth", func(m map[string]string) { m["DASHBOARD_USER"] = "admin" }, nil, "must be set together"},
		{"listeners collide", func(m map[string]string) { m["DASHBOARD_ADDR"] = "127.0.0.1:8080" }, nil, "must not equal LISTEN_ADDR"},
		{"mem budget exceeded", func(m map[string]string) {
			m["GOMEMLIMIT_MIB"] = "4096"
			m["MEM_BUDGET_MIB"] = "1000"
		}, nil, "PERF-2 memory budget exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := minimalEnv()
			tc.mutate(env)
			_, err := LoadBase(envMap(env))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want Is %v", err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadBaseTextOnlyBudgetDoesNotReserveInactiveKimi(t *testing.T) {
	env := minimalEnv()
	env["INSTALL_DAILY_SPEND_MICRO_USD"] = "100000"
	env["MULTIMODAL_UPSTREAM_MODEL"] = "inactive-unknown-model"
	env["KIMI_DAILY_SPEND_MICRO_USD"] = "15000000"
	if _, err := LoadBase(envMap(env)); err != nil {
		t.Fatalf("Kimi-disabled deployment should validate only active text cost: %v", err)
	}

	env["KIMI_API_KEY"] = "gem-key"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "KIMI_DAILY_SPEND_MICRO_USD") {
		t.Fatalf("enabling Kimi must activate its wallet/model checks, got %v", err)
	}
}

func TestLoadBaseAllowsExplicitLoopbackHTTP(t *testing.T) {
	t.Parallel()
	for _, baseURL := range []string{
		"http://127.0.0.1:18080",
		"http://[::1]:18080",
	} {
		t.Run(baseURL, func(t *testing.T) {
			env := minimalEnv()
			env["DEEPSEEK_BASE_URL"] = baseURL
			if got := mustLoad(t, env).DeepSeekBaseURL; got != baseURL {
				t.Fatalf("DeepSeekBaseURL = %q, want %q", got, baseURL)
			}
		})
	}
}

func TestLoadBaseRejectsHostnameHTTPEvenForLocalhost(t *testing.T) {
	t.Parallel()
	env := minimalEnv()
	env["DEEPSEEK_BASE_URL"] = "http://localhost:18080"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "literal loopback IP") {
		t.Fatalf("localhost HTTP must not be trusted for credential transport: %v", err)
	}
}

func TestLoadBaseInvalidTzPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic on unresolvable RESET_TZ, got none")
		}
	}()
	env := minimalEnv()
	env["RESET_TZ"] = "Mars/Phobos"
	_, _ = LoadBase(envMap(env))
}

func TestLoadBasePowSecretConfigured(t *testing.T) {
	env := minimalEnv()
	env["INSTALL_POW_MODE"] = "shadow"
	env["INSTALL_POW_SECRET"] = "topsecret"
	c := mustLoad(t, env)
	if c.InstallPowSecretSource != "configured" || string(c.InstallPowSecret) != "topsecret" {
		t.Fatalf("pow secret not loaded: src=%q", c.InstallPowSecretSource)
	}
}

func TestLoadLockFreeReturnsLatestAfterSwap(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	before := p.Load()
	if before.RatePerMin != 0 {
		t.Fatalf("seed RatePerMin = %d", before.RatePerMin)
	}
	if _, err := p.ApplyOverrides(context.Background(), map[string]string{"RATE_PER_MIN": "99"}, ss); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}
	after := p.Load()
	if after.RatePerMin != 99 {
		t.Fatalf("after swap RatePerMin = %d, want 99", after.RatePerMin)
	}
	// The previously-held snapshot is immutable — never mutated by the swap.
	if before.RatePerMin != 0 {
		t.Fatalf("old snapshot mutated: %d", before.RatePerMin)
	}
}

func TestApplyOverridesPersistsThenSwaps(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	_, err := p.ApplyOverrides(context.Background(), map[string]string{
		"RATE_PER_MIN":  "33",
		"MONTHLY_QUOTA": "7777",
	}, ss)
	if err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}
	if len(ss.persisted) != 1 {
		t.Fatalf("persist batches = %d, want 1", len(ss.persisted))
	}
	if ss.data["RATE_PER_MIN"] != "33" || ss.data["MONTHLY_QUOTA"] != "7777" {
		t.Fatalf("persisted overlay = %v", ss.data)
	}
	c := p.Load()
	if c.RatePerMin != 33 || c.MonthlyQuota != 7777 {
		t.Fatalf("live config not swapped: rate=%d quota=%d", c.RatePerMin, c.MonthlyQuota)
	}
}

func TestApplyOverridesNoSwapOnValidateFailure(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	// Over-ceiling value → domain rejects before any persist.
	_, err := p.ApplyOverrides(context.Background(), map[string]string{"RATE_PER_MIN": "999999999"}, ss)
	if err == nil {
		t.Fatal("want validate error, got nil")
	}
	if len(ss.persisted) != 0 {
		t.Fatalf("persisted despite validate failure: %v", ss.persisted)
	}
	if p.Load().RatePerMin != 0 {
		t.Fatalf("live config swapped despite validate failure")
	}
}

func TestApplyOverridesRejectsSecretAndRestartKeys(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	for _, k := range []string{"DEEPSEEK_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_PASSWORD", "GOMEMLIMIT_MIB", "LISTEN_ADDR"} {
		if _, err := p.ApplyOverrides(context.Background(), map[string]string{k: "x"}, ss); err == nil {
			t.Fatalf("override of %q: want rejection, got nil", k)
		}
	}
	if len(ss.persisted) != 0 {
		t.Fatalf("a rejected key was persisted: %v", ss.persisted)
	}
}

func TestApplyOverridesNoSwapOnPersistFailure(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	ss.persErr = errors.New("disk full")
	_, err := p.ApplyOverrides(context.Background(), map[string]string{"RATE_PER_MIN": "55"}, ss)
	if err == nil || !strings.Contains(err.Error(), "persist overrides") {
		t.Fatalf("want persist error, got %v", err)
	}
	if p.Load().RatePerMin != 0 {
		t.Fatalf("live config swapped despite persist failure: %d", p.Load().RatePerMin)
	}
}

func TestApplyOverridesEmptyNoOp(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	c, err := p.ApplyOverrides(context.Background(), nil, ss)
	if err != nil {
		t.Fatalf("empty ApplyOverrides: %v", err)
	}
	if c != p.Load() {
		t.Fatal("empty ApplyOverrides should return the current snapshot pointer")
	}
	if len(ss.persisted) != 0 {
		t.Fatal("empty batch should open no persist")
	}
}

func TestLoadWithOverlayOrderEnvThenDB(t *testing.T) {
	base := mustLoad(t, minimalEnv()) // RatePerMin default 0
	ss := newFakeStore()
	ss.data["RATE_PER_MIN"] = "77"      // DB overlay overrides env default
	ss.data["MONTHLY_QUOTA"] = "123456" // ditto
	merged, err := LoadWithOverlay(context.Background(), base, ss)
	if err != nil {
		t.Fatalf("LoadWithOverlay: %v", err)
	}
	if merged.RatePerMin != 77 || merged.MonthlyQuota != 123456 {
		t.Fatalf("overlay not applied: rate=%d quota=%d", merged.RatePerMin, merged.MonthlyQuota)
	}
	// Untouched key keeps the env default.
	if merged.MaxTokensCap != 4096 {
		t.Fatalf("untouched key changed: %d", merged.MaxTokensCap)
	}
}

func TestLoadWithOverlayEmptyReturnsBase(t *testing.T) {
	base := mustLoad(t, minimalEnv())
	ss := newFakeStore()
	merged, err := LoadWithOverlay(context.Background(), base, ss)
	if err != nil {
		t.Fatalf("LoadWithOverlay: %v", err)
	}
	if merged.RatePerMin != base.RatePerMin {
		t.Fatal("empty overlay should return base unchanged")
	}
}

func TestLoadWithOverlayInvalidFailsFast(t *testing.T) {
	base := mustLoad(t, minimalEnv())
	ss := newFakeStore()
	ss.data["RATE_PER_MIN"] = "not-a-number"
	if _, err := LoadWithOverlay(context.Background(), base, ss); err == nil {
		t.Fatal("want fail-fast on corrupt overlay, got nil")
	}
}

func TestLoadWithOverlayLoadErrorPropagates(t *testing.T) {
	base := mustLoad(t, minimalEnv())
	ss := newFakeStore()
	ss.loadErr = errors.New("db down")
	if _, err := LoadWithOverlay(context.Background(), base, ss); err == nil {
		t.Fatal("want overlay read error, got nil")
	}
}

func TestDumpMasksSecretsAndCarriesBounds(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	items := p.Dump()

	byKey := map[string]DumpItem{}
	for _, it := range items {
		byKey[it.Key] = it
		// No secret key may ever surface in Dump.
		switch it.Key {
		case "DEEPSEEK_API_KEY", "KIMI_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD":
			t.Fatalf("secret %q leaked into Dump", it.Key)
		}
	}

	// A bounded runtime knob carries Min/Max.
	rate, ok := byKey["RATE_PER_MIN"]
	if !ok {
		t.Fatal("RATE_PER_MIN missing from Dump")
	}
	if !rate.Editable || rate.Min == nil || rate.Max == nil || *rate.Max != int64(config.MaxRatePerMin) {
		t.Fatalf("RATE_PER_MIN bounds wrong: %+v", rate)
	}
	// A startup-hard item is read-only + restart-required + carries no bounds.
	mem, ok := byKey["GOMEMLIMIT_MIB"]
	if !ok {
		t.Fatal("GOMEMLIMIT_MIB missing from Dump")
	}
	if mem.Editable || !mem.RestartRequired || mem.Min != nil || mem.Max != nil {
		t.Fatalf("GOMEMLIMIT_MIB surface wrong: %+v", mem)
	}
	// N_GLOBAL_CONCURRENCY is editable yet restart-required.
	ng := byKey["N_GLOBAL_CONCURRENCY"]
	if !ng.Editable || !ng.RestartRequired {
		t.Fatalf("N_GLOBAL_CONCURRENCY surface wrong: %+v", ng)
	}
}

func TestSnapshotMasksSecrets(t *testing.T) {
	env := minimalEnv()
	env["INSTALL_POW_MODE"] = "shadow"
	env["INSTALL_POW_SECRET"] = "supersecretvalue"
	env["DASHBOARD_USER"] = "admin"
	env["DASHBOARD_PASSWORD"] = "hunter2pw"
	env["KIMI_API_KEY"] = "kimi-supersecret"
	p := New(mustLoad(t, env))

	attrs := p.Snapshot()
	if len(attrs)%2 != 0 {
		t.Fatalf("Snapshot must be key/value pairs, got %d items", len(attrs))
	}
	// Concatenate only the VALUES (key names like "admin_addr" legitimately
	// contain words; the leak surface is the values printed for secrets).
	vals := ""
	kv := map[string]any{}
	for i := 0; i+1 < len(attrs); i += 2 {
		k, _ := attrs[i].(string)
		kv[k] = attrs[i+1]
		vals += valStr(attrs[i+1]) + ";"
	}
	for _, leak := range []string{"sk-a", "sk-b", "kimi-supersecret", "supersecretvalue", "hunter2pw"} {
		if strings.Contains(vals, leak) {
			t.Fatalf("secret %q leaked into Snapshot values: %s", leak, vals)
		}
	}
	if kv["deepseek_keys"] != "sk-*** (2 configured)" {
		t.Fatalf("deepseek_keys mask = %v", kv["deepseek_keys"])
	}
	if kv["kimi_keys"] != "*** (1 configured)" {
		t.Fatalf("kimi_keys mask = %v", kv["kimi_keys"])
	}
	if kv["install_pow_secret"] != "configured" {
		t.Fatalf("pow secret mask = %v", kv["install_pow_secret"])
	}
	if kv["dashboard_auth"] != "configured (user+password set)" {
		t.Fatalf("dashboard auth mask = %v", kv["dashboard_auth"])
	}
}

func valStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func TestConcurrentLoadUnderRace(t *testing.T) {
	p := New(mustLoad(t, minimalEnv()))
	ss := newFakeStore()
	var wg sync.WaitGroup

	// Many concurrent lock-free readers while writers swap.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c := p.Load()
				_ = c.RatePerMin
				_ = c.MonthlyQuota
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = p.ApplyOverrides(context.Background(),
					map[string]string{"RATE_PER_MIN": "50"}, ss)
			}
		}(i)
	}
	wg.Wait()
	if p.Load().RatePerMin != 50 {
		t.Fatalf("final RatePerMin = %d, want 50", p.Load().RatePerMin)
	}
}
