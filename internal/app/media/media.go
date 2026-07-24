// Package media orchestrates safe resumable staging without naming a concrete DB or filesystem.
// The file and SQLite state machines have different crash semantics, so this layer is the only
// place allowed to order them: durable bytes first, then monotonic DB cursor; on a lost DB CAS the
// unaccounted file tail is immediately removed.
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
	"github.com/sunweilin/anselm/gateway/internal/pkg/idgen"
)

var (
	ErrInvalidInput = errors.New("mediaapp: invalid input")
	ErrNotFound     = errors.New("mediaapp: upload or lease not found")
	ErrConflict     = errors.New("mediaapp: upload state conflict")
	ErrIntegrity    = errors.New("mediaapp: staged media integrity mismatch")
)

type Repository interface {
	CreateUpload(ctx context.Context, upload dmedia.Upload) error
	GetUploadForInstall(ctx context.Context, installID, uploadID string) (*dmedia.Upload, bool, error)
	AdvanceReceived(ctx context.Context, installID, uploadID string, expectedReceived, nextReceived int64, now time.Time) (bool, error)
	CompleteUpload(ctx context.Context, installID, uploadID, actualSHA256 string, now time.Time, lease dmedia.Lease) (*dmedia.Lease, bool, error)
}

type Files interface {
	Create(ctx context.Context, uploadID string) error
	Append(ctx context.Context, uploadID string, expectedOffset int64, chunk []byte) (int64, error)
	Size(ctx context.Context, uploadID string) (int64, error)
	Truncate(ctx context.Context, uploadID string, size int64) error
	SHA256(ctx context.Context, uploadID string) (sha256 string, size int64, err error)
	Remove(ctx context.Context, uploadID string) error
	Open(ctx context.Context, uploadID string) (io.ReadCloser, error)
}

type Clock interface{ Now() time.Time }

type Limits struct {
	MaxBytes  int64
	UploadTTL time.Duration
	LeaseTTL  time.Duration
}

type CreateInput struct {
	InstallID      string
	ExpectedSHA256 string
	MIMEType       string
	TotalBytes     int64
}

// PrivateLease is application-internal. Its token is emitted only as the query
// component of a short-lived fetchPath after proof-protected completion; SQLite
// retains only the hash and no API field exposes a standalone bearer token.
type PrivateLease struct {
	Lease      *dmedia.Lease
	FetchToken string
}

type Service struct {
	repo   Repository
	files  Files
	clock  Clock
	limits Limits
	signer FetchSigner
}

// RecoveryRepository is intentionally an optional extension of the request
// repository. Unit-level request fakes need not emulate a database sweeper,
// while the production media store provides the full restart/TTL lifecycle.
type RecoveryRepository interface {
	Expire(ctx context.Context, now time.Time) error
	OpenUploads(ctx context.Context) ([]dmedia.Upload, error)
	ExpiredFileIDs(ctx context.Context) ([]string, error)
	ExpireOpen(ctx context.Context, uploadID string, now time.Time) error
	AcknowledgeRemoved(ctx context.Context, uploadID string, now time.Time) error
}

type LeaseRepository interface {
	GetLease(ctx context.Context, leaseID string) (*dmedia.Lease, bool, error)
}

type LeaseSource struct {
	MIMEType  string
	SizeBytes int64
	Body      io.ReadCloser
}

func New(repo Repository, files Files, clock Clock, limits Limits, signer FetchSigner) *Service {
	if repo == nil || files == nil || clock == nil || signer == nil || limits.MaxBytes <= 0 || limits.UploadTTL <= 0 || limits.LeaseTTL <= 0 {
		panic("mediaapp.New: repo, files, clock, signer, positive limits are required")
	}
	return &Service{repo: repo, files: files, clock: clock, limits: limits, signer: signer}
}

// Recover is safe at startup and on a periodic loop. It first advances durable
// lifecycle expiry, repairs only fsync-before-cursor crash tails, fails closed
// when an open cursor points beyond its file, then removes already-expired bytes.
// File deletion is acknowledged in SQLite afterwards so an interruption is
// idempotently retried on the next run.
func (s *Service) Recover(ctx context.Context) (int, error) {
	repo, ok := s.repo.(RecoveryRepository)
	if !ok {
		return 0, errors.New("mediaapp.Recover: repository lacks recovery lifecycle")
	}
	now := s.clock.Now().UTC()
	if err := repo.Expire(ctx, now); err != nil {
		return 0, fmt.Errorf("mediaapp.Recover expire: %w", err)
	}
	open, err := repo.OpenUploads(ctx)
	if err != nil {
		return 0, fmt.Errorf("mediaapp.Recover open: %w", err)
	}
	for _, u := range open {
		size, serr := s.files.Size(ctx, u.ID)
		if serr != nil && !errors.Is(serr, os.ErrNotExist) {
			return 0, fmt.Errorf("mediaapp.Recover size: %w", serr)
		}
		if errors.Is(serr, os.ErrNotExist) || size < u.ReceivedBytes {
			if err := repo.ExpireOpen(ctx, u.ID, now); err != nil {
				return 0, fmt.Errorf("mediaapp.Recover expire corrupt: %w", err)
			}
			continue
		}
		if size > u.ReceivedBytes {
			if err := s.files.Truncate(ctx, u.ID, u.ReceivedBytes); err != nil {
				return 0, fmt.Errorf("mediaapp.Recover truncate: %w", err)
			}
		}
	}
	ids, err := repo.ExpiredFileIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("mediaapp.Recover candidates: %w", err)
	}
	for _, id := range ids {
		if err := s.files.Remove(ctx, id); err != nil {
			return 0, fmt.Errorf("mediaapp.Recover remove: %w", err)
		}
		if err := repo.AcknowledgeRemoved(ctx, id, now); err != nil {
			return 0, fmt.Errorf("mediaapp.Recover acknowledge: %w", err)
		}
	}
	return len(ids), nil
}

func (s *Service) OpenLease(ctx context.Context, leaseID, token string) (*LeaseSource, error) {
	repo, ok := s.repo.(LeaseRepository)
	if !ok {
		return nil, errors.New("mediaapp.OpenLease: repository lacks lease lookup")
	}
	lease, found, err := repo.GetLease(ctx, leaseID)
	if err != nil {
		return nil, fmt.Errorf("mediaapp.OpenLease lookup: %w", err)
	}
	if !found || lease.State != dmedia.LeaseActive || !s.clock.Now().UTC().Before(lease.ExpiresAt) || !s.signer.Verify(token, lease.ID, lease.InstallID, lease.ExpiresAt) || dmedia.HashSecret(token) != lease.FetchTokenHash {
		return nil, ErrNotFound
	}
	body, err := s.files.Open(ctx, lease.UploadID)
	if err != nil {
		return nil, fmt.Errorf("mediaapp.OpenLease file: %w", err)
	}
	return &LeaseSource{MIMEType: lease.MIMEType, SizeBytes: lease.SizeBytes, Body: body}, nil
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*dmedia.Upload, error) {
	if strings.TrimSpace(in.InstallID) == "" || !dmedia.ValidSHA256(in.ExpectedSHA256) || !supportedMIME(in.MIMEType) ||
		in.TotalBytes <= 0 || in.TotalBytes > s.limits.MaxBytes {
		return nil, ErrInvalidInput
	}
	now := s.clock.Now().UTC()
	u := dmedia.Upload{ID: idgen.MediaUploadID(), InstallID: in.InstallID, ExpectedSHA256: in.ExpectedSHA256,
		MIMEType: in.MIMEType, TotalBytes: in.TotalBytes, State: dmedia.UploadOpen,
		ExpiresAt: now.Add(s.limits.UploadTTL), CreatedAt: now, UpdatedAt: now}
	if err := s.files.Create(ctx, u.ID); err != nil {
		return nil, fmt.Errorf("mediaapp.Create file: %w", err)
	}
	if err := s.repo.CreateUpload(ctx, u); err != nil {
		_ = s.files.Remove(context.Background(), u.ID)
		return nil, fmt.Errorf("mediaapp.Create record: %w", err)
	}
	return &u, nil
}

// Append first reconciles a crash tail, then fsyncs the new bytes before moving the DB cursor. A
// concurrent/replayed request which loses the DB CAS has its own unrecorded tail truncated again.
func (s *Service) Append(ctx context.Context, installID, uploadID string, expectedOffset int64, chunk []byte) (*dmedia.Upload, error) {
	if expectedOffset < 0 || len(chunk) == 0 || len(chunk) > int(s.limits.MaxBytes) {
		return nil, ErrInvalidInput
	}
	u, err := s.openOwned(ctx, installID, uploadID)
	if err != nil {
		return nil, err
	}
	if u.ReceivedBytes != expectedOffset {
		return nil, ErrConflict
	}
	physical, err := s.files.Size(ctx, uploadID)
	if err != nil {
		return nil, fmt.Errorf("mediaapp.Append size: %w", err)
	}
	if physical < u.ReceivedBytes {
		return nil, ErrIntegrity // DB must never point beyond durable bytes.
	}
	if physical > u.ReceivedBytes {
		if err := s.files.Truncate(ctx, uploadID, u.ReceivedBytes); err != nil {
			return nil, fmt.Errorf("mediaapp.Append repair: %w", err)
		}
	}
	next := u.ReceivedBytes + int64(len(chunk))
	if next > u.TotalBytes {
		return nil, ErrInvalidInput
	}
	if _, err := s.files.Append(ctx, uploadID, u.ReceivedBytes, chunk); err != nil {
		return nil, fmt.Errorf("mediaapp.Append file: %w", err)
	}
	ok, err := s.repo.AdvanceReceived(ctx, installID, uploadID, u.ReceivedBytes, next, s.clock.Now().UTC())
	if err != nil {
		_ = s.files.Truncate(context.Background(), uploadID, u.ReceivedBytes)
		return nil, fmt.Errorf("mediaapp.Append cursor: %w", err)
	}
	if !ok {
		_ = s.files.Truncate(context.Background(), uploadID, u.ReceivedBytes)
		return nil, ErrConflict
	}
	u.ReceivedBytes = next
	u.UpdatedAt = s.clock.Now().UTC()
	return u, nil
}

// Complete verifies exact byte count and digest from disk, then atomically seals the upload and
// creates one lease. The token is represented durably only by its hash and is
// later embedded exclusively in the short-lived fetch path for model retrieval.
func (s *Service) Complete(ctx context.Context, installID, uploadID string) (*PrivateLease, error) {
	u, err := s.openOwned(ctx, installID, uploadID)
	if err != nil {
		return nil, err
	}
	if u.ReceivedBytes != u.TotalBytes {
		return nil, ErrConflict
	}
	sha, size, err := s.files.SHA256(ctx, uploadID)
	if err != nil {
		return nil, fmt.Errorf("mediaapp.Complete hash: %w", err)
	}
	if size != u.TotalBytes || sha != u.ExpectedSHA256 {
		return nil, ErrIntegrity
	}
	now := s.clock.Now().UTC()
	lease := dmedia.Lease{ID: idgen.MediaLeaseID(), InstallID: installID, UploadID: uploadID, SHA256: sha,
		MIMEType: u.MIMEType, SizeBytes: size, State: dmedia.LeaseActive,
		ExpiresAt: now.Add(s.limits.LeaseTTL), CreatedAt: now}
	token := s.signer.Token(lease.ID, lease.InstallID, lease.ExpiresAt)
	lease.FetchTokenHash = dmedia.HashSecret(token)
	got, completed, err := s.repo.CompleteUpload(ctx, installID, uploadID, sha, now, lease)
	if err != nil {
		return nil, err
	}
	if !completed || got == nil {
		return nil, ErrConflict
	}
	return &PrivateLease{Lease: got, FetchToken: token}, nil
}

func (s *Service) openOwned(ctx context.Context, installID, uploadID string) (*dmedia.Upload, error) {
	if strings.TrimSpace(installID) == "" || strings.TrimSpace(uploadID) == "" {
		return nil, ErrInvalidInput
	}
	u, ok, err := s.repo.GetUploadForInstall(ctx, installID, uploadID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if u.State != dmedia.UploadOpen || !s.clock.Now().UTC().Before(u.ExpiresAt) {
		return nil, ErrConflict
	}
	return u, nil
}

func supportedMIME(v string) bool {
	switch v {
	case "image/jpeg", "image/png", "image/webp", "video/mp4", "audio/wav", "audio/mpeg":
		return true
	default:
		return false
	}
}
