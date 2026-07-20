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

func probeConfig(deepSeekURL string) config.Config {
	return config.Config{
		DeepSeekBaseURL:   deepSeekURL,
		DeepSeekAPIKeys:   []string{"ds-key"},
		TextUpstreamModel: billing.DeepSeekV4Flash,
	}
}

func TestUpstreamProberAuthenticatesAndRequiresEveryConfiguredModel(t *testing.T) {
	deepSeek := modelServer(t, "ds-key", billing.DeepSeekV4Flash)
	t.Cleanup(deepSeek.Close)
	gemini := modelServer(t, "gem-key", billing.Gemini31FlashLite)
	t.Cleanup(gemini.Close)

	cfg := probeConfig(deepSeek.URL)
	cfg.GeminiBaseURL = gemini.URL
	cfg.GeminiAPIKeys = []string{"gem-key"}
	cfg.MultimodalUpstreamModel = billing.Gemini31FlashLite
	p := newUpstreamProber(cfg)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); !ok {
		t.Fatal("both authenticated provider/model probes should make readiness fresh")
	}

	bad := cfg
	bad.GeminiAPIKeys = []string{"wrong-key"}
	p = newUpstreamProber(bad)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); ok {
		t.Fatal("a configured Gemini auth failure must keep aggregate readiness cold")
	}

	bad = cfg
	bad.MultimodalUpstreamModel = "model-not-advertised"
	p = newUpstreamProber(bad)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); ok {
		t.Fatal("a configured but unavailable Gemini model must fail readiness")
	}
}

func TestUpstreamProberAllowsTextOnlyDeploymentAndSiblingKey(t *testing.T) {
	deepSeek := modelServer(t, "working-key", billing.DeepSeekV4Flash)
	t.Cleanup(deepSeek.Close)
	cfg := probeConfig(deepSeek.URL)
	cfg.DeepSeekAPIKeys = []string{"expired-key", "working-key"}

	p := newUpstreamProber(cfg)
	p.probe(context.Background())
	if ok, _ := p.LastOK(); !ok {
		t.Fatal("one working DeepSeek key should make a Gemini-disabled deployment ready")
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
