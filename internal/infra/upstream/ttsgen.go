package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// TTSGen is the DashScope speech-synthesis client (WRK-082 批C; rewritten onto the duplex
// WebSocket protocol in H9).
//
// **It returns BYTES, not a URL, and the upstream forced that — we did not choose it.**
// `qwen-audio-3.0-tts-flash` is served ONLY over `api-ws/v1/inference`; both HTTP shapes answer
// `url error, please check url` (真机实测 2026-07-28). A duplex stream has no artifact URL to pass
// through, so P13's URL-relay contract cannot apply to speech at all. It still applies to images
// and video, whose upstreams really do hand back URLs.
//
// **Why that model**: it is the one model that synthesizes with BOTH preset voices and voices
// minted by `voice-enrollment` (实测:预置三个 + 克隆一个,全部出真音频). Keeping the older
// `qwen3-tts-flash` beside it would mean two models and two dialects serving one capability — and
// the older one is marked 即将部分下线 anyway.
//
// TTSGen 是 DashScope 语音合成 client(批C;H9 重写到双工 WebSocket 协议上)。
//
// **它返回字节、不是 URL,而这是上游逼的、不是我们选的。** `qwen-audio-3.0-tts-flash` **只**在
// `api-ws/v1/inference` 上提供服务,两种 HTTP 形状都答 `url error, please check url`(真机实测
// 2026-07-28)。一条双工流**没有产物 URL 可以直通**,故 P13 的 URL 直通契约在语音上根本无从适用;
// 它对图像与视频依然成立——那两家上游真的给 URL。
//
// **为什么是这个模型**:它是唯一一个既能用预置音色、又能用 `voice-enrollment` 铸出的音色合成的模型
// (实测:预置三个 + 克隆一个,全部出真音频)。把旧的 `qwen3-tts-flash` 并排留着,等于为一个能力养
// 两个模型两套方言——而旧的那个本身还标着「即将部分下线」。
type TTSGen struct {
	base    string // NATIVE DashScope origin, no trailing slash
	apiKey  string
	dialer  *websocket.Dialer
	timeout time.Duration
}

// ttsGenTimeout bounds one whole synthesis, connection included. A duplex stream that stalls
// mid-utterance would otherwise hold a gateway goroutine and an open reservation indefinitely.
//
// ttsGenTimeout 界一次完整合成(含建连)。一条在句子中间卡住的双工流,否则会无限期占着一个网关
// goroutine 和一笔未结的预留。
const ttsGenTimeout = 60 * time.Second

// maxAudioBytes caps one synthesis's audio. The wire already caps input at maxInputChars, so this
// is a runaway guard rather than a product limit — an upstream that never sends `task-finished`
// must not be able to grow gateway memory without bound.
//
// maxAudioBytes 界一次合成的音频。线缆已把输入封在 maxInputChars,故这是**失控**闸、不是产品上限
// ——一个永远不发 `task-finished` 的上游,绝不能无界地撑大网关内存。
const maxAudioBytes = 32 << 20

// NewTTSGen builds the client over the native base and ONE key (no failover pool — a deterministic
// reservation must map to a single attempt).
//
// NewTTSGen 在原生 base 与**一把** key 上构建(无 failover 池——确定性预留必须对应单次尝试)。
func NewTTSGen(nativeBase, apiKey string) *TTSGen {
	return &TTSGen{
		base:    strings.TrimRight(strings.TrimSpace(nativeBase), "/"),
		apiKey:  apiKey,
		dialer:  &websocket.Dialer{HandshakeTimeout: 20 * time.Second},
		timeout: ttsGenTimeout,
	}
}

// wsFrame is the control envelope both directions share. Audio travels as BINARY frames alongside
// these text frames, which is why the read loop switches on the message type instead of assuming
// every frame is JSON.
//
// wsFrame 是两个方向共用的控制信封。音频以**二进制帧**与这些文本帧并行走,故读循环按消息类型分支、
// 不假定每一帧都是 JSON。
type wsFrame struct {
	Header struct {
		Action       string `json:"action,omitempty"`
		TaskID       string `json:"task_id,omitempty"`
		Streaming    string `json:"streaming,omitempty"`
		Event        string `json:"event,omitempty"`
		ErrorCode    string `json:"error_code,omitempty"`
		ErrorMessage string `json:"error_message,omitempty"`
	} `json:"header"`
	Payload struct {
		TaskGroup  string         `json:"task_group,omitempty"`
		Task       string         `json:"task,omitempty"`
		Function   string         `json:"function,omitempty"`
		Model      string         `json:"model,omitempty"`
		Parameters map[string]any `json:"parameters,omitempty"`
		Input      map[string]any `json:"input"`
	} `json:"payload"`
}

// GenerateSpeech synthesizes one utterance and returns the raw audio bytes.
//
// The protocol is the upstream's: `run-task` opens with model + voice, `continue-task` carries the
// text, `finish-task` closes, and audio arrives as binary frames in between. The text is sent
// AFTER `task-started` rather than optimistically, because a rejected task (unknown voice,
// unavailable model) fails exactly at that boundary — sending first would mean discovering the
// refusal with a whole utterance already on the wire.
//
// GenerateSpeech 合成一段话并返回**裸音频字节**。
//
// 协议是上游的:`run-task` 带模型与音色开场,`continue-task` 送文本,`finish-task` 收尾,音频以
// 二进制帧夹在中间回来。文本在 `task-started` **之后**才发、不抢跑,因为被拒的任务(未知音色、模型
// 不可用)正是在那个边界失败的——先发等于在一整段话已经上了线缆之后才发现被拒。
func (g *TTSGen) GenerateSpeech(ctx context.Context, model, text, voice string) ([]byte, bool, error) {
	if g == nil || g.base == "" || g.apiKey == "" {
		// Nothing left this process, so nothing was billed.
		// 什么也没离开本进程,故什么也没被计费。
		return nil, true, apierr.ErrTTSUnavailable
	}
	endpoint, err := inferenceWSURL(g.base)
	if err != nil {
		return nil, true, apierr.Internal()
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	conn, resp, err := g.dialer.DialContext(cctx, endpoint,
		http.Header{"Authorization": []string{"bearer " + g.apiKey}})
	if err != nil {
		// A handshake rejection carries a status, and a 4xx there is a provably pre-synthesis
		// refusal. Everything else is ambiguous and keeps the charge (GW-INV-50).
		// 握手被拒会带状态码,那里的 4xx 是可证明的**合成前**拒绝。其余一切都是歧义,保留计费。
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, true, apierr.UpstreamRejected(apierr.RejectedInvalid)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, false, apierr.ErrUpstreamTimeout
		}
		return nil, false, apierr.ErrUpstreamError
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := cctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
		_ = conn.SetWriteDeadline(dl)
	}

	taskID := newTaskID()
	if err := writeFrame(conn, runTaskFrame(taskID, model, voice)); err != nil {
		return nil, false, apierr.ErrUpstreamError
	}

	var audio []byte
	started := false
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			// The stream died before `task-finished`. Whether the provider completed (and billed)
			// the synthesis is unknowable from here — ambiguous, keep the charge.
			// 流在 `task-finished` 之前断了。上游到底有没有完成(并计费)这次合成,从这里无从得知
			// ——歧义,保留计费。
			return nil, false, apierr.ErrUpstreamError
		}
		if msgType == websocket.BinaryMessage {
			if len(audio)+len(data) > maxAudioBytes {
				return nil, false, apierr.ErrUpstreamError
			}
			audio = append(audio, data...)
			continue
		}
		var f wsFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Header.Event {
		case "task-started":
			if started {
				continue
			}
			started = true
			if err := writeFrame(conn, continueTaskFrame(taskID, text)); err != nil {
				return nil, false, apierr.ErrUpstreamError
			}
			if err := writeFrame(conn, finishTaskFrame(taskID)); err != nil {
				return nil, false, apierr.ErrUpstreamError
			}
		case "task-failed":
			// A failure BEFORE any audio arrived is a pre-synthesis refusal — provably unbilled.
			// Once bytes have flowed the provider has done work, so the charge stands. Upstream
			// text is discarded (redaction iron rule).
			// **在任何音频到达之前**失败,是一次合成前拒绝——可证明未计费。一旦字节已经流出来,上游
			// 就已经干了活,故计费保留。上游原文丢弃(脱敏铁律)。
			return nil, len(audio) == 0, apierr.UpstreamRejected(apierr.RejectedInvalid)
		case "task-finished":
			if len(audio) == 0 {
				// A clean finish with no audio is ambiguous evidence: the provider may have
				// synthesized and billed while we failed to collect it.
				// 干净收尾却没有音频是歧义证据:上游可能已合成已计费,而我们没收到。
				return nil, false, apierr.ErrUpstreamError
			}
			return audio, false, nil
		}
	}
}

func runTaskFrame(taskID, model, voice string) wsFrame {
	var f wsFrame
	f.Header.Action = "run-task"
	f.Header.TaskID = taskID
	f.Header.Streaming = "duplex"
	f.Payload.TaskGroup = "audio"
	f.Payload.Task = "tts"
	f.Payload.Function = "SpeechSynthesizer"
	f.Payload.Model = model
	// No `format` knob: the model answers 24kHz/16bit/mono WAV, and a format field on the gateway
	// wire would be a promise the upstream cannot keep (代拍 C3 — unchanged by the transport swap).
	// 不设 `format` 旋钮:模型恒返 24kHz/16bit/mono WAV,而网关线缆上的 format 字段是一个上游兑现不了
	// 的承诺(代拍 C3——不因换传输而改变)。
	f.Payload.Parameters = map[string]any{"text_type": "PlainText", "voice": voice}
	f.Payload.Input = map[string]any{}
	return f
}

func continueTaskFrame(taskID, text string) wsFrame {
	var f wsFrame
	f.Header.Action = "continue-task"
	f.Header.TaskID = taskID
	f.Header.Streaming = "duplex"
	f.Payload.Input = map[string]any{"text": text}
	return f
}

func finishTaskFrame(taskID string) wsFrame {
	var f wsFrame
	f.Header.Action = "finish-task"
	f.Header.TaskID = taskID
	f.Header.Streaming = "duplex"
	f.Payload.Input = map[string]any{}
	return f
}

func writeFrame(conn *websocket.Conn, f wsFrame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}

func newTaskID() string {
	b := make([]byte, 16)
	// Go 1.24+ crypto/rand.Read never returns an error; the stdlib idiom is to panic on the
	// impossible rather than mint a weak id.
	// Go 1.24+ 的 crypto/rand.Read 永不返错;标准库惯例是对不可能的错 panic、而不是铸一个弱 id。
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// inferenceWSURL turns the native HTTPS origin into the duplex inference endpoint. It derives from
// the CREDENTIAL's own origin rather than a constant, for the same reason every other generation
// route does: the region is a property of the key, and a hardcoded host once really did send a
// Singapore key to Beijing (WRK-082 H0).
//
// inferenceWSURL 把原生 HTTPS origin 变成双工推理端点。它从**凭证自己的** origin 派生、不用常量,
// 理由与其余每条生成路由相同:区域是 key 的属性,而写死主机曾真的把一把新加坡的 key 送去北京(H0)。
func inferenceWSURL(nativeBase string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(nativeBase))
	if err != nil || u.Host == "" {
		if err == nil {
			err = errors.New("missing host")
		}
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	default:
		u.Scheme = "wss"
	}
	u.Path = "/api-ws/v1/inference"
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}
