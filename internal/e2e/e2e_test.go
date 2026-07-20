//go:build integration

// Package e2e is the black-box, full-stack integration harness for the clean-arch
// gateway. It stands up the REAL :8080 business handler (router.BuildHandler =
// Recover→DenyCORS→MaxBody→ServeMux) over httptest.NewServer, wires it to the SAME
// app services (app/quota, app/install, app/chat, app/model) over the SAME infra
// (sqlite in t.TempDir, the real upstream client pointed at a fake DeepSeek
// httptest server, a configprovider seeded from a test env map) that
// internal/bootstrap.Build assembles in production — then drives it with a real
// http.Client over the loopback socket. Unlike the per-handler unit tests (which
// call ServeHTTP directly and bypass the chain + the mux), this exercises the exact
// wiring the binary uses: middleware ordering, ServeMux method routing, header
// echo-back, the reserve→forward→settle saga, and the tools/tool_choice agentic
// passthrough — all end to end with NO stubs.
//
// 真 http.Server 起栈 + 假上游 + 真 http.Client:用与 bootstrap.Build 同样的 app/infra
// 装配(router.BuildHandler 业务 mux + 真 sqlite + 真 upstream client 指向假 DeepSeek
// + 测试 env map 的 configprovider),覆盖被直调 ServeHTTP 绕过的中间件链与路由。
// build tag=integration,纯 httptest 无 Docker,CI 单列一步跑。
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	appmodel "github.com/sunweilin/anselm/gateway/internal/app/model"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/infra/chatprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/diskguard"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/ratelimit"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/installstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/quotastore"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/router"
)

// --- the in-test composition root (mirrors internal/bootstrap.Build) ----------

// stack bundles the wired handler with the few collaborators a test reaches back
// into (the quota service + the raw quota store for spend-wallet polling).
type stack struct {
	handler http.Handler
	quota   *appquota.Service
	qstore  *quotastore.Store
	bgWG    *sync.WaitGroup
}

// testEnv builds a getenv-style map → the env-driven half of the config. We do NOT
// call configprovider.LoadBase (it carries production-hard fail-fasts irrelevant
// to a single-listener httptest harness); instead we assemble the same
// config.Config bootstrap would hold post-overlay and seed the live Provider with
// it — exercising the REAL configprovider.Provider the request hot path reads.
func baseConfig(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		DeepSeekAPIKeys:         []string{"sk-test-key-never-leaks"},
		DeepSeekBaseURL:         strings.TrimRight(upstreamURL, "/"),
		PublicModelID:           "anselm-auto",
		TextUpstreamModel:       billing.DeepSeekV4Flash,
		MultimodalUpstreamModel: billing.KimiK26,
		MonthlyQuota:            100,
		GlobalDailySpendPUSD:    10 * billing.PicoUSDPerUSD,
		InstallDailySpendPUSD:   2 * billing.PicoUSDPerUSD,
		DeepSeekDailySpendPUSD:  10 * billing.PicoUSDPerUSD,
		KimiDailySpendPUSD:    10 * billing.PicoUSDPerUSD,
		MaxTokensCap:            4096,
		InputTokenCap:           16384,
		MaxMessages:             256,
		MaxMessageChars:         131072,
		MaxMediaParts:           8,
		MaxMediaDecodedBytes:    192 * 1024,
		MaxBodyBytes:            256 * 1024,
		NGlobalConcurrency:      8,
		RatePerMin:              1000,
		InstallPerIPHour:        100,
		QueueWait:               0,
		Location:                loc,
		ResetTZ:                 "Asia/Shanghai",
		UpstreamHeaderTimeout:   5 * time.Second,
		InstallPowMode:          config.PowModeOff,
	}
}

// buildStack constructs the real components (sqlite store / quota / install / chat
// / model + the real upstream client pointed at the fake DeepSeek) exactly as
// bootstrap.Build does, and hands them to the SAME router.BuildHandler the binary
// calls — so the harness exercises the production routing + middleware chain + the
// app saga itself, with zero hand-copied twin to drift.
func buildStack(t *testing.T, upstreamURL string) *stack {
	return buildStackWith(t, upstreamURL, nil)
}

// buildStackWith is buildStack with an optional mutator on the assembled config
// before the Provider is seeded — used to flip the M2 Sybil/PoW gates or shrink a
// spend wallet end to end without forking the whole assembly.
func buildStackWith(t *testing.T, upstreamURL string, mutate func(*config.Config)) *stack {
	return buildStackWithProviders(t, upstreamURL, "", mutate)
}

// buildStackWithProviders additionally wires a Kimi compatibility endpoint.
// Most legacy e2e cases leave it blank to prove text service remains independent;
// multimodal cases opt in and exercise the same two-client registry as bootstrap.
func buildStackWithProviders(t *testing.T, upstreamURL, kimiURL string, mutate func(*config.Config)) *stack {
	t.Helper()
	cfg := baseConfig(t, upstreamURL)
	if kimiURL != "" {
		cfg.KimiAPIKeys = []string{"kimi-test-key-never-leaks"}
		cfg.KimiBaseURL = strings.TrimRight(kimiURL, "/")
	}
	if mutate != nil {
		mutate(cfg)
	}

	// Real SQLite in a temp dir (write + read pools), migrated by Open — the same
	// store bootstrap opens. Closed LAST (after the bgWG drains) so a detached
	// settle/rollback never races a closed DB (REL-4 / GW-INV-24).
	db, err := sqlite.Open(sqlite.Config{
		Path:             t.TempDir() + "/e2e.db",
		ReadPoolMaxConns: 2,
		ConnMaxLifetime:  30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Real hot-swappable config Provider seeded with the assembled effective config
	// (the same type the request hot path Load()s in production).
	cfgP := configprovider.New(*cfg)

	// Stores over the real pools.
	quotaStore := quotastore.New(db.Writer, db.Reader)
	installStore := installstore.New(db.Writer, db.Reader)

	// Metrics bundle (RED + golden signals) — real, like bootstrap; mounted nowhere
	// here but the app services + router wrap against it as in production.
	mx := metrics.New()
	inflight := &atomic.Int64{}

	// App services over their PORTS via the same structural adapters bootstrap uses
	// (re-declared locally so the harness names only app + infra, never bootstrap's
	// unexported adapter types).
	quotaSvc := appquota.New(quotaStore, quotaCfg{p: cfgP})
	nonces := noncecache.New(10 * time.Minute)
	installSvc := appinstall.New(cfgP, installStore, nonces, counterStub{}, powStub{})
	modelCat := appmodel.New(cfgP)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Real provider-local client pointed at the fake DeepSeek, then placed in the
	// same no-fallback registry bootstrap uses. Endpoint, key pool and breaker are
	// construction-time facts; the request can never redirect credentials.
	deepSeekClient := upstream.NewBackend(upstream.Options{
		Backend:            upstream.BackendDeepSeek,
		ChatCompletionsURL: cfg.DeepSeekBaseURL + "/chat/completions",
		APIKeys:            cfg.DeepSeekAPIKeys,
		HeaderTimeout:      cfg.UpstreamHeaderTimeout,
		Logger:             logger,
	})
	var kimiClient upstream.BackendClient
	if len(cfg.KimiAPIKeys) > 0 {
		kimiClient = upstream.NewBackend(upstream.Options{
			Backend:            upstream.BackendKimi,
			ChatCompletionsURL: cfg.KimiBaseURL + "/chat/completions",
			APIKeys:            cfg.KimiAPIKeys,
			HeaderTimeout:      cfg.UpstreamHeaderTimeout,
			Logger:             logger,
		})
	}
	providers := chatprovider.New(deepSeekClient, kimiClient)

	// Shared rate limiter (the bucket the chat RL gate reaches through).
	rl := ratelimit.New(cfg.RatePerMin)

	// REL-6 disk guard over the data disk (the DB path).
	dg := diskguard.New(diskguard.Config{Path: cfg.DBPath, Logger: logger})

	// REL-4 shutdown barrier: drain detached settle/rollback BEFORE closing the DB.
	bgWG := &sync.WaitGroup{}
	t.Cleanup(func() {
		bgWG.Wait()
		_ = db.Close()
	})

	chatSvc := appchat.New(appchat.Deps{
		Auth:     installSvc,
		Quota:    quotaSvc,
		Upstream: upstreamAdapter{r: providers},
		RL:       rl,
		Throttle: noThrottle{},
		Disk:     dg,
		Config:   cfgP,
		Clock:    sysClock{},
		Logger:   discardLogger{},
		Metrics:  chatMx{m: mx, inflight: inflight},
		BgWG:     bgWG,
	})

	// The production assembly — the SAME function main calls, so this harness tests
	// the real routing + middleware chain (no divergent copy).
	handler := router.BuildHandler(router.Deps{
		Install: installSvc,
		Chat:    chatSvc,
		Quota:   quotaSvc,
		Models:  modelCat,
		Mx:      mx,
		OnPanic: mx.Panics,
		// Mirror bootstrap: the configured body cap flows into the chain (a zero
		// value falls back to the 256KiB contract default, same as production).
		MaxBodyBytes: cfg.MaxBodyBytes,
	})
	return &stack{handler: handler, quota: quotaSvc, qstore: quotaStore, bgWG: bgWG}
}

// --- structural port adapters (mirror internal/bootstrap/adapters.go) ---------

// upstreamAdapter widens *upstream.Stream into the app's chat.UpstreamStream (Go
// has no return-type covariance), mapping the nil-stream case so a nil *Stream
// never becomes a non-nil interface — exactly bootstrap's adapter.
type upstreamAdapter struct{ r *chatprovider.Registry }

func (a upstreamAdapter) Open(ctx context.Context, provider billing.Provider, model string, req domchat.CompletionRequest, firstByteTimeout time.Duration) (appchat.UpstreamStream, *appchat.UpstreamFailure) {
	s, failure := a.r.Open(ctx, provider, model, req, firstByteTimeout)
	if failure != nil {
		return nil, &appchat.UpstreamFailure{APIError: failure.APIError, Exposure: failure.Exposure}
	}
	return s, nil
}
func (a upstreamAdapter) Available(provider billing.Provider) bool {
	return a.r != nil && a.r.Available(provider)
}
func (a upstreamAdapter) BreakerOpen(provider billing.Provider) bool {
	return a.r != nil && a.r.BreakerOpen(provider)
}

// quotaCfg adapts the live Provider into app/quota.ConfigSource off ONE atomic
// Load so a hot-edit never splits a reservation's view.
type quotaCfg struct{ p *configprovider.Provider }

func (q quotaCfg) Limits() appquota.Limits {
	c := q.p.Load()
	return appquota.Limits{
		MonthlyQuota:          c.MonthlyQuota,
		InstallDailySpendPUSD: c.InstallDailySpendPUSD,
		ProviderDailySpendPUSD: map[billing.Provider]int64{
			billing.ProviderDeepSeek: c.DeepSeekDailySpendPUSD,
			billing.ProviderKimi:   c.KimiDailySpendPUSD,
		},
		GlobalDailySpendPUSD: c.GlobalDailySpendPUSD,
		DailySublimit:        c.DailySublimit,
	}
}
func (q quotaCfg) Location() *time.Location { return q.p.Load().Location }

// sysClock / discardLogger / chatMx / noThrottle mirror bootstrap's chat adapters.
type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }

type discardLogger struct{}

func (discardLogger) Warn(ctx context.Context, msg string, args ...any) {}

type chatMx struct {
	m        *metrics.Metrics
	inflight *atomic.Int64
}

func (c chatMx) Inflight(n int) {
	c.m.InflightConc.Set(float64(n))
	c.inflight.Store(int64(n))
}
func (c chatMx) Upstream(provider billing.Provider, outcome string) {
	c.m.UpstreamRequests.WithLabelValues(string(provider), outcome).Inc()
}
func (c chatMx) BillingDrift(provider billing.Provider) {
	c.m.BillingDrifts.WithLabelValues(string(provider)).Inc()
}
func (c chatMx) SettleFailure()   { c.m.SettleFailures.Inc() }
func (c chatMx) RollbackFailure() { c.m.RollbackFailures.Inc() }

// noThrottle is the dormant M2 anomaly throttle (TOKEN_ANOMALY_RPM=0): zero work,
// never tightens, never flags.
type noThrottle struct{}

func (noThrottle) Observe(installID string) bool { return false }

// counterStub / powStub satisfy the install service's optional counter ports.
type counterStub struct{}

func (counterStub) Inc() {}

type powStub struct{}

func (powStub) Inc(result string) {}

// --- fake DeepSeek upstreams --------------------------------------------------

// fakeDeepSeek is an SSE upstream that streams one delta + an include_usage final
// frame + [DONE], recording what it received so the e2e flow can assert the gateway
// sanitized the request (model rewrite, key injection, tools/tool_choice
// passthrough) — and stripped the upstream Set-Cookie on the way back.
func fakeDeepSeek(t *testing.T) (*httptest.Server, func() (auth, body string)) {
	t.Helper()
	var mu sync.Mutex
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "leak=1") // must be stripped by the gateway
		fw, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\""+billing.DeepSeekV4Flash+"\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fw.Flush()
		_, _ = io.WriteString(w, "data: {\"model\":\""+billing.DeepSeekV4Flash+"\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":8,\"total_tokens\":11}}\n\n")
		fw.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fw.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return gotAuth, gotBody
	}
}

// fakeDeepSeekNonStream is a NON-streaming upstream (stream:false path). Modes:
//   - "ok":       a complete OpenAI JSON body with usage.total_tokens=usageTokens
//   - "truncate": 2xx headers + a Content-Length larger than the bytes actually
//     written, then the conn is hijacked & closed mid-body so the gateway's bounded
//     ReadAll of the upstream body errors AFTER output is committed (exercises
//     nonStreamThrough's post-output error branch that must still full-settle).
func fakeDeepSeekNonStream(t *testing.T, mode string, usageTokens int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "truncate" {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("response writer is not a Hijacker")
				return
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n")
			_, _ = bufrw.WriteString(`{"choices":[{"message":{"content":"hi"`) // far short of 4096, then close
			_ = bufrw.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "leak=1") // must be stripped by the gateway
		w.WriteHeader(http.StatusOK)
		body := `{"id":"cmpl-1","object":"chat.completion","model":"` + billing.DeepSeekV4Flash + `","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":` +
			strconv.FormatInt(usageTokens-3, 10) + `,"total_tokens":` +
			strconv.FormatInt(usageTokens, 10) + `}}`
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- helpers ------------------------------------------------------------------

// installToken posts /v1/install over the real socket and returns the issued token
// (top-level `token` field — the gateway writes a BARE entity, not a {"data":...}
// envelope).
func installToken(t *testing.T, srv *httptest.Server, client *http.Client) string {
	t.Helper()
	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var inst struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if inst.Token == "" {
		t.Fatal("install returned no token")
	}
	return inst.Token
}

// waitGlobalSpend polls the shared daily pUSD wallet until it equals want or the
// deadline elapses, since settle runs on a DETACHED background goroutine (REL-4)
// rather than synchronously with the HTTP response return.
func waitGlobalSpend(t *testing.T, s *stack, want int64) int64 {
	t.Helper()
	return pollGlobalSpend(t, s, func(spend int64) bool { return spend == want })
}

func pollGlobalSpend(t *testing.T, s *stack, done func(int64) bool) int64 {
	t.Helper()
	period := s.quota.SnapshotPeriod(time.Now())
	deadline := time.Now().Add(3 * time.Second)
	var spend int64
	for time.Now().Before(deadline) {
		if u := globalSpendPUSD(t, s, period); done(u) {
			return u
		} else {
			spend = u
		}
		time.Sleep(10 * time.Millisecond)
	}
	return spend
}

// globalSpendPUSD reads the shared day wallet via the real store View. Monthly
// request count and install-local spend are intentionally ignored here.
func globalSpendPUSD(t *testing.T, s *stack, p domquota.Period) int64 {
	t.Helper()
	_, _, spend, err := s.qstore.View(context.Background(), "any", p)
	if err != nil {
		t.Fatalf("global spend view: %v", err)
	}
	return spend
}

// deepSeekCostPUSD prices an authoritative usage vector through the same frozen
// rate card as production, so e2e assertions never duplicate price constants.
func deepSeekCostPUSD(t *testing.T, prompt, completion int64) int64 {
	t.Helper()
	plan, err := billing.NewPlan(billing.ProviderDeepSeek, billing.DeepSeekV4Flash,
		billing.InputStandard, prompt, completion)
	if err != nil {
		t.Fatalf("build DeepSeek cost plan: %v", err)
	}
	cost, ok, err := plan.Cost(billing.Usage{
		Present: true, PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion,
	})
	if err != nil || !ok {
		t.Fatalf("price DeepSeek usage: cost=%d ok=%v err=%v", cost, ok, err)
	}
	return cost
}

func kimiCostPUSD(t *testing.T, prompt, completion int64) int64 {
	t.Helper()
	plan, err := billing.NewPlan(billing.ProviderKimi, billing.KimiK26,
		billing.InputStandard, prompt, completion)
	if err != nil {
		t.Fatalf("build Kimi cost plan: %v", err)
	}
	cost, ok, err := plan.Cost(billing.Usage{
		Present: true, PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion,
	})
	if err != nil || !ok {
		t.Fatalf("price Kimi usage: cost=%d ok=%v err=%v", cost, ok, err)
	}
	return cost
}

// --- tests --------------------------------------------------------------------

// TestE2EInstallChatQuotaFlow drives the full client journey over a real socket:
// POST /v1/install → use the issued token to POST /v1/chat/completions (streamed,
// relayed to the fake upstream) → GET /v1/quota and see the count reflected.
// Asserts the gateway injected the upstream key, rewrote the model, PASSED tools
// VERBATIM (the agentic contract), and stripped the upstream Set-Cookie — through
// the real middleware chain.
func TestE2EInstallChatQuotaFlow(t *testing.T) {
	up, inspect := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	// 1) Install → fresh token.
	instResp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer instResp.Body.Close()
	if instResp.StatusCode != http.StatusOK {
		t.Fatalf("install want 200 got %d", instResp.StatusCode)
	}
	var inst struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(instResp.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inst.Token, "gwk_") {
		t.Fatalf("bad token: %q", inst.Token)
	}

	// 2) Chat (stream) → relayed to the fake upstream, streamed back to [DONE]. The
	// body carries tools + tool_choice; the clean-arch gateway PASSES them through.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"garbage","messages":[{"role":"user","content":"hi"}],"stream":true,`+
			`"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto","logit_bias":{"5":-100}}`))
	req.Header.Set("Authorization", "Bearer "+inst.Token)
	chatResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(chatResp.Body)
	chatResp.Body.Close()
	if chatResp.StatusCode != http.StatusOK {
		t.Fatalf("chat want 200 got %d body=%s", chatResp.StatusCode, body)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream not relayed to [DONE]: %s", body)
	}
	if strings.Count(string(body), `"model":"anselm-auto"`) != 2 || strings.Contains(string(body), billing.DeepSeekV4Flash) {
		t.Fatalf("client stream must expose only PUBLIC_MODEL_ID, got: %s", body)
	}
	// X-Request-ID echoed back by the Recover middleware (chain is in play).
	if chatResp.Header.Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID not echoed — Recover middleware not in the chain")
	}
	// Upstream Set-Cookie must NOT leak to the client (header whitelist).
	if chatResp.Header.Get("Set-Cookie") != "" {
		t.Fatal("upstream Set-Cookie leaked through the gateway")
	}

	// Upstream received the injected key + rewritten model + tools/tool_choice
	// passthrough, with the danger field logit_bias stripped.
	gotAuth, gotBody := inspect()
	if gotAuth != "Bearer sk-test-key-never-leaks" {
		t.Fatalf("upstream key not injected: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"`+billing.DeepSeekV4Flash+`"`) {
		t.Fatalf("exact text upstream model not forced: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"get_weather"`) || !strings.Contains(gotBody, `"tool_choice":"auto"`) {
		t.Fatalf("tools/tool_choice not passed through (agentic contract): %s", gotBody)
	}
	if strings.Contains(gotBody, "logit_bias") {
		t.Fatalf("danger field 'logit_bias' leaked upstream: %s", gotBody)
	}

	// 3) Quota → count reflects the one chat.
	qreq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/quota", nil)
	qreq.Header.Set("Authorization", "Bearer "+inst.Token)
	qResp, err := client.Do(qreq)
	if err != nil {
		t.Fatal(err)
	}
	var qv struct {
		Used      int64 `json:"used"`
		Available bool  `json:"available"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qv)
	qResp.Body.Close()
	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("quota want 200 got %d", qResp.StatusCode)
	}
	if qv.Used != 1 {
		t.Fatalf("quota used=%d want 1 after one chat", qv.Used)
	}
}

func TestE2EMultimodalRoutesOnlyToKimiAndSettlesKimiCost(t *testing.T) {
	var deepSeekHits atomic.Int32
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deepSeekHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer deepSeek.Close()

	var mu sync.Mutex
	var gotPath, gotAuth, gotBody string
	kimi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+billing.KimiK26+`","choices":[{"message":{"role":"assistant","content":"an image"}}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
	}))
	defer kimi.Close()

	s := buildStackWithProviders(t, deepSeek.URL, kimi.URL, nil)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()
	token := installToken(t, srv, client)

	png := []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	body := `{"model":"please-use-deepseek","stream":false,"messages":[` +
		`{"role":"user","content":"remember this"},` +
		`{"role":"assistant","content":"noted","reasoning_content":"provider-private-state"},` +
		`{"role":"user","content":[{"type":"text","text":"describe"},` +
		`{"type":"image_url","image_url":{"url":` + strconv.Quote(dataURI) + `}}]}` +
		`]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multimodal chat want 200 got %d body=%s", resp.StatusCode, responseBody)
	}
	if !strings.Contains(string(responseBody), `"model":"anselm-auto"`) || strings.Contains(string(responseBody), billing.KimiK26) {
		t.Fatalf("Kimi response must expose only PUBLIC_MODEL_ID, got: %s", responseBody)
	}
	if deepSeekHits.Load() != 0 {
		t.Fatalf("multimodal request reached DeepSeek %d times", deepSeekHits.Load())
	}

	mu.Lock()
	path, auth, upstreamBody := gotPath, gotAuth, gotBody
	mu.Unlock()
	if path != "/chat/completions" || auth != "Bearer kimi-test-key-never-leaks" {
		t.Fatalf("Kimi wire target path=%q auth=%q", path, auth)
	}
	for _, want := range []string{
		`"model":"` + billing.KimiK26 + `"`,
		`"type":"image_url"`,
	} {
		if !strings.Contains(upstreamBody, want) {
			t.Fatalf("Kimi payload missing %q: %s", want, upstreamBody)
		}
	}
	if strings.Contains(upstreamBody, "please-use-deepseek") || strings.Contains(upstreamBody, "provider-private-state") {
		t.Fatalf("client model or DeepSeek reasoning state leaked to Kimi: %s", upstreamBody)
	}

	wantSpend := kimiCostPUSD(t, 11, 3)
	if got := waitGlobalSpend(t, s, wantSpend); got != wantSpend {
		t.Fatalf("Kimi spend settled to %d pUSD, want %d", got, wantSpend)
	}
}

// TestE2EMultiTurnToolLoopPreserved drives a multi-turn agentic body — an assistant
// message carrying tool_calls and a tool message carrying tool_call_id — and proves
// the gateway forwards BOTH verbatim. Stripping either breaks the tool loop, so this
// is the load-bearing agentic-contract assertion.
func TestE2EMultiTurnToolLoopPreserved(t *testing.T) {
	up, inspect := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)

	multiTurn := `{"model":"deepseek-chat","stream":true,"messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(multiTurn))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi-turn chat want 200 got %d", resp.StatusCode)
	}

	_, gotBody := inspect()
	for _, want := range []string{`"tool_calls"`, `"call_1"`, `"tool_call_id":"call_1"`, `"name":"get_weather"`, `"role":"tool"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("multi-turn agentic field %q not preserved upstream: %s", want, gotBody)
		}
	}
}

// TestE2EDangerFieldsStripped proves the strict whitelist drops the dangerous
// fields end to end: logit_bias / function_call / response_format never reach the
// upstream, while the legitimate sibling (tool_choice) does.
func TestE2EDangerFieldsStripped(t *testing.T) {
	up, inspect := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)

	body := `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":"none",` +
		`"logit_bias":{"50256":-100},"function_call":"auto","response_format":{"type":"json_object"}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat want 200 got %d", resp.StatusCode)
	}

	_, gotBody := inspect()
	for _, danger := range []string{"logit_bias", "function_call", "response_format"} {
		if strings.Contains(gotBody, danger) {
			t.Fatalf("danger field %q leaked upstream: %s", danger, gotBody)
		}
	}
	if !strings.Contains(gotBody, `"tool_choice":"none"`) {
		t.Fatalf("legitimate tool_choice was dropped: %s", gotBody)
	}
}

// TestE2ENonStreamRelayAndSettle drives the stream:false path (reachable: the
// whitelist forwards `stream`) end to end through the real stack:
//   - (a) the 2xx JSON body is relayed with only model rewritten to PUBLIC_MODEL_ID
//     (all completion fields preserved; upstream Set-Cookie stripped)
//   - (b) settlement prices the ACTUAL structured usage under the DeepSeek rate
//     card, rather than retaining the pessimistic reservation quote.
func TestE2ENonStreamRelayAndSettle(t *testing.T) {
	const actualTokens = 11
	up := fakeDeepSeekNonStream(t, "ok", actualTokens)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"garbage","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// (a) 2xx + completion body preserved except for the public model boundary +
	// upstream Set-Cookie not leaked.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-stream chat want 200 got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"content":"hello there"`) {
		t.Fatalf("non-stream completion content not preserved: %s", body)
	}
	if !strings.Contains(string(body), `"model":"anselm-auto"`) || strings.Contains(string(body), billing.DeepSeekV4Flash) {
		t.Fatalf("non-stream response must expose only PUBLIC_MODEL_ID: %s", body)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("upstream Set-Cookie leaked on non-stream path")
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("non-stream Content-Type want application/json got %q", resp.Header.Get("Content-Type"))
	}

	// (b) prompt=3 + completion=8 is converted to pUSD by the frozen rate card.
	// Equality proves settle-to-actual-cost rather than retain-the-quote.
	wantSpend := deepSeekCostPUSD(t, 3, 8)
	if got := waitGlobalSpend(t, s, wantSpend); got != wantSpend {
		t.Fatalf("global spend settled to %d pUSD, want actual cost %d pUSD", got, wantSpend)
	}
}

// TestE2ENonStreamPostOutputErrorFullSettles: when the upstream commits 2xx headers
// but the body read then fails (truncated mid-body), output is already committed so
// the gateway cannot retry/rollback — it must FULL-settle the reservation (honest,
// conservative; never under-charge / leak spend) and surface a normalized
// UPSTREAM_ERROR. Asserts the full quote remains charged (no double-bill, no leak).
func TestE2ENonStreamPostOutputErrorFullSettles(t *testing.T) {
	up := fakeDeepSeekNonStream(t, "truncate", 0)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)

	// The quote includes the conservative prompt estimate + clamped output bound.
	// With no client max_tokens, at least 4096 output tokens are reserved at the
	// DeepSeek output rate; a lower balance would prove an incorrect refund.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	minimumQuote := deepSeekCostPUSD(t, 1, 4096)
	got := pollGlobalSpend(t, s, func(spend int64) bool { return spend >= minimumQuote })
	if got < minimumQuote {
		t.Fatalf("post-output body-read error charged %d pUSD, want full quote >= %d pUSD", got, minimumQuote)
	}
}

// TestE2ENonStream8MBBodyLimit: the gateway caps the upstream RESPONSE body read at
// 8MB (io.LimitReader in nonStreamThrough). An upstream that floods a body far over
// 8MB must be truncated by the gateway — the relayed body never exceeds the cap, so
// a malicious/runaway upstream cannot pin gateway memory.
func TestE2ENonStream8MBBodyLimit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`)
		fw, _ := w.(http.Flusher)
		chunk := strings.Repeat("a", 64*1024)
		for written := 0; written < 10*1024*1024; written += len(chunk) {
			_, _ = io.WriteString(w, chunk)
			if fw != nil {
				fw.Flush()
			}
		}
		_, _ = io.WriteString(w, `"}}],"usage":{"total_tokens":7}}`)
	}))
	t.Cleanup(up.Close)

	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(body) == 0 {
		t.Fatal("expected a relayed (truncated) body, got empty")
	}
	if len(body) > 8*1024*1024+1024 {
		t.Fatalf("relayed body %d bytes exceeds the 8MB upstream-body cap — LimitReader not enforced", len(body))
	}
}

// TestE2EModelsDeclaration drives GET /v1/models over the real socket: an install
// token → 200 + the one OpenAI-compatible provider-neutral logical model; an
// unauthenticated call → 401. Exercises the production routing + middleware chain
// and the shared install auth without exposing either upstream model id.
func TestE2EModelsDeclaration(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.PublicModelID = "anselm-test"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	// Unauthenticated → 401 INVALID_TOKEN (same auth exit as /chat, /quota).
	noauth, err := client.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(noauth.Body).Decode(&env)
	noauth.Body.Close()
	if noauth.StatusCode != http.StatusUnauthorized || env.Error.Code != "INVALID_TOKEN" {
		t.Fatalf("no-token /v1/models want 401 INVALID_TOKEN got %d %q", noauth.StatusCode, env.Error.Code)
	}

	token := installToken(t, srv, client)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models want 200 got %d", resp.StatusCode)
	}
	var lr struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if lr.Object != "list" {
		t.Fatalf("object = %q want list", lr.Object)
	}
	if len(lr.Data) != 1 {
		t.Fatalf("data len = %d want exactly one logical model", len(lr.Data))
	}
	if lr.Data[0].ID != "anselm-test" {
		t.Fatalf("model id = %q want anselm-test", lr.Data[0].ID)
	}
	for i, m := range lr.Data {
		if m.Object != "model" || m.OwnedBy != "anselm-gateway" {
			t.Fatalf("data[%d] = %+v want object=model owned_by=anselm-gateway", i, m)
		}
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID not echoed on /v1/models — middleware chain missing")
	}
}

// TestE2EMaxBodyRejectsHugeBody: a body over 256KB is rejected (the MaxBody
// middleware + the handler's MaxBytesReader) before any chat logic runs (DoS guard,
// §5.3). Over a real socket the server caps the body and the handler returns a 4xx,
// never 200.
func TestE2EMaxBodyRejectsHugeBody(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	huge := `{"model":"deepseek-chat","messages":[{"role":"user","content":"` + strings.Repeat("a", 300*1024) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(huge))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("over-256KB body was accepted (status 200) — MaxBody not enforced")
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit body want 400/413 got %d", resp.StatusCode)
	}
}

// TestE2EConfiguredMaxBodyBytes: a NON-default MAX_BODY_BYTES flows through the
// full assembly (config → router.Deps → MaxBody middleware + handler re-cap):
// with the cap at its 4KiB floor, a ~5KiB body is rejected while a small body
// still reaches the normal pipeline.
func TestE2EConfiguredMaxBodyBytes(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.MaxBodyBytes = 4096 // spec floor — far below the 256KiB default
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	over := `{"model":"deepseek-chat","messages":[{"role":"user","content":"` + strings.Repeat("a", 5*1024) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(over))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over the configured 4KiB cap: want 400/413 got %d", resp.StatusCode)
	}

	small := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(small))
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("small body under the configured cap must pass: got %d", resp2.StatusCode)
	}
}

// TestE2EBadTokenUnauthorized: a syntactically-present but unknown bearer token →
// 401 INVALID_TOKEN over the real stack (the §2 auth tree's !found exit).
func TestE2EBadTokenUnauthorized(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gwk_does_not_exist")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || env.Error.Code != "INVALID_TOKEN" {
		t.Fatalf("bad token want 401 INVALID_TOKEN got %d %q", resp.StatusCode, env.Error.Code)
	}
}

// TestE2EQuotaExhaustion: with MONTHLY_QUOTA=1, the first chat succeeds (200) and
// the second is rejected 429 QUOTA_EXHAUSTED by the reserve gate — end to end.
func TestE2EQuotaExhaustion(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) { c.MonthlyQuota = 1 })
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &env)
		resp.Body.Close()
		return resp.StatusCode, env.Error.Code
	}

	if code, _ := chat(); code != http.StatusOK {
		t.Fatalf("first chat want 200 got %d", code)
	}
	if code, wire := chat(); code != http.StatusTooManyRequests || wire != "QUOTA_EXHAUSTED" {
		t.Fatalf("second chat want 429 QUOTA_EXHAUSTED got %d %q", code, wire)
	}
}

// TestE2EBudgetExhaustion: with a global daily pUSD wallet smaller than a single
// request's frozen rate-card quote, Reserve denies before any upstream call and
// returns 402 BUDGET_EXHAUSTED over the real stack.
func TestE2EBudgetExhaustion(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	// One pUSD is below every non-empty DeepSeek quote. The install/provider wallets
	// remain ample, so this isolates the shared-wallet gate.
	s := buildStackWith(t, up.URL, func(c *config.Config) { c.GlobalDailySpendPUSD = 1 })
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || env.Error.Code != "BUDGET_EXHAUSTED" {
		t.Fatalf("budget-exhausted chat want 402 BUDGET_EXHAUSTED got %d %q", resp.StatusCode, env.Error.Code)
	}
}

// TestE2EUpstreamRejectedRollsBackAndKeepsBreakerClosed: a DeepSeek-style 400
// (context overflow) end to end through the real stack —
//   - (a) the client receives 400 UPSTREAM_REJECTED with details.reason ==
//     "context_length" (the coarse enum; the upstream text never passes through)
//   - (b) the reservation is rolled back: the shared daily spend returns to 0
//     (reserve committed BEFORE the upstream call, so 0 proves the rollback landed)
//   - (c) rejections are NON-fault (ADR-011): even past the breaker's
//     5-consecutive-failure threshold, a follow-up request still reaches upstream
//     and succeeds, settling to its ACTUAL usage only.
func TestE2EUpstreamRejectedRollsBackAndKeepsBreakerClosed(t *testing.T) {
	const rejectN = 6 // past the breaker's 5-consecutive threshold
	const deepseekBody = `{"error":{"message":"This model's maximum context length is 131072 tokens. ` +
		`However, you requested 200069 tokens (198021 in the messages, 2048 in the completion). ` +
		`Please reduce the length of the messages or completion.","type":"invalid_request_error",` +
		`"param":null,"code":"invalid_request_error"}}`
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= rejectN {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, deepseekBody)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fw, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fw.Flush()
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":8,\"total_tokens\":11}}\n\n")
		fw.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fw.Flush()
	}))
	t.Cleanup(up.Close)

	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	token := installToken(t, srv, client)
	chat := func() (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// (a) Every rejected request → 400 UPSTREAM_REJECTED, reason=context_length,
	// and no upstream text leaked into the envelope.
	for i := 0; i < rejectN; i++ {
		resp, body := chat()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("rejected chat %d want 400 got %d body=%s", i, resp.StatusCode, body)
		}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("rejected chat %d: bad envelope %s: %v", i, body, err)
		}
		if env.Error.Code != apierr.CodeUpstreamRejected {
			t.Fatalf("rejected chat %d code = %q want %q", i, env.Error.Code, apierr.CodeUpstreamRejected)
		}
		if got := env.Error.Details["reason"]; got != apierr.RejectedContextLength {
			t.Fatalf("rejected chat %d details.reason = %v want %q", i, got, apierr.RejectedContextLength)
		}
		if strings.Contains(string(body), "131072") {
			t.Fatalf("upstream error text leaked to the client (GW-INV-11): %s", body)
		}
	}

	// (b) Rollback landed: the reserve committed BEFORE each upstream call, so a
	// shared spend back at 0 proves every rejected reservation was rolled back.
	if got := waitGlobalSpend(t, s, 0); got != 0 {
		t.Fatalf("rejected reservations not rolled back: spend=%d pUSD want 0", got)
	}

	// (c) Breaker never opened (rejections are non-fault, ADR-011): the next
	// request reaches upstream and streams to [DONE]...
	resp, body := chat()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("follow-up chat after %d rejections want 200+[DONE] (breaker closed) got %d body=%s",
			rejectN, resp.StatusCode, body)
	}
	if got := calls.Load(); got != rejectN+1 {
		t.Fatalf("upstream calls = %d want %d (rejections never retried, follow-up not shed)", got, rejectN+1)
	}
	// ...and settles to its ACTUAL priced usage only — the sole wallet spend.
	wantSpend := deepSeekCostPUSD(t, 3, 8)
	if got := waitGlobalSpend(t, s, wantSpend); got != wantSpend {
		t.Fatalf("spend after success = %d pUSD want %d pUSD", got, wantSpend)
	}
}

// TestE2ECORSPreflightForbidden: a browser preflight (OPTIONS + Origin) is 403'd by
// the DenyCORS middleware over the real stack, with no Access-Control-* header.
func TestE2ECORSPreflightForbidden(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight want 403 got %d", resp.StatusCode)
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("Access-Control header leaked on denied preflight: %s", k)
		}
	}
}

// TestE2EMethodAndRouteErrors: ServeMux method/route handling end to end —
//   - GET on the POST-only chat route → 405 (mux method mismatch)
//   - unknown path → 404
//   - GET /healthz → 200 (liveness, the only public health surface) + X-Request-ID
func TestE2EMethodAndRouteErrors(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	resp, err := client.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on POST-only route want 405 got %d", resp.StatusCode)
	}

	resp2, err := client.Get(srv.URL + "/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route want 404 got %d", resp2.StatusCode)
	}

	resp3, err := client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK || !strings.Contains(string(hb), "ok") {
		t.Fatalf("healthz want 200 ok got %d body=%s", resp3.StatusCode, hb)
	}
	if resp3.Header.Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID not echoed on /healthz")
	}
}

// TestE2ERequestIDEchoed: a client-supplied X-Request-ID is sanitized + echoed back
// over the real chain (the Recover middleware reuses a safe id rather than always
// minting), so a sidecar can correlate its own id end to end.
func TestE2ERequestIDEchoed(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	const rid = "e2e-correlation-123"
	req.Header.Set("X-Request-ID", rid)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != rid {
		t.Fatalf("X-Request-ID echo = %q want %q (safe client id should be reused)", got, rid)
	}
}

// TestE2EInstallGlobalCapOverSocket: with INSTALL_GLOBAL_DAILY_CAP=N, the (N+1)th
// install over the real socket is rejected 429 + INSTALL_CAP_REACHED — the M2
// coarse valve end to end through the real routing + middleware chain.
func TestE2EInstallGlobalCapOverSocket(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	const capN = 3
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallGlobalDailyCap = capN
		c.InstallPerIPHour = 1000 // out of the way; only the global valve gates
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	for i := 0; i < capN; i++ {
		resp, err := client.Post(srv.URL+"/v1/install", "application/json",
			strings.NewReader(`{"fingerprint":"fp-`+strconv.Itoa(i)+`","client":"e2e"}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("install %d want 200 got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp-over","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap install want 429 got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "INSTALL_CAP_REACHED" {
		t.Fatalf("over-cap wire code = %q want INSTALL_CAP_REACHED", env.Error.Code)
	}
}

// solvePoWE2E mines a nonce satisfying difficulty leading-zero bits over the real
// SHA256 (the client-side proof-of-work, no shortcut).
func solvePoWE2E(t *testing.T, challenge string, difficulty int) string {
	t.Helper()
	for i := 0; i < 50_000_000; i++ {
		nonce := strconv.Itoa(i)
		h := sha256.Sum256([]byte(challenge + "." + nonce))
		if leadingZeroBitsE2E(h[:]) >= difficulty {
			return nonce
		}
	}
	t.Fatalf("no nonce found for difficulty %d", difficulty)
	return ""
}

func leadingZeroBitsE2E(b []byte) int {
	n := 0
	for _, by := range b {
		if by == 0 {
			n += 8
			continue
		}
		n += bits.LeadingZeros8(by)
		break
	}
	return n
}

// fetchChallenge GETs /v1/install/challenge over the socket and returns its fields.
func fetchChallenge(t *testing.T, srv *httptest.Server, client *http.Client) (challenge string, difficulty int, required bool) {
	t.Helper()
	resp, err := client.Get(srv.URL + "/v1/install/challenge")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("challenge want 200 got %d", resp.StatusCode)
	}
	var cr struct {
		Challenge  string `json:"challenge"`
		Difficulty int    `json:"difficulty"`
		Required   bool   `json:"required"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	return cr.Challenge, cr.Difficulty, cr.Required
}

// TestE2EPoWEnforceOverSocket: with INSTALL_POW_MODE=enforce, /install over the real
// socket rejects a missing PoW (403) and a replayed proof (403 nonce-once), and
// admits a genuinely-mined one (200) — the full challenge→solve→install journey.
func TestE2EPoWEnforceOverSocket(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallPowMode = config.PowModeEnforce
		c.InstallPowDifficulty = 8 // low so the test mines quickly
		c.InstallPowSecret = []byte("e2e-pow-secret")
		c.InstallPowSecretSource = "configured"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	// Missing X-PoW → 403 INSTALL_POW_REQUIRED.
	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || env.Error.Code != "INSTALL_POW_REQUIRED" {
		t.Fatalf("missing PoW want 403 INSTALL_POW_REQUIRED got %d %q", resp.StatusCode, env.Error.Code)
	}

	// Fetch a challenge, mine it, install → 200.
	ch, diff, required := fetchChallenge(t, srv, client)
	if !required {
		t.Fatal("enforce mode challenge must report required=true")
	}
	nonce := solvePoWE2E(t, ch, diff)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/install",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	req.Header.Set("X-PoW", ch+"."+nonce)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&ok)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.HasPrefix(ok.Token, "gwk_") {
		t.Fatalf("mined PoW install want 200 + token got %d %q", resp2.StatusCode, ok.Token)
	}

	// Replay the SAME proof → 403 INSTALL_POW_INVALID (nonce-once).
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/install",
		strings.NewReader(`{"fingerprint":"fp2","client":"e2e"}`))
	req2.Header.Set("X-PoW", ch+"."+nonce)
	resp3, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	var env3 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&env3)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden || env3.Error.Code != "INSTALL_POW_INVALID" {
		t.Fatalf("PoW replay want 403 INSTALL_POW_INVALID got %d %q", resp3.StatusCode, env3.Error.Code)
	}
}

// TestE2EPoWShadowDoesNotBreakExisting: with INSTALL_POW_MODE=shadow, a client that
// sends NO X-PoW (the existing/legacy behavior) is still admitted (200) over the
// real socket, and the challenge endpoint reports required=false — shadow never
// breaks an existing client.
func TestE2EPoWShadowDoesNotBreakExisting(t *testing.T) {
	up, _ := fakeDeepSeek(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallPowMode = config.PowModeShadow
		c.InstallPowDifficulty = 20
		c.InstallPowSecret = []byte("e2e-shadow-secret")
		c.InstallPowSecretSource = "configured"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := srv.Client()

	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ok.Token, "gwk_") {
		t.Fatalf("shadow no-PoW want 200 + token got %d %q", resp.StatusCode, ok.Token)
	}

	if _, _, required := fetchChallenge(t, srv, client); required {
		t.Fatal("shadow mode challenge must report required=false")
	}
}

// compile-time interface assertions: the local adapters MUST satisfy the SAME app
// ports bootstrap's adapters do, so this harness can never drift from the wiring
// the binary uses (a signature change breaks the build here, not silently in prod).
var (
	_ appchat.Upstream                  = upstreamAdapter{}
	_ appquota.ConfigSource             = quotaCfg{}
	_ appchat.Clock                     = sysClock{}
	_ appchat.Logger                    = discardLogger{}
	_ appchat.Metrics                   = chatMx{}
	_ appchat.Throttle                  = noThrottle{}
	_ appinstall.InstallsCreatedCounter = counterStub{}
	_ appinstall.PoWCounter             = powStub{}
)
