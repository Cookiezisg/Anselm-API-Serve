// Package media orchestrates safe resumable staging without naming a concrete DB or filesystem.
// The file and SQLite state machines have different crash semantics, so this layer is the only
// place allowed to order them: durable bytes first, then monotonic DB cursor; on a lost DB CAS the
// unaccounted file tail is immediately removed.
package media

import (
	"context"
	"errors"
	"fmt"
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

// PrivateLease must never cross the public HTTP boundary. FetchToken is retained only until the
// future provider-URL signer is wired; its hash alone is persisted.
type PrivateLease struct {
	Lease      *dmedia.Lease
	FetchToken string
}

type Service struct {
	repo   Repository
	files  Files
	clock  Clock
	limits Limits
}

func New(repo Repository, files Files, clock Clock, limits Limits) *Service {
	if repo == nil || files == nil || clock == nil || limits.MaxBytes <= 0 || limits.UploadTTL <= 0 || limits.LeaseTTL <= 0 {
		panic("mediaapp.New: repo, files, clock, positive limits are required")
	}
	return &Service{repo: repo, files: files, clock: clock, limits: limits}
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
func (s *Service) Append(ctx context.Context, installID, uploadID string, chunk []byte) (*dmedia.Upload, error) {
	if len(chunk) == 0 || len(chunk) > int(s.limits.MaxBytes) {
		return nil, ErrInvalidInput
	}
	u, err := s.openOwned(ctx, installID, uploadID)
	if err != nil {
		return nil, err
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
// creates one lease. The public caller receives only Lease; FetchToken stays private for provider
// fetch URL construction and is represented durably only by its hash.
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
	token := idgen.MediaFetchToken()
	lease := dmedia.Lease{ID: idgen.MediaLeaseID(), InstallID: installID, UploadID: uploadID, SHA256: sha,
		MIMEType: u.MIMEType, SizeBytes: size, FetchTokenHash: dmedia.HashSecret(token), State: dmedia.LeaseActive,
		ExpiresAt: now.Add(s.limits.LeaseTTL), CreatedAt: now}
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
