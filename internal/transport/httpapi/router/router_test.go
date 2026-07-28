package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoHandler writes the given label so a test can assert which route served the
// request (proving the method+path table dispatches correctly).
func echoHandler(label string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(label))
	})
}

// stubWrapper records the labels it wraps so the test proves each business route
// (and ONLY those) gets a RED label, and /healthz does not.
type stubWrapper struct{ labels []string }

func (s *stubWrapper) Wrap(label string, h http.Handler) http.Handler {
	s.labels = append(s.labels, label)
	return h
}

func testRoutes() routes {
	return routes{
		install:        echoHandler("install"),
		challenge:      echoHandler("challenge"),
		proofChallenge: echoHandler("proof_challenge"),
		chat:           echoHandler("chat"),
		quota:          echoHandler("quota"),
		models:         echoHandler("models"),
		speechASR:      echoHandler("speech_asr"),
		imagesGenerate: echoHandler("images_generate"),
		imagesEdit:     echoHandler("images_edit"),
		videosAnimate:  echoHandler("videos_animate"),
		audioSpeech:    echoHandler("audio_speech"),
		voiceEnroll:    echoHandler("voice_enroll"),
		voiceList:      echoHandler("voice_list"),
		voiceDelete:    echoHandler("voice_delete"),
		videosGenerate: echoHandler("videos_generate"),
		videoStatus:    echoHandler("video_status"),
		mediaCreate:    echoHandler("media_create"),
		mediaStatus:    echoHandler("media_status"),
		mediaCancel:    echoHandler("media_cancel"),
		mediaAppend:    echoHandler("media_append"),
		mediaComplete:  echoHandler("media_complete"),
		mediaFetch:     echoHandler("media_fetch"),
		healthz:        echoHandler("healthz"),
	}
}

func TestRouter_DispatchAndLabels(t *testing.T) {
	mx := &stubWrapper{}
	h := assemble(testRoutes(), mx, nil, 0)

	cases := []struct {
		method, path, want string
	}{
		{"POST", "/v1/install", "install"},
		{"GET", "/v1/install/challenge", "challenge"},
		{"GET", "/v1/proof/challenge", "proof_challenge"},
		{"POST", "/v1/chat/completions", "chat"},
		{"GET", "/v1/quota", "quota"},
		{"GET", "/v1/models", "models"},
		{"GET", "/v1/speech/asr", "speech_asr"},
		{"POST", "/v1/images/generations", "images_generate"},
		{"POST", "/v1/images/edits", "images_edit"},
		{"POST", "/v1/videos/animations", "videos_animate"},
		{"POST", "/v1/audio/speech", "audio_speech"},
		{"POST", "/v1/voices", "voice_enroll"},
		{"GET", "/v1/voices", "voice_list"},
		// The `:action` suffix must dispatch as its own pattern, not get swallowed by /v1/voices.
		// `:action` 后缀必须作为**自己的**模式派发,而不是被 /v1/voices 吞掉。
		{"POST", "/v1/voices:delete", "voice_delete"},
		{"POST", "/v1/videos/generations", "videos_generate"},
		{"GET", "/v1/videos/abc.def", "video_status"},
		{"POST", "/v1/media/uploads", "media_create"},
		{"GET", "/v1/media/uploads/mup_test", "media_status"},
		{"DELETE", "/v1/media/uploads/mup_test", "media_cancel"},
		{"PUT", "/v1/media/uploads/mup_test", "media_append"},
		{"POST", "/v1/media/uploads/mup_test/complete", "media_complete"},
		{"GET", "/v1/media/leases/mls_test/content", "media_fetch"},
		{"GET", "/healthz", "healthz"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 200 || rec.Body.String() != c.want {
			t.Fatalf("%s %s → code=%d body=%q want %q", c.method, c.path, rec.Code, rec.Body.String(), c.want)
		}
	}

	// Exactly the five business routes are RED-labeled; /healthz is NOT.
	want := []string{"install", "install_challenge", "proof_challenge", "chat_completions", "quota", "models", "speech_asr", "images_generate", "images_edit", "audio_speech", "voice_enroll", "voice_list", "voice_delete", "videos_generate", "videos_animate", "video_status", "media_create", "media_status", "media_cancel", "media_append", "media_complete", "media_fetch"}
	if strings.Join(mx.labels, ",") != strings.Join(want, ",") {
		t.Fatalf("RED labels = %v, want %v", mx.labels, want)
	}
}

func TestRouter_RequestIDEchoed(t *testing.T) {
	h := assemble(testRoutes(), nil, nil, 0)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("X-Request-ID not echoed: %q", got)
	}
}

func TestRouter_CORSDenied(t *testing.T) {
	h := assemble(testRoutes(), nil, nil, 0)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin must be 403, got %d", rec.Code)
	}
}

func TestRouter_NotFound(t *testing.T) {
	h := assemble(testRoutes(), nil, nil, 0)
	r := httptest.NewRequest("GET", "/v1/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path must be 404, got %d", rec.Code)
	}
}

func TestRouter_MethodNotAllowed(t *testing.T) {
	// The Go 1.22 mux returns 405 for a known path with a wrong method.
	h := assemble(testRoutes(), nil, nil, 0)
	r := httptest.NewRequest("GET", "/v1/install", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method on /v1/install must be 405, got %d", rec.Code)
	}
}

func TestRouter_PanicRecovered(t *testing.T) {
	rt := testRoutes()
	rt.quota = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	var panics int
	h := assemble(rt, nil, counterFunc(func() { panics++ }), 0)

	r := httptest.NewRequest("GET", "/v1/quota", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic must render 500, got %d", rec.Code)
	}
	if panics != 1 {
		t.Fatalf("panic counter = %d, want 1", panics)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("must not leak panic value: %s", rec.Body.String())
	}
}

// counterFunc adapts a func into the PanicCounter port for the panic test.
type counterFunc func()

func (f counterFunc) Inc() { f() }
