// Package audio is the thin HTTP handler for POST /v1/audio/speech: decode and
// validate the closed request shape (bounded input, bounded voice), run
// app/tts.Synthesize, and write the audio back.
//
// **The response is raw `audio/wav` bytes** — OpenAI's own shape for this endpoint, and no longer
// the `{created,data:[{url}]}` images shape it used to share.
//
// This was not a preference. The upstream that can synthesize with BOTH preset and cloned voices
// (`qwen-audio-3.0-tts-flash`) is served ONLY over a duplex WebSocket — both HTTP shapes answer
// `url error` (真机实测 2026-07-28). A duplex stream hands back FRAMES, not an artifact URL, so
// there is nothing for the URL passthrough to pass through. The alternative — park the bytes in
// the media store and mint a lease — would buy storage, expiry, cleanup and a second round trip
// purely to preserve a JSON envelope, while the gateway ends up holding the bytes either way.
//
// URL passthrough is unchanged for images and video, whose upstreams really do return URLs.
//
// Package audio 是 POST /v1/audio/speech 的薄 handler。**响应是裸 `audio/wav` 字节**——
// OpenAI 自己在这条端点上的形状,不再是它此前与图像共用的 `{created,data:[{url}]}`。
//
// 这不是偏好。那个既能用预置音色、又能用克隆音色合成的上游(`qwen-audio-3.0-tts-flash`)**只**在
// 双工 WebSocket 上提供服务,两种 HTTP 形状都答 `url error`(真机实测 2026-07-28)。双工流回来的是
// **帧**、不是产物 URL,故 URL 直通**没有东西可直通**。另一条路——把字节停进媒体库再签一个
// lease——是为了保住一个 JSON 信封而买下存储、过期、清理与第二次往返,而网关**两种做法都一样**要
// 持有那些字节。
//
// URL 直通对图像与视频不变——那两家上游真的返 URL。
package audio

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	apptts "github.com/sunweilin/anselm/gateway/internal/app/tts"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	// maxInputChars bounds ONE synthesis. qwen3-tts caps a single request around
	// 500 characters, and the gateway holds the harder line deliberately:
	// chunking long text HERE would make one reservation cover N upstream calls, and a
	// partial failure across them is exactly the ambiguous-billing hole GW-INV-50
	// exists to keep shut. The desktop splits at sentence boundaries and
	// concatenates PCM — where the retry story is already per-chunk.
	//
	// maxInputChars 界**一次**合成。qwen3-tts 单请求约 500 字符封顶,而网关刻意守更硬的线:
	// 在**这里**切块会让一次预留覆盖 N 次上游调用,而其中的部分失败恰是 GW-INV-50
	// 要堵死的歧义计费洞。切块归桌面端(按句读切、拼 PCM),那里的重试故事本就是逐块的。
	maxInputChars = 500
	// maxVoiceChars bounds the voice name. The gateway is NOT the authority on the
	// voice catalog (48 names today, growing) — validating against a copied list
	// would drift into rejecting voices the upstream happily serves. Bounding the
	// shape is enough; an unknown name comes back as an upstream rejection.
	//
	// maxVoiceChars 界音色名长度。网关**不是**音色目录的权威(今天 48 个、还在长)——照抄一份
	// 名单来校验,迟早会拒掉上游明明支持的音色。界住形状就够,未知名字由上游拒回来。
	maxVoiceChars = 64
)

// Handler serves POST /v1/audio/speech.
type Handler struct {
	svc *apptts.Service
}

// New wires the handler to the tts service.
func New(svc *apptts.Service) *Handler { return &Handler{svc: svc} }

type requestBody struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Voice string `json:"voice"`
}

// ServeHTTP guards method + shape then delegates. The request `model` is a
// logical alias and never selects an upstream (the same rule as chat and images).
// There is no `format` field: qwen3-tts always answers 24kHz/16-bit/mono WAV, so
// accepting a format would be a promise the upstream cannot keep.
//
// ServeHTTP 守方法与形状后委派。请求 `model` 是逻辑别名、绝不选上游。**无** `format` 字段:
// qwen3-tts 恒返 24kHz/16bit/mono WAV,收一个 format 等于许一个上游兑现不了的诺。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	var body requestBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	input := strings.TrimSpace(body.Input)
	if input == "" || utf8.RuneCountInString(input) > maxInputChars {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	voice := strings.TrimSpace(body.Voice)
	if utf8.RuneCountInString(voice) > maxVoiceChars {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}

	installID := r.Header.Get(proofhttp.HeaderInstallID)
	audio, ae := h.svc.Synthesize(r.Context(), installID, input, voice)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	// The model answers 24kHz/16bit/mono WAV and the wire carries no format knob, so the content
	// type is a fact about the upstream rather than something the caller negotiated.
	// 模型恒返 24kHz/16bit/mono WAV,且线缆上没有 format 旋钮,故 content type 是一个**关于上游的
	// 事实**、不是调用方协商出来的东西。
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}
