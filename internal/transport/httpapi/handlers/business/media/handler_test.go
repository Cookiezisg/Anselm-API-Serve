package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appmedia "github.com/sunweilin/anselm/gateway/internal/app/media"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
	proofhttp "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/business/proof"
)

type fakeAuth struct{ found bool }

func (a fakeAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return "ins_a", dominstall.StatusActive, a.found, nil
}

type fakeService struct {
	created      appmedia.CreateInput
	appendOffset int64
	appendChunk  []byte
	err          error
}

func (s *fakeService) Create(_ context.Context, in appmedia.CreateInput) (*dmedia.Upload, error) {
	s.created = in
	if s.err != nil {
		return nil, s.err
	}
	return &dmedia.Upload{ID: "mup_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReceivedBytes: 0, ExpiresAt: time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)}, nil
}
func (s *fakeService) Status(_ context.Context, _ string, _ string) (*dmedia.Upload, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &dmedia.Upload{ID: "mup_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReceivedBytes: 2, ExpiresAt: time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)}, nil
}
func (s *fakeService) Append(_ context.Context, _ string, _ string, offset int64, chunk []byte) (*dmedia.Upload, error) {
	s.appendOffset, s.appendChunk = offset, append([]byte(nil), chunk...)
	if s.err != nil {
		return nil, s.err
	}
	return &dmedia.Upload{ID: "mup_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReceivedBytes: offset + int64(len(chunk)), ExpiresAt: time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)}, nil
}
func (s *fakeService) Complete(context.Context, string, string) (*appmedia.PrivateLease, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &appmedia.PrivateLease{Lease: &dmedia.Lease{ID: "mls_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MIMEType: "image/png", SizeBytes: 3, ExpiresAt: time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)}, FetchToken: "private-must-not-leak"}, nil
}
func (s *fakeService) OpenLease(context.Context, string, string) (*appmedia.LeaseSource, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &appmedia.LeaseSource{MIMEType: "image/png", SizeBytes: 3, Body: io.NopCloser(bytes.NewReader([]byte("abc")))}, nil
}

func newHandler(s Service) *Handler { return New(fakeAuth{found: true}, s, 4) }

func signedRequest(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set(proofhttp.HeaderInstallID, "ins_a")
	return r
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Error.Code
}

func TestCreateStrictlyValidatesAndUsesAuthenticatedInstall(t *testing.T) {
	s := &fakeService{}
	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Create(rec, signedRequest(http.MethodPost, "/v1/media/uploads", []byte(`{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mimeType":"image/png","totalBytes":3}`)))
	if rec.Code != http.StatusOK || s.created.InstallID != "ins_a" || s.created.TotalBytes != 3 {
		t.Fatalf("create code=%d input=%+v", rec.Code, s.created)
	}
	rec = httptest.NewRecorder()
	h.Create(rec, signedRequest(http.MethodPost, "/v1/media/uploads", []byte(`{"totalBytes":3,"unknown":1}`)))
	if rec.Code != http.StatusBadRequest || code(t, rec) != "MEDIA_UPLOAD_INVALID" {
		t.Fatalf("strict JSON=%d %s", rec.Code, rec.Body.String())
	}
}

func TestAppendRequiresExactOffsetAndChunkBound(t *testing.T) {
	s := &fakeService{}
	h := newHandler(s)
	r := signedRequest(http.MethodPut, "/v1/media/uploads/mup_a", []byte("abc"))
	r.SetPathValue("uploadId", "mup_a")
	r.Header.Set(HeaderUploadOffset, "12")
	rec := httptest.NewRecorder()
	h.Append(rec, r)
	if rec.Code != http.StatusOK || s.appendOffset != 12 || string(s.appendChunk) != "abc" {
		t.Fatalf("append code=%d offset=%d chunk=%q", rec.Code, s.appendOffset, s.appendChunk)
	}
	r = signedRequest(http.MethodPut, "/v1/media/uploads/mup_a", []byte("12345"))
	r.SetPathValue("uploadId", "mup_a")
	r.Header.Set(HeaderUploadOffset, "0")
	rec = httptest.NewRecorder()
	h.Append(rec, r)
	if rec.Code != http.StatusBadRequest || code(t, rec) != "MEDIA_UPLOAD_INVALID" {
		t.Fatalf("oversized chunk=%d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusReturnsOnlyOwnedOpenCursor(t *testing.T) {
	h := newHandler(&fakeService{})
	r := signedRequest(http.MethodGet, "/v1/media/uploads/mup_a", nil)
	r.SetPathValue("uploadId", "mup_a")
	rec := httptest.NewRecorder()
	h.Status(rec, r)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"offset":2`)) {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
}

func TestCompleteReturnsOnlyShortLivedFetchCapability(t *testing.T) {
	h := newHandler(&fakeService{})
	r := signedRequest(http.MethodPost, "/v1/media/uploads/mup_a/complete", nil)
	r.SetPathValue("uploadId", "mup_a")
	rec := httptest.NewRecorder()
	h.Complete(rec, r)
	if rec.Code != http.StatusCreated || !bytes.Contains(rec.Body.Bytes(), []byte("fetchPath")) || bytes.Contains(rec.Body.Bytes(), []byte("fetchToken")) {
		t.Fatalf("completion capability shape: %d %s", rec.Code, rec.Body.String())
	}
	r = signedRequest(http.MethodPost, "/v1/media/uploads/mup_a/complete", []byte("not-empty"))
	r.SetPathValue("uploadId", "mup_a")
	r.ContentLength = -1 // chunked transport must not bypass the empty-body rule.
	rec = httptest.NewRecorder()
	h.Complete(rec, r)
	if rec.Code != http.StatusBadRequest || code(t, rec) != "MEDIA_UPLOAD_INVALID" {
		t.Fatalf("non-empty complete=%d %s", rec.Code, rec.Body.String())
	}
}

func TestUnavailableAndAppErrorsAreStable(t *testing.T) {
	h := New(fakeAuth{found: true}, nil, 4)
	rec := httptest.NewRecorder()
	h.Create(rec, signedRequest(http.MethodPost, "/v1/media/uploads", []byte(`{}`)))
	if rec.Code != http.StatusServiceUnavailable || code(t, rec) != "MEDIA_UNAVAILABLE" {
		t.Fatalf("disabled=%d %s", rec.Code, rec.Body.String())
	}
	h = newHandler(&fakeService{err: appmedia.ErrIntegrity})
	rec = httptest.NewRecorder()
	h.Create(rec, signedRequest(http.MethodPost, "/v1/media/uploads", []byte(`{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mimeType":"image/png","totalBytes":3}`)))
	if rec.Code != http.StatusUnprocessableEntity || code(t, rec) != "MEDIA_INTEGRITY_FAILED" {
		t.Fatalf("mapped error=%d %s", rec.Code, rec.Body.String())
	}
}

func TestFetchStreamsOnlyCapabilityAuthorizedBytes(t *testing.T) {
	h := newHandler(&fakeService{})
	r := httptest.NewRequest(http.MethodGet, "/v1/media/leases/mls_a/content?token=cap", nil)
	r.SetPathValue("leaseId", "mls_a")
	rec := httptest.NewRecorder()
	h.Fetch(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != "abc" || rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("fetch=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	h = newHandler(&fakeService{err: appmedia.ErrNotFound})
	rec = httptest.NewRecorder()
	h.Fetch(rec, r)
	if rec.Code != http.StatusNotFound || code(t, rec) != "MEDIA_LEASE_NOT_FOUND" {
		t.Fatalf("invalid capability=%d %s", rec.Code, rec.Body.String())
	}
}
