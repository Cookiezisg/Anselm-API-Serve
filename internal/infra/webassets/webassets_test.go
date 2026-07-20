package webassets

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSPAFallbackServesIndex: any non-/static GET returns index.html as html so
// the client hash-router owns the path.
func TestSPAFallbackServesIndex(t *testing.T) {
	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/overview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), "<!doctype html") && !strings.Contains(rr.Body.String(), "<!DOCTYPE html") {
		t.Fatalf("SPA fallback did not serve index.html: %.80q", rr.Body.String())
	}
}

// TestStaticServesBundleWithContentType: the built JS bundle is served from
// /static/assets/<hash>.js with an explicit text/javascript type (nosniff-safe).
func TestStaticServesBundleWithContentType(t *testing.T) {
	// Discover the hashed bundle name from the embedded dist.
	entries, err := webFS.ReadDir("ui/dist/assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	var js string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			js = e.Name()
			break
		}
	}
	if js == "" {
		t.Fatal("no .js bundle in embedded ui/dist/assets")
	}
	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/assets/"+js, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("content-type = %q, want text/javascript", ct)
	}
	b, _ := io.ReadAll(rr.Body)
	if len(b) == 0 {
		t.Fatal("empty JS bundle served")
	}
}

// TestEmbeddedDashboardShowsBothProviderStates is a feature-level smoke guard
// over the exact bundle Go embeds. It catches a source/dist mismatch that would
// leave operators with the old ambiguous aggregate breaker card even though the
// overview API exposes provider-specific state.
func TestEmbeddedDashboardShowsBothProviderStates(t *testing.T) {
	entries, err := webFS.ReadDir("ui/dist/assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	var bundle strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		b, err := webFS.ReadFile("ui/dist/assets/" + entry.Name())
		if err != nil {
			t.Fatalf("read embedded JS %q: %v", entry.Name(), err)
		}
		bundle.Write(b)
	}
	if bundle.Len() == 0 {
		t.Fatal("no embedded JS bundle")
	}
	bundleText := bundle.String()
	for _, label := range []string{"DeepSeek", "Kimi", "已配置", "未配置", "BREAKER N/A", "BREAKER OPEN", "BREAKER CLOSED"} {
		if !strings.Contains(bundleText, label) {
			t.Fatalf("embedded dashboard bundle is missing provider-state label %q", label)
		}
	}
}

// TestStaticTraversalFallsBackToSPA: a path traversal or unknown asset never
// escapes the embed and never 404s — it renders the SPA shell.
func TestStaticTraversalFallsBackToSPA(t *testing.T) {
	for _, p := range []string{"/static/../webassets.go", "/static/nope.js"} {
		rr := httptest.NewRecorder()
		Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: expected SPA html fallback, got %d %q", p, rr.Code, rr.Header().Get("Content-Type"))
		}
	}
}
