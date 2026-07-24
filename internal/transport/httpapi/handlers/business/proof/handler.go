// Package proof adapts the device-proof use case to HTTP. It owns the protocol
// header names, the public challenge endpoint, and the protected-route wrapper.
package proof

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	appdeviceproof "github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const (
	HeaderInstallID = "X-Anselm-Install-ID"
	HeaderProof     = "X-Anselm-Proof"
	HeaderPublicKey = "X-Anselm-Public-Key"
)

// Handler exposes GET /v1/proof/challenge.
type Handler struct{ svc *appdeviceproof.Service }

func New(svc *appdeviceproof.Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	c := h.svc.IssueChallenge()
	response.WriteJSON(w, http.StatusOK, struct {
		Nonce     string `json:"nonce"`
		ExpiresAt string `json:"expiresAt"`
	}{Nonce: c.Nonce, ExpiresAt: c.ExpiresAt.Format(time.RFC3339)})
}

// Protect verifies the signed method, target and body before a protected handler
// sees the request. The body is restored byte-for-byte for the downstream decoder.
func Protect(svc *appdeviceproof.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		installID := r.Header.Get(HeaderInstallID)
		compact := r.Header.Get(HeaderProof)
		if installID == "" || compact == "" {
			response.WriteError(w, apierr.ErrDeviceProofRequired)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				response.WriteError(w, apierr.ErrRequestBodyTooLarge)
				return
			}
			response.WriteErrorWith(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if ae := svc.VerifyInstall(r.Context(), appdeviceproof.Request{
			Method: r.Method, Target: target(r), Body: body,
		}, installID, compact); ae != nil {
			response.WriteError(w, ae)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Registration verifies POST /install against the public key carried by it.
func Registration(svc *appdeviceproof.Service, r *http.Request, body []byte) (appdeviceproof.Registration, *apierr.APIError) {
	reg, ae := svc.VerifyRegistration(appdeviceproof.Request{
		Method: r.Method, Target: target(r), Body: body,
	}, r.Header.Get(HeaderPublicKey), r.Header.Get(HeaderProof))
	if ae != nil {
		return appdeviceproof.Registration{}, ae
	}
	return reg, nil
}

func target(r *http.Request) string {
	t := strings.ToLower(r.Host) + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		t += "?" + r.URL.RawQuery
	}
	return t
}
