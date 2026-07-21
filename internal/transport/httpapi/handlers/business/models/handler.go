// Package models is the thin HTTP handler for GET /v1/models: device proof → install
// lookup → app/model.List → the OpenAI list envelope (§7). Read-only discovery
// surface — it reserves no quota and bills nothing — but it IS device-proof-gated
// (anti-anonymous-scrape) via the SAME shared auth as chat/quota. The catalog is
// the single live provider-neutral model id (hot-reload reflected next request); this handler only
// guards the method, runs the auth tree (§2), and serializes the envelope verbatim.
package models

import (
	"context"
	"net/http"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	dommodel "github.com/sunweilin/anselm/gateway/internal/domain/model"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/response"
)

// Authenticator is the shared install-id status lookup (§2) — the same port the chat
// and quota handlers use; one auth implementation, never a second validator.
type Authenticator interface {
	LookupInstall(ctx context.Context, installID string) (id string, status dominstall.Status, found bool, err error)
}

// Catalog is the app/model surface: it assembles the OpenAI envelope from the live
// logical model id. An interface so the handler unit-tests against a stub; *app/model.Catalog
// satisfies it structurally.
type Catalog interface {
	List() dommodel.ListEnvelope
}

// Handler serves GET /v1/models.
type Handler struct {
	auth    Authenticator
	catalog Catalog
}

// New wires the handler to the auth lookup + the model catalog.
func New(auth Authenticator, catalog Catalog) *Handler {
	return &Handler{auth: auth, catalog: catalog}
}

// ServeHTTP guards GET, runs the shared auth tree, and serializes the live
// logical-model envelope verbatim. Unlike the other endpoints this returns the OpenAI
// {object,data} list wrapper (a deliberate compatibility deviation from the
// bare-entity rule, §7), produced wholesale by app/model so the wire shape has a
// single home.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.WriteErrorWith(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "method not allowed")
		return
	}
	if ae := authGate(r.Context(), h.auth, r.Header.Get(proofhttp.HeaderInstallID)); ae != nil {
		response.WriteError(w, ae)
		return
	}
	response.WriteJSON(w, http.StatusOK, h.catalog.List())
}

// authGate runs the §2 status decision tree (empty/!found → INVALID_INSTALL; err →
// INTERNAL; banned → ACCOUNT_BANNED). Models needs only the verdict, not the id.
func authGate(ctx context.Context, auth Authenticator, installID string) *apierr.APIError {
	if installID == "" {
		return apierr.ErrInvalidInstall
	}
	_, status, found, err := auth.LookupInstall(ctx, installID)
	if err != nil {
		return apierr.Internal()
	}
	if !found {
		return apierr.ErrInvalidInstall
	}
	if status == dominstall.StatusBanned {
		return apierr.ErrAccountBanned
	}
	return nil
}
