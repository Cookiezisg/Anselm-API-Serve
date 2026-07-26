// Package images is the thin HTTP handler for POST /v1/images/generations
// (WRK-082 批B): decode + validate the closed request shape (n=1, bounded
// prompt, WxH size within the pixel envelope), run app/image.Generate, and
// render the OpenAI-form url response ({created, data:[{url}]} — URL 直通, P13).
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

// Handler serves POST /v1/images/generations.
type Handler struct {
	svc *appimage.Service
}

// New wires the handler to the image service.
func New(svc *appimage.Service) *Handler { return &Handler{svc: svc} }

type requestBody struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Size   string `json:"size"`
	N      *int   `json:"n"`
}

type responseBody struct {
	Created int64               `json:"created"`
	Data    []responseImageItem `json:"data"`
}

type responseImageItem struct {
	URL string `json:"url"`
}

// ServeHTTP guards method + shape then delegates. The request `model` is a
// logical alias and never selects an upstream (the same rule as chat); `n` must
// be absent or exactly 1 (closed union, P12).
//
// ServeHTTP 守方法与形状后委派。请求 `model` 是逻辑别名、绝不选上游(与 chat 同律);`n` 缺席
// 或恒 1(关闭联合,P12)。
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
	imageURL, ae := h.svc.Generate(r.Context(), installID, prompt, size)
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
