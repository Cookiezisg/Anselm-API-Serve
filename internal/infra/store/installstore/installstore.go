// Package installstore is the infra UoW store for install identity + the Sybil
// rate buckets. It structurally satisfies the app/install Store port (it never
// imports app). All multi-row writes go through one BEGIN IMMEDIATE transaction
// on the writer pool (ADR-005): Issue runs the enabled Sybil gates AND the
// installs INSERT atomically, so a later-gate reject leaves NO earlier-gate
// counter consumed, regenerates the install id once on a UNIQUE collision,
// and opportunistically prunes stale rate-bucket windows inside the same tx. *sql.Tx never leaks out of this package.
package installstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	dinstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/idgen"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// Store holds the writer (serialized BEGIN IMMEDIATE) and reader (concurrent WAL)
// pools over the same SQLite file.
type Store struct {
	w *orm.DB
	r *orm.DB
}

// New builds a Store over the given write/read pools (sqlite.DB.Writer /
// sqlite.DB.Reader).
func New(writer, reader *orm.DB) *Store {
	return &Store{w: writer, r: reader}
}

// Issue runs the enabled Sybil gates and, only if all admit, inserts a fresh
// installs row — ALL in one transaction. Order is fixed: per-IP/hour → global
// daily → per-fp daily+cooldown → INSERT. Each gate is a single conditional
// UPDATE (never read-modify-write) so concurrent issuances serialize on the write
// pool and a loser sees RowsAffected==0; on a 0 the whole tx rolls back (deferred)
// and NO counter persists. A disabled gate is skipped before any DB work
// (dormant zero-cost). The install id regenerates once on a UNIQUE conflict.
func (s *Store) Issue(ctx context.Context, p dinstall.IssueParams) (dinstall.IssueResult, error) {
	var result dinstall.IssueResult
	err := s.w.Transaction(ctx, func(tx *orm.DB) error {
		// Registration is idempotent by device key. The same installation may
		// recover its public id without consuming another quota pool or gate bucket.
		var existingID string
		err := tx.QueryRow(ctx, `SELECT id FROM installs WHERE key_thumbprint = ?`, p.Thumbprint).Scan(&existingID)
		if err == nil {
			result = dinstall.IssueResult{Admitted: true, InstallID: existingID, Existing: true}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// gate 1: per-IP hourly (skipped when disabled). Prunes stale windows.
		if p.IPGate.Enabled {
			ok, err := bumpIPRate(ctx, tx, p.IPGate)
			if err != nil {
				return err
			}
			if !ok {
				result = dinstall.IssueResult{Admitted: false, Gate: dinstall.GateIP}
				return errGateReject
			}
		}

		// gate 2: global daily coarse valve (skipped when disabled).
		if p.GlobalGate.Enabled {
			ok, err := bumpGlobalRate(ctx, tx, p.GlobalGate)
			if err != nil {
				return err
			}
			if !ok {
				result = dinstall.IssueResult{Admitted: false, Gate: dinstall.GateGlobal}
				return errGateReject
			}
		}

		// gate 3: per-fp daily + cooldown (skipped when disabled / empty fp).
		if p.FPGate.Enabled {
			ok, err := bumpFPRate(ctx, tx, p.FPGate)
			if err != nil {
				return err
			}
			if !ok {
				result = dinstall.IssueResult{Admitted: false, Gate: dinstall.GateFP}
				return errGateReject
			}
		}

		// admit: insert the brand-new install row, regenerating the id once on a
		// UNIQUE collision. A second collision is astronomically improbable
		// with a 16-byte id and surfaces as a real error rather than an infinite loop.
		actualID := p.InstallID
		if err := insertInstall(ctx, tx, actualID, p); err != nil {
			if isUniqueConflict(err) {
				actualID = idgen.InstallID()
				if err := insertInstall(ctx, tx, actualID, p); err != nil {
					return err
				}
			} else {
				return err
			}
		}
		result = dinstall.IssueResult{Admitted: true, Gate: dinstall.GateNone, InstallID: actualID}
		return nil
	})
	// errGateReject is the internal sentinel that rolls the tx back on a gate
	// reject; it is NOT a real error to the caller (result already carries the gate).
	if errors.Is(err, errGateReject) {
		return result, nil
	}
	if err != nil {
		return dinstall.IssueResult{}, err
	}
	return result, nil
}

// errGateReject rolls back the issuance tx when a gate denies, without surfacing
// as an error (the IssueResult carries the tripped gate).
var errGateReject = errors.New("installstore: gate reject")

// bumpIPRate enforces the per-IP hourly bucket and prunes this key's stale
// windows (retain only the current window). A conditional UPDATE bumps only
// while below the cap; RowsAffected==0 means the limit is hit.
func bumpIPRate(ctx context.Context, tx *orm.DB, g dinstall.IPGate) (bool, error) {
	// B8 prune: drop older windows for this key so an enabled install_ip_rate gate
	// never accumulates history. Lexical compare is valid for the "2006-01-02T15"
	// window format. Retains the current window.
	if _, err := tx.Exec(ctx,
		`DELETE FROM install_ip_rate WHERE ip_key = ? AND window_hour < ?`,
		g.Key, g.WindowHour); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT OR IGNORE INTO install_ip_rate(ip_key, window_hour, count) VALUES (?, ?, 0)`,
		g.Key, g.WindowHour); err != nil {
		return false, err
	}
	res, err := tx.Exec(ctx,
		`UPDATE install_ip_rate SET count = count + 1
		   WHERE ip_key = ? AND window_hour = ? AND count < ?`,
		g.Key, g.WindowHour, g.Max)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// bumpGlobalRate enforces the global daily cap via a conditional UPDATE and prunes
// older days.
func bumpGlobalRate(ctx context.Context, tx *orm.DB, g dinstall.GlobalGate) (bool, error) {
	if _, err := tx.Exec(ctx,
		`DELETE FROM install_global_rate WHERE window_day < ?`, g.WindowDay); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT OR IGNORE INTO install_global_rate(window_day, count) VALUES (?, 0)`,
		g.WindowDay); err != nil {
		return false, err
	}
	res, err := tx.Exec(ctx,
		`UPDATE install_global_rate SET count = count + 1 WHERE window_day = ? AND count < ?`,
		g.WindowDay, g.Cap)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// bumpFPRate enforces the per-fp daily cap AND cooldown in ONE conditional UPSERT
// (never read-modify-write): the INSERT path is the first issuance this day
// (count=1); the ON CONFLICT path bumps only when BOTH the daily cap and the
// cooldown allow, so two concurrent same-fp requests serialize and the loser sees
// RowsAffected==0. Prunes older days first. A disabled sub-gate is made
// non-binding via the sentinel cap / far-future cutoff resolved in the app.
func bumpFPRate(ctx context.Context, tx *orm.DB, g dinstall.FPGate) (bool, error) {
	if _, err := tx.Exec(ctx,
		`DELETE FROM install_fp_rate WHERE fp_sha256 = ? AND window_day < ?`,
		g.FPSHA256, g.WindowDay); err != nil {
		return false, err
	}
	res, err := tx.Exec(ctx,
		`INSERT INTO install_fp_rate(fp_sha256, window_day, count, last_at)
		   VALUES (?, ?, 1, ?)
		 ON CONFLICT(fp_sha256, window_day) DO UPDATE
		   SET count = count + 1, last_at = excluded.last_at
		   WHERE install_fp_rate.count < ? AND install_fp_rate.last_at < ?`,
		g.FPSHA256, g.WindowDay, g.Cutoff.UTC(), g.Cap, g.Cutoff.UTC())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// insertInstall inserts the brand-new install row (status 'active'). last_seen_at
// starts equal to created_at; empty fingerprint/client store as SQL NULL.
func insertInstall(ctx context.Context, tx *orm.DB, id string, p dinstall.IssueParams) error {
	now := p.Now.UTC()
	_, err := tx.Exec(ctx,
		`INSERT INTO installs(id, public_key, key_thumbprint, fingerprint, client, status, created_at, last_seen_at)
		   VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.PublicKey, p.Thumbprint, nullStr(p.Fingerprint), nullStr(p.Client),
		string(dinstall.StatusActive), now, now)
	return err
}

// Lookup resolves a public install id to its status from the read pool.
func (s *Store) Lookup(ctx context.Context, installID string) (dinstall.Status, bool, error) {
	var status string
	err := s.r.QueryRow(ctx,
		`SELECT status FROM installs WHERE id = ?`, installID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return dinstall.Status(status), true, nil
}

// PublicKey resolves the Ed25519 public key bound to installID.
func (s *Store) PublicKey(ctx context.Context, installID string) ([]byte, bool, error) {
	var key []byte
	err := s.r.QueryRow(ctx, `SELECT public_key FROM installs WHERE id = ?`, installID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// RefreshLastSeen bumps installs.last_seen_at, DB-side throttled by the WHERE
// predicate as a backstop to the app's in-Go throttle. Best-effort: the
// caller swallows the error (a missed activity timestamp is cosmetic).
func (s *Store) RefreshLastSeen(ctx context.Context, id string, now time.Time, interval time.Duration) error {
	cutoff := now.UTC().Add(-interval)
	_, err := s.w.Exec(ctx,
		`UPDATE installs SET last_seen_at = ? WHERE id = ? AND last_seen_at < ?`,
		now.UTC(), id, cutoff)
	return err
}

// InstallsToday returns today's global issuance count; a missing row reads as 0.
func (s *Store) InstallsToday(ctx context.Context, windowDay string) (int64, error) {
	var count int64
	err := s.r.QueryRow(ctx,
		`SELECT count FROM install_global_rate WHERE window_day = ?`, windowDay).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

// nullStr maps an empty string to SQL NULL (an empty fingerprint/client is stored
// as NULL, never the empty string).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueConflict reports whether err is a SQLite UNIQUE constraint violation
// (the install id or key thumbprint collision path). The pure-Go driver
// surfaces it in the error text; matching on the substring keeps this driver-
// agnostic without importing the driver's error type.
func isUniqueConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}
