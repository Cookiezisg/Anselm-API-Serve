package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
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
func (f *memFiles) MIMEType(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if bytes.HasPrefix(f.data[id], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "image/png", nil
	}
	return "", nil
}
func (f *memFiles) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, id)
	return nil
}
func (f *memFiles) Open(_ context.Context, id string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.data[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
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
func (r *memRepo) AbortOpen(_ context.Context, install, id string, now time.Time) (bool, error) {
	if r.u == nil || r.u.InstallID != install || r.u.ID != id || r.u.State != dmedia.UploadOpen || !r.u.ExpiresAt.After(now) {
		return false, nil
	}
	r.u.State = dmedia.UploadAborted
	return true, nil
}
func (r *memRepo) AdvanceReceived(_ context.Context, install, id string, expected, next int64, _ time.Time) (bool, error) {
	if r.rejectAdvance || r.u == nil || r.u.InstallID != install || r.u.ID != id || r.u.ReceivedBytes != expected {
		return false, nil
	}
	r.u.ReceivedBytes = next
	return true, nil
}

func TestCancelAbortsBeforeDeletingAndIsRetrySafe(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	sum := sha256.Sum256([]byte("abc"))
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(context.Background(), "ins_a", u.ID); err != nil {
		t.Fatal(err)
	}
	if repo.u.State != dmedia.UploadAborted {
		t.Fatalf("state=%q, want aborted", repo.u.State)
	}
	if _, ok := files.data[u.ID]; ok {
		t.Fatal("cancel must delete staged bytes")
	}
	if err := svc.Cancel(context.Background(), "ins_a", u.ID); err != nil {
		t.Fatalf("retry cancel=%v", err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 3, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("append after cancel=%v, want conflict", err)
	}
}
func (r *memRepo) CompleteUpload(_ context.Context, install, id, sha string, _ time.Time, l dmedia.Lease) (*dmedia.Lease, bool, error) {
	if r.u == nil || r.lease != nil || r.u.InstallID != install || r.u.ID != id || r.u.ReceivedBytes != r.u.TotalBytes || r.u.ExpectedSHA256 != sha {
		return nil, false, nil
	}
	r.u.State = dmedia.UploadCompleted
	r.lease = &l
	return r.lease, true, nil
}

// GetLease makes memRepo satisfy LeaseRepository so the chat-path predicate (VerifyLease) can be
// exercised against the same fixture that mints the lease. 让 memRepo 满足 LeaseRepository,使 chat
// 路径的谓词能在**铸造它的同一夹具**上被检验。
func (r *memRepo) GetLease(_ context.Context, leaseID string) (*dmedia.Lease, bool, error) {
	if r.lease == nil || r.lease.ID != leaseID {
		return nil, false, nil
	}
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
	b := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
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

func TestCompleteRejectsDeclaredMIMENotMatchingStagedMagic(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	b := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	sum := sha256.Sum256(b)
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/jpeg", TotalBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(context.Background(), "ins_a", u.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("complete declared jpeg / actual png = %v, want integrity failure", err)
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

// VerifyLease is the chat path's authorization predicate (ADR 0011). The case that matters most is
// the one OpenLease does NOT cover: a lease whose token verifies but which belongs to ANOTHER
// install must be refused, and refused indistinguishably from "no such lease" so the endpoint is
// never an existence oracle over other installs' ids.
//
// VerifyLease 是 chat 路径的授权谓词(ADR 0011)。最要紧的正是 OpenLease **不覆盖**的那一条:token 验得过
// 但属于**别的 install** 的 lease 必须被拒,且拒得与「查无此 lease」不可区分——否则该端点会变成针对他人
// lease id 的存在性预言机。
// OpenLeaseForInstall must carry VerifyLease's ENTIRE predicate — ownership included — because it
// hands out the bytes themselves (chat inlines them upstream, ADR 0012). A weaker predicate here
// would let any install read any other install's media by naming its lease.
// OpenLeaseForInstall 必须携带 VerifyLease 的**全部**谓词——含归属——因为它交出的是字节本身(chat 将其
// 内联上游,ADR 0012)。这里谓词稍弱,任一 install 就能指名读走他人媒体。
func TestOpenLeaseForInstallBindsOwnershipAndReturnsBytes(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	b := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	sum := sha256.Sum256(b)
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Complete(context.Background(), "ins_a", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, token := got.Lease.ID, got.FetchToken

	src, err := svc.OpenLeaseForInstall(context.Background(), "ins_a", leaseID, token)
	if err != nil {
		t.Fatalf("the owning install must be able to open its own lease: %v", err)
	}
	data, err := io.ReadAll(src.Body)
	_ = src.Body.Close()
	if err != nil || !bytes.Equal(data, b) || src.MIMEType != "image/png" || src.SizeBytes != int64(len(b)) {
		t.Fatalf("opened lease must hand back the exact staged bytes + MIME + size: err=%v len=%d mime=%q size=%d", err, len(data), src.MIMEType, src.SizeBytes)
	}

	for name, call := range map[string]func() error{
		"another install": func() error { _, e := svc.OpenLeaseForInstall(context.Background(), "ins_b", leaseID, token); return e },
		"tampered token": func() error {
			_, e := svc.OpenLeaseForInstall(context.Background(), "ins_a", leaseID, token+"x")
			return e
		},
		"unknown lease": func() error {
			_, e := svc.OpenLeaseForInstall(context.Background(), "ins_a", "mls_missing", token)
			return e
		},
		"empty install": func() error { _, e := svc.OpenLeaseForInstall(context.Background(), "", leaseID, token); return e },
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s must be refused as not-found (no existence oracle), got %v", name, err)
		}
	}
}

func TestVerifyLeaseBindsToTheRequestingInstall(t *testing.T) {
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	b := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	sum := sha256.Sum256(b)
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a", ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Complete(context.Background(), "ins_a", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, token := got.Lease.ID, got.FetchToken

	mime, err := svc.VerifyLease(context.Background(), "ins_a", leaseID, token)
	if err != nil {
		t.Fatalf("the owning install must be able to reference its own lease: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("VerifyLease must answer the lease MIME, got %q", mime)
	}

	for name, call := range map[string]func() error{
		"another install": func() error { _, e := svc.VerifyLease(context.Background(), "ins_b", leaseID, token); return e },
		"tampered token":  func() error { _, e := svc.VerifyLease(context.Background(), "ins_a", leaseID, token+"x"); return e },
		"unknown lease":   func() error { _, e := svc.VerifyLease(context.Background(), "ins_a", "mls_missing", token); return e },
		"empty token":     func() error { _, e := svc.VerifyLease(context.Background(), "ins_a", leaseID, ""); return e },
		"empty install":   func() error { _, e := svc.VerifyLease(context.Background(), "", leaseID, token); return e },
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s must be refused as not-found (no existence oracle), got %v", name, err)
		}
	}
}

func (r *memRepo) GetLeaseForInstall(_ context.Context, install, id string) (*dmedia.Lease, bool, error) {
	if r.lease == nil || r.lease.ID != id || r.lease.InstallID != install {
		return nil, false, nil
	}
	return r.lease, true, nil
}

func (r *memRepo) RevokeLease(_ context.Context, install, id string, _ time.Time) (bool, error) {
	if r.lease == nil || r.lease.ID != id || r.lease.InstallID != install || r.lease.State != dmedia.LeaseActive {
		return false, nil
	}
	r.lease.State = dmedia.LeaseExpired
	return true, nil
}

// TestSampleFetchToken_OwnershipAndLiveness: an unknown id, another install's lease, a retired one
// and an expired one all read the same way. A lease id must never become an existence oracle.
//
// TestSampleFetchToken_OwnershipAndLiveness:未知 id、别的 install 的 lease、已退役的、已过期的,读起来
// **全都一样**。lease id 绝不能变成一个存在性预言机。
func TestSampleFetchToken_OwnershipAndLiveness(t *testing.T) {
	svc, repo, _ := completedLeaseService(t)
	lease := repo.lease

	if _, err := svc.SampleFetchToken(context.Background(), lease.InstallID, lease.ID); err != nil {
		t.Fatalf("the owner must be able to re-derive the token: %v", err)
	}
	if _, err := svc.SampleFetchToken(context.Background(), "ins_other", lease.ID); err != ErrNotFound {
		t.Fatalf("another install got %v, want ErrNotFound", err)
	}
	if _, err := svc.SampleFetchToken(context.Background(), lease.InstallID, "mls_nope"); err != ErrNotFound {
		t.Fatalf("unknown id got %v, want ErrNotFound", err)
	}
	lease.State = dmedia.LeaseExpired
	if _, err := svc.SampleFetchToken(context.Background(), lease.InstallID, lease.ID); err != ErrNotFound {
		t.Fatalf("retired lease got %v, want ErrNotFound", err)
	}
}

// TestRevokeSample_SpendsTheCapabilityAndIsIdempotent: after the upstream has fetched once, the URL
// must stop working immediately rather than idle out — and a retried cleanup is not an error.
//
// TestRevokeSample_SpendsTheCapabilityAndIsIdempotent:上游取过一次之后,那个 URL 必须**立刻**失效、
// 而不是慢慢空转到过期——且重试的清理不是错误。
func TestRevokeSample_SpendsTheCapabilityAndIsIdempotent(t *testing.T) {
	svc, repo, _ := completedLeaseService(t)
	lease := repo.lease
	if err := svc.RevokeSample(context.Background(), lease.InstallID, lease.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if repo.lease.State != dmedia.LeaseExpired {
		t.Fatalf("state = %q, want the capability spent", repo.lease.State)
	}
	if _, err := svc.SampleFetchToken(context.Background(), lease.InstallID, lease.ID); err != ErrNotFound {
		t.Fatal("a spent capability must stop resolving")
	}
	if err := svc.RevokeSample(context.Background(), lease.InstallID, lease.ID); err != nil {
		t.Fatalf("a retried cleanup must not error: %v", err)
	}
}

// completedLeaseService drives one upload to a real lease, so the token these tests re-derive is
// the same one the uploader received rather than a hand-made value.
//
// completedLeaseService 把一次上传真的走到 lease,使这些测试重新派生出的 token 与上传者当初拿到的
// **是同一个**,而不是手工捏的值。
func completedLeaseService(t *testing.T) (*Service, *memRepo, *PrivateLease) {
	t.Helper()
	repo := &memRepo{}
	files := &memFiles{data: map[string][]byte{}}
	svc := testService(t, repo, files)
	b := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	sum := sha256.Sum256(b)
	u, err := svc.Create(context.Background(), CreateInput{InstallID: "ins_a",
		ExpectedSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", TotalBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(context.Background(), "ins_a", u.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Complete(context.Background(), "ins_a", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, got
}

// TestSampleFetchToken_ReDerivesTheUploadersOwnToken: only the hash is stored, so the plaintext must
// come back from the signer. If it did not match, the public route would 404 the upstream and the
// failure would look like a fetch problem rather than a token one.
//
// TestSampleFetchToken_ReDerivesTheUploadersOwnToken:库里只存哈希,故明文必须由 signer 还原。若对不上,
// 公开路由会对上游 404,而那个失败看起来像**取回**出了问题、不像 token 出了问题。
func TestSampleFetchToken_ReDerivesTheUploadersOwnToken(t *testing.T) {
	svc, _, issued := completedLeaseService(t)
	got, err := svc.SampleFetchToken(context.Background(), "ins_a", issued.Lease.ID)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != issued.FetchToken {
		t.Fatal("the re-derived token differs from the one the uploader received")
	}
}
