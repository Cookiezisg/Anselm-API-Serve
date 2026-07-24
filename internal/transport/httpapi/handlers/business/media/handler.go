// Package media is the proof-gated durable-media HTTP boundary. It accepts only
// fixed-size raw chunks; bytes are never parsed as multipart or inserted into a
// chat request, so a model prompt cannot accidentally carry raw media payloads.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	appmedia "github.com/sunweilin/anselm/gateway/internal/app/media"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

const HeaderUploadOffset = "Upload-Offset"

type Authenticator interface {
	LookupInstall(ctx context.Context, installID string) (id string, status dominstall.Status, found bool, err error)
}

type Service interface {
	Create(ctx context.Context, in appmedia.CreateInput) (*dmedia.Upload, error)
	Append(ctx context.Context, installID, uploadID string, expectedOffset int64, chunk []byte) (*dmedia.Upload, error)
	Complete(ctx context.Context, installID, uploadID string) (*appmedia.PrivateLease, error)
	OpenLease(ctx context.Context, leaseID, token string) (*appmedia.LeaseSource, error)
}

type Handler struct {
	auth          Authenticator
	svc           Service // nil is the deliberate MEDIA_ENABLED=false state.
	chunkMaxBytes int64
}

func New(auth Authenticator, svc Service, chunkMaxBytes int64) *Handler {
	return &Handler{auth: auth, svc: svc, chunkMaxBytes: chunkMaxBytes}
}

type createRequest struct {
	SHA256     string `json:"sha256"`
	MIMEType   string `json:"mimeType"`
	TotalBytes int64  `json:"totalBytes"`
}

type uploadResponse struct {
	UploadID      string `json:"uploadId"`
	Offset        int64  `json:"offset"`
	ExpiresAt     string `json:"expiresAt"`
	ChunkMaxBytes int64  `json:"chunkMaxBytes"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	installID, ok := h.authorize(w, r)
	if !ok {
		return
	}
	if !h.available(w) {
		return
	}
	var in createRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		response.WriteError(w, apierr.ErrMediaUploadInvalid)
		return
	}
	u, err := h.svc.Create(r.Context(), appmedia.CreateInput{InstallID: installID, ExpectedSHA256: in.SHA256, MIMEType: in.MIMEType, TotalBytes: in.TotalBytes})
	if err != nil {
		h.writeAppError(w, err)
		return
	}
	h.writeUpload(w, u)
}

func (h *Handler) Append(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	installID, ok := h.authorize(w, r)
	if !ok || !h.available(w) {
		return
	}
	offset, err := parseOffset(r.Header.Get(HeaderUploadOffset))
	if err != nil {
		response.WriteError(w, apierr.ErrMediaUploadInvalid)
		return
	}
	chunk, err := io.ReadAll(r.Body)
	if err != nil || int64(len(chunk)) > h.chunkMaxBytes {
		response.WriteError(w, apierr.ErrMediaUploadInvalid)
		return
	}
	u, err := h.svc.Append(r.Context(), installID, r.PathValue("uploadId"), offset, chunk)
	if err != nil {
		h.writeAppError(w, err)
		return
	}
	h.writeUpload(w, u)
}

func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	installID, ok := h.authorize(w, r)
	if !ok || !h.available(w) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 {
		response.WriteError(w, apierr.ErrMediaUploadInvalid)
		return
	}
	lease, err := h.svc.Complete(r.Context(), installID, r.PathValue("uploadId"))
	if err != nil {
		h.writeAppError(w, err)
		return
	}
	// fetchPath is a short-lived provider capability, not an attachment URL: it
	// carries no install/path/hash and is useless without its HMAC query token.
	response.WriteJSON(w, http.StatusCreated, struct {
		LeaseID   string `json:"leaseId"`
		FetchPath string `json:"fetchPath"`
		MIMEType  string `json:"mimeType"`
		SizeBytes int64  `json:"sizeBytes"`
		ExpiresAt string `json:"expiresAt"`
	}{LeaseID: lease.Lease.ID, FetchPath: fetchPath(lease.Lease.ID, lease.FetchToken), MIMEType: lease.Lease.MIMEType, SizeBytes: lease.Lease.SizeBytes, ExpiresAt: lease.Lease.ExpiresAt.Format(time.RFC3339)})
}

// Fetch is intentionally not proof-wrapped: the upstream model fetcher cannot
// carry a desktop's Ed25519 proof. The expiring HMAC capability is the complete
// authorization boundary; every failure deliberately renders the same 404.
func (h *Handler) Fetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !h.availableFetch(w) {
		return
	}
	source, err := h.svc.OpenLease(r.Context(), r.PathValue("leaseId"), r.URL.Query().Get("token"))
	if err != nil {
		response.WriteError(w, apierr.ErrMediaLeaseNotFound)
		return
	}
	defer func() { _ = source.Body.Close() }()
	w.Header().Set("Content-Type", source.MIMEType)
	w.Header().Set("Content-Length", strconv.FormatInt(source.SizeBytes, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-site")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, source.Body)
}

func (h *Handler) availableFetch(w http.ResponseWriter) bool {
	if h.svc == nil {
		response.WriteError(w, apierr.ErrMediaLeaseNotFound)
		return false
	}
	return true
}

func fetchPath(leaseID, token string) string {
	return "/v1/media/leases/" + leaseID + "/content?token=" + url.QueryEscape(token)
}

func (h *Handler) available(w http.ResponseWriter) bool {
	if h.svc == nil {
		response.WriteError(w, apierr.ErrMediaUnavailable)
		return false
	}
	return true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.auth == nil {
		response.WriteError(w, apierr.Internal())
		return "", false
	}
	id := r.Header.Get(proofhttp.HeaderInstallID)
	if id == "" {
		response.WriteError(w, apierr.ErrInvalidInstall)
		return "", false
	}
	got, status, found, err := h.auth.LookupInstall(r.Context(), id)
	if err != nil {
		response.WriteError(w, apierr.Internal())
		return "", false
	}
	if !found {
		response.WriteError(w, apierr.ErrInvalidInstall)
		return "", false
	}
	if status == dominstall.StatusBanned {
		response.WriteError(w, apierr.ErrAccountBanned)
		return "", false
	}
	return got, true
}

func (h *Handler) writeUpload(w http.ResponseWriter, u *dmedia.Upload) {
	response.WriteJSON(w, http.StatusOK, uploadResponse{UploadID: u.ID, Offset: u.ReceivedBytes, ExpiresAt: u.ExpiresAt.Format(time.RFC3339), ChunkMaxBytes: h.chunkMaxBytes})
}

func (h *Handler) writeAppError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, appmedia.ErrInvalidInput):
		response.WriteError(w, apierr.ErrMediaUploadInvalid)
	case errors.Is(err, appmedia.ErrNotFound):
		response.WriteError(w, apierr.ErrMediaUploadNotFound)
	case errors.Is(err, appmedia.ErrConflict):
		response.WriteError(w, apierr.ErrMediaUploadConflict)
	case errors.Is(err, appmedia.ErrIntegrity):
		response.WriteError(w, apierr.ErrMediaIntegrityFailed)
	default:
		response.WriteError(w, apierr.Internal())
	}
}

func parseOffset(raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, errors.New("missing offset")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid offset")
	}
	return n, nil
}
