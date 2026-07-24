package speech

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	appspeech "github.com/sunweilin/anselm/gateway/internal/app/speech"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
)

type fakeSpeechSvc struct {
	up appspeech.Upstream
	ae *apierr.APIError
}

func (f fakeSpeechSvc) Authorize(context.Context, string) *apierr.APIError { return f.ae }
func (f fakeSpeechSvc) Upstream() (appspeech.Upstream, *apierr.APIError)   { return f.up, f.ae }

func TestHandler_ProxiesPCMToQwenRealtimeEvents(t *testing.T) {
	events := make(chan map[string]any, 4)
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer qwen-key" {
			t.Errorf("Authorization = %q", got)
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = c.Close() }()
		for i := 0; i < 3; i++ {
			_, payload, err := c.ReadMessage()
			if err != nil {
				t.Errorf("upstream read: %v", err)
				return
			}
			var evt map[string]any
			if err := json.Unmarshal(payload, &evt); err != nil {
				t.Errorf("event json: %v", err)
				return
			}
			events <- evt
			if evt["type"] == "input_audio_buffer.append" {
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "text": "你", "stash": ""})
			}
			if evt["type"] == "session.finish" {
				_ = c.WriteJSON(map[string]any{"type": "session.finished"})
			}
		}
	}))
	defer upstream.Close()

	h := New(fakeSpeechSvc{up: appspeech.Upstream{
		URL: strings.Replace(upstream.URL, "http://", "ws://", 1), APIKey: "qwen-key",
	}})
	downstream := httptest.NewServer(h)
	defer downstream.Close()

	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(downstream.URL, "http://", "ws://", 1)+"?language=zh",
		http.Header{proofhttp.HeaderInstallID: []string{"ins_1"}})
	if err != nil {
		t.Fatalf("dial downstream: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("pcm")); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}
	if !strings.Contains(string(payload), "input_audio_transcription.delta") {
		t.Fatalf("missing upstream delta: %s", payload)
	}
	if err := conn.WriteJSON(map[string]string{"type": "finish"}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read finished: %v", err)
	}
	if !strings.Contains(string(payload), "session.finished") {
		t.Fatalf("missing finished event: %s", payload)
	}

	first := <-events
	if first["type"] != "session.update" {
		t.Fatalf("first upstream event = %#v", first)
	}
	session := first["session"].(map[string]any)
	if session["input_audio_format"] != "pcm" || session["sample_rate"].(float64) != 16000 {
		t.Fatalf("bad session update = %#v", session)
	}
	appendEvt := <-events
	if appendEvt["type"] != "input_audio_buffer.append" || appendEvt["audio"] != base64.StdEncoding.EncodeToString([]byte("pcm")) {
		t.Fatalf("bad append event = %#v", appendEvt)
	}
	finishEvt := <-events
	if finishEvt["type"] != "session.finish" {
		t.Fatalf("bad finish event = %#v", finishEvt)
	}
}

func TestHandler_CancelClosesWithoutForwardingControl(t *testing.T) {
	events := make(chan map[string]any, 2)
	done := make(chan struct{})
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = c.Close() }()
		defer close(done)
		for {
			_, payload, err := c.ReadMessage()
			if err != nil {
				return
			}
			var evt map[string]any
			if err := json.Unmarshal(payload, &evt); err != nil {
				t.Errorf("event json: %v", err)
				return
			}
			events <- evt
		}
	}))
	defer upstream.Close()

	h := New(fakeSpeechSvc{up: appspeech.Upstream{
		URL: strings.Replace(upstream.URL, "http://", "ws://", 1), APIKey: "qwen-key",
	}})
	downstream := httptest.NewServer(h)
	defer downstream.Close()

	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(downstream.URL, "http://", "ws://", 1),
		http.Header{proofhttp.HeaderInstallID: []string{"ins_1"}})
	if err != nil {
		t.Fatalf("dial downstream: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(map[string]string{"type": "cancel"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err == nil && strings.Contains(string(payload), "SPEECH_CONTROL_INVALID") {
		t.Fatalf("cancel was rejected: %s", payload)
	}

	first := <-events
	if first["type"] != "session.update" {
		t.Fatalf("first upstream event = %#v", first)
	}
	<-done
	select {
	case evt := <-events:
		t.Fatalf("cancel leaked upstream as event %#v", evt)
	default:
	}
}

func TestParseControlAcceptsCancel(t *testing.T) {
	for _, raw := range []string{`{"type":"commit"}`, `{"type":"finish"}`, `{"type":"cancel"}`} {
		if _, err := parseControl([]byte(raw)); err != nil {
			t.Fatalf("parseControl(%s): %v", raw, err)
		}
	}
}
