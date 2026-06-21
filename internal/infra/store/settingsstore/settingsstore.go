// Package settingsstore is the settings-overlay persistence: the dashboard's
// runtime-config DB overlay (config hot-reload). It owns ONLY the DML over the
// `settings` table (key TEXT PK, value TEXT, updated_at). Secrets and startup
// hard-constraints never reach this layer — config.ApplyOverrides rejects them
// upstream. See docs/working/spec-database.md §1.8.
package settingsstore

import (
	"context"
	"fmt"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// Store reads and writes the settings overlay. Reads go to the bounded read
// pool; writes go to the single serialized writer so the multi-key persist is a
// single BEGIN IMMEDIATE transaction with no writer contention.
type Store struct {
	writer *orm.DB
	reader *orm.DB
}

// New wires the store to the separated pools. writer is the single-conn write
// pool (PersistAll's atomic UPSERT); reader is the concurrent read pool
// (LoadAll's full-table scan).
func New(writer, reader *orm.DB) *Store {
	return &Store{writer: writer, reader: reader}
}

// LoadAll returns every settings row as a key→value map (the full overlay read
// at startup). An empty table yields an empty, non-nil map.
func (s *Store) LoadAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.reader.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settingsstore: query overlay: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("settingsstore: scan overlay row: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settingsstore: iterate overlay: %w", err)
	}
	return out, nil
}

// PersistAll writes every key/value as one all-or-nothing UPSERT transaction on
// the writer: either all keys land or none do (rollback on any failure), so a
// crash mid-persist can never leave the on-disk overlay half-written and
// divergent from the in-memory config. An empty map is a no-op (no tx opened).
func (s *Store) PersistAll(ctx context.Context, overlay map[string]string) error {
	if len(overlay) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return s.writer.Transaction(ctx, func(tx *orm.DB) error {
		for k, v := range overlay {
			if _, err := tx.Exec(ctx,
				`INSERT INTO settings(key, value, updated_at) VALUES (?, ?, ?)
				   ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				k, v, now); err != nil {
				return fmt.Errorf("settingsstore: upsert %q: %w", k, err)
			}
		}
		return nil
	})
}
