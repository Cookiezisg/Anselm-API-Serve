package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

func fakeDashScope(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const okBody = `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"image":"https://oss.example.com/a.png?Expires=1&OSSAccessKeyId=TMP&Signature=s"}]}}]},"usage":{"image_count":1},"request_id":"r1"}`

// TestImageGen_SuccessParsesURLAndSpeaksNativeWire: the wire body is the native
// multimodal-generation form (messages + parameters with W*H size and n=1), the key rides only
// the Authorization header, and the artifact URL is relayed verbatim.
func TestImageGen_SuccessParsesURLAndSpeaksNativeWire(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	g := NewImageGen(srv.URL, "sk-secret")
	u, unbilled, err := g.GenerateImage(context.Background(), "qwen-image-2.0", "a cat in a hat", "1024x1024")
	if err != nil || unbilled {
		t.Fatalf("generate: %v unbilled=%v", err, unbilled)
	}
	if !strings.HasPrefix(u, "https://oss.example.com/a.png") {
		t.Fatalf("url = %q", u)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPath != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Fatalf("path = %q", gotPath)
	}
	var wire struct {
		Model string `json:"model"`
		Input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"input"`
		Parameters struct {
			Size string `json:"size"`
			N    int    `json:"n"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("wire decode: %v (%s)", err, gotBody)
	}
	if wire.Model != "qwen-image-2.0" || wire.Parameters.N != 1 || wire.Parameters.Size != "1024*1024" {
		t.Fatalf("wire = %+v, want native form with W*H size and n=1", wire)
	}
	if len(wire.Input.Messages) != 1 || wire.Input.Messages[0].Content[0].Text != "a cat in a hat" {
		t.Fatalf("prompt lost: %+v", wire.Input)
	}
}

// TestImageGen_ExplicitRejectionIsUnbilled: an upstream 400 is a provable pre-generation
// rejection — unbilled=true, sentinel UPSTREAM_REJECTED, and no upstream text in the error.
func TestImageGen_ExplicitRejectionIsUnbilled(t *testing.T) {
	srv := fakeDashScope(t, http.StatusBadRequest, `{"code":"InvalidParameter","message":"SECRET upstream text"}`)
	defer srv.Close()
	g := NewImageGen(srv.URL, "sk-secret")
	_, unbilled, err := g.GenerateImage(context.Background(), "m", "p", "1024x1024")
	if !unbilled {
		t.Fatal("400 must classify as unbilled")
	}
	var ae *apierr.APIError
	if !asAPIErr(err, &ae) || ae.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("err = %v, want UPSTREAM_REJECTED", err)
	}
	if strings.Contains(ae.Message, "SECRET") {
		t.Fatal("upstream text leaked through the error")
	}
}

// TestImageGen_AmbiguousOutcomesKeepCharge: 5xx and a 200-without-artifact both classify as
// billed-ambiguous (unbilled=false) — GW-INV-50's evidence rule at the client layer.
func TestImageGen_AmbiguousOutcomesKeepCharge(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"upstream 500":         {http.StatusInternalServerError, `{}`},
		"200 without artifact": {http.StatusOK, `{"output":{"choices":[]}}`},
	} {
		srv := fakeDashScope(t, tc.status, tc.body)
		g := NewImageGen(srv.URL, "sk")
		_, unbilled, err := g.GenerateImage(context.Background(), "m", "p", "1024x1024")
		srv.Close()
		if err == nil || unbilled {
			t.Errorf("%s: err=%v unbilled=%v, want billed-ambiguous error", name, err, unbilled)
		}
	}
}

// TestImageGen_Busy429: upstream 429 maps to UPSTREAM_BUSY (billed-ambiguous is wrong here —
// but 429 means the provider did not start generating; still, the chat fault taxonomy keeps
// 429 out of rollback-eligible classes, so the conservative charge stands. The sentinel is
// what the client retries on.)
func TestImageGen_Busy429(t *testing.T) {
	srv := fakeDashScope(t, http.StatusTooManyRequests, `{}`)
	defer srv.Close()
	g := NewImageGen(srv.URL, "sk")
	_, _, err := g.GenerateImage(context.Background(), "m", "p", "1024x1024")
	var ae *apierr.APIError
	if !asAPIErr(err, &ae) || ae.Code != "UPSTREAM_BUSY" {
		t.Fatalf("err = %v, want UPSTREAM_BUSY", err)
	}
}

// TestImageGen_MalformedArtifactURLRejected: a non-https or relative artifact URL never
// reaches the client (the gateway relays only well-formed absolute https URLs, P13).
func TestImageGen_MalformedArtifactURLRejected(t *testing.T) {
	bad := `{"output":{"choices":[{"message":{"content":[{"image":"http://insecure.example/a.png"}]}}]}}`
	srv := fakeDashScope(t, http.StatusOK, bad)
	defer srv.Close()
	g := NewImageGen(srv.URL, "sk")
	if _, _, err := g.GenerateImage(context.Background(), "m", "p", "1024x1024"); err == nil {
		t.Fatal("non-https artifact URL must be rejected")
	}
}

func asAPIErr(err error, target **apierr.APIError) bool {
	for err != nil {
		if ae, ok := err.(*apierr.APIError); ok {
			*target = ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
