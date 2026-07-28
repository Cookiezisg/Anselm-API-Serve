package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// The duplex dialect is new in H9 and nothing else in the repo speaks it, so these tests pin the
// protocol shape itself: what we SEND in what order, and how each way the stream can end maps onto
// the billed/unbilled decision. A transport swap that silently stopped sending `finish-task` would
// hang forever against a real upstream and look fine in every artifact-side assertion.
//
// 双工方言是 H9 才有的,仓库里没有别的东西说它,故这些测试钉的是**协议形状本身**:我们**按什么顺序
// 发什么**,以及流的每一种结束方式如何映射到「计不计费」。一次悄悄不再发 `finish-task` 的传输改动,
// 对着真上游会永远挂住,而在一切**产物侧**断言里都看着没事。

// fakeTTS is a WebSocket upstream scripted by the test: it records the frames it receives and
// replays a chosen ending.
//
// fakeTTS 是一个由测试编排的 WebSocket 上游:它记录收到的帧,并重放指定的结局。
type fakeTTS struct {
	srv      *httptest.Server
	got      []wsFrame
	ending   string // "finished" | "failed-before-audio" | "failed-after-audio" | "silent-finish"
	audio    []byte
	upgrader websocket.Upgrader
}

func newFakeTTS(t *testing.T, ending string, audio []byte) *fakeTTS {
	t.Helper()
	f := &fakeTTS{ending: ending, audio: audio}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api-ws/v1/inference" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		c, err := f.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var in wsFrame
			if json.Unmarshal(data, &in) != nil {
				continue
			}
			f.got = append(f.got, in)
			switch in.Header.Action {
			case "run-task":
				_ = c.WriteJSON(eventFrame(in.Header.TaskID, "task-started"))
			case "finish-task":
				switch f.ending {
				case "failed-before-audio":
					_ = c.WriteJSON(eventFrame(in.Header.TaskID, "task-failed"))
				case "failed-after-audio":
					_ = c.WriteMessage(websocket.BinaryMessage, f.audio)
					_ = c.WriteJSON(eventFrame(in.Header.TaskID, "task-failed"))
				case "silent-finish":
					_ = c.WriteJSON(eventFrame(in.Header.TaskID, "task-finished"))
				default:
					_ = c.WriteMessage(websocket.BinaryMessage, f.audio[:len(f.audio)/2])
					_ = c.WriteMessage(websocket.BinaryMessage, f.audio[len(f.audio)/2:])
					_ = c.WriteJSON(eventFrame(in.Header.TaskID, "task-finished"))
				}
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func eventFrame(taskID, event string) wsFrame {
	var f wsFrame
	f.Header.TaskID = taskID
	f.Header.Event = event
	return f
}

func (f *fakeTTS) client() *TTSGen {
	g := NewTTSGen(strings.Replace(f.srv.URL, "http://", "http://", 1), "k")
	return g
}

// TestTTS_ProtocolOrderAndAudio: the happy path. Three frames in the upstream's order, one task id
// throughout, and binary frames concatenated into the returned audio.
//
// TestTTS_ProtocolOrderAndAudio:顺利路径。按上游的顺序发三帧、全程同一个 task id,二进制帧拼接成
// 返回的音频。
func TestTTS_ProtocolOrderAndAudio(t *testing.T) {
	want := []byte("RIFF....WAVEpcm-bytes")
	f := newFakeTTS(t, "finished", want)
	audio, unbilled, err := f.client().GenerateSpeech(context.Background(), "m", "你好", "v")
	if err != nil || unbilled {
		t.Fatalf("err=%v unbilled=%v", err, unbilled)
	}
	if string(audio) != string(want) {
		t.Fatalf("audio = %q, want %q", audio, want)
	}
	if len(f.got) != 3 {
		t.Fatalf("frames = %d, want run-task/continue-task/finish-task", len(f.got))
	}
	for i, want := range []string{"run-task", "continue-task", "finish-task"} {
		if f.got[i].Header.Action != want {
			t.Fatalf("frame %d = %q, want %q", i, f.got[i].Header.Action, want)
		}
	}
	if f.got[0].Payload.Model != "m" || f.got[0].Payload.Parameters["voice"] != "v" {
		t.Fatalf("run-task did not carry model+voice: %+v", f.got[0].Payload)
	}
	// The text rides continue-task, NOT run-task: a task rejected at `task-started` must not have
	// already carried the utterance.
	// 文本走 continue-task、**不走** run-task:一个在 `task-started` 就被拒的任务,不该已经把整段话
	// 带上去了。
	if f.got[1].Payload.Input["text"] != "你好" {
		t.Fatalf("continue-task text = %v", f.got[1].Payload.Input["text"])
	}
	if _, leaked := f.got[0].Payload.Input["text"]; leaked {
		t.Fatal("run-task carried the text — a pre-synthesis rejection would waste the whole utterance")
	}
	id := f.got[0].Header.TaskID
	if id == "" || f.got[1].Header.TaskID != id || f.got[2].Header.TaskID != id {
		t.Fatalf("task id not stable across the stream: %+v", f.got)
	}
}

// TestTTS_FailureBeforeAudioIsUnbilled / _AfterAudioIsBilled: GW-INV-50's two halves. Once bytes
// have flowed the provider has done work, so a later failure must NOT roll the charge back.
//
// TestTTS_FailureBeforeAudioIsUnbilled / _AfterAudioIsBilled:GW-INV-50 的两半。一旦字节已经流出来,
// 上游就已经干了活,故此后的失败**绝不能**把钱退回去。
func TestTTS_FailureBeforeAudioIsUnbilled(t *testing.T) {
	f := newFakeTTS(t, "failed-before-audio", nil)
	_, unbilled, err := f.client().GenerateSpeech(context.Background(), "m", "你好", "v")
	if err == nil || !unbilled {
		t.Fatalf("err=%v unbilled=%v; a failure with no audio is provably pre-synthesis", err, unbilled)
	}
}

func TestTTS_FailureAfterAudioKeepsTheCharge(t *testing.T) {
	f := newFakeTTS(t, "failed-after-audio", []byte("some-pcm"))
	_, unbilled, err := f.client().GenerateSpeech(context.Background(), "m", "你好", "v")
	if err == nil {
		t.Fatal("a task-failed must surface as an error")
	}
	if unbilled {
		t.Fatal("audio had already flowed — the provider did work and the charge must stand")
	}
}

// TestTTS_SilentFinishIsAmbiguous: a clean `task-finished` with no audio is not success. The
// provider may have synthesized and billed while we collected nothing, so the charge stands and
// the caller sees an error rather than an empty artifact.
//
// TestTTS_SilentFinishIsAmbiguous:干净的 `task-finished` 却没有音频,**不是**成功。上游可能已合成
// 已计费而我们什么也没收到,故计费保留、调用方看到的是错误而不是一件空产物。
func TestTTS_SilentFinishIsAmbiguous(t *testing.T) {
	f := newFakeTTS(t, "silent-finish", nil)
	audio, unbilled, err := f.client().GenerateSpeech(context.Background(), "m", "你好", "v")
	if err == nil || len(audio) != 0 {
		t.Fatalf("audio=%d err=%v; an empty finish must not read as success", len(audio), err)
	}
	if unbilled {
		t.Fatal("an empty finish is ambiguous evidence — the charge must stand")
	}
}

// TestTTS_UnconfiguredIsUnbilled: nothing left this process, so nothing was billed.
//
// TestTTS_UnconfiguredIsUnbilled:什么也没离开本进程,故什么也没被计费。
func TestTTS_UnconfiguredIsUnbilled(t *testing.T) {
	_, unbilled, err := NewTTSGen("", "").GenerateSpeech(context.Background(), "m", "x", "v")
	if err != apierr.ErrTTSUnavailable || !unbilled {
		t.Fatalf("err=%v unbilled=%v", err, unbilled)
	}
}

// TestInferenceWSURL_DerivesFromTheCredentialOrigin: the region is a property of the key. A
// hardcoded host once really did send a Singapore key to Beijing (WRK-082 H0), so the endpoint is
// derived, never constant — and http→ws is kept so a local test server is reachable.
//
// TestInferenceWSURL_DerivesFromTheCredentialOrigin:区域是 key 的属性。写死主机曾真的把一把新加坡
// 的 key 送去北京(H0),故端点是**派生**的、绝不是常量——且保留 http→ws,使本地测试服务器可达。
func TestInferenceWSURL_DerivesFromTheCredentialOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://ws-abc.ap-southeast-1.maas.aliyuncs.com", "wss://ws-abc.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/inference"},
		{"https://dashscope.aliyuncs.com/", "wss://dashscope.aliyuncs.com/api-ws/v1/inference"},
		{"http://127.0.0.1:9", "ws://127.0.0.1:9/api-ws/v1/inference"},
	} {
		got, err := inferenceWSURL(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("inferenceWSURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := inferenceWSURL("::not a url"); err == nil {
		t.Fatal("a malformed base must fail rather than dial something unintended")
	}
}

// TestTTS_RunTaskPinsTheAudioFormat: the gateway labels its response `audio/wav` and the desktop's
// chunk rejoin parses it as WAV, so the bytes MUST be WAV — and they only are because this frame
// asks for it. Left to the engine's own default the answer is raw headerless PCM, which no player
// opens and the joiner rejects; on the wire it looks like a perfectly successful synthesis. That is
// exactly the shape of defect a fake upstream can never show you, so the frame itself is the thing
// under assertion here.
//
// TestTTS_RunTaskPinsTheAudioFormat:网关把响应标成 `audio/wav`、桌面侧分块重接又按 WAV 解析它,故字节
// **必须**是 WAV——而它们之所以是,只因为这个帧**要了**。交给引擎自己的默认,答案是裸的无头 PCM:没有
// 播放器打得开、拼接器也不收,而在线缆上它看起来是一次**完全成功**的合成。这正是假上游永远给不出的那类
// 缺陷,故这里被断言的东西就是**这个帧本身**。
func TestTTS_RunTaskPinsTheAudioFormat(t *testing.T) {
	f := runTaskFrame("task-1", "qwen-audio-3.0-tts-flash", "longanhuan_v3.6")
	if got := f.Payload.Parameters["format"]; got != "wav" {
		t.Fatalf("format = %v, want wav — the response is labeled audio/wav", got)
	}
	if got := f.Payload.Parameters["sample_rate"]; got != 24000 {
		t.Fatalf("sample_rate = %v, want 24000", got)
	}
	// The voice still travels verbatim: a name the caller chose must never be rewritten here, and a
	// cloned voice's id is exactly such a name.
	// 音色仍逐字传递:调用方选的名字绝不能在这里被改写,而克隆音色的 id 正是这样一个名字。
	if got := f.Payload.Parameters["voice"]; got != "longanhuan_v3.6" {
		t.Fatalf("voice = %v, want it passed through verbatim", got)
	}
}
