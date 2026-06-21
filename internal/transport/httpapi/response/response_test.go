package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

func TestWriteJSON_BareEntityNoWrapper(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, map[string]any{"object": "list", "data": []int{1, 2}})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type: %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, wrapped := m["data"]; !wrapped {
		t.Fatalf("bare entity should expose its own fields, got %v", m)
	}
	if _, hasErr := m["error"]; hasErr {
		t.Fatalf("success must not be wrapped in an error envelope: %v", m)
	}
}

func TestWriteError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, apierr.ErrRateLimited)
	if rec.Code != 429 {
		t.Fatalf("status: %d", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "RATE_LIMITED" || env.Error.Message == "" {
		t.Fatalf("envelope: %+v", env.Error)
	}
}

func TestWriteError_NonAPIErrorNormalizesTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, errString("boom: secret detail"))
	if rec.Code != 500 {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "INTERNAL") {
		t.Fatalf("expected INTERNAL code: %s", body)
	}
	if contains(body, "secret detail") {
		t.Fatalf("internal detail leaked: %s", body)
	}
}

func TestWriteError_RetryAfterFromDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, apierr.LoginLocked(42))
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After header: %q", got)
	}
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Details["retryAfterSec"] == nil {
		t.Fatalf("details should carry retryAfterSec: %+v", env.Error.Details)
	}
}

func TestBearer(t *testing.T) {
	mk := func(h string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}
	if got := Bearer(mk("Bearer tok123")); got != "tok123" {
		t.Fatalf("bearer: %q", got)
	}
	if got := Bearer(mk("Basic abc")); got != "" {
		t.Fatalf("non-bearer must be empty: %q", got)
	}
	if got := Bearer(mk("")); got != "" {
		t.Fatalf("absent must be empty: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
