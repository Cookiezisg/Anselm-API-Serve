// Package mediastore persists the gateway media-upload/lease state machine. File bytes deliberately
// live elsewhere; this store owns only the atomic capability transitions, all serialized through
// SQLite's single BEGIN IMMEDIATE writer. A file can never become completion-addressable until its
// upload and unique install-bound lease commit together.
package mediastore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dmedia "github.com/sunweilin/anselm/gateway/internal/domain/media"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

type Store struct {
	w *orm.DB
	r *orm.DB
}

func New(writer, reader *orm.DB) *Store { return &Store{w: writer, r: reader} }

// CreateUpload records a new private staging capability before any file bytes are accepted.
func (s *Store) CreateUpload(ctx context.Context, upload dmedia.Upload) error {
	_, err := s.w.Exec(ctx, `INSERT INTO media_uploads(
		id,install_id,expected_sha256,mime_type,total_bytes,received_bytes,state,expires_at,created_at,updated_at,completed_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		upload.ID, upload.InstallID, upload.ExpectedSHA256, upload.MIMEType, upload.TotalBytes, upload.ReceivedBytes,
		upload.State, upload.ExpiresAt.UTC(), upload.CreatedAt.UTC(), upload.UpdatedAt.UTC(), upload.CompletedAt)
	if err != nil {
		return fmt.Errorf("mediastore.CreateUpload: %w", err)
	}
	return nil
}

// GetUploadForInstall is ownership enforcing: an unknown or another install's id deliberately
// reads as absent so media ids never become an existence oracle.
func (s *Store) GetUploadForInstall(ctx context.Context, installID, uploadID string) (*dmedia.Upload, bool, error) {
	row := s.r.QueryRow(ctx, `SELECT id,install_id,expected_sha256,mime_type,total_bytes,received_bytes,state,expires_at,created_at,updated_at,completed_at
		FROM media_uploads WHERE id=? AND install_id=?`, uploadID, installID)
	return scanUpload(row)
}

// AdvanceReceived CASes the resumable byte cursor. expectedReceived is the file writer's verified
// offset, so a replayed/out-of-order chunk cannot move the DB progress or overwrite a later chunk.
// It never changes an expired/non-open upload and never permits received_bytes > total_bytes.
func (s *Store) AdvanceReceived(ctx context.Context, installID, uploadID string, expectedReceived, nextReceived int64, now time.Time) (bool, error) {
	if expectedReceived < 0 || nextReceived < expectedReceived {
		return false, nil
	}
	res, err := s.w.Exec(ctx, `UPDATE media_uploads
		SET received_bytes=?, updated_at=?
		WHERE id=? AND install_id=? AND state=? AND expires_at>? AND received_bytes=? AND ?<=total_bytes`,
		nextReceived, now.UTC(), uploadID, installID, dmedia.UploadOpen, now.UTC(), expectedReceived, nextReceived)
	if err != nil {
		return false, fmt.Errorf("mediastore.AdvanceReceived: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mediastore.AdvanceReceived rows: %w", err)
	}
	return n == 1, nil
}

// CompleteUpload atomically seals a fully received upload and creates its only lease. The caller
// has already hashed the staged bytes; expected SHA is rechecked in the WHERE clause so no caller
// can complete an upload with a substituted object. lease.UploadID is UNIQUE in DDL as a second
// line of defense against an accidental future call path bypassing this CAS.
func (s *Store) CompleteUpload(ctx context.Context, installID, uploadID, actualSHA256 string, now time.Time, lease dmedia.Lease) (*dmedia.Lease, bool, error) {
	var completed bool
	err := s.w.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx, `UPDATE media_uploads
			SET state=?, completed_at=?, updated_at=?
			WHERE id=? AND install_id=? AND state=? AND expires_at>? AND received_bytes=total_bytes AND expected_sha256=?`,
			dmedia.UploadCompleted, now.UTC(), now.UTC(), uploadID, installID, dmedia.UploadOpen, now.UTC(), actualSHA256)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		completed = true
		_, err = tx.Exec(ctx, `INSERT INTO media_leases(
			id,install_id,upload_id,sha256,mime_type,size_bytes,fetch_token_hash,state,expires_at,created_at,deleted_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			lease.ID, installID, uploadID, actualSHA256, lease.MIMEType, lease.SizeBytes, lease.FetchTokenHash,
			dmedia.LeaseActive, lease.ExpiresAt.UTC(), lease.CreatedAt.UTC(), nil)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("mediastore.CompleteUpload: %w", err)
	}
	if !completed {
		return nil, false, nil
	}
	got, ok, err := s.GetLeaseForInstall(ctx, installID, lease.ID)
	if err != nil || !ok {
		return nil, false, err
	}
	return got, true, nil
}

// GetLeaseForInstall makes lease ownership non-enumerable to clients.
func (s *Store) GetLeaseForInstall(ctx context.Context, installID, leaseID string) (*dmedia.Lease, bool, error) {
	row := s.r.QueryRow(ctx, `SELECT id,install_id,upload_id,sha256,mime_type,size_bytes,fetch_token_hash,state,expires_at,created_at,deleted_at
		FROM media_leases WHERE id=? AND install_id=?`, leaseID, installID)
	return scanLease(row)
}

// Expire advances only lifecycle metadata; physical deletion happens afterwards
// in app/media, because deleting a file before its durable state says expired
// would create a retryable request whose cursor points at missing bytes.
func (s *Store) Expire(ctx context.Context, now time.Time) error {
	return s.w.Transaction(ctx, func(tx *orm.DB) error {
		if _, err := tx.Exec(ctx, `UPDATE media_uploads SET state=?,updated_at=? WHERE state=? AND expires_at<=?`, dmedia.UploadExpired, now.UTC(), dmedia.UploadOpen, now.UTC()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE media_leases SET state=? WHERE state=? AND expires_at<=?`, dmedia.LeaseExpired, dmedia.LeaseActive, now.UTC())
		return err
	})
}

// OpenUploads is the restart-recovery set: only still-live incomplete files may
// have an fsync-before-cursor crash tail that needs truncation.
func (s *Store) OpenUploads(ctx context.Context) ([]dmedia.Upload, error) {
	rows, err := s.r.Query(ctx, `SELECT id,install_id,expected_sha256,mime_type,total_bytes,received_bytes,state,expires_at,created_at,updated_at,completed_at
		FROM media_uploads WHERE state=?`, dmedia.UploadOpen)
	if err != nil {
		return nil, fmt.Errorf("mediastore.OpenUploads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []dmedia.Upload
	for rows.Next() {
		var u dmedia.Upload
		if err := rows.Scan(&u.ID, &u.InstallID, &u.ExpectedSHA256, &u.MIMEType, &u.TotalBytes, &u.ReceivedBytes, &u.State, &u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt, &u.CompletedAt); err != nil {
			return nil, fmt.Errorf("mediastore.OpenUploads scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mediastore.OpenUploads rows: %w", err)
	}
	return out, nil
}

// ExpiredFileIDs returns every staged object whose durable lifecycle no longer
// permits provider use: abandoned uploads and completed uploads with an expired
// lease. DISTINCT is required because a completed upload has exactly one lease.
func (s *Store) ExpiredFileIDs(ctx context.Context) ([]string, error) {
	rows, err := s.r.Query(ctx, `SELECT DISTINCT u.id FROM media_uploads u
		LEFT JOIN media_leases l ON l.upload_id=u.id
		WHERE u.state=? OR l.state=?`, dmedia.UploadExpired, dmedia.LeaseExpired)
	if err != nil {
		return nil, fmt.Errorf("mediastore.ExpiredFileIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("mediastore.ExpiredFileIDs scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mediastore.ExpiredFileIDs rows: %w", err)
	}
	return out, nil
}

// ExpireOpen closes a corrupt/missing staging object before recovery deletes its
// bytes. It is deliberately idempotent and refuses to touch a completed object.
func (s *Store) ExpireOpen(ctx context.Context, uploadID string, now time.Time) error {
	_, err := s.w.Exec(ctx, `UPDATE media_uploads SET state=?,updated_at=? WHERE id=? AND state=?`, dmedia.UploadExpired, now.UTC(), uploadID, dmedia.UploadOpen)
	if err != nil {
		return fmt.Errorf("mediastore.ExpireOpen: %w", err)
	}
	return nil
}

// AcknowledgeRemoved finalizes only after the file remove has succeeded (or was
// already absent). Expired incomplete rows are deleted; completed metadata stays
// auditable while its lease becomes deleted.
func (s *Store) AcknowledgeRemoved(ctx context.Context, uploadID string, now time.Time) error {
	return s.w.Transaction(ctx, func(tx *orm.DB) error {
		res, err := tx.Exec(ctx, `DELETE FROM media_uploads WHERE id=? AND state=?`, uploadID, dmedia.UploadExpired)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE media_leases SET state=?,deleted_at=? WHERE upload_id=? AND state=?`, dmedia.LeaseDeleted, now.UTC(), uploadID, dmedia.LeaseExpired)
		return err
	})
}

func scanUpload(row *sql.Row) (*dmedia.Upload, bool, error) {
	var out dmedia.Upload
	if err := row.Scan(&out.ID, &out.InstallID, &out.ExpectedSHA256, &out.MIMEType, &out.TotalBytes, &out.ReceivedBytes,
		&out.State, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt, &out.CompletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("mediastore.scanUpload: %w", err)
	}
	return &out, true, nil
}

func scanLease(row *sql.Row) (*dmedia.Lease, bool, error) {
	var out dmedia.Lease
	if err := row.Scan(&out.ID, &out.InstallID, &out.UploadID, &out.SHA256, &out.MIMEType, &out.SizeBytes,
		&out.FetchTokenHash, &out.State, &out.ExpiresAt, &out.CreatedAt, &out.DeletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("mediastore.scanLease: %w", err)
	}
	return &out, true, nil
}
