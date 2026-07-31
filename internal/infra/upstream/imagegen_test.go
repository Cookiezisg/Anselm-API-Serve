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

// TestImageGen_Busy429: an upstream 429 is UPSTREAM_BUSY and REFUNDABLE.
//
// This test previously asserted only the sentinel, and its comment rationalized the charge with
// "the chat fault taxonomy keeps 429 out of rollback-eligible classes". That was wrong about this
// very repository: the chat path classifies 429 as DefinitelyUnbilled (see backend_test.go). What
// 429 is excluded from is the BREAKER fault set — a different axis, as architecture.md §4 spells
// out. Conflating "not a fault" with "not refundable" made users pay for being rate-limited.
//
// 一次上游 429 是 UPSTREAM_BUSY 且**可退**。
//
// 本测试此前只断言了那个 sentinel,而它的注释用「chat 的故障分类把 429 排除在可回滚之外」来为
// 扣款辩护。那句话**记错了本仓自己的代码**:chat 路径把 429 判为 DefinitelyUnbilled(见
// backend_test.go)。429 被排除的是**熔断器**故障集——那是另一条轴,architecture.md §4 写得很清楚。
// 把「不算故障」当成「不可退」,结果是**用户为被限流付费**。
func TestImageGen_Busy429(t *testing.T) {
	srv := fakeDashScope(t, http.StatusTooManyRequests, `{}`)
	defer srv.Close()
	g := NewImageGen(srv.URL, "sk")
	_, unbilled, err := g.GenerateImage(context.Background(), "m", "p", "1024x1024")
	var ae *apierr.APIError
	if !asAPIErr(err, &ae) || ae.Code != "UPSTREAM_BUSY" {
		t.Fatalf("err = %v, want UPSTREAM_BUSY", err)
	}
	if !unbilled {
		t.Fatal("a 429 never reached generation, so it must be refundable")
	}
}

// TestImageGen_MalformedArtifactURLRejected: a non-https or relative artifact URL never
// reaches the client (the gateway relays only well-formed absolute https URLs).
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
