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

// minimalEnv supplies the sole required secret plus an explicit spend-wallet value
// that satisfy the compiled model quotes. PUBLIC_MODEL_ID is included so its
// default/override behavior remains visible in fixtures even though it is optional.
func minimalEnv() map[string]string {
	return map[string]string{
		"DEEPSEEK_API_KEY":               "sk-a,sk-b",
		"PUBLIC_MODEL_ID":                "anselm-auto",
		"GLOBAL_MONTHLY_SPEND_MICRO_USD": "420000000",
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
	if c.MonthlyQuota != 5000 || c.MaxTokensCap != 4096 || c.InputTokenCap != 0 {
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

func TestLoadBaseDurableMediaIsExplicitAndComplete(t *testing.T) {
	c := mustLoad(t, minimalEnv())
	if c.MediaEnabled || c.MediaSigningSecretSource != "disabled" || c.MediaStagingRoot == "" {
		t.Fatalf("media defaults = enabled:%v source:%q root:%q", c.MediaEnabled, c.MediaSigningSecretSource, c.MediaStagingRoot)
	}
	env := minimalEnv()
	env["MEDIA_ENABLED"] = "true"
	env["MEDIA_SIGNING_SECRET"] = "01234567890123456789012345678901"
	env["MAX_BODY_BYTES"] = "4194304"
	env["MEDIA_CHUNK_MAX_BYTES"] = "4194304"
	// Enabling media without declaring this gateway's own public origin must fail closed: a chat
	// request may only name a lease RELATIVELY (ADR 0011), so without this value the gateway has no
	// safe way to absolutize the reference for an upstream fetch.
	// 开了媒体却不声明网关自身的公开 origin 必须失败关闭:chat 只能**相对**指称 lease(ADR 0011),
	// 少了这个值,网关没有安全的方式把引用绝对化给上游拉取。
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "MEDIA_PUBLIC_BASE_URL") {
		t.Fatalf("MEDIA_ENABLED without MEDIA_PUBLIC_BASE_URL must fail, got %v", err)
	}
	env["MEDIA_PUBLIC_BASE_URL"] = "https://gw.example"
	c = mustLoad(t, env)
	if c.MediaPublicBaseURL != "https://gw.example" {
		t.Fatalf("MediaPublicBaseURL = %q", c.MediaPublicBaseURL)
	}
	// A non-bare or non-https origin is refused: a path/query would let a crafted relative reference
	// resolve somewhere unintended, and plain http would hand a capability-bearing URL to the
	// provider in clear text. 非裸/非 https 的 origin 一律拒。
	for _, bad := range []string{"http://gw.example", "https://gw.example/v1", "https://gw.example?a=1", "https://u@gw.example", "gw.example", "https://"} {
		env["MEDIA_PUBLIC_BASE_URL"] = bad
		if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "MEDIA_PUBLIC_BASE_URL") {
			t.Errorf("MEDIA_PUBLIC_BASE_URL=%q must be rejected, got %v", bad, err)
		}
	}
	env["MEDIA_PUBLIC_BASE_URL"] = "https://gw.example"
	if !c.MediaEnabled || c.MediaSigningSecretSource != "configured" || c.MediaChunkMaxBytes != 4194304 {
		t.Fatalf("media enabled config not assembled: %+v", c)
	}
	env["MEDIA_SIGNING_SECRET"] = "too-short"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "CONFIG_MEDIA_SIGNING_SECRET_REQUIRED") {
		t.Fatalf("weak media secret must fail, got %v", err)
	}
	env["MEDIA_SIGNING_SECRET"] = "01234567890123456789012345678901"
	env["MEDIA_CHUNK_MAX_BYTES"] = "4194305"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "MEDIA_CHUNK_MAX_BYTES must be <= 4194304") {
		t.Fatalf("chunk over body cap must fail, got %v", err)
	}
}

func TestLoadBaseInputTokenCapZeroCompatibilityValue(t *testing.T) {
	// The deprecated key still parses so old env/settings remain bootable, but
	// its value no longer affects request admission.
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

func TestLoadBaseQwenKeyIsOptionalAndWorkspaceDerived(t *testing.T) {
	env := minimalEnv()
	c := mustLoad(t, env)
	if len(c.QwenAPIKeys) != 0 {
		t.Fatalf("unset Qwen key must keep multimodal disabled, got %d keys", len(c.QwenAPIKeys))
	}
	env["DASHSCOPE_API_KEY"] = " qwen-a, ,qwen-b "
	env["DASHSCOPE_WORKSPACE_ID"] = "ws_test-1"
	c = mustLoad(t, env)
	if len(c.QwenAPIKeys) != 2 || c.QwenAPIKeys[1] != "qwen-b" {
		t.Fatalf("Qwen keys=%v", c.QwenAPIKeys)
	}
	if c.QwenBaseURL != "https://ws_test-1.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("workspace base URL=%q", c.QwenBaseURL)
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
		{"DashScope base URL userinfo", func(m map[string]string) { m["DASHSCOPE_BASE_URL"] = "https://secret@example.com/compatible-mode/v1" }, nil, "absolute HTTPS base URL"},
		{"Qwen key needs endpoint", func(m map[string]string) { m["DASHSCOPE_API_KEY"] = "qwen-key" }, nil, "DASHSCOPE_API_KEY requires"},
		{"invalid workspace id", func(m map[string]string) { m["DASHSCOPE_WORKSPACE_ID"] = "bad.example" }, nil, "DASHSCOPE_WORKSPACE_ID"},
		{"remote plaintext base URL", func(m map[string]string) { m["DEEPSEEK_BASE_URL"] = "http://api.deepseek.com" }, nil, "HTTP is allowed only for a literal loopback IP"},
		{"unsafe public model id", func(m map[string]string) { m["PUBLIC_MODEL_ID"] = "bad model" }, nil, "PUBLIC_MODEL_ID"},
		{"unknown priced model", func(m map[string]string) { m["TEXT_UPSTREAM_MODEL"] = "deepseek-latest" }, nil, "no exact rate card"},
		{"budget zero", func(m map[string]string) { m["GLOBAL_MONTHLY_SPEND_MICRO_USD"] = "0" }, nil, "GLOBAL_MONTHLY_SPEND_MICRO_USD must be >= 1"},
		{"bad int", func(m map[string]string) { m["MONTHLY_QUOTA"] = "abc" }, nil, "invalid int"},
		{"over ceiling", func(m map[string]string) { m["MAX_TOKENS_CAP"] = "9999999" }, nil, "MAX_TOKENS_CAP must be <="},
		{"body cap under floor", func(m map[string]string) { m["MAX_BODY_BYTES"] = "4095" }, nil, "MAX_BODY_BYTES must be >= 4096"},
		{"body cap over ceiling", func(m map[string]string) { m["MAX_BODY_BYTES"] = "8388609" }, nil, "MAX_BODY_BYTES must be <= 8388608"},
		{"pow mode typo", func(m map[string]string) { m["INSTALL_POW_MODE"] = "enabled" }, nil, "INSTALL_POW_MODE"},
		{"pow secret required", func(m map[string]string) { m["INSTALL_POW_MODE"] = "enforce" }, nil, "CONFIG_POW_SECRET_REQUIRED"},
		{"per-req exceeds monthly budget", func(m map[string]string) {
			m["DASHSCOPE_API_KEY"] = "qwen-key"
			m["DASHSCOPE_WORKSPACE_ID"] = "ws_test"
			m["GLOBAL_MONTHLY_SPEND_MICRO_USD"] = "200000"
		}, nil, "multimodal hard-limit quote"},
		{"dashboard builtin missing password", func(m map[string]string) {
			m["DASHBOARD_AUTH_MODE"] = "builtin"
			m["DASHBOARD_USER"] = "admin"
		}, nil, "DASHBOARD_AUTH_MODE=builtin requires"},
		{"dashboard auth mode typo", func(m map[string]string) { m["DASHBOARD_AUTH_MODE"] = "cloudflare" }, nil, "DASHBOARD_AUTH_MODE"},
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

func TestLoadBaseTextOnlyBudgetDoesNotReserveInactiveQwen(t *testing.T) {
	env := minimalEnv()
	env["GLOBAL_MONTHLY_SPEND_MICRO_USD"] = "200000"
	env["MULTIMODAL_UPSTREAM_MODEL"] = "inactive-unknown-model"
	if _, err := LoadBase(envMap(env)); err != nil {
		t.Fatalf("Qwen-disabled deployment should validate only active text cost: %v", err)
	}

	env["DASHSCOPE_API_KEY"] = "qwen-key"
	env["DASHSCOPE_WORKSPACE_ID"] = "ws_test"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "MULTIMODAL_UPSTREAM_MODEL") {
		t.Fatalf("enabling Qwen must activate its model checks, got %v", err)
	}
}

func TestLoadBaseDashboardAuthModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{name: "default disabled", want: "disabled"},
		{name: "builtin", mut: func(m map[string]string) {
			m["DASHBOARD_AUTH_MODE"] = "builtin"
			m["DASHBOARD_USER"] = "admin"
			m["DASHBOARD_PASSWORD"] = "s3cret"
		}, want: "builtin"},
		{name: "external ignores migration credentials", mut: func(m map[string]string) {
			m["DASHBOARD_AUTH_MODE"] = "external"
			m["DASHBOARD_USER"] = "admin"
			m["DASHBOARD_PASSWORD"] = "s3cret"
		}, want: "external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := minimalEnv()
			if tc.mut != nil {
				tc.mut(env)
			}
			got, err := LoadBase(envMap(env))
			if err != nil {
				t.Fatal(err)
			}
			if got.DashboardAuthMode != tc.want {
				t.Fatalf("DashboardAuthMode=%q, want %q", got.DashboardAuthMode, tc.want)
			}
		})
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
	for _, k := range []string{"DEEPSEEK_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_PASSWORD", "DASHBOARD_AUTH_MODE", "GOMEMLIMIT_MIB", "LISTEN_ADDR"} {
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
		case "DEEPSEEK_API_KEY", "DASHSCOPE_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD":
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
	env["DASHSCOPE_API_KEY"] = "qwen-supersecret"
	env["DASHSCOPE_WORKSPACE_ID"] = "ws_test"
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
	for _, leak := range []string{"sk-a", "sk-b", "qwen-supersecret", "supersecretvalue", "hunter2pw"} {
		if strings.Contains(vals, leak) {
			t.Fatalf("secret %q leaked into Snapshot values: %s", leak, vals)
		}
	}
	if kv["deepseek_keys"] != "sk-*** (2 configured)" {
		t.Fatalf("deepseek_keys mask = %v", kv["deepseek_keys"])
	}
	if kv["qwen_keys"] != "*** (1 configured)" {
		t.Fatalf("qwen_keys mask = %v", kv["qwen_keys"])
	}
	if kv["install_pow_secret"] != "configured" {
		t.Fatalf("pow secret mask = %v", kv["install_pow_secret"])
	}
	if kv["dashboard_auth_mode"] != "disabled" {
		t.Fatalf("dashboard auth mode = %v", kv["dashboard_auth_mode"])
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
