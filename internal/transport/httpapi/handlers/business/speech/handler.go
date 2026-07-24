// Package speech adapts realtime microphone transcription to HTTP WebSocket.
// It is a narrow proxy: clients send PCM bytes and small control messages; the
// gateway owns Qwen session configuration and upstream credentials.
package speech

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	appspeech "github.com/sunweilin/anselm/gateway/internal/app/speech"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	maxAudioFrameBytes = 256 * 1024
	sessionMaxAge      = 2 * time.Minute
	writeWait          = 10 * time.Second
)

type Service interface {
	Authorize(ctx context.Context, installID string) *apierr.APIError
	Upstream() (appspeech.Upstream, *apierr.APIError)
}

type Handler struct {
	svc    Service
	dialer *websocket.Dialer
}

func New(svc Service) *Handler {
	return &Handler{svc: svc, dialer: websocket.DefaultDialer}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	if h.svc == nil {
		response.WriteError(w, apierr.ErrSpeechUnavailable)
		return
	}
	if ae := h.svc.Authorize(r.Context(), r.Header.Get(proofhttp.HeaderInstallID)); ae != nil {
		response.WriteError(w, ae)
		return
	}
	up, ae := h.svc.Upstream()
	if ae != nil {
		response.WriteError(w, ae)
		return
	}

	upConn, resp, err := h.dialer.DialContext(r.Context(), up.URL, http.Header{"Authorization": []string{"Bearer " + up.APIKey}})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		response.WriteError(w, apierr.ErrSpeechUnavailable)
		return
	}
	defer func() { _ = upConn.Close() }()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  maxAudioFrameBytes,
		WriteBufferSize: maxAudioFrameBytes,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	downConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = downConn.Close() }()
	downConn.SetReadLimit(maxAudioFrameBytes)
	sessionDeadline := time.Now().Add(sessionMaxAge)
	_ = downConn.SetReadDeadline(sessionDeadline)
	_ = upConn.SetReadDeadline(sessionDeadline.Add(writeWait))
	client := &clientWriter{conn: downConn}

	language := strings.TrimSpace(r.URL.Query().Get("language"))
	if err := writeUpstreamJSON(upConn, sessionUpdate(language)); err != nil {
		_ = client.writeJSON(map[string]any{"type": "error", "code": "SPEECH_UPSTREAM_WRITE_FAILED"})
		return
	}

	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		for {
			mt, payload, err := upConn.ReadMessage()
			if err != nil {
				_ = client.writeJSON(map[string]any{"type": "error", "code": "SPEECH_UPSTREAM_CLOSED"})
				_ = downConn.SetReadDeadline(time.Now())
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			if err := client.writeRaw(websocket.TextMessage, payload); err != nil {
				return
			}
			var evt struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &evt) == nil && evt.Type == "session.finished" {
				_ = downConn.SetReadDeadline(time.Now())
				return
			}
		}
	}()

	for {
		select {
		case <-upDone:
			return
		default:
		}
		mt, payload, err := downConn.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if len(payload) == 0 || len(payload) > maxAudioFrameBytes {
				_ = client.writeJSON(map[string]any{"type": "error", "code": "SPEECH_AUDIO_FRAME_INVALID"})
				return
			}
			if err := writeUpstreamJSON(upConn, map[string]any{
				"event_id": nextEventID(),
				"type":     "input_audio_buffer.append",
				"audio":    base64.StdEncoding.EncodeToString(payload),
			}); err != nil {
				return
			}
		case websocket.TextMessage:
			action, err := parseControl(payload)
			if err != nil {
				_ = client.writeJSON(map[string]any{"type": "error", "code": "SPEECH_CONTROL_INVALID"})
				return
			}
			if action == "finish" {
				_ = writeUpstreamJSON(upConn, map[string]any{"event_id": nextEventID(), "type": "session.finish"})
				continue
			}
			if action == "cancel" {
				return
			}
			if action == "commit" {
				_ = writeUpstreamJSON(upConn, map[string]any{"event_id": nextEventID(), "type": "input_audio_buffer.commit"})
			}
		}
	}
}

func sessionUpdate(language string) map[string]any {
	transcription := map[string]any{}
	if language != "" {
		transcription["language"] = language
	}
	return map[string]any{
		"event_id": nextEventID(),
		"type":     "session.update",
		"session": map[string]any{
			"input_audio_format":        "pcm",
			"sample_rate":               16000,
			"input_audio_transcription": transcription,
			"turn_detection": map[string]any{
				"type":                "server_vad",
				"threshold":           0.0,
				"silence_duration_ms": 400,
			},
		},
	}
}

func parseControl(payload []byte) (string, error) {
	var in struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return "", err
	}
	switch in.Type {
	case "finish", "commit", "cancel":
		return in.Type, nil
	default:
		return "", errors.New("unknown speech control")
	}
}

func writeUpstreamJSON(conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(websocket.TextMessage, b)
}

type clientWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *clientWriter) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeRaw(websocket.TextMessage, b)
}

func (w *clientWriter) writeRaw(mt int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return w.conn.WriteMessage(mt, payload)
}

var eventSeq uint64

func nextEventID() string {
	n := atomic.AddUint64(&eventSeq, 1)
	return "event_" + strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000000000"), ".", "") + "_" + strconv.FormatUint(n, 10)
}
