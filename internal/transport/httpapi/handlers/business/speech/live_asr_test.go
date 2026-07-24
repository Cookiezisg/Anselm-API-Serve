package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	appspeech "github.com/sunweilin/anselm/gateway/internal/app/speech"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
)

// TestLiveQwenASRThroughGatewayProxy is an explicitly billable smoke/eval. It is
// skipped by ordinary test/verify runs and only executes when EVALS_QWEN_ASR=1.
// With EVALS_ASR_WAV set, it expects a real transcription text; without a WAV it
// verifies only the live DashScope WebSocket protocol and gateway accounting
// finalization path.
func TestLiveQwenASRThroughGatewayProxy(t *testing.T) {
	up := liveASRConfig(t)
	svc := newFakeSpeechSvc(up)
	h := New(svc)
	downstream := httptest.NewServer(h)
	defer downstream.Close()

	language := strings.TrimSpace(os.Getenv("EVALS_ASR_LANGUAGE"))
	wsURL := strings.Replace(downstream.URL, "http://", "ws://", 1)
	if language != "" {
		wsURL += "?language=" + url.QueryEscape(language)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), wsURL, http.Header{proofhttp.HeaderInstallID: []string{"ins_live_asr"}})
	if err != nil {
		t.Fatalf("dial gateway ASR proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))

	audio := liveASRAudio(t)
	for len(audio) > 0 {
		n := min(len(audio), 3200) // 100ms PCM16/16k/mono.
		if err := conn.WriteMessage(websocket.BinaryMessage, audio[:n]); err != nil {
			t.Fatalf("write live audio frame: %v", err)
		}
		audio = audio[n:]
	}
	if err := conn.WriteJSON(map[string]string{"type": "finish"}); err != nil {
		t.Fatalf("finish live ASR session: %v", err)
	}

	needTranscript := strings.TrimSpace(os.Getenv("EVALS_ASR_WAV")) != ""
	seenFinished := false
	seenTranscript := false
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read live ASR event: %v", err)
		}
		var evt map[string]any
		if err := json.Unmarshal(payload, &evt); err != nil {
			t.Fatalf("live ASR returned non-JSON event: %s", payload)
		}
		typ, _ := evt["type"].(string)
		if strings.Contains(typ, "transcription") && eventHasUserText(evt) {
			seenTranscript = true
		}
		if typ == "session.finished" {
			seenFinished = true
			break
		}
	}
	if !seenFinished {
		t.Fatalf("live ASR did not finish")
	}
	if needTranscript && !seenTranscript {
		t.Fatalf("live ASR WAV completed without transcription text")
	}
	_, settledBytes, rolledBack := svc.waitFinal(t)
	if needTranscript {
		if settledBytes <= 0 || rolledBack {
			t.Fatalf("live ASR with audio settledBytes=%d rolledBack=%v", settledBytes, rolledBack)
		}
	} else if !rolledBack {
		t.Fatalf("live ASR smoke without audio should rollback")
	}
}

func liveASRConfig(t *testing.T) appspeech.Upstream {
	t.Helper()
	if os.Getenv("EVALS_QWEN_ASR") != "1" {
		t.Skip("set EVALS_QWEN_ASR=1 to run the billable Qwen realtime ASR eval")
	}
	key := firstEnv("DASHSCOPE_API_KEY", "EVALS_KEY")
	if key == "" {
		t.Fatal("EVALS_QWEN_ASR=1 requires DASHSCOPE_API_KEY or EVALS_KEY")
	}
	if i := strings.IndexByte(key, ','); i >= 0 {
		key = strings.TrimSpace(key[:i])
	}
	base := firstEnv("EVALS_BASE_URL", "DASHSCOPE_BASE_URL")
	if base == "" {
		workspace := strings.TrimSpace(os.Getenv("DASHSCOPE_WORKSPACE_ID"))
		if workspace == "" {
			t.Fatal("EVALS_QWEN_ASR=1 requires EVALS_BASE_URL, DASHSCOPE_BASE_URL, or DASHSCOPE_WORKSPACE_ID")
		}
		if !validLiveWorkspaceID(workspace) {
			t.Fatal("DASHSCOPE_WORKSPACE_ID must contain only ASCII letters, digits, '_' or '-'")
		}
		base = "https://" + workspace + ".ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
	}
	upURL, err := appspeech.RealtimeURL(base, appspeech.DefaultModel)
	if err != nil {
		t.Fatalf("derive live ASR URL: %v", err)
	}
	return appspeech.Upstream{URL: upURL, APIKey: key, Model: appspeech.DefaultModel}
}

func liveASRAudio(t *testing.T) []byte {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("EVALS_ASR_WAV"))
	if path == "" {
		return nil
	}
	pcm, err := readPCM16Mono16kWAV(path)
	if err != nil {
		t.Fatalf("read EVALS_ASR_WAV: %v", err)
	}
	maxSeconds := positiveEnvInt("EVALS_ASR_MAX_SECONDS", 10)
	maxBytes := maxSeconds * 32_000
	if len(pcm) > maxBytes {
		return pcm[:maxBytes]
	}
	return pcm
}

func readPCM16Mono16kWAV(path string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(raw)
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, errors.New("expected RIFF/WAVE file")
	}
	var fmtSeen bool
	var data []byte
	for r.Len() >= 8 {
		var chunkID [4]byte
		if _, err := io.ReadFull(r, chunkID[:]); err != nil {
			return nil, err
		}
		var size uint32
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return nil, err
		}
		if uint64(size) > uint64(r.Len()) {
			return nil, fmt.Errorf("chunk %q exceeds file size", string(chunkID[:]))
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		if size%2 == 1 && r.Len() > 0 {
			_, _ = r.ReadByte()
		}
		switch string(chunkID[:]) {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, errors.New("short fmt chunk")
			}
			audioFormat := binary.LittleEndian.Uint16(chunk[0:2])
			channels := binary.LittleEndian.Uint16(chunk[2:4])
			sampleRate := binary.LittleEndian.Uint32(chunk[4:8])
			bitsPerSample := binary.LittleEndian.Uint16(chunk[14:16])
			if audioFormat != 1 || channels != 1 || sampleRate != 16_000 || bitsPerSample != 16 {
				return nil, fmt.Errorf("expected PCM16 mono 16k WAV, got format=%d channels=%d sampleRate=%d bits=%d", audioFormat, channels, sampleRate, bitsPerSample)
			}
			fmtSeen = true
		case "data":
			data = chunk
		}
	}
	if !fmtSeen {
		return nil, errors.New("missing fmt chunk")
	}
	if len(data) == 0 {
		return nil, errors.New("missing data chunk")
	}
	return data, nil
}

func eventHasUserText(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "type" || k == "event_id" || k == "item_id" || k == "content_index" {
				continue
			}
			if eventHasUserText(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if eventHasUserText(child) {
				return true
			}
		}
	case string:
		return strings.TrimSpace(x) != ""
	}
	return false
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func positiveEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func validLiveWorkspaceID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		b := id[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_' || b == '-' {
			continue
		}
		return false
	}
	return true
}
