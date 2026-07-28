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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

	"github.com/gorilla/websocket"
	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appdeviceproof "github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
	appimage "github.com/sunweilin/anselm/gateway/internal/app/image"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	appmodel "github.com/sunweilin/anselm/gateway/internal/app/model"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	apptts "github.com/sunweilin/anselm/gateway/internal/app/tts"
	appvideo "github.com/sunweilin/anselm/gateway/internal/app/video"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
	"github.com/sunweilin/anselm/gateway/internal/infra/chatprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/diskguard"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/ratelimit"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/installstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/quotastore"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/voicestore"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
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
		MultimodalUpstreamModel: billing.Qwen37Plus,
		MonthlyQuota:            100,
		GlobalMonthlySpendPUSD:  10 * billing.PicoUSDPerUSD,
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
//
// **The one fake serves BOTH provider slots, and that is not a shortcut.** The free tier resolves
// every chat request to the multimodal model now (deepseek 撤除), so a stack with only the DeepSeek
// slot wired answers 503 MULTIMODAL_UNAVAILABLE to everything — which is precisely what this whole
// tagged package did after that change, unnoticed, because a build tag keeps it out of `go test ./...`.
//
// **一个假件同时坐两个 provider 槽,这不是图省事。** 免费档现在把**每一条** chat 都解析到多模态模型
// (deepseek 已撤),故只接了 DeepSeek 槽的栈对什么都答 503 MULTIMODAL_UNAVAILABLE——而那正是这整个
// 打标签的包在那次改动之后的状态,没人发现,因为 build tag 让它不进 `go test ./...`。
func buildStackWith(t *testing.T, upstreamURL string, mutate func(*config.Config)) *stack {
	return buildStackWithProviders(t, upstreamURL, upstreamURL, mutate)
}

// buildStackWithProviders wires the Qwen compatible endpoint explicitly — for the cases that need
// the two upstreams to be DISTINGUISHABLE (different fakes, different assertions on which one was
// reached). Everything else goes through buildStackWith and lets one fake answer for both.
func buildStackWithProviders(t *testing.T, upstreamURL, qwenURL string, mutate func(*config.Config)) *stack {
	t.Helper()
	cfg := baseConfig(t, upstreamURL)
	if qwenURL != "" {
		cfg.QwenAPIKeys = []string{"qwen-test-key-never-leaks"}
		cfg.QwenBaseURL = strings.TrimRight(qwenURL, "/")
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
	proofSvc := appdeviceproof.New(installStore, noncecache.NewFailClosed(2*time.Minute, noncecache.DefaultMax))
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
	var qwenClient upstream.BackendClient
	if len(cfg.QwenAPIKeys) > 0 {
		qwenClient = upstream.NewBackend(upstream.Options{
			Backend:            upstream.BackendQwen,
			ChatCompletionsURL: cfg.QwenBaseURL + "/chat/completions",
			APIKeys:            cfg.QwenAPIKeys,
			HeaderTimeout:      cfg.UpstreamHeaderTimeout,
			Logger:             logger,
		})
	}
	providers := chatprovider.New(deepSeekClient, qwenClient)

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

	// The three generation capabilities, constructed UNCONDITIONALLY exactly as
	// bootstrap does — each answers Available() from live config, so a test that
	// leaves the capability off still exercises the real "capability absent" path
	// instead of a nil route. They share the Qwen key and the native origin.
	// 三个生成能力,与 bootstrap 一样**无条件**构造——可用性由各自 Available() 按 live 配置回答,
	// 于是一个不开该能力的用例仍然走**真的**「能力不存在」路径、而不是一条 nil 路由。三者共用
	// Qwen key 与原生 origin。
	genKey := ""
	if len(cfg.QwenAPIKeys) > 0 {
		genKey = cfg.QwenAPIKeys[0]
	}
	imageSvc := appimage.New(appimage.Deps{
		Auth: installSvc, Quota: quotaSvc, RL: rl, Config: cfgP,
		Upstream: upstream.NewImageGen(cfg.DashScopeNativeBase, genKey),
		Clock:    sysClock{}, Metrics: chatMx{m: mx, inflight: inflight},
	})
	voiceStore := voicestore.New(db.Writer, db.Reader)
	ttsSvc := apptts.New(apptts.Deps{
		Auth: installSvc, Quota: quotaSvc, RL: rl, Config: cfgP,
		Upstream: upstream.NewTTSGen(cfg.DashScopeNativeBase, genKey),
		Voices:   voiceStore,
		Clock:    sysClock{}, Metrics: chatMx{m: mx, inflight: inflight},
	})
	videoSvc := appvideo.New(appvideo.Deps{
		Auth: installSvc, Quota: quotaSvc, RL: rl, Config: cfgP,
		Upstream: videoUp{g: upstream.NewVideoGen(cfg.DashScopeNativeBase, genKey)},
		Clock:    sysClock{}, Metrics: chatMx{m: mx, inflight: inflight},
	})

	// The production assembly — the SAME function main calls, so this harness tests
	// the real routing + middleware chain (no divergent copy).
	handler := router.BuildHandler(router.Deps{
		Install: installSvc,
		Proof:   proofSvc,
		Chat:    chatSvc,
		Quota:   quotaSvc,
		Models:  modelCat,
		Images:  imageSvc,
		TTS:     ttsSvc,
		Video:   videoSvc,
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
	// The three category caps MUST be here. This mirror is the second copy of
	// bootstrap's adapter, and the first copy is exactly where SPEECH_DAILY_LIMIT
	// was silently dropped — an e2e harness missing the same line would prove a
	// gate works while production's never fires.
	// 三个品类上限**必须**在这里。本镜像是 bootstrap 适配器的第二份拷贝,而**第一份**正是
	// SPEECH_DAILY_LIMIT 被静默丢掉的地方——一个漏了同一行的 e2e 会「证明」一道在生产上从未
	// 生效过的闸是好的。
	return appquota.Limits{
		MonthlyQuota:           c.MonthlyQuota,
		GlobalMonthlySpendPUSD: c.GlobalMonthlySpendPUSD,
		DailySublimit:          c.DailySublimit,
		ImageDailyLimit:        c.ImageDailyLimit,
		SpeechDailyLimit:       c.SpeechDailyLimit,
		VideoDailyLimit:        c.VideoDailyLimit,
	}
}

// videoUp mirrors bootstrap's video port adapter (app names its own status type
// so it never imports infra).
type videoUp struct{ g *upstream.VideoGen }

func (v videoUp) SubmitVideo(ctx context.Context, model, prompt string, seconds int, ratio, resolution, firstFrame string) (string, bool, error) {
	return v.g.SubmitVideo(ctx, model, prompt, seconds, ratio, resolution, firstFrame)
}

func (v videoUp) PollVideo(ctx context.Context, taskID string) (appvideo.VideoStatus, error) {
	st, err := v.g.PollVideo(ctx, taskID)
	if err != nil {
		return appvideo.VideoStatus{}, err
	}
	return appvideo.VideoStatus{Phase: st.Phase, URL: st.URL}, nil
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

// --- fake chat upstreams ------------------------------------------------------

// fakeChatUpstream is an SSE upstream that streams one delta + an include_usage final
// frame + [DONE], recording what it received so the e2e flow can assert the gateway
// sanitized the request (model rewrite, key injection, tools/tool_choice
// passthrough) — and stripped the upstream Set-Cookie on the way back.
func fakeChatUpstream(t *testing.T) (*httptest.Server, func() (auth, body string)) {
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
		_, _ = io.WriteString(w, "data: {\"model\":\""+billing.Qwen37Plus+"\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fw.Flush()
		_, _ = io.WriteString(w, "data: {\"model\":\""+billing.Qwen37Plus+"\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":8,\"total_tokens\":11}}\n\n")
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

// fakeChatUpstreamNonStream is a NON-streaming upstream (stream:false path). Modes:
//   - "ok":       a complete OpenAI JSON body with usage.total_tokens=usageTokens
//   - "truncate": 2xx headers + a Content-Length larger than the bytes actually
//     written, then the conn is hijacked & closed mid-body so the gateway's bounded
//     ReadAll of the upstream body errors AFTER output is committed (exercises
//     nonStreamThrough's post-output error branch that must still full-settle).
func fakeChatUpstreamNonStream(t *testing.T, mode string, usageTokens int64) *httptest.Server {
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
		body := `{"id":"cmpl-1","object":"chat.completion","model":"` + billing.Qwen37Plus + `","choices":[{"index":0,` +
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

type proofTransport struct {
	base    http.RoundTripper
	private ed25519.PrivateKey
}

func newProofClient(t *testing.T, client *http.Client) *http.Client {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clone := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &proofTransport{base: base, private: private}
	return &clone
}

func (t *proofTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v1/proof/challenge" || req.URL.Path == "/v1/install/challenge" || req.URL.Path == "/healthz" {
		return t.base.RoundTrip(req)
	}
	kid := req.Header.Get(proofhttp.HeaderInstallID)
	public := t.private.Public().(ed25519.PublicKey)
	if req.URL.Path == "/v1/install" {
		thumb := sha256.Sum256(public)
		kid = base64.RawURLEncoding.EncodeToString(thumb[:])
	}
	if kid == "" {
		return t.base.RoundTrip(req)
	}

	challengeReq, _ := http.NewRequestWithContext(req.Context(), http.MethodGet,
		req.URL.Scheme+"://"+req.URL.Host+"/v1/proof/challenge", nil)
	challengeResp, err := t.base.RoundTrip(challengeReq)
	if err != nil {
		return nil, err
	}
	var challenge struct {
		Nonce string `json:"nonce"`
	}
	err = json.NewDecoder(challengeResp.Body).Decode(&challenge)
	_ = challengeResp.Body.Close()
	if err != nil {
		return nil, err
	}
	raw := []byte(nil)
	if req.Body != nil {
		raw, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(raw)))
	}
	bh := sha256.Sum256(raw)
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	target := strings.ToLower(req.URL.Host) + req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	payload, _ := json.Marshal(struct {
		Version int    `json:"v"`
		KeyID   string `json:"kid"`
		Issued  int64  `json:"iat"`
		ID      string `json:"jti"`
		Nonce   string `json:"nonce"`
		Method  string `json:"htm"`
		Target  string `json:"htu"`
		Body    string `json:"bh"`
	}{1, kid, time.Now().Unix(), base64.RawURLEncoding.EncodeToString(jti), challenge.Nonce,
		req.Method, target, base64.RawURLEncoding.EncodeToString(bh[:])})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Body = io.NopCloser(strings.NewReader(string(raw)))
	clone.ContentLength = int64(len(raw))
	clone.Header.Set(proofhttp.HeaderProof, encoded+"."+base64.RawURLEncoding.EncodeToString(ed25519.Sign(t.private, []byte(encoded))))
	if req.URL.Path == "/v1/install" {
		clone.Header.Set(proofhttp.HeaderPublicKey, base64.RawURLEncoding.EncodeToString(public))
	}
	return t.base.RoundTrip(clone)
}

// registerInstall posts /v1/install over the real socket and returns the public install id
// (top-level `installId` field — the gateway writes a BARE entity, not a {"data":...}
// envelope).
func registerInstall(t *testing.T, srv *httptest.Server, client *http.Client) string {
	t.Helper()
	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var inst struct {
		InstallID string `json:"installId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if inst.InstallID == "" {
		t.Fatal("install returned no install id")
	}
	return inst.InstallID
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

func qwenCostPUSD(t *testing.T, prompt, completion int64) int64 {
	t.Helper()
	plan, err := billing.NewPlan(billing.ProviderQwen, billing.Qwen37Plus,
		billing.InputStandard, prompt, completion)
	if err != nil {
		t.Fatalf("build Qwen cost plan: %v", err)
	}
	cost, ok, err := plan.Cost(billing.Usage{
		Present: true, PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion,
	})
	if err != nil || !ok {
		t.Fatalf("price Qwen usage: cost=%d ok=%v err=%v", cost, ok, err)
	}
	return cost
}

// --- tests --------------------------------------------------------------------

// TestE2EInstallChatQuotaFlow drives the full client journey over a real socket:
// POST /v1/install → use the public id plus device proof to POST /v1/chat/completions (streamed,
// relayed to the fake upstream) → GET /v1/quota and see the count reflected.
// Asserts the gateway injected the upstream key, rewrote the model, PASSED tools
// VERBATIM (the agentic contract), and stripped the upstream Set-Cookie — through
// the real middleware chain.
func TestE2EInstallChatQuotaFlow(t *testing.T) {
	up, inspect := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	// 1) Install → public id bound to this client's proof key.
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
		InstallID string `json:"installId"`
	}
	if err := json.NewDecoder(instResp.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inst.InstallID, "ins_") {
		t.Fatalf("bad install id: %q", inst.InstallID)
	}

	// 2) Chat (stream) → relayed to the fake upstream, streamed back to [DONE]. The
	// body carries tools + tool_choice; the clean-arch gateway PASSES them through.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"garbage","messages":[{"role":"user","content":"hi"}],"stream":true,`+
			`"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto","logit_bias":{"5":-100}}`))
	req.Header.Set("X-Anselm-Install-ID", inst.InstallID)
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
	if strings.Count(string(body), `"model":"anselm-auto"`) != 2 || strings.Contains(string(body), billing.Qwen37Plus) {
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
	if gotAuth != "Bearer qwen-test-key-never-leaks" {
		t.Fatalf("upstream key not injected: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"`+billing.Qwen37Plus+`"`) {
		t.Fatalf("exact upstream model not forced: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"get_weather"`) || !strings.Contains(gotBody, `"tool_choice":"auto"`) {
		t.Fatalf("tools/tool_choice not passed through (agentic contract): %s", gotBody)
	}
	if strings.Contains(gotBody, "logit_bias") {
		t.Fatalf("danger field 'logit_bias' leaked upstream: %s", gotBody)
	}

	// 3) Quota → count reflects the one chat.
	qreq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/quota", nil)
	qreq.Header.Set("X-Anselm-Install-ID", inst.InstallID)
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

func TestE2EMultimodalRoutesOnlyToQwenAndSettlesQwenCost(t *testing.T) {
	var deepSeekHits atomic.Int32
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deepSeekHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer deepSeek.Close()

	var mu sync.Mutex
	var gotPath, gotAuth, gotBody string
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+billing.Qwen37Plus+`","choices":[{"message":{"role":"assistant","content":"an image"}}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
	}))
	defer qwen.Close()

	s := buildStackWithProviders(t, deepSeek.URL, qwen.URL, nil)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	png := []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	body := `{"model":"please-use-deepseek","stream":false,"messages":[` +
		`{"role":"user","content":"remember this"},` +
		`{"role":"assistant","content":"noted","reasoning_content":"provider-private-state"},` +
		`{"role":"user","content":[{"type":"text","text":"describe"},` +
		`{"type":"image_url","image_url":{"url":` + strconv.Quote(dataURI) + `}}]}` +
		`]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Anselm-Install-ID", installID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multimodal chat want 200 got %d body=%s", resp.StatusCode, responseBody)
	}
	if !strings.Contains(string(responseBody), `"model":"anselm-auto"`) || strings.Contains(string(responseBody), billing.Qwen37Plus) {
		t.Fatalf("Qwen response must expose only PUBLIC_MODEL_ID, got: %s", responseBody)
	}
	if deepSeekHits.Load() != 0 {
		t.Fatalf("multimodal request reached DeepSeek %d times", deepSeekHits.Load())
	}

	mu.Lock()
	path, auth, upstreamBody := gotPath, gotAuth, gotBody
	mu.Unlock()
	if path != "/chat/completions" || auth != "Bearer qwen-test-key-never-leaks" {
		t.Fatalf("Qwen wire target path=%q auth=%q", path, auth)
	}
	for _, want := range []string{
		`"model":"` + billing.Qwen37Plus + `"`,
		`"type":"image_url"`,
		`"enable_thinking":true`,
	} {
		if !strings.Contains(upstreamBody, want) {
			t.Fatalf("Qwen payload missing %q: %s", want, upstreamBody)
		}
	}
	if strings.Contains(upstreamBody, "please-use-deepseek") || strings.Contains(upstreamBody, "provider-private-state") {
		t.Fatalf("client model or DeepSeek reasoning state leaked to Qwen: %s", upstreamBody)
	}

	wantSpend := qwenCostPUSD(t, 11, 3)
	if got := waitGlobalSpend(t, s, wantSpend); got != wantSpend {
		t.Fatalf("Qwen spend settled to %d pUSD, want %d", got, wantSpend)
	}
}

// TestE2EMultiTurnToolLoopPreserved drives a multi-turn agentic body — an assistant
// message carrying tool_calls and a tool message carrying tool_call_id — and proves
// the gateway forwards BOTH verbatim. Stripping either breaks the tool loop, so this
// is the load-bearing agentic-contract assertion.
func TestE2EMultiTurnToolLoopPreserved(t *testing.T) {
	up, inspect := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)

	multiTurn := `{"model":"deepseek-chat","stream":true,"messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(multiTurn))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	up, inspect := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)

	body := `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":"none",` +
		`"logit_bias":{"50256":-100},"function_call":"auto","response_format":{"type":"json_object"}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	up := fakeChatUpstreamNonStream(t, "ok", actualTokens)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"garbage","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	if !strings.Contains(string(body), `"model":"anselm-auto"`) || strings.Contains(string(body), billing.Qwen37Plus) {
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
	wantSpend := qwenCostPUSD(t, 3, 8)
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
	up := fakeChatUpstreamNonStream(t, "truncate", 0)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)

	// The quote includes the conservative prompt estimate + clamped output bound.
	// With no client max_tokens, at least 4096 output tokens are reserved at the
	// DeepSeek output rate; a lower balance would prove an incorrect refund.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
// device proof → 200 + the one OpenAI-compatible provider-neutral logical model; an
// unauthenticated call → 401. Exercises the production routing + middleware chain
// and the shared install auth without exposing either upstream model id.
func TestE2EModelsDeclaration(t *testing.T) {
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.PublicModelID = "anselm-test"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	// Unauthenticated → 401 DEVICE_PROOF_REQUIRED.
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
	if noauth.StatusCode != http.StatusUnauthorized || env.Error.Code != "DEVICE_PROOF_REQUIRED" {
		t.Fatalf("unproved /v1/models want 401 DEVICE_PROOF_REQUIRED got %d %q", noauth.StatusCode, env.Error.Code)
	}

	installID := registerInstall(t, srv, client)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	up, _ := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	huge := `{"model":"deepseek-chat","messages":[{"role":"user","content":"` + strings.Repeat("a", 300*1024) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(huge))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.MaxBodyBytes = 4096 // spec floor — far below the 256KiB default
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	over := `{"model":"deepseek-chat","messages":[{"role":"user","content":"` + strings.Repeat("a", 5*1024) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(over))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	req2.Header.Set("X-Anselm-Install-ID", installID)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("small body under the configured cap must pass: got %d", resp2.StatusCode)
	}
}

// TestE2EBadInstallUnauthorized: an unknown public id cannot resolve a verification key.
func TestE2EBadInstallUnauthorized(t *testing.T) {
	up, _ := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Anselm-Install-ID", "ins_does_not_exist")
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
	if resp.StatusCode != http.StatusUnauthorized || env.Error.Code != "INVALID_INSTALL" {
		t.Fatalf("bad install want 401 INVALID_INSTALL got %d %q", resp.StatusCode, env.Error.Code)
	}
}

// TestE2EQuotaExhaustion: with MONTHLY_QUOTA=1, the first chat succeeds (200) and
// the second is rejected 429 QUOTA_EXHAUSTED by the reserve gate — end to end.
func TestE2EQuotaExhaustion(t *testing.T) {
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) { c.MonthlyQuota = 1 })
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("X-Anselm-Install-ID", installID)
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

// TestE2EBudgetExhaustion: with a global monthly pUSD wallet smaller than a single
// request's frozen rate-card quote, Reserve denies before any upstream call and
// returns 402 BUDGET_EXHAUSTED over the real stack.
func TestE2EBudgetExhaustion(t *testing.T) {
	up, _ := fakeChatUpstream(t)
	// One pUSD is below every non-empty DeepSeek quote, isolating the shared
	// operator monthly budget gate.
	s := buildStackWith(t, up.URL, func(c *config.Config) { c.GlobalMonthlySpendPUSD = 1 })
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("X-Anselm-Install-ID", installID)
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
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	chat := func() (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("X-Anselm-Install-ID", installID)
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
	wantSpend := qwenCostPUSD(t, 3, 8)
	if got := waitGlobalSpend(t, s, wantSpend); got != wantSpend {
		t.Fatalf("spend after success = %d pUSD want %d pUSD", got, wantSpend)
	}
}

// TestE2ECORSPreflightForbidden: a browser preflight (OPTIONS + Origin) is 403'd by
// the DenyCORS middleware over the real stack, with no Access-Control-* header.
func TestE2ECORSPreflightForbidden(t *testing.T) {
	up, _ := fakeChatUpstream(t)
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
	up, _ := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

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
	up, _ := fakeChatUpstream(t)
	s := buildStack(t, up.URL)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

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
	up, _ := fakeChatUpstream(t)
	const capN = 3
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallGlobalDailyCap = capN
		c.InstallPerIPHour = 1000 // out of the way; only the global valve gates
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	for i := 0; i < capN; i++ {
		client := newProofClient(t, srv.Client())
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

	client := newProofClient(t, srv.Client())
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
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallPowMode = config.PowModeEnforce
		c.InstallPowDifficulty = 8 // low so the test mines quickly
		c.InstallPowSecret = []byte("e2e-pow-secret")
		c.InstallPowSecretSource = "configured"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

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
		InstallID string `json:"installId"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&ok)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.HasPrefix(ok.InstallID, "ins_") {
		t.Fatalf("mined PoW install want 200 + install id got %d %q", resp2.StatusCode, ok.InstallID)
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
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.InstallPowMode = config.PowModeShadow
		c.InstallPowDifficulty = 20
		c.InstallPowSecret = []byte("e2e-shadow-secret")
		c.InstallPowSecretSource = "configured"
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	resp, err := client.Post(srv.URL+"/v1/install", "application/json",
		strings.NewReader(`{"fingerprint":"fp","client":"e2e"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ok struct {
		InstallID string `json:"installId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ok.InstallID, "ins_") {
		t.Fatalf("shadow no-PoW want 200 + install id got %d %q", resp.StatusCode, ok.InstallID)
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

// --- generation capabilities: image / speech / video (WRK-082 H2) -------------

// fakeDashScopeNative stands in for the NATIVE DashScope origin all three
// generation routes call. One server serves all four upstream shapes because in
// production they ARE one origin differing only by path — splitting them in the
// harness would hide a path mistake that production would suffer.
//
// fakeDashScopeNative 扮演三条生成路由共同调用的**原生** DashScope origin。一台服务器伺候全部四种
// 上游形状,因为在生产上它们**本就是**同一个 origin、只在 path 上不同——在测试里把它们拆开,会藏住
// 一个生产上真会犯的 path 错误。
type fakeDashScopeNative struct {
	srv *httptest.Server

	mu        sync.Mutex
	authSeen  []string
	asyncSeen []string // the X-DashScope-Async header value per submit
	bodies    []string
	paths     []string
	videoDone atomic.Bool // flip to make the next poll answer SUCCEEDED
}

func newFakeDashScope(t *testing.T) *fakeDashScopeNative {
	t.Helper()
	f := &fakeDashScopeNative{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Speech left HTTP entirely in H9: `qwen-audio-3.0-tts-flash` is served ONLY over
		// `api-ws/v1/inference` (真机实测——两条 HTTP 形状都答 `url error`). It rides the SAME origin
		// as the other three, which is why it belongs on this one fake rather than a second server:
		// splitting them would hide a path mistake that production really can make.
		// H9 之后语音整个离开了 HTTP:`qwen-audio-3.0-tts-flash` **只**在 `api-ws/v1/inference` 上提供
		// (真机实测——两条 HTTP 形状都答 `url error`)。它与另外三条**同一个 origin**,故它属于这台假件
		// 而不是第二台服务器:拆开会藏住一个生产上真会犯的 path 错误。
		if r.URL.Path == "/api-ws/v1/inference" {
			f.mu.Lock()
			f.paths = append(f.paths, r.URL.Path)
			f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
			f.asyncSeen = append(f.asyncSeen, "")
			f.mu.Unlock()
			f.serveTTS(t, w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		f.asyncSeen = append(f.asyncSeen, r.Header.Get("X-DashScope-Async"))
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/services/aigc/multimodal-generation/generation":
			// Image only — speech moved to the duplex socket above (H9).
			// 只剩图像——语音已搬到上面那条双工套接字(H9)。
			_, _ = io.WriteString(w, `{"output":{"choices":[{"message":{"content":[{"image":"https://oss.example/i.png"}]}}]}}`)
		case r.URL.Path == "/api/v1/services/aigc/video-generation/video-synthesis":
			_, _ = io.WriteString(w, `{"output":{"task_id":"task-e2e-1"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			if f.videoDone.Load() {
				_, _ = io.WriteString(w, `{"output":{"task_status":"SUCCEEDED","video_url":"https://oss.example/v.mp4"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"output":{"task_status":"RUNNING"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ttsAudio is what the fake synthesizer "speaks". The real model answers 24kHz/16bit/mono WAV; the
// bytes' CONTENT is irrelevant to the gateway (it relays them), their identity is not — the e2e
// asserts the client got exactly what the upstream produced.
// ttsAudio 是假合成器「说」出来的东西。真模型答 24kHz/16bit/mono WAV;字节**内容**与网关无关(它只
// 是中继),字节**身份**有关——e2e 断言客户端拿到的正是上游产出的那些。
var ttsAudio = []byte("RIFF....WAVEfake-e2e-audio")

// serveTTS speaks the upstream's duplex protocol: run-task → task-started, continue-task carries
// the text, finish-task closes, audio arrives as BINARY frames in between.
//
// serveTTS 说上游那套双工协议:run-task → task-started,continue-task 送文本,finish-task 收尾,
// 音频以**二进制帧**夹在中间回来。
func (f *fakeDashScopeNative) serveTTS(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var f2 struct {
			Header struct {
				Action string `json:"action"`
				TaskID string `json:"task_id"`
			} `json:"header"`
			Payload struct {
				Input map[string]any `json:"input"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(data, &f2); err != nil {
			return
		}
		reply := func(event string) error {
			return conn.WriteJSON(map[string]any{
				"header":  map[string]any{"event": event, "task_id": f2.Header.TaskID},
				"payload": map[string]any{},
			})
		}
		switch f2.Header.Action {
		case "run-task":
			if err := reply("task-started"); err != nil {
				return
			}
		case "continue-task":
			f.mu.Lock()
			f.bodies = append(f.bodies, string(data))
			f.mu.Unlock()
			if err := conn.WriteMessage(websocket.BinaryMessage, ttsAudio); err != nil {
				return
			}
		case "finish-task":
			_ = reply("task-finished")
			return
		}
	}
}

func (f *fakeDashScopeNative) seen() (paths, auths, asyncs, bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.authSeen...),
		append([]string(nil), f.asyncSeen...), append([]string(nil), f.bodies...)
}

// generationEnabled turns all three capabilities on against the fake native origin
// and installs the derived handle key, mirroring a production deployment.
func generationEnabled(native string) func(*config.Config) {
	return func(c *config.Config) {
		c.DashScopeNativeBase = strings.TrimRight(native, "/")
		c.ImageEnabled, c.ImageUpstreamModel, c.ImageDailyLimit = true, billing.QwenImage20, 10
		c.SpeechEnabled, c.TTSUpstreamModel, c.TTSDefaultVoice, c.SpeechDailyLimit = true, billing.QwenAudio30TTSFlash, "Cherry", 50_000
		c.VideoEnabled, c.VideoUpstreamModel, c.VideoDailyLimit = true, billing.Wan27T2V, 10
		c.MediaSigningSecret = []byte("e2e-media-signing-secret-32-bytes!!")
		c.VideoHandleKey = domvideo.DeriveKey(c.MediaSigningSecret)
	}
}

func postJSON(t *testing.T, client *http.Client, url, installID, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Anselm-Install-ID", installID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, client *http.Client, url, installID string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Anselm-Install-ID", installID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error envelope: %s", body)
	}
	return e.Error.Code
}

// TestE2EImageAndSpeechGenerateOverTheRealStack drives both synchronous
// generation routes through the production chain and proves the two things that
// only a full-stack run can: the upstream key never reaches the client, and the
// artifact URL is relayed verbatim (P13 URL 直通).
func TestE2EImageAndSpeechGenerateOverTheRealStack(t *testing.T) {
	deepSeek := fakeChatUpstreamNonStream(t, "ok", 10)
	defer deepSeek.Close()
	native := newFakeDashScope(t)
	s := buildStackWithProviders(t, deepSeek.URL, "http://qwen.invalid", generationEnabled(native.srv.URL))
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	code, body := postJSON(t, client, srv.URL+"/v1/images/generations", installID, `{"prompt":"a cat","size":"1024x1024"}`)
	if code != http.StatusOK || !strings.Contains(string(body), "https://oss.example/i.png") {
		t.Fatalf("image generate → %d %s", code, body)
	}
	// Speech answers with the AUDIO ITSELF, not a URL to it (H9: the model is duplex-WebSocket only,
	// so there is no artifact URL in existence to relay). Asserting the exact bytes is the point —
	// a response that merely 200s could be an empty body, and an empty body is what a client hears
	// as silence.
	// 语音答的是**音频本身**、不是指向它的 URL(H9:那个模型只有双工 WebSocket,故世上根本不存在一个
	// 产物 URL 可以中继)。断言**逐字节相同**才是要点——一个只是 200 的响应可能是空 body,而空 body
	// 在客户端听起来就是一片寂静。
	code, body = postJSON(t, client, srv.URL+"/v1/audio/speech", installID, `{"input":"hello there"}`)
	if code != http.StatusOK || !bytes.Equal(body, ttsAudio) {
		t.Fatalf("speech generate → %d %q, want the upstream's own bytes", code, body)
	}

	paths, auths, _, _ := native.seen()
	if len(paths) != 2 {
		t.Fatalf("upstream calls = %v, want two", paths)
	}
	for _, a := range auths {
		// EqualFold, not ==: the auth SCHEME is case-insensitive (RFC 7235 §2.1) and the two
		// transports spell it differently — the HTTP generation path sends `Bearer`, the duplex
		// socket sends `bearer`, which is the spelling DashScope's own WebSocket examples use and
		// the one proven against the real API. What must be identical is the KEY, and that is what
		// this loop is here to check.
		// 用 EqualFold 而非 ==:鉴权**方案**大小写不敏感(RFC 7235 §2.1),而两种传输拼法不同——HTTP
		// 生成路径发 `Bearer`,双工套接字发 `bearer`(那是 DashScope 自己 WebSocket 示例的拼法,也是
		// 在真 API 上验过的那个)。必须逐字相同的是**那把 key**,而这正是本循环要查的东西。
		if !strings.EqualFold(a, "Bearer qwen-test-key-never-leaks") {
			t.Fatalf("upstream auth = %q", a)
		}
	}
	// The key must not appear in ANY client-visible byte (GW-INV-51).
	// key 不得出现在**任何**客户端可见字节里(GW-INV-51)。
	if strings.Contains(string(body), "qwen-test-key-never-leaks") {
		t.Fatalf("upstream key leaked to the client: %s", body)
	}
}

// TestE2EVideoSubmitPollAndOwnership is the H1 contract end to end: 202 + a
// signed handle, polling that reports phases without moving money, and a second
// install being unable to read the first one's task.
func TestE2EVideoSubmitPollAndOwnership(t *testing.T) {
	deepSeek := fakeChatUpstreamNonStream(t, "ok", 10)
	defer deepSeek.Close()
	native := newFakeDashScope(t)
	s := buildStackWithProviders(t, deepSeek.URL, "http://qwen.invalid", generationEnabled(native.srv.URL))
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	code, body := postJSON(t, client, srv.URL+"/v1/videos/generations", installID, `{"prompt":"a cat walking","seconds":5}`)
	if code != http.StatusAccepted {
		t.Fatalf("video submit → %d %s (want 202)", code, body)
	}
	var submitted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &submitted); err != nil || submitted.Status != "pending" || submitted.Object != "video.generation" {
		t.Fatalf("submit body = %s (%v)", body, err)
	}
	// The handle must NOT be the raw upstream task id — that is the whole point.
	// 句柄**不得**是裸的上游 task id——这正是全部的意义所在。
	if submitted.ID == "task-e2e-1" || strings.Contains(submitted.ID, "task-e2e-1") {
		t.Fatalf("raw upstream task id handed to the client: %q", submitted.ID)
	}
	// The async header is mandatory on submit and must NOT appear on the poll.
	// 异步头在提交时是强制的,在轮询时**不得**出现。
	_, _, asyncs, _ := native.seen()
	if len(asyncs) != 1 || asyncs[0] != "enable" {
		t.Fatalf("submit async header = %v, want [enable]", asyncs)
	}

	code, body = getJSON(t, client, srv.URL+"/v1/videos/"+submitted.ID, installID)
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"running"`) {
		t.Fatalf("poll(running) → %d %s", code, body)
	}
	if strings.Contains(string(body), "oss.example") {
		t.Fatalf("a running task must not carry an artifact url: %s", body)
	}
	native.videoDone.Store(true)
	code, body = getJSON(t, client, srv.URL+"/v1/videos/"+submitted.ID, installID)
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"succeeded"`) ||
		!strings.Contains(string(body), "https://oss.example/v.mp4") {
		t.Fatalf("poll(succeeded) → %d %s", code, body)
	}
	_, _, asyncs, _ = native.seen()
	for i, a := range asyncs[1:] {
		if a != "" {
			t.Fatalf("poll %d carried the async header %q", i, a)
		}
	}

	// A SECOND install holding the first one's handle verbatim must be refused,
	// and the refusal must not be distinguishable from a forgotten task.
	// **第二个** install 拿着第一个的句柄原文必须被拒,且该拒绝与「任务已忘掉」不可区分。
	otherClient := newProofClient(t, srv.Client())
	otherID := registerInstall(t, srv, otherClient)
	code, body = getJSON(t, otherClient, srv.URL+"/v1/videos/"+submitted.ID, otherID)
	if code != http.StatusNotFound || errorCode(t, body) != "VIDEO_TASK_NOT_FOUND" {
		t.Fatalf("cross-install poll → %d %s, want 404 VIDEO_TASK_NOT_FOUND", code, body)
	}
	code, body = getJSON(t, otherClient, srv.URL+"/v1/videos/bm9wZQ.bm9wZQ", otherID)
	if code != http.StatusNotFound || errorCode(t, body) != "VIDEO_TASK_NOT_FOUND" {
		t.Fatalf("garbage handle → %d %s", code, body)
	}
}

// TestE2ECategoryLedgersAreIndependent is the guard the SPEECH_DAILY_LIMIT bug
// would have failed: it exhausts ONE category over the socket and proves the
// other two still work, each denying with its OWN wire code.
//
// 这条守卫正是 SPEECH_DAILY_LIMIT 那个 bug 会挂在上面的:它经真 socket 耗尽**一个**品类,并证明另外
// 两个照常工作、且各自以**自己的** wire 码拒绝。
func TestE2ECategoryLedgersAreIndependent(t *testing.T) {
	deepSeek := fakeChatUpstreamNonStream(t, "ok", 10)
	defer deepSeek.Close()
	native := newFakeDashScope(t)
	// Chat resolves to the multimodal model now, so the chat fake must sit in the QWEN slot — the
	// final assertion here is "plain chat still works", and it can only mean that if chat has a
	// reachable upstream at all. 现在 chat 解析到多模态模型,故 chat 假件必须坐在 **qwen** 槽上——本
	// 用例最后那条断言是「普通 chat 照常」,而只有 chat 真有一个够得着的上游时它才可能有这个含义。
	s := buildStackWith(t, deepSeek.URL, func(c *config.Config) {
		generationEnabled(native.srv.URL)(c)
		// One of each: the smallest cap that still admits exactly one call.
		// 各留一发:恰好放行一次调用的最小上限。
		c.ImageDailyLimit = 1
		c.SpeechDailyLimit = 11 // "hello there" is 11 runes
		c.VideoDailyLimit = 1
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	for _, tc := range []struct {
		name, url, body, wantCode string
	}{
		{"image", "/v1/images/generations", `{"prompt":"a cat"}`, "IMAGE_QUOTA_EXHAUSTED"},
		{"speech", "/v1/audio/speech", `{"input":"hello there"}`, "TTS_QUOTA_EXHAUSTED"},
		{"video", "/v1/videos/generations", `{"prompt":"a cat","seconds":5}`, "VIDEO_QUOTA_EXHAUSTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// First call consumes the whole cap.
			if code, body := postJSON(t, client, srv.URL+tc.url, installID, tc.body); code >= 400 {
				t.Fatalf("first %s call → %d %s", tc.name, code, body)
			}
			// Second is denied — with this category's own code, not a neighbour's.
			code, body := postJSON(t, client, srv.URL+tc.url, installID, tc.body)
			if code != http.StatusTooManyRequests || errorCode(t, body) != tc.wantCode {
				t.Fatalf("second %s call → %d %s, want 429 %s", tc.name, code, body, tc.wantCode)
			}
		})
	}

	// Every category is now exhausted but the WALLET is untouched: plain chat must
	// still work. A shared counter would have killed it.
	// 三个品类都已耗尽,但**钱包**没被动:普通 chat 必须照常。共用计数器会把它一起弄死。
	code, body := postJSON(t, client, srv.URL+"/v1/chat/completions", installID,
		`{"model":"anselm-auto","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("chat after category exhaustion → %d %s", code, body)
	}
}

// TestE2ECapabilitiesOffDegradeHonestly proves each capability answers with its
// OWN unavailable code when its switch is off, and that turning one off does not
// take the others down.
func TestE2ECapabilitiesOffDegradeHonestly(t *testing.T) {
	deepSeek := fakeChatUpstreamNonStream(t, "ok", 10)
	defer deepSeek.Close()
	native := newFakeDashScope(t)
	s := buildStackWithProviders(t, deepSeek.URL, "http://qwen.invalid", func(c *config.Config) {
		generationEnabled(native.srv.URL)(c)
		c.ImageEnabled = false
		// Video keeps its switch but loses the handle key — the third half.
		// 视频开关还在,但丢了句柄密钥——那第三半。
		c.VideoHandleKey = nil
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	code, body := postJSON(t, client, srv.URL+"/v1/images/generations", installID, `{"prompt":"a cat"}`)
	if code != http.StatusServiceUnavailable || errorCode(t, body) != "IMAGE_UNAVAILABLE" {
		t.Fatalf("image off → %d %s", code, body)
	}
	code, body = postJSON(t, client, srv.URL+"/v1/videos/generations", installID, `{"prompt":"a cat"}`)
	if code != http.StatusServiceUnavailable || errorCode(t, body) != "VIDEO_UNAVAILABLE" {
		t.Fatalf("video without a handle key → %d %s", code, body)
	}
	// Speech is untouched and must still work.
	if code, body := postJSON(t, client, srv.URL+"/v1/audio/speech", installID, `{"input":"hi"}`); code != http.StatusOK {
		t.Fatalf("speech should be unaffected → %d %s", code, body)
	}
	// And nothing was spent on the two refused routes.
	if paths, _, _, _ := native.seen(); len(paths) != 1 {
		t.Fatalf("refused capabilities reached upstream: %v", paths)
	}
}

// TestE2EMixedModalitiesInOneRequest answers the question directly: can ONE chat
// request carry more than one KIND of media? It sends an image and a video part
// in the same user message and proves both survive to the upstream verbatim.
//
// This is structural, not incidental: the gateway's media admission is per-PART
// (each part is validated and counted on its own) with no rule anywhere that a
// message may only contain one modality. Until this test existed that was an
// unverified belief.
//
// 本测试直接回答那个问题:**一次** chat 请求能不能带**多种**媒体?它在同一条 user 消息里送一张图与
// 一段视频,并证明两者都原样活到上游。
//
// 这是**结构性**的、不是碰巧:网关的媒体准入是**逐 part** 的(每个 part 各自校验、各自计数),
// 任何地方都没有「一条消息只能有一种模态」的规则。在这个测试存在之前,那只是一个未经验证的信念。
func TestE2EMixedModalitiesInOneRequest(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody string
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+billing.Qwen37Plus+`","choices":[{"message":{"role":"assistant","content":"both seen"}}],`+
			`"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	}))
	defer qwen.Close()
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer deepSeek.Close()

	s := buildStackWithProviders(t, deepSeek.URL, qwen.URL, nil)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())
	installID := registerInstall(t, srv, client)

	png := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	mp4 := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'})
	body := `{"model":"anselm-auto","stream":false,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"compare these"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}},` +
		`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,` + mp4 + `"}}` +
		`]}]}`
	code, out := postJSON(t, client, srv.URL+"/v1/chat/completions", installID, body)
	if code != http.StatusOK {
		t.Fatalf("mixed-modality chat → %d %s", code, out)
	}
	mu.Lock()
	got := upstreamBody
	mu.Unlock()
	for _, want := range []string{`"type":"text"`, `"type":"image_url"`, `"type":"video_url"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("upstream payload lost %s: %s", want, got)
		}
	}
}

// TestE2EDebugModeOpensTheQuotaGateButStillBills is TestE2EQuotaExhaustion with
// GATEWAY_MODE=debug: the SAME MONTHLY_QUOTA=1 that denies the second request in
// production must not deny it here, because Provider.Load() hands the reserve gate
// the masked ceiling. It then asserts the other half of the contract over the real
// store — spend_ledger still moved for BOTH requests. debug means "never denied",
// never "never counted"; a debug mode that also stopped billing would make the
// operator's own spend view lie to them for the entire development phase.
//
// 本用例是 TestE2EQuotaExhaustion 的 debug 版:同一个 MONTHLY_QUOTA=1,在 production 下拒第二条,
// 在这里必须放行(Provider.Load() 交给 reserve 闸的是掩码后的天花板)。随后经真实 store 断言契约的
// 另一半——两条请求都照样进了账。debug 是「永不拒绝」,绝不是「永不记账」:一个顺手停掉记账的
// debug 模式,会让运营者自己的花费视图在整个开发期都在骗他。
func TestE2EDebugModeOpensTheQuotaGateButStillBills(t *testing.T) {
	up, _ := fakeChatUpstream(t)
	s := buildStackWith(t, up.URL, func(c *config.Config) {
		c.MonthlyQuota = 1
		c.RuntimeMode = config.RuntimeModeDebug
	})
	srv := httptest.NewServer(s.handler)
	defer srv.Close()
	client := newProofClient(t, srv.Client())

	installID := registerInstall(t, srv, client)
	// The wire code travels with the status, because a bare "got 503" cannot tell a masked gate from
	// an absent provider — and this test exists to distinguish exactly that kind of thing.
	// 线缆码随状态一起带出来:光一句「got 503」分不清「闸被掩了」与「供应商根本不在」,而本用例存在
	// 的意义正是分辨这一类差别。
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("X-Anselm-Install-ID", installID)
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

	if code, wire := chat(); code != http.StatusOK {
		t.Fatalf("first chat want 200 got %d %q", code, wire)
	}
	// The one that 429s under production.
	if code, wire := chat(); code != http.StatusOK {
		t.Fatalf("debug mode must not deny past MONTHLY_QUOTA: got %d %q", code, wire)
	}

	s.bgWG.Wait() // let the detached settles land before reading the ledger.
	// The period MUST be snapshotted in the stack's own RESET_TZ: reading the
	// ledger in UTC lands on the previous day's row for 8 hours out of every 24
	// and would report a spend of 0 that never happened (GW-INV-05's rule, applied
	// to the reader). 必须按栈自己的 RESET_TZ 取周期,否则每天有 8 小时会读到前一天的行。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	p := domquota.SnapshotPeriod(time.Now(), loc)
	used, installSpend, globalSpend, err := s.qstore.View(context.Background(), installID, p)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if used != 2 {
		t.Fatalf("both requests must be counted, used = %d", used)
	}
	if installSpend <= 0 || globalSpend <= 0 {
		t.Fatalf("debug must still bill: installSpend=%d globalSpend=%d", installSpend, globalSpend)
	}
}
