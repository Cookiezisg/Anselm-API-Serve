package mediastore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
)

const shaA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func openT(t *testing.T) *Store {
	t.Helper()
	cfg := sqlite.DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "media.db")
	db, err := sqlite.Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db.Writer, db.Reader)
}

func upload(now time.Time) dmedia.Upload {
	return dmedia.Upload{
		ID: "mup_a", InstallID: "ins_a", ExpectedSHA256: shaA, MIMEType: "image/png", TotalBytes: 4,
		State: dmedia.UploadOpen, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

func lease(now time.Time) dmedia.Lease {
	return dmedia.Lease{
		ID: "mls_a", InstallID: "ins_a", UploadID: "mup_a", SHA256: shaA, MIMEType: "image/png", SizeBytes: 4,
		FetchTokenHash: dmedia.HashSecret("provider-only-fetch-token"), State: dmedia.LeaseActive,
		ExpiresAt: now.Add(20 * time.Minute), CreatedAt: now,
	}
}

func TestUploadCursorIsOwnedMonotonicAndBounded(t *testing.T) {
	s := openT(t)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateUpload(context.Background(), upload(now)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.GetUploadForInstall(context.Background(), "ins_b", "mup_a"); err != nil || ok || got != nil {
		t.Fatalf("cross-install lookup = (%+v,%v,%v), want absent", got, ok, err)
	}
	if ok, err := s.AdvanceReceived(context.Background(), "ins_a", "mup_a", 0, 2, now); err != nil || !ok {
		t.Fatalf("first chunk = (%v,%v)", ok, err)
	}
	if ok, err := s.AdvanceReceived(context.Background(), "ins_a", "mup_a", 0, 3, now); err != nil || ok {
		t.Fatalf("replayed offset must not advance = (%v,%v)", ok, err)
	}
	if ok, err := s.AdvanceReceived(context.Background(), "ins_a", "mup_a", 2, 5, now); err != nil || ok {
		t.Fatalf("over-total cursor must not advance = (%v,%v)", ok, err)
	}
	got, ok, err := s.GetUploadForInstall(context.Background(), "ins_a", "mup_a")
	if err != nil || !ok || got.ReceivedBytes != 2 {
		t.Fatalf("cursor = (%+v,%v,%v), want 2", got, ok, err)
	}
}

func TestCompleteIsOneAtomicUploadToLeaseTransition(t *testing.T) {
	s := openT(t)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateUpload(context.Background(), upload(now)); err != nil {
		t.Fatal(err)
	}
	if got, completed, err := s.CompleteUpload(context.Background(), "ins_a", "mup_a", shaA, now, lease(now)); err != nil || completed || got != nil {
		t.Fatalf("partial upload completed = (%+v,%v,%v)", got, completed, err)
	}
	if ok, err := s.AdvanceReceived(context.Background(), "ins_a", "mup_a", 0, 4, now); err != nil || !ok {
		t.Fatalf("fill cursor = (%v,%v)", ok, err)
	}
	got, completed, err := s.CompleteUpload(context.Background(), "ins_a", "mup_a", shaA, now, lease(now))
	if err != nil || !completed || got == nil || got.UploadID != "mup_a" || got.State != dmedia.LeaseActive {
		t.Fatalf("complete = (%+v,%v,%v)", got, completed, err)
	}
	if again, completed, err := s.CompleteUpload(context.Background(), "ins_a", "mup_a", shaA, now, lease(now)); err != nil || completed || again != nil {
		t.Fatalf("duplicate complete = (%+v,%v,%v), want no second lease", again, completed, err)
	}
	if owned, ok, err := s.GetLeaseForInstall(context.Background(), "ins_b", "mls_a"); err != nil || ok || owned != nil {
		t.Fatalf("cross-install lease lookup = (%+v,%v,%v), want absent", owned, ok, err)
	}
}
