package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type memFiles struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (f *memFiles) Create(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[id] = nil
	return nil
}
func (f *memFiles) Append(_ context.Context, id string, offset int64, b []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if int64(len(f.data[id])) != offset {
		return 0, errors.New("offset")
	}
	f.data[id] = append(f.data[id], b...)
	return int64(len(f.data[id])), nil
}
func (f *memFiles) Size(_ context.Context, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.data[id])), nil
}
func (f *memFiles) Truncate(_ context.Context, id string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 || n > int64(len(f.data[id])) {
		return errors.New("offset")
	}
	f.data[id] = f.data[id][:n]
	return nil
}
func (f *memFiles) SHA256(_ context.Context, id string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256(f.data[id])
	return hex.EncodeToString(sum[:]), int64(len(f.data[id])), nil
}
func (f *memFiles) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, id)
	return nil
}

type memRepo struct {
	u             *dmedia.Upload
	lease         *dmedia.Lease
	rejectAdvance bool
}

func (r *memRepo) Expire(_ context.Context, now time.Time) error {
	if r.u != nil && r.u.State == dmedia.UploadOpen && !r.u.ExpiresAt.After(now) {
		r.u.State = dmedia.UploadExpired
	}
	if r.lease != nil && r.lease.State == dmedia.LeaseActive && !r.lease.ExpiresAt.After(now) {
		r.lease.State = dmedia.LeaseExpired
	}
	return nil
}
func (r *memRepo) OpenUploads(_ context.Context) ([]dmedia.Upload, error) {
	if r.u == nil || r.u.State != dmedia.UploadOpen {
		return nil, nil
	}
	return []dmedia.Upload{*r.u}, nil
}
func (r *memRepo) ExpiredFileIDs(_ context.Context) ([]string, error) {
	if r.u == nil {
		return nil, nil
	}
	if r.u.State == dmedia.UploadExpired || (r.lease != nil && r.lease.State == dmedia.LeaseExpired) {
		return []string{r.u.ID}, nil
	}
	return nil, nil
}
func (r *memRepo) ExpireOpen(_ context.Context, id string, _ time.Time) error {
	if r.u != nil && r.u.ID == id && r.u.State == dmedia.UploadOpen {
		r.u.State = dmedia.UploadExpired
	}
	return nil
}
func (r *memRepo) AcknowledgeRemoved(_ context.Context, id string, _ time.Time) error {
	if r.u != nil && r.u.ID == id && r.u.State == dmedia.UploadExpired {
		r.u = nil
		return nil
	}
	if r.lease != nil && r.lease.UploadID == id && r.lease.State == dmedia.LeaseExpired {
		r.lease.State = dmedia.LeaseDeleted
	}
	return nil
}

func (r *memRepo) CreateUpload(_ context.Context, u dmedia.Upload) error { r.u = &u; return nil }
func (r *memRepo) GetUploadForInstall(_ context.Context, install, id string) (*dmedia.Upload, bool, error) {
	if r.u == nil || r.u.InstallID != install || r.u.ID != id {
		return nil, false, nil
	}
	cp := *r.u
	return &cp, true, nil
}
func (r *memRepo) AdvanceReceived(_ context.Context, install, id string, expected, next int64, _ time.Time) (bool, error) {
	if r.rejectAdvance || r.u == nil || r.u.InstallID != install || r.u.ID != id || r.u.ReceivedBytes != expected {
		return false, nil
	}
	r.u.ReceivedBytes = next
	return true, nil
}
func (r *memRepo) CompleteUpload(_ context.Context, install, id, sha string, _ time.Time, l dmedia.Lease) (*dmedia.Lease, bool, error) {
	if r.u == nil || r.lease != nil || r.u.InstallID != install || r.u.ID != id || r.u.ReceivedBytes != r.u.TotalBytes || r.u.ExpectedSHA256 != sha {
		return nil, false, nil
	}
	r.u.State = dmedia.UploadCompleted
	r.lease = &l
	return r.lease, true, nil
}

func testService(t *testing.T, repo *memRepo, files *memFiles) *Service {
	t.Helper()
	signer, err := NewHMACSigner([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	return New(repo, files, fixedClock{time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)}, Limits{MaxBytes: 1024, UploadTTL: time.Hour, LeaseTTL: time.Hour}, signer)
}

func TestCreateAppendCompleteReturnsOnlyOpaqueLease(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	b := []byte("payload")
	sum := sha256.Sum256(b)
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Complete(context.Background(), "ins_a", u.ID)
	if err != nil || got.Lease == nil || got.Lease.ID == "" || got.FetchToken == "" || got.Lease.FetchTokenHash == got.FetchToken {
		t.Fatalf("complete=%+v err=%v", got, err)
	}
	if got.Lease.UploadID != u.ID || got.Lease.InstallID != "ins_a" {
		t.Fatalf("lease binding=%+v", got.Lease)
	}
	signer, _ := NewHMACSigner([]byte("01234567890123456789012345678901"))
	if !signer.Verify(got.FetchToken, got.Lease.ID, got.Lease.InstallID, got.Lease.ExpiresAt) {
		t.Fatal("completion token must be deterministic and restart-verifiable")
	}
}

func TestAppendLostCursorCASTruncatesUnrecordedTail(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	sum := sha256.Sum256([]byte("abc"))
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	repo.rejectAdvance = true
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, []byte("abc")); !errors.Is(err, ErrConflict) {
		t.Fatalf("append err=%v", err)
	}
	if n, _ := files.Size(context.Background(), u.ID); n != 0 {
		t.Fatalf("unrecorded tail=%d", n)
	}
}

func TestAppendRejectsStaleClientOffsetBeforeWriting(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	sum := sha256.Sum256([]byte("abc"))
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 1, []byte("abc")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale offset error=%v", err)
	}
	if n, _ := files.Size(context.Background(), u.ID); n != 0 {
		t.Fatalf("stale offset wrote %d bytes", n)
	}
}

func TestRecoverTruncatesCrashTailAndRemovesExpiredStaging(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	sum := sha256.Sum256([]byte("ab"))
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after fsync("abc") and before the DB cursor from 2 to 3.
	files.data[u.ID] = []byte("abc")
	repo.u.ReceivedBytes = 2
	if n, err := svc.Recover(context.Background()); err != nil || n != 0 {
		t.Fatalf("tail recover=(%d,%v)", n, err)
	}
	if got := string(files.data[u.ID]); got != "ab" {
		t.Fatalf("tail=%q, want ab", got)
	}
	repo.u.ExpiresAt = fixedClock{time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)}.Now()
	if n, err := svc.Recover(context.Background()); err != nil || n != 1 {
		t.Fatalf("expiry recover=(%d,%v)", n, err)
	}
	if repo.u != nil {
		t.Fatal("expired upload row must be removed after its file")
	}
	if _, ok := files.data[u.ID]; ok {
		t.Fatal("expired staging bytes retained")
	}
}
