// Package images is the thin HTTP handler for POST /v1/images/generations and
// POST /v1/images/edits (edits added in): decode + validate the closed request shape (n=1, bounded
// prompt, WxH size within the pixel envelope), run app/image.Generate, and
// render the OpenAI-form url response ({created, data:[{url}]} — URL 直通).
package images

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	appimage "github.com/sunweilin/anselm/gateway/internal/app/image"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	// maxPromptChars bounds the prompt (the upstream's own ceiling is lower for
	// most models; the gateway only guards memory and obvious abuse here).
	// maxPromptChars 界 prompt(多数上游自身上限更低;网关这里只守内存与明显滥用)。
	maxPromptChars = 2000
	defaultSize    = "1024x1024"
	minSidePixels  = 256
	maxSidePixels  = 4096
	minTotalPixels = int64(512) * 512
	maxTotalPixels = int64(2048) * 2048
)

// Handler serves POST /v1/images/generations and POST /v1/images/edits. One handler, one flag,
// because the two requests differ by exactly one field and share every other guard — a second
// package would have duplicated the size envelope, the prompt bound and the n=1 union to express
// that difference.
//
// Handler 同时服务 /v1/images/generations 与 /v1/images/edits。一个 handler、一个开关,因为两种请求
// **只差一个字段**、其余每一道闸都共用——另起一个包,等于把像素包络、prompt 上限、n=1 联合复制一遍,
// 只为表达那一处差别。
type Handler struct {
	svc  *appimage.Service
	edit bool
}

// New wires the generation handler.
func New(svc *appimage.Service) *Handler { return &Handler{svc: svc} }

// NewEdit wires the editing handler.
func NewEdit(svc *appimage.Service) *Handler { return &Handler{svc: svc, edit: true} }

type requestBody struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Size   string `json:"size"`
	N      *int   `json:"n"`
	// Image is the edit source: a base64 data URL, never an address (ADR 0011). Present only on
	// /images/edits — the generations route rejects it via DisallowUnknownFields, which is what
	// keeps the two shapes from quietly merging into one permissive one.
	// Image 是改图的源:base64 data URL、绝不是地址(ADR 0011)。**只**出现在 /images/edits 上——
	// generations 那条经 DisallowUnknownFields 拒掉它,正是这一点让两个形状不会悄悄并成一个更宽松的。
	Image string `json:"image"`
}

// maxSourceChars bounds the base64 source before it reaches the app layer. 10MB of image is ~13.4M
// base64 characters; this ceiling is the memory guard, while the SHAPE guard (data: and no scheme)
// is the security one and lives in the service.
// maxSourceChars 在源图抵达 app 层之前界住它。10MB 图约 1340 万 base64 字符;这个顶棚是**内存**闸,
// 而**形状**闸(data: 且无 scheme)是**安全**闸、住在 service 里。
const maxSourceChars = 14_000_000

type responseBody struct {
	Created int64               `json:"created"`
	Data    []responseImageItem `json:"data"`
}

type responseImageItem struct {
	URL string `json:"url"`
}

// ServeHTTP guards method + shape then delegates. The request `model` is a
// logical alias and never selects an upstream (the same rule as chat); `n` must
// be absent or exactly 1 (closed union).
//
// ServeHTTP 守方法与形状后委派。请求 `model` 是逻辑别名、绝不选上游(与 chat 同律);`n` 缺席
// 或恒 1(关闭联合)。
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
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" || len([]rune(prompt)) > maxPromptChars {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	if body.N != nil && *body.N != 1 {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	size := strings.TrimSpace(body.Size)
	if size == "" {
		size = defaultSize
	}
	if !validSize(size) {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}

	installID := r.Header.Get(proofhttp.HeaderInstallID)
	var (
		imageURL string
		ae       *apierr.APIError
	)
	if h.edit {
		if body.Image == "" || len(body.Image) > maxSourceChars {
			response.WriteError(w, apierr.ErrImageSourceInvalid)
			return
		}
		imageURL, ae = h.svc.Edit(r.Context(), installID, prompt, size, body.Image)
	} else {
		imageURL, ae = h.svc.Generate(r.Context(), installID, prompt, size)
	}
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, responseBody{
		Created: time.Now().Unix(),
		Data:    []responseImageItem{{URL: imageURL}},
	})
}

// validSize accepts the WxH wire form within the pixel envelope (side bounds +
// total-pixel bounds — the 2.0-series contract).
//
// validSize 收 WxH 线缆形且在像素包络内(边界 + 总像素——2.0 系契约)。
func validSize(s string) bool {
	w, hgt, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok {
		return false
	}
	wd, err1 := strconv.Atoi(w)
	ht, err2 := strconv.Atoi(hgt)
	if err1 != nil || err2 != nil {
		return false
	}
	if wd < minSidePixels || wd > maxSidePixels || ht < minSidePixels || ht > maxSidePixels {
		return false
	}
	total := int64(wd) * int64(ht)
	return total >= minTotalPixels && total <= maxTotalPixels
}
