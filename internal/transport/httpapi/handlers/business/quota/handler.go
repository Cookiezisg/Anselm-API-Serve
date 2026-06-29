// Package quota is the thin HTTP handler for GET /v1/quota: bearer → install
// lookup → app/quota.View → bare {limit,used,remaining,resetAt,available} entity.
// The availability semantics live in app/quota (not here) so every caller agrees
// on what "available" means; this handler only guards the method, runs the shared
// auth decision tree (§2), and renders. It mirrors the X-Quota-Limit / X-Quota-
// Reset headers the chat path sets so a client polling quota sees the same pair.
package quota

import (
	"context"
	"net/http"
	"strconv"
	"time"

	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Authenticator is the shared bearer→install lookup (§2). The same port the chat
// and models handlers use — ONE auth implementation (*app/install.Service),
// never a second validator.
type Authenticator interface {
	LookupInstall(ctx context.Context, token string) (id string, status dominstall.Status, found bool, err error)
}

// Viewer is the app/quota read-model surface. An interface so the handler unit-
// tests against a stub; *app/quota.Service satisfies it structurally.
type Viewer interface {
	SnapshotPeriod(now time.Time) domquota.Period
	View(ctx context.Context, installID string, p domquota.Period) (*appquota.View, error)
}

// Handler serves GET /v1/quota.
type Handler struct {
	auth Authenticator
	view Viewer
}

// New wires the handler to the auth lookup + the quota read model.
func New(auth Authenticator, view Viewer) *Handler { return &Handler{auth: auth, view: view} }

// body is the success entity (§6). `used` is the authoritative monthly count;
// `available` folds the remaining + day-budget rule (owned by app/quota).
type body struct {
	Limit     int64  `json:"limit"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	ResetAt   string `json:"resetAt"`
	Available bool   `json:"available"`
}

// ServeHTTP guards GET, runs the shared auth tree, reads the view, and renders.
// X-Quota-Limit / X-Quota-Reset are set before the body so a header-only client
// (matching the chat path) need not parse JSON.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	installID, ae := authInstall(r.Context(), h.auth, response.Bearer(r))
	if ae != nil {
		response.WriteError(w, ae)
		return
	}

	p := h.view.SnapshotPeriod(time.Now())
	v, err := h.view.View(r.Context(), installID, p)
	if err != nil {
		response.WriteErrorWith(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	w.Header().Set("X-Quota-Limit", strconv.FormatInt(v.Limit, 10))
	w.Header().Set("X-Quota-Reset", v.ResetAt.Format(time.RFC3339))
	response.WriteJSON(w, http.StatusOK, body{
		Limit:     v.Limit,
		Used:      v.Used,
		Remaining: v.Remaining,
		ResetAt:   v.ResetAt.Format(time.RFC3339),
		Available: v.Available,
	})
}

// authInstall runs the §2 auth decision tree shared by the token-gated endpoints:
// empty/!found → INVALID_TOKEN (401); err → INTERNAL (500); banned → ACCOUNT_BANNED
// (403). Returns the install id on success. Exported-shape duplicated per package
// would drift, so this is the one place quota encodes the tree.
func authInstall(ctx context.Context, auth Authenticator, token string) (string, *apierr.APIError) {
	if token == "" {
		return "", apierr.ErrInvalidToken
	}
	id, status, found, err := auth.LookupInstall(ctx, token)
	if err != nil {
		return "", apierr.Internal()
	}
	if !found {
		return "", apierr.ErrInvalidToken
	}
	if status == dominstall.StatusBanned {
		return "", apierr.ErrAccountBanned
	}
	return id, nil
}
