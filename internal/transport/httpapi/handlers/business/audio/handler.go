// Package audio is the thin HTTP handler for POST /v1/audio/speech (WRK-082
// 批C): decode + validate the closed request shape (bounded input, bounded
// voice), run app/tts.Synthesize, and render the artifact URL.
//
// The response is `{created, data:[{url}]}` — the images shape, NOT OpenAI's
// raw-audio-bytes body. Two reasons: P13 makes URL passthrough the gateway's
// whole media contract (the gateway never holds artifact bytes), and matching
// the sibling generation endpoint means one client-side shape for both.
//
// Package audio 是 POST /v1/audio/speech 的薄 handler(批C)。响应用 `{created,data:[{url}]}`
// ——图像那一形,**不是** OpenAI 的裸音频字节体。两个理由:P13 让 URL 直通成为网关的整个媒体契约
// (网关从不持有产物字节),且与兄弟生成端点同形意味着客户端只需认一种形状。
package audio

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	apptts "github.com/sunweilin/anselm/gateway/internal/app/tts"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	// maxInputChars bounds ONE synthesis. qwen3-tts caps a single request around
	// 500 characters, and the gateway holds the harder line deliberately (代拍 C5):
	// chunking长 text HERE would make one reservation cover N upstream calls, and a
	// partial failure across them is exactly the ambiguous-billing hole GW-INV-50
	// exists to keep shut. The desktop splits at sentence boundaries and
	// concatenates PCM — where the retry story is already per-chunk.
	//
	// maxInputChars 界**一次**合成。qwen3-tts 单请求约 500 字符封顶,而网关刻意守更硬的线
	// (代拍 C5):在**这里**切块会让一次预留覆盖 N 次上游调用,而其中的部分失败恰是 GW-INV-50
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

type responseBody struct {
	Created int64               `json:"created"`
	Data    []responseAudioItem `json:"data"`
}

type responseAudioItem struct {
	URL string `json:"url"`
}

// ServeHTTP guards method + shape then delegates. The request `model` is a
// logical alias and never selects an upstream (the same rule as chat and images).
// There is no `format` field: qwen3-tts always answers 24kHz/16-bit/mono WAV, so
// accepting a format would be a promise the upstream cannot keep (代拍 C3).
//
// ServeHTTP 守方法与形状后委派。请求 `model` 是逻辑别名、绝不选上游。**无** `format` 字段:
// qwen3-tts 恒返 24kHz/16bit/mono WAV,收一个 format 等于许一个上游兑现不了的诺(代拍 C3)。
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
	audioURL, ae := h.svc.Synthesize(r.Context(), installID, input, voice)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, responseBody{
		Created: time.Now().Unix(),
		Data:    []responseAudioItem{{URL: audioURL}},
	})
}
