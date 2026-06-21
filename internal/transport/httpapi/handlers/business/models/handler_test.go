package models

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	dommodel "github.com/sunweilin/anselm/gateway/internal/domain/model"
)

type stubAuth struct {
	status dominstall.Status
	found  bool
	err    error
}

func (s stubAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return "ins", s.status, s.found, s.err
}

type stubCatalog struct{ env dommodel.ListEnvelope }

func (s stubCatalog) List() dommodel.ListEnvelope { return s.env }

func authOK() stubAuth { return stubAuth{status: dominstall.StatusActive, found: true} }

func TestModels_Success(t *testing.T) {
	env := dommodel.ListEnvelope{
		Object: dommodel.ObjectList,
		Data:   []dommodel.Model{{ID: "deepseek-chat", Object: dommodel.ObjectModel, OwnedBy: dommodel.OwnedBy}},
	}
	h := New(authOK(), stubCatalog{env: env})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var got dommodel.ListEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 1 || got.Data[0].ID != "deepseek-chat" {
		t.Fatalf("body: %+v", got)
	}
}

func TestModels_NoBearer401(t *testing.T) {
	h := New(authOK(), stubCatalog{})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrInvalidToken.Status {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestModels_Banned403(t *testing.T) {
	h := New(stubAuth{status: dominstall.StatusBanned, found: true}, stubCatalog{})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != apierr.ErrAccountBanned.Status {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ACCOUNT_BANNED") {
		t.Fatalf("code: %s", rec.Body.String())
	}
}

func TestModels_MethodNotAllowed(t *testing.T) {
	h := New(authOK(), stubCatalog{})
	r := httptest.NewRequest("POST", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST must be 405, got %d", rec.Code)
	}
}
