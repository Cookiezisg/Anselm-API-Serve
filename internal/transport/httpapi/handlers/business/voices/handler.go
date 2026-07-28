// Package voices is the thin HTTP handler for the cloned-voice lifecycle (WRK-082 H9):
// POST /v1/voices (enroll), GET /v1/voices (list), POST /v1/voices:delete.
//
// **Delete is a POST with the id in the body, not DELETE /v1/voices/{id}, and that is deliberate.**
// Every managed route on this gateway is authenticated by a device proof carried in headers over a
// POST; a path-parameter DELETE would be the only shape in the surface, and the desktop's managed
// client would need a second request builder for one verb. The `:action` suffix is the project's
// own convention for exactly this (N5), and it keeps voice ids out of URLs — out of proxy logs,
// browser history and referrers — for a resource that costs real money to recreate.
//
// Package voices 是克隆音色生命周期的薄 HTTP handler(H9):POST /v1/voices(登记)、
// GET /v1/voices(列出)、POST /v1/voices:delete(删除)。
//
// **删除是「POST + id 在体里」而非 DELETE /v1/voices/{id},这是刻意的。** 本网关上每一条受管路由都由
// 走在 header 里的 device proof 鉴权、且都是 POST;一条带路径参数的 DELETE 会是整个面上唯一的异形,
// 而桌面侧的受管 client 得为这一个动词再写一套请求构造。`:action` 后缀正是本项目为这种情况定的约定
// (N5),而且它让音色 id 不进 URL——不进代理日志、浏览器历史与 referrer——对一个重建要花真钱的资源
// 而言,这是免费拿到的好处。
package voices

import (
	"encoding/json"
	"net/http"
	"strings"

	appvoice "github.com/sunweilin/anselm/gateway/internal/app/voice"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	// maxNameChars bounds the voice name. The name is the handle a later synthesis uses, so it is
	// short by nature; the bound is a memory guard, not a product opinion.
	// maxNameChars 界音色名。名字是此后合成用的把手,天然就短;这个界是内存闸、不是产品意见。
	maxNameChars = 120
)

// Handler serves the three voice routes.
//
// Handler 服务那三条音色路由。
type Handler struct {
	svc    *appvoice.Service
	action action
}

type action int

const (
	actionEnroll action = iota
	actionList
	actionDelete
)

// NewEnroll / NewList / NewDelete wire the three verbs onto one handler type — they share the
// install header, the error rendering and the response envelope, and differ only in what they do.
//
// NewEnroll / NewList / NewDelete 把三个动词接到同一个 handler 类型上——它们共用 install 头、错误
// 渲染与响应信封,只在**做什么**上不同。
func NewEnroll(svc *appvoice.Service) *Handler { return &Handler{svc: svc, action: actionEnroll} }
func NewList(svc *appvoice.Service) *Handler   { return &Handler{svc: svc, action: actionList} }
func NewDelete(svc *appvoice.Service) *Handler { return &Handler{svc: svc, action: actionDelete} }

type enrollRequest struct {
	Name string `json:"name"`
	// LeaseID names a media lease this install already uploaded — **never an address**. ADR 0011's
	// inbound half is untouched: a caller cannot hand this gateway a URL to fetch. The gateway
	// resolves the lease into ITS OWN public address on the outbound hop, because
	// `voice-enrollment` accepts nothing else (真机实测).
	//
	// The clip travels through the ordinary resumable upload, so a 30-second sample never has to
	// fit in one JSON body.
	//
	// LeaseID 指名一个本 install 已经上传好的媒体 lease——**绝不是地址**。ADR 0011 的入站那半原样
	// 不动:调用方不能递给本网关一个 URL 让它去取。网关在**出站**那一跳把 lease 解析成**它自己的**
	// 公开地址,因为 `voice-enrollment` 别的什么都不收(真机实测)。
	//
	// 音频走普通的断点上传,故一段 30 秒的样本永远不必塞进一个 JSON body。
	LeaseID string `json:"leaseId"`
}

type deleteRequest struct {
	VoiceID string `json:"voiceId"`
}

type voiceItem struct {
	VoiceID   string `json:"voiceId"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
}

// listResponse carries the inventory arithmetic alongside the rows, because the cap is the whole
// reason a caller reads this: a list of two that does not say "that is all you may keep" leaves the
// next enrollment's refusal unexplained.
//
// listResponse 让库存算术与行同行,因为**上限正是调用方来读它的理由**:一个列出两行却不说「你只能留
// 这些」的列表,会让下一次登记的拒绝无从解释。
type listResponse struct {
	Voices    []voiceItem `json:"voices"`
	Capacity  int         `json:"capacity"`
	Remaining int         `json:"remaining"`
}

// ServeHTTP guards method + shape then delegates.
//
// ServeHTTP 守方法与形状后委派。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	installID := r.Header.Get(proofhttp.HeaderInstallID)
	switch h.action {
	case actionList:
		if r.Method != http.MethodGet {
			response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
			return
		}
		h.list(w, r, installID)
	case actionEnroll:
		if r.Method != http.MethodPost {
			response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
			return
		}
		h.enroll(w, r, installID)
	case actionDelete:
		if r.Method != http.MethodPost {
			response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
			return
		}
		h.del(w, r, installID)
	}
}

func (h *Handler) enroll(w http.ResponseWriter, r *http.Request, installID string) {
	var body enrollRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len([]rune(name)) > maxNameChars {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	if strings.TrimSpace(body.LeaseID) == "" {
		response.WriteError(w, apierr.ErrVoiceSampleInvalid)
		return
	}
	v, ae := h.svc.Enroll(r.Context(), installID, name, strings.TrimSpace(body.LeaseID))
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, voiceItem{
		VoiceID: v.ID, Name: v.Name, CreatedAt: v.CreatedAt.Unix(),
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, installID string) {
	rows, ae := h.svc.List(r.Context(), installID)
	if ae != nil {
		response.WriteError(w, ae)
		return
	}
	// A non-nil empty slice so an empty inventory serialises as [] rather than null.
	// 非 nil 的空切片,使空库存序列化成 [] 而非 null。
	items := make([]voiceItem, 0, len(rows))
	for _, v := range rows {
		items = append(items, voiceItem{VoiceID: v.ID, Name: v.Name, CreatedAt: v.CreatedAt.Unix()})
	}
	response.WriteJSON(w, http.StatusOK, listResponse{
		Voices:    items,
		Capacity:  domvoice.PerInstallInventory,
		Remaining: max(0, domvoice.PerInstallInventory-len(items)),
	})
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request, installID string) {
	var body deleteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	id := strings.TrimSpace(body.VoiceID)
	if id == "" {
		response.WriteError(w, apierr.ErrBadRequest)
		return
	}
	if ae := h.svc.Delete(r.Context(), installID, id); ae != nil {
		response.WriteError(w, ae)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
