// Package mediafs is the private on-disk staging area for gateway media. Files are addressed only
// by opaque upload ids, never original filenames or content hashes. It writes bytes before the
// SQLite cursor advances, fsyncs every accepted chunk, and offers exact truncation for crash repair
// when a process died after the file write but before the DB CAS committed.
package mediafs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrBadUploadID = errors.New("mediafs: invalid upload id")
	ErrOffset      = errors.New("mediafs: staging offset mismatch")
)

type Store struct {
	root string
	mu   sync.Mutex // process-local serialization makes append + truncate indivisible per gateway binary.
}

func New(root string) *Store { return &Store{root: root} }

func (s *Store) Create(ctx context.Context, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mediafs.Create mkdir: %w", err)
	}
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("mediafs.Create: %w", err)
	}
	return f.Close()
}

// Append writes at exactly expectedOffset and fsyncs before returning. It is safe to retry only
// after the caller has reconciled disk size with its DB cursor; a stale retry fails with ErrOffset.
func (s *Store) Append(ctx context.Context, uploadID string, expectedOffset int64, chunk []byte) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if expectedOffset < 0 || len(chunk) == 0 {
		return 0, ErrOffset
	}
	p, err := s.path(uploadID)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("mediafs.Append open: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("mediafs.Append stat: %w", err)
	}
	if info.Size() != expectedOffset {
		return 0, ErrOffset
	}
	if _, err := f.Seek(expectedOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("mediafs.Append seek: %w", err)
	}
	if _, err := f.Write(chunk); err != nil {
		return 0, fmt.Errorf("mediafs.Append write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("mediafs.Append sync: %w", err)
	}
	return expectedOffset + int64(len(chunk)), nil
}

func (s *Store) Size(ctx context.Context, uploadID string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(p)
	if err != nil {
		return 0, fmt.Errorf("mediafs.Size: %w", err)
	}
	return info.Size(), nil
}

// Truncate is the crash-repair primitive. It may only remove bytes past a durable DB cursor; it
// never manufactures bytes or grows a file. Sync makes the repaired position durable before a retry.
func (s *Store) Truncate(ctx context.Context, uploadID string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size < 0 {
		return ErrOffset
	}
	p, err := s.path(uploadID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("mediafs.Truncate open: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("mediafs.Truncate stat: %w", err)
	}
	if size > info.Size() {
		return ErrOffset
	}
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("mediafs.Truncate: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("mediafs.Truncate sync: %w", err)
	}
	return nil
}

func (s *Store) SHA256(ctx context.Context, uploadID string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.Open(p)
	if err != nil {
		return "", 0, fmt.Errorf("mediafs.SHA256 open: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("mediafs.SHA256 read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// MIMEType identifies only the small, explicit set of binary containers accepted by the media
// staging API. It intentionally examines bytes, not the client-declared Content-Type or filename.
// This is a fail-closed magic check rather than a decoder: provider-side validation remains the
// final parser, while an unknown/truncated prefix can never become a fetchable lease.
func (s *Store) MIMEType(ctx context.Context, uploadID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("mediafs.MIMEType open: %w", err)
	}
	defer func() { _ = f.Close() }()
	prefix := make([]byte, 64)
	n, err := io.ReadFull(f, prefix)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("mediafs.MIMEType read: %w", err)
	}
	return detectMIME(prefix[:n]), nil
}

func (s *Store) Open(ctx context.Context, uploadID string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- p comes only from path(), which accepts opaque mup_<32 lowercase hex> ids.
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("mediafs.Open: %w", err)
	}
	return f, nil
}

func (s *Store) Remove(ctx context.Context, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(uploadID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mediafs.Remove: %w", err)
	}
	return nil
}

func (s *Store) path(uploadID string) (string, error) {
	if !validUploadID(uploadID) {
		return "", ErrBadUploadID
	}
	return filepath.Join(s.root, "uploads", uploadID[:4], uploadID), nil
}

func validUploadID(uploadID string) bool {
	if !strings.HasPrefix(uploadID, "mup_") || len(uploadID) != len("mup_")+32 {
		return false
	}
	for _, c := range uploadID[len("mup_"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func detectMIME(prefix []byte) string {
	switch {
	case len(prefix) >= 8 && string(prefix[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(prefix) >= 3 && prefix[0] == 0xff && prefix[1] == 0xd8 && prefix[2] == 0xff:
		return "image/jpeg"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp"
	case len(prefix) >= 12 && string(prefix[4:8]) == "ftyp":
		return "video/mp4"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WAVE":
		return "audio/wav"
	case len(prefix) >= 3 && string(prefix[:3]) == "ID3":
		return "audio/mpeg"
	case len(prefix) >= 2 && prefix[0] == 0xff && prefix[1]&0xe0 == 0xe0:
		return "audio/mpeg"
	default:
		return ""
	}
}
