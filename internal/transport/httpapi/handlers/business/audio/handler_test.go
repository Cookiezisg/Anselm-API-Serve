package audio

import (
	"context"
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
		SpeechEnabled: true, TTSUpstreamModel: billing.QwenAudio30TTSFlash,
		TTSDefaultVoice: "longanhuan_v3.6", QwenAPIKeys: []string{"k"},
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

// fakeAudio stands in for a synthesized utterance — bytes, because that is what a duplex stream
// hands back. 代表一段合成出来的话——**字节**,因为双工流回来的就是字节。
var fakeAudio = []byte("RIFF....WAVEfake-pcm")

type okUpstream struct{ voiceSeen *string }

func (u okUpstream) GenerateSpeech(_ context.Context, _, _, voice string) ([]byte, bool, error) {
	if u.voiceSeen != nil {
		*u.voiceSeen = voice
	}
	return fakeAudio, false, nil
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

// TestHandler_SuccessIsRawAudio: the body IS the audio (OpenAI's own shape for this endpoint), and
// an omitted voice reaches the upstream as the configured default.
//
// The envelope changed in H9 because the upstream did: a duplex WebSocket streams frames, so there
// is no artifact URL for P13's passthrough to relay. Asserting the content type matters as much as
// asserting the bytes — a client that receives WAV bytes labeled `application/json` cannot play
// them without sniffing.
//
// TestHandler_SuccessIsRawAudio:响应体**就是**音频(OpenAI 在这条端点上自己的形状),且省略 voice
// 时抵达上游的是配置的默认音色。
//
// 信封在 H9 变了是因为上游变了:双工 WebSocket 流的是帧,没有产物 URL 可供 P13 直通。**断言
// content type 与断言字节同样要紧**——一个收到被标成 `application/json` 的 WAV 字节的客户端,不去
// 嗅探就播不了。
func TestHandler_SuccessIsRawAudio(t *testing.T) {
	var voice string
	w := post(t, testHandler(&voice), `{"input":"你好，世界"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav", got)
	}
	if got := w.Body.String(); got != string(fakeAudio) {
		t.Fatalf("body = %q, want the synthesized bytes", got)
	}
	if voice != "longanhuan_v3.6" {
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
		"missingInput":   `{"voice":"longanhuan_v3.6"}`,
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
