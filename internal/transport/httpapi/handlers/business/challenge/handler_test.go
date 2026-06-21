package challenge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type stub struct {
	challenge  string
	difficulty int
	required   bool
	err        error
}

func (s stub) MintChallenge(context.Context) (string, int, bool, error) {
	return s.challenge, s.difficulty, s.required, s.err
}

func TestChallenge_Success(t *testing.T) {
	h := New(stub{challenge: "abc", difficulty: 12, required: true})
	r := httptest.NewRequest("GET", "/v1/install/challenge", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var got body
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Challenge != "abc" || got.Difficulty != 12 || !got.Required {
		t.Fatalf("body: %+v", got)
	}
}

func TestChallenge_MintError(t *testing.T) {
	h := New(stub{err: errors.New("boom")})
	r := httptest.NewRequest("GET", "/v1/install/challenge", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "INTERNAL") || strings.Contains(got, "boom") {
		t.Fatalf("must render INTERNAL without leaking detail: %s", got)
	}
}

func TestChallenge_MethodNotAllowed(t *testing.T) {
	h := New(stub{})
	r := httptest.NewRequest("POST", "/v1/install/challenge", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST must be 405, got %d", rec.Code)
	}
}
