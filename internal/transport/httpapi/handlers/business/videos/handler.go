// Package videos is the thin HTTP surface for the async video capability
// (WRK-082 H1): POST /v1/videos/generations submits and returns a signed handle,
// GET /v1/videos/{videoId} reports that handle's phase and, once succeeded, the
// artifact URL. Two routes because the family has no synchronous form — the
// minutes of generation live on the CLIENT's clock, never in a gateway request.
//
// videos 包是异步视频能力的薄 HTTP 面(H1):POST /v1/videos/generations 提交并返回签名句柄,
// GET /v1/videos/{videoId} 报该句柄的状态,成功后带产物 URL。两条路由,因为本族没有同步形态——
// 那几分钟的生成活在**客户端**的钟上,绝不活在一次网关请求里。
package videos

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	appvideo "github.com/sunweilin/anselm/gateway/internal/app/video"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	maxPromptChars = 2000
	// minSeconds/maxSeconds bound one clip. The floor is the provider's own; the
	// ceiling is deliberately the provider ceiling and NOT a product choice — the
	// product limit that matters (10 clips a day) is a category ledger, not a
	// length cap, because rationing seconds would make users write shorter prompts
	// rather than fewer videos.
	// minSeconds/maxSeconds 界定单条片长。下限是上游自己的;上限刻意取**上游上限**而非产品选择——
	// 真正重要的产品限制(一天 10 条)是品类账本、不是长度上限,因为配给秒数只会让用户写更短的
	// 提示词、而不是少生成视频。
	minSeconds     = 2
	maxSeconds     = 15
	defaultSeconds = 5
)

// aspects is the closed request vocabulary, translated here into the provider's
// ratio spelling. The wire takes shape WORDS rather than "16:9" so a future
// provider with different ratios does not force a client change.
//
// aspects 是封闭的请求词表,在此译成上游的 ratio 拼法。线缆收的是形状**词**而非 "16:9",这样将来
// 换一家比例不同的上游时,客户端不必跟着改。
var aspects = map[string]string{
	"landscape": "16:9",
	"portrait":  "9:16",
	"square":    "1:1",
}

// resolutions is the closed quality vocabulary. 1080p is accepted but is NOT the
// default: it costs materially more per second and the desktop's video cards are
// rendered well below that width anyway.
//
// resolutions 是封闭的画质词表。1080p 收但**不是默认**:它每秒贵得多,而桌面端的视频卡本来就渲得
// 远比那个宽度小。
var resolutions = map[string]string{
	"720p":  "720P",
	"1080p": "1080P",
}

// Handler serves both video routes.
type Handler struct {
	svc *appvideo.Service
}

// New wires the handler to the video service.
func New(svc *appvideo.Service) *Handler { return &Handler{svc: svc} }

type generateRequest struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	Seconds    *int   `json:"seconds"`
	Aspect     string `json:"aspect"`
	Resolution string `json:"resolution"`
}

type generateResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Status  string `json:"status"`
	Created int64  `json:"created"`
}

type statusResponse struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

// Generate serves POST /v1/videos/generations. The request `model` is a logical
// alias and never selects an upstream (the same rule as chat and images).
//
// Generate 服务 POST /v1/videos/generations。请求 `model` 是逻辑别名、绝不选上游(与 chat、
// images 同律)。
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	var body generateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" || len([]rune(prompt)) > maxPromptChars {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	seconds := defaultSeconds
	if body.Seconds != nil {
		seconds = *body.Seconds
	}
	if seconds < minSeconds || seconds > maxSeconds {
		// An impossible length is refused BEFORE the reservation — the caller gets a
		// 400 and pays nothing, rather than a daily allowance spent on an upstream 400.
		// 不可能的时长在**预留之前**就被拒——调用方拿 400 且分文未付,而不是把当天的额度花在一个
		// 上游 400 上。
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	aspect := strings.ToLower(strings.TrimSpace(body.Aspect))
	if aspect == "" {
		// Landscape, not square: a video is 16:9 unless somebody says otherwise.
		// 横向、不是方形:一段视频除非另有交代就是 16:9。
		aspect = "landscape"
	}
	ratio, ok := aspects[aspect]
	if !ok {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	quality := strings.ToLower(strings.TrimSpace(body.Resolution))
	if quality == "" {
		quality = "720p"
	}
	resolution, ok := resolutions[quality]
	if !ok {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}

	installID := r.Header.Get(proofhttp.HeaderInstallID)
	handle, ae := h.svc.Submit(r.Context(), installID, prompt, seconds, ratio, resolution)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	// 202, not 200: nothing has been generated yet, and the status line is the one
	// place the wire can say so without the client having to read a field.
	// 202 而非 200:此刻什么都还没生成,而状态行是线缆唯一能不靠客户端读字段就说清这件事的地方。
	response.WriteJSON(w, http.StatusAccepted, generateResponse{
		ID:      handle,
		Object:  "video.generation",
		Status:  string(domvideo.PhasePending),
		Created: time.Now().Unix(),
	})
}

// Status serves GET /v1/videos/{videoId}. A handle that does not verify against
// the calling install answers exactly like a task the provider has forgotten:
// telling the two apart would confirm that some OTHER install owns it.
//
// Status 服务 GET /v1/videos/{videoId}。一个对调用方 install 验不过的句柄,与一个上游已忘掉的任务
// **答案完全相同**:区分两者等于确认「有**别的** install 拥有它」。
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("videoId")
	if strings.TrimSpace(handle) == "" {
		response.WriteError(w, apierr.ErrVideoTaskNotFound)
		return
	}
	installID := r.Header.Get(proofhttp.HeaderInstallID)
	st, ae := h.svc.Status(r.Context(), installID, handle)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, statusResponse{
		ID:     handle,
		Object: "video.generation",
		Status: string(st.Phase),
		URL:    st.URL,
	})
}
