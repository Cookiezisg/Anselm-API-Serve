package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func modelServer(t *testing.T, wantKey, modelID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			http.Error(w, "wrong request", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, modelID)
	}))
}

// probeConfig builds the ONE target the prober probes. An unrouted upstream is left out after WRK-082 H9:
// nothing routes there, and probing an unrouted upstream spends a live credential on an answer that
// changes nothing.
//
// probeConfig 构造探测器**会探的那唯一一个**目标。无流量的上游不探:没有流量去那儿,而
// 探测一条无流量上游,是拿一把活凭证去换一个改变不了任何事的答案。
func probeConfig(qwenURL string) config.Config {
	return config.Config{
		QwenBaseURL:             qwenURL,
		QwenAPIKeys:             []string{"qwen-key"},
		MultimodalUpstreamModel: billing.Qwen37Plus,
	}
}

// TestUpstreamProberAuthenticatesTheRoutedModel: the probe must prove BOTH that the credential
// works and that the exact configured model is advertised — a 200 from /models with the wrong
// catalog is not readiness.
//
// TestUpstreamProberAuthenticatesTheRoutedModel:探测必须同时证明**凭证能用**与**那个精确的模型被
// 宣称**——/models 回 200 但目录里没有那个模型,不算就绪。
func TestUpstreamProberAuthenticatesTheRoutedModel(t *testing.T) {
	qwen := modelServer(t, "qwen-key", billing.Qwen37Plus)
	t.Cleanup(qwen.Close)

	p := newUpstreamProber(probeConfig(qwen.URL))
	p.probe(context.Background())
	if ok, _ := p.LastOK(); !ok {
		t.Fatal("an authenticated provider/model probe should make readiness fresh")
	}

	bad := probeConfig(qwen.URL)
	bad.QwenAPIKeys = []string{"wrong-key"}
	p = newUpstreamProber(bad)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); ok {
		t.Fatal("an auth failure must keep readiness cold")
	}

	bad = probeConfig(qwen.URL)
	bad.MultimodalUpstreamModel = "model-not-advertised"
	p = newUpstreamProber(bad)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); ok {
		t.Fatal("a configured but unadvertised model must fail readiness")
	}
}

// TestUpstreamProberTriesEveryKeyBeforeFailing: the sibling-key property is what this test was
// really about — one expired key among several must not sink readiness. Its old framing ("a
// Qwen-disabled, text-only deployment") is gone: after WRK-082 H9 every chat request routes to
// the one model, so there is no separate text-only shape to probe.
//
// TestUpstreamProberTriesEveryKeyBeforeFailing:这个测试真正在意的是**兄弟 key**那条性质——一把过期
// key 夹在几把里,不该把就绪拖下水。它旧的说法(「一个关掉 Qwen 的纯文本部署」)已经不存在了:H9 之后
// 每次 chat 都路由到同一个模型,故没有单独的纯文本形态需要探测。
func TestUpstreamProberTriesEveryKeyBeforeFailing(t *testing.T) {
	qwen := modelServer(t, "working-key", billing.Qwen37Plus)
	t.Cleanup(qwen.Close)
	cfg := probeConfig(qwen.URL)
	cfg.QwenAPIKeys = []string{"expired-key", "working-key"}

	p := newUpstreamProber(cfg)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); !ok {
		t.Fatal("one working key among several must make the deployment ready")
	}
}

func TestUpstreamProberNeverFollowsCredentialBearingRedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked to redirect target")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	p := newUpstreamProber(probeConfig(source.URL))
	p.probe(context.Background())
	if ok, _ := p.LastOK(); ok {
		t.Fatal("redirect response must not count as a provider/model success")
	}
	if redirected.Load() {
		t.Fatal("readiness client followed a provider redirect")
	}
}

func TestProbeBearerTransportInjectsOnlyOnClone(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://models.example/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := probeBearerTransport{
		apiKey: "probe-secret",
		base: roundTripFunc(func(got *http.Request) (*http.Response, error) {
			if got == req {
				t.Fatal("transport mutated the caller-owned request")
			}
			if auth := got.Header.Get("Authorization"); auth != "Bearer probe-secret" {
				t.Fatalf("Authorization=%q", auth)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
				Header:     make(http.Header),
				Request:    got,
			}, nil
		}),
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Fatalf("caller request retained credential %q", auth)
	}
}
