package audio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apptts "github.com/sunweilin/anselm/gateway/internal/app/tts"
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
	return &config.Config{
		SpeechEnabled: true, TTSUpstreamModel: billing.Qwen3TTSFlash,
		TTSDefaultVoice: "Cherry", QwenAPIKeys: []string{"k"},
	}
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

type okUpstream struct{ voiceSeen *string }

func (u okUpstream) GenerateSpeech(_ context.Context, _, _, voice string) (string, bool, error) {
	if u.voiceSeen != nil {
		*u.voiceSeen = voice
	}
	return "https://oss.example/a.wav", false, nil
}

type nowClock struct{}

func (nowClock) Now() time.Time { return time.Unix(1_800_000_000, 0) }

func testHandler(voiceSeen *string) *Handler {
	return New(apptts.New(apptts.Deps{
		Auth: okAuth{}, Quota: okQuota{}, Config: okCfg{},
		Upstream: okUpstream{voiceSeen: voiceSeen}, Clock: nowClock{},
	}))
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.Header.Set(proofhttp.HeaderInstallID, "ins_1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestHandler_SuccessEnvelope: the url envelope (the images shape, not OpenAI's raw audio body —
// P13 makes URL passthrough the whole media contract), and an omitted voice reaches the upstream
// as the configured default.
func TestHandler_SuccessEnvelope(t *testing.T) {
	var voice string
	w := post(t, testHandler(&voice), `{"input":"你好，世界"}`)
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
	if resp.Created == 0 || len(resp.Data) != 1 || resp.Data[0].URL != "https://oss.example/a.wav" {
		t.Fatalf("envelope = %+v", resp)
	}
	if voice != "Cherry" {
		t.Fatalf("upstream voice = %q, want the configured default", voice)
	}
}

// TestHandler_ClosedShapeRejections: every invalid-shape axis dies as BAD_REQUEST before any
// money path runs. `format` is in this list on purpose: the upstream always answers WAV, so
// accepting the field would be a promise it cannot keep (代拍 C3) — DisallowUnknownFields is
// what makes that refusal explicit rather than a silently ignored parameter.
//
// `format` 刻意在这张表里:上游恒返 WAV,收下这个字段就是许一个它兑现不了的诺(代拍 C3)——
// DisallowUnknownFields 让这次拒绝是明说的,而不是一个被静默忽略的参数。
func TestHandler_ClosedShapeRejections(t *testing.T) {
	long := strings.Repeat("好", maxInputChars+1)
	cases := map[string]string{
		"badJSON":        `{`,
		"unknownField":   `{"input":"hi","speed":1.2}`,
		"formatRejected": `{"input":"hi","format":"mp3"}`,
		"emptyInput":     `{"input":"   "}`,
		"missingInput":   `{"voice":"Cherry"}`,
		"oversizedInput": `{"input":"` + long + `"}`,
		"oversizedVoice": `{"input":"hi","voice":"` + strings.Repeat("v", maxVoiceChars+1) + `"}`,
	}
	for name, body := range cases {
		w := post(t, testHandler(nil), body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", name, w.Code, w.Body.String())
		}
	}
}

// TestHandler_InputBoundIsRunesNotBytes: the length gate counts characters. A byte-counting gate
// would reject a perfectly legal 200-character Chinese message while accepting a 500-character
// English one — the cap has to mean the same thing in every language it will actually see.
//
// 长度闸按**字符**计。按字节的闸会拒掉一条完全合法的 200 字中文,却放行 500 字英文——这个上限
// 必须在它真正会遇到的每种语言里表示同一件事。
func TestHandler_InputBoundIsRunesNotBytes(t *testing.T) {
	// 400 CJK characters = 1200 bytes: well over a byte-based 500 cap, well within the rune cap.
	w := post(t, testHandler(nil), `{"input":"`+strings.Repeat("字", 400)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("400 CJK characters rejected (status %d) — the gate is counting bytes", w.Code)
	}
}

// TestHandler_MethodGuard: anything but POST is refused before the body is read.
func TestHandler_MethodGuard(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/speech", nil)
	w := httptest.NewRecorder()
	testHandler(nil).ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
