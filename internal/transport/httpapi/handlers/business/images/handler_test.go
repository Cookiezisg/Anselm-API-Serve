package images

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appimage "github.com/sunweilin/anselm/gateway/internal/app/image"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
)

type okAuth struct{}

func (okAuth) LookupInstall(_ context.Context, id string) (string, dominstall.Status, bool, error) {
	return id, dominstall.StatusActive, id != "", nil
}

type okCfg struct{}

func (okCfg) Load() *config.Config {
	return &config.Config{ImageEnabled: true, ImageUpstreamModel: billing.QwenImage20, QwenAPIKeys: []string{"k"}}
}

type okQuota struct{}

func (okQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-07", Day: "2026-07-27"}
}

func (okQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	return &domquota.Reservation{RequestID: "r", InstallID: installID, Period: p, Plan: plan, ReservedPUSD: plan.ReservedPUSD}, nil
}
func (okQuota) Settle(context.Context, *domquota.Reservation, int64) error { return nil }
func (okQuota) Rollback(context.Context, *domquota.Reservation) error      { return nil }

type okUpstream struct{ sizeSeen *string }

func (u okUpstream) GenerateImage(_ context.Context, _, _, size string) (string, bool, error) {
	if u.sizeSeen != nil {
		*u.sizeSeen = size
	}
	return "https://oss.example/i.png", false, nil
}

func (u okUpstream) EditImage(_ context.Context, _, _, size, _ string) (string, bool, error) {
	if u.sizeSeen != nil {
		*u.sizeSeen = size
	}
	return "https://oss.example/edited.png", false, nil
}

type nowClock struct{}

func (nowClock) Now() time.Time { return time.Unix(1_800_000_000, 0) }

func testHandler(sizeSeen *string) *Handler {
	return New(appimage.New(appimage.Deps{
		Auth: okAuth{}, Quota: okQuota{}, Config: okCfg{},
		Upstream: okUpstream{sizeSeen: sizeSeen}, Clock: nowClock{},
	}))
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set(proofhttp.HeaderInstallID, "ins_1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestHandler_SuccessEnvelope: the OpenAI-form url envelope with the relayed artifact URL,
// and the omitted size defaults to 1024x1024 before it reaches the upstream.
func TestHandler_SuccessEnvelope(t *testing.T) {
	var size string
	w := post(t, testHandler(&size), `{"prompt":"a cat"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Created == 0 || len(resp.Data) != 1 || resp.Data[0].URL != "https://oss.example/i.png" {
		t.Fatalf("envelope = %+v", resp)
	}
	if size != "1024x1024" {
		t.Fatalf("default size = %q, want 1024x1024", size)
	}
}

// TestHandler_ClosedShapeRejections: every invalid-shape axis dies as BAD_REQUEST before any
// money path runs: bad JSON, unknown fields, empty/oversized prompt, n≠1, malformed sizes.
func TestHandler_ClosedShapeRejections(t *testing.T) {
	long := strings.Repeat("字", 2001)
	cases := map[string]string{
		"bad json":        `{`,
		"unknown field":   `{"prompt":"p","style":"anime"}`,
		"empty prompt":    `{"prompt":"  "}`,
		"oversize prompt": `{"prompt":"` + long + `"}`,
		"n=2":             `{"prompt":"p","n":2}`,
		"n=0":             `{"prompt":"p","n":0}`,
		"size garbage":    `{"prompt":"p","size":"huge"}`,
		"size too small":  `{"prompt":"p","size":"256x256"}`,
		"size too large":  `{"prompt":"p","size":"4096x4096"}`,
		"size star form":  `{"prompt":"p","size":"1024*1024"}`,
	}
	for name, body := range cases {
		if w := post(t, testHandler(nil), body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)
	w := httptest.NewRecorder()
	testHandler(nil).ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", w.Code)
	}
}

// TestHandler_SizeEnvelopeAccepts: the documented shapes pass (square, the 2.0-series
// recommended wide/tall forms).
func TestHandler_SizeEnvelopeAccepts(t *testing.T) {
	for _, size := range []string{"1024x1024", "2048x2048", "2688x1536", "1536x2688", "512x512"} {
		w := post(t, testHandler(nil), `{"prompt":"p","size":"`+size+`"}`)
		if w.Code != http.StatusOK {
			t.Errorf("size %s: status = %d body=%s", size, w.Code, w.Body.String())
		}
	}
}
