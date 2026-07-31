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

// minimalEnv supplies the required secret plus an explicit spend-wallet value that satisfies the
// compiled model quote. PUBLIC_MODEL_ID is included so its default/override behavior stays visible
// in fixtures even though it is optional.
//
// The credential is supplied as TWO comma-separated keys because multi-key parsing is part of the
// contract: a pool of keys is how one account survives a per-key cooldown, and a fixture with a
// single key would leave the splitting/trimming path untested by every case that builds on this.
//
// 凭证刻意给**两把**逗号分隔的 key,因为多 key 解析是契约的一部分:key 池正是一个账号扛过单 key
// 冷却的方式,而只给一把的夹具会让所有以它为基础的用例都测不到切分/去空白那条路。
func minimalEnv() map[string]string {
	return map[string]string{
		"DASHSCOPE_API_KEY":              "qwen-a,qwen-b",
		"DASHSCOPE_WORKSPACE_ID":         "ws-test",
		"PUBLIC_MODEL_ID":                "anselm-auto",
		"GLOBAL_MONTHLY_SPEND_MICRO_USD": "420000000",
	}
}

// productionEnv is minimalEnv with the GATEWAY_MODE rationing mask OFF. The
// tests that assert a knob's value reaches the live config VERBATIM need it:
// under the default debug mode those knobs are masked open on purpose, which
// would fail a swap-mechanics test for a reason that has nothing to do with
// swapping. Tests about the mask itself use minimalEnv (the real default).
//
// productionEnv = minimalEnv + 关掉掩码。凡断言某旋钮**逐字**到达生效配置的测试都要用它:
// 默认 debug 下那些旋钮是**故意**被掩开的,拿它测 swap 会因为与 swap 无关的原因挂掉。
// 测掩码本身的用例则用 minimalEnv(即真实默认)。
func productionEnv() map[string]string {
	m := minimalEnv()
	m["GATEWAY_MODE"] = config.RuntimeModeProduction
	return m
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

	if len(c.QwenAPIKeys) != 2 || c.QwenAPIKeys[0] != "qwen-a" {
		t.Fatalf("QwenAPIKeys = %v", c.QwenAPIKeys)
	}
	if c.PublicModelID != "anselm-auto" {
		t.Fatalf("logical model = %q, want anselm-auto", c.PublicModelID)
	}
	// No DASHSCOPE_BASE_URL in the fixture, so the endpoint must be DERIVED from
	// the workspace id. Pinning the derived form here is what keeps a future
	// hardcoded region from silently replacing it — the same key answers 200 in
	// one region and 401 in another.
	// 夹具里没有 DASHSCOPE_BASE_URL,故端点必须由 workspace id **派生**。把派生结果钉在这里,
	// 是为了防止将来某个写死的区域悄悄取代它——同一把 key,一个区域 200、另一个 401。
	if want := "https://ws-test.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"; c.QwenBaseURL != want {
		t.Fatalf("QwenBaseURL = %q, want %q", c.QwenBaseURL, want)
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
	// ADR 0012 removed MEDIA_PUBLIC_BASE_URL entirely: verified lease content is INLINED into the
	// upstream request, so the gateway no longer needs to know its own public origin (the provider's
	// fetcher refused to download from it anyway — measured live 2026-07-25).
	// ADR 0012 整个移除了 MEDIA_PUBLIC_BASE_URL:已校验的 lease 内容**内联**进上游请求,网关不再需要知道
	// 自己的公开 origin(反正 provider 的拉取器也拒绝从它下载——2026-07-25 线上实测)。
	c = mustLoad(t, env)
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
	env["DASHSCOPE_BASE_URL"] = "https://x.example/"
	env["DASHSCOPE_API_KEY"] = " sk-1 , , sk-2 "
	c := mustLoad(t, env)
	if c.QwenBaseURL != "https://x.example" {
		t.Fatalf("base url not trimmed: %q", c.QwenBaseURL)
	}
	if len(c.QwenAPIKeys) != 2 || c.QwenAPIKeys[1] != "sk-2" {
		t.Fatalf("keys = %v", c.QwenAPIKeys)
	}
}

// TestLoadBaseQwenKeyIsRequiredAndWorkspaceDerived: the Qwen credential went from optional to
// mandatory when every chat request started routing to the multimodal model (WRK-082 H9). A
// deployment without it can answer nothing, so boot is the honest place to fail — not per request.
//
// TestLoadBaseQwenKeyIsRequiredAndWorkspaceDerived:当每一次 chat 都开始路由到多模态模型时(H9),
// Qwen 凭证从**可选**变成了**必需**。没有它的部署什么也答不了,故**启动**才是诚实的失败点——不是
// 每个请求失败一次。
func TestLoadBaseQwenKeyIsRequiredAndWorkspaceDerived(t *testing.T) {
	env := minimalEnv()
	delete(env, "DASHSCOPE_API_KEY")
	if _, err := LoadBase(envMap(env)); !errors.Is(err, ErrQwenKeyRequired) {
		t.Fatalf("a deployment with no Qwen key must refuse to start, got %v", err)
	}
	env["DASHSCOPE_API_KEY"] = " qwen-a, ,qwen-b "
	env["DASHSCOPE_WORKSPACE_ID"] = "ws_test-1"
	c := mustLoad(t, env)
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
		// The credential gates boot because every route needs it: a gateway without
		// it can answer nothing, so it must fail at boot rather than once per request.
		// 凭证是启动门禁,因为**每一条**路由都要它:没有它的网关什么也答不了,故必须在**启动时**
		// 失败,而不是每个请求失败一次。
		{"missing Qwen key", func(m map[string]string) { delete(m, "DASHSCOPE_API_KEY") }, ErrQwenKeyRequired, ""},
		{"blank Qwen key", func(m map[string]string) { m["DASHSCOPE_API_KEY"] = "  , " }, ErrQwenKeyRequired, ""},
		{"DashScope base URL userinfo", func(m map[string]string) { m["DASHSCOPE_BASE_URL"] = "https://secret@example.com/compatible-mode/v1" }, nil, "absolute HTTPS base URL"},
		{"Qwen key needs endpoint", func(m map[string]string) { delete(m, "DASHSCOPE_WORKSPACE_ID") }, nil, "DASHSCOPE_API_KEY requires"},
		{"invalid workspace id", func(m map[string]string) { m["DASHSCOPE_WORKSPACE_ID"] = "bad.example" }, nil, "DASHSCOPE_WORKSPACE_ID"},
		{"remote plaintext base URL", func(m map[string]string) { m["DASHSCOPE_BASE_URL"] = "http://dashscope.example.com" }, nil, "HTTP is allowed only for a literal loopback IP"},
		{"unsafe public model id", func(m map[string]string) { m["PUBLIC_MODEL_ID"] = "bad model" }, nil, "PUBLIC_MODEL_ID"},
		{"unknown priced model", func(m map[string]string) { m["MULTIMODAL_UPSTREAM_MODEL"] = "qwen-not-a-real-model" }, nil, "no exact rate card"},
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
		}, nil, "worst-case request quote"},
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

// TestLoadBaseBudgetMustCoverTheRoutedModel: the wallet check used to skip Qwen when no key was
// configured — a "text-only deployment" could boot on a small budget. That deployment shape no
// longer exists (H9), so the multimodal card is ALWAYS the one the budget must cover, and a model
// id with no compiled card fails at boot instead of at the first request.
//
// TestLoadBaseBudgetMustCoverTheRoutedModel:钱包校验此前在没配 Qwen key 时**跳过**它——一个「纯文本
// 部署」可以靠小预算启动。那种部署形态**已经不存在**(H9),故多模态卡**永远**是预算必须覆盖的那张,
// 而一个没有编译期卡的 model id 会在**启动时**失败、不是在第一个请求时。
func TestLoadBaseBudgetMustCoverTheRoutedModel(t *testing.T) {
	env := minimalEnv()
	env["MULTIMODAL_UPSTREAM_MODEL"] = "inactive-unknown-model"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "MULTIMODAL_UPSTREAM_MODEL") {
		t.Fatalf("an unknown routed model must fail at boot, got %v", err)
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
			env["DASHSCOPE_BASE_URL"] = baseURL
			if got := mustLoad(t, env).QwenBaseURL; got != baseURL {
				t.Fatalf("QwenBaseURL = %q, want %q", got, baseURL)
			}
		})
	}
}

func TestLoadBaseRejectsHostnameHTTPEvenForLocalhost(t *testing.T) {
	t.Parallel()
	env := minimalEnv()
	env["DASHSCOPE_BASE_URL"] = "http://localhost:18080"
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
	p := New(mustLoad(t, productionEnv()))
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
	p := New(mustLoad(t, productionEnv()))
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
	for _, k := range []string{"DASHSCOPE_API_KEY", "INSTALL_POW_SECRET", "MEDIA_SIGNING_SECRET", "GOMEMLIMIT_MIB", "LISTEN_ADDR"} {
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
		case "DASHSCOPE_API_KEY", "INSTALL_POW_SECRET", "DASHBOARD_USER", "DASHBOARD_PASSWORD":
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
	for _, leak := range []string{"qwen-a", "qwen-b", "qwen-supersecret", "supersecretvalue", "hunter2pw"} {
		if strings.Contains(vals, leak) {
			t.Fatalf("secret %q leaked into Snapshot values: %s", leak, vals)
		}
	}
	if kv["qwen_keys"] != "*** (1 configured)" {
		t.Fatalf("qwen_keys mask = %v", kv["qwen_keys"])
	}
	if kv["install_pow_secret"] != "configured" {
		t.Fatalf("pow secret mask = %v", kv["install_pow_secret"])
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
	p := New(mustLoad(t, productionEnv()))
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

// --- GATEWAY_MODE: default, mask, and the non-destructive round trip ----------

func TestLoadBaseGatewayModeDefaultsToDebug(t *testing.T) {
	// A fresh deployment that names no mode is a DEVELOPMENT deployment. This is
	// the one default whose direction is deliberately permissive, so it is pinned.
	if c := mustLoad(t, minimalEnv()); c.RuntimeMode != config.RuntimeModeDebug || !c.DebugMode() {
		t.Fatalf("default GATEWAY_MODE = %q, want debug", c.RuntimeMode)
	}
	env := minimalEnv()
	env["GATEWAY_MODE"] = "production"
	if c := mustLoad(t, env); c.RuntimeMode != config.RuntimeModeProduction || c.DebugMode() {
		t.Fatalf("explicit production not honored: %q", c.RuntimeMode)
	}
	env["GATEWAY_MODE"] = "prod"
	if _, err := LoadBase(envMap(env)); err == nil || !strings.Contains(err.Error(), "GATEWAY_MODE") {
		t.Fatalf("typo GATEWAY_MODE must fail fast, got %v", err)
	}
}

func TestProviderDebugMasksLoadButNotConfigured(t *testing.T) {
	env := minimalEnv() // GATEWAY_MODE unset → debug
	env["RATE_PER_MIN"] = "8"
	env["DAILY_SUBLIMIT"] = "100"
	env["MONTHLY_QUOTA"] = "5000"
	p := New(mustLoad(t, env))

	// Enforcement reads the mask...
	eff := p.Load()
	if eff.RatePerMin != 0 || eff.DailySublimit != 0 {
		t.Fatalf("debug must open the gates: rate=%d sublimit=%d", eff.RatePerMin, eff.DailySublimit)
	}
	if eff.MonthlyQuota != config.MaxMonthlyQuota {
		t.Fatalf("debug MonthlyQuota = %d, want ceiling", eff.MonthlyQuota)
	}
	// ...while the operator's real numbers survive untouched behind it.
	cfgd := p.Configured()
	if cfgd.RatePerMin != 8 || cfgd.DailySublimit != 100 || cfgd.MonthlyQuota != 5000 {
		t.Fatalf("configured values corrupted by the mask: %+v", cfgd)
	}
}

func TestDumpShowsConfiguredValuesWhileDebugMasks(t *testing.T) {
	// The dashboard config table is an EDITOR: showing the mask would invite the
	// operator to "correct" a masked 0 back to 8 and thereby persist the mask.
	env := minimalEnv() // debug
	env["RATE_PER_MIN"] = "8"
	p := New(mustLoad(t, env))

	got := map[string]string{}
	for _, it := range p.Dump() {
		got[it.Key] = it.Value
	}
	if got["RATE_PER_MIN"] != "8" {
		t.Fatalf("Dump RATE_PER_MIN = %q, want the configured 8", got["RATE_PER_MIN"])
	}
	if got["GATEWAY_MODE"] != "debug" {
		t.Fatalf("Dump must surface GATEWAY_MODE=debug, got %q", got["GATEWAY_MODE"])
	}
}

func TestHotFlipToProductionArmsConfiguredValuesAndBack(t *testing.T) {
	// The whole point of a mode: one runtime-hot key, no restart, and the
	// production numbers are never re-entered.
	env := minimalEnv() // debug
	env["RATE_PER_MIN"] = "8"
	env["DAILY_SUBLIMIT"] = "100"
	p := New(mustLoad(t, env))
	ss := newFakeStore()

	if _, err := p.ApplyOverrides(context.Background(),
		map[string]string{"GATEWAY_MODE": "production"}, ss); err != nil {
		t.Fatalf("flip to production: %v", err)
	}
	if c := p.Load(); c.RatePerMin != 8 || c.DailySublimit != 100 {
		t.Fatalf("production must arm configured values: rate=%d sublimit=%d", c.RatePerMin, c.DailySublimit)
	}
	if ss.data["GATEWAY_MODE"] != "production" {
		t.Fatalf("mode not persisted: %v", ss.data)
	}

	// ...and back, still without losing anything.
	if _, err := p.ApplyOverrides(context.Background(),
		map[string]string{"GATEWAY_MODE": "debug"}, ss); err != nil {
		t.Fatalf("flip back to debug: %v", err)
	}
	if c := p.Load(); c.RatePerMin != 0 || c.DailySublimit != 0 {
		t.Fatalf("debug must re-open the gates: rate=%d sublimit=%d", c.RatePerMin, c.DailySublimit)
	}
	if c := p.Configured(); c.RatePerMin != 8 || c.DailySublimit != 100 {
		t.Fatalf("round trip lost the configured values: %+v", c)
	}
}

func TestOverrideDuringDebugDoesNotPersistTheMask(t *testing.T) {
	// The regression this design exists to prevent: an UNRELATED dashboard edit
	// made while debug is live must not bake the masked zeros into settings.
	env := minimalEnv() // debug
	env["RATE_PER_MIN"] = "8"
	env["DAILY_SUBLIMIT"] = "100"
	p := New(mustLoad(t, env))
	ss := newFakeStore()

	if _, err := p.ApplyOverrides(context.Background(),
		map[string]string{"PUBLIC_MODEL_ID": "anselm-next"}, ss); err != nil {
		t.Fatalf("unrelated override: %v", err)
	}
	if c := p.Configured(); c.RatePerMin != 8 || c.DailySublimit != 100 {
		t.Fatalf("unrelated edit destroyed configured limits: rate=%d sublimit=%d", c.RatePerMin, c.DailySublimit)
	}
	if _, ok := ss.data["RATE_PER_MIN"]; ok {
		t.Fatalf("mask leaked into the persisted overlay: %v", ss.data)
	}
	// The mask is still live and the unrelated edit still took effect.
	if c := p.Load(); c.RatePerMin != 0 || c.PublicModelID != "anselm-next" {
		t.Fatalf("post-override state wrong: rate=%d model=%q", c.RatePerMin, c.PublicModelID)
	}
}

func TestSnapshotReportsEffectiveModeAndValues(t *testing.T) {
	// The startup line answers "what is this process enforcing", so it reports the
	// mask — and leads with gateway_mode so masked zeros are never misread.
	env := minimalEnv() // debug
	env["RATE_PER_MIN"] = "8"
	p := New(mustLoad(t, env))
	snap := p.Snapshot()
	if len(snap) < 2 || snap[0] != "gateway_mode" || valStr(snap[1]) != "debug" {
		t.Fatalf("snapshot must lead with gateway_mode=debug, got %v %v", snap[0], snap[1])
	}
	// rate_per_min is logged as an int, so assert on the value, not valStr (which
	// is deliberately string-only for the secret-masking assertions).
	seen := false
	for i := 0; i+1 < len(snap); i += 2 {
		if snap[i] != "rate_per_min" {
			continue
		}
		seen = true
		if got, ok := snap[i+1].(int); !ok || got != 0 {
			t.Fatalf("snapshot rate_per_min = %v, want the enforced 0", snap[i+1])
		}
	}
	if !seen {
		t.Fatal("snapshot missing rate_per_min")
	}
}
