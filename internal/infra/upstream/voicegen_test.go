package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

func TestDeleteVoice_ProviderAlreadyAbsentIsIdempotent(t *testing.T) {
	for _, code := range []string{"InvalidParameter.ResourceNotExist", "BadRequest.VoiceNotFound"} {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/services/audio/tts/customization" {
					t.Fatalf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":"` + code + `","message":"voice is gone"}`))
			}))
			defer srv.Close()

			gen := &VoiceGen{c: nativeClient{
				base: srv.URL, apiKey: "sk-test", httpc: srv.Client(), timeout: time.Second,
			}}
			err := gen.DeleteVoice(context.Background(), "upstream-voice-1")
			if !errors.Is(err, domvoice.ErrUpstreamAlreadyAbsent) {
				t.Fatalf("err = %v, want ErrUpstreamAlreadyAbsent", err)
			}
		})
	}
}

func TestDeleteVoice_GenericBadRequestIsNotIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"bad request"}`))
	}))
	defer srv.Close()

	gen := &VoiceGen{c: nativeClient{
		base: srv.URL, apiKey: "sk-test", httpc: srv.Client(), timeout: time.Second,
	}}
	err := gen.DeleteVoice(context.Background(), "upstream-voice-1")
	var ae *apierr.APIError
	if !errors.As(err, &ae) || ae.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("err = %v, want %s", err, apierr.CodeUpstreamRejected)
	}
	if errors.Is(err, domvoice.ErrUpstreamAlreadyAbsent) {
		t.Fatal("generic provider 400 must not release the local pointer")
	}
}

func TestVoiceGen_AlreadyAbsentCodeOnlyAppliesToDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"InvalidParameter.ResourceNotExist"}`))
	}))
	defer srv.Close()

	gen := &VoiceGen{c: nativeClient{
		base: srv.URL, apiKey: "sk-test", httpc: srv.Client(), timeout: time.Second,
	}}
	err := gen.AwaitVoiceReady(context.Background(), "upstream-voice-1")
	var ae *apierr.APIError
	if !errors.As(err, &ae) || ae.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("err = %v, want %s", err, apierr.CodeUpstreamRejected)
	}
	if errors.Is(err, domvoice.ErrUpstreamAlreadyAbsent) {
		t.Fatal("the provider missing code on query_voice must not be treated as delete success")
	}
}
