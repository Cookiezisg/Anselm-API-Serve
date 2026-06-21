// Package orm is a slim Unit-of-Work wrapper over database/sql: explicit SQL via
// a pool/tx-agnostic handle, with flat-nesting transactions. See
// docs/concepts/architecture.md.
package orm

import (
	"context"
	"database/sql"
	"fmt"
)

// DBTX is the subset of *sql.DB and *sql.Tx the gateway executes through, so a
// caller behaves identically inside or outside a transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps a database/sql handle (a connection pool, or a transaction). The pool
// field is non-nil only on the root — inside a transaction it is nil, which is
// how Transaction detects flat nesting.
type DB struct {
	h    DBTX
	pool *sql.DB
}

// Open wraps a *sql.DB pool as the root. Close it with DB.Close.
func Open(pool *sql.DB) *DB {
	return &DB{h: pool, pool: pool}
}

// Transaction runs fn inside one SQL transaction: commit on nil error, rollback
// otherwise. A call already inside a transaction reuses the outer tx (flat
// nesting — no savepoints), so composing transactional methods stays atomic.
func (db *DB) Transaction(ctx context.Context, fn func(tx *DB) error) error {
	if db.pool == nil {
		return fn(db) // already inside a tx — reuse it.
	}
	sqlTx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("orm: begin tx: %w", err)
	}
	if err := fn(&DB{h: sqlTx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("orm: commit tx: %w", err)
	}
	return nil
}

// Exec runs a raw statement — the escape hatch for non-CRUD SQL: migrations
// (DDL), PRAGMA, one-off maintenance.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.h.ExecContext(ctx, query, args...)
}

// Query runs a raw row-returning statement.
func (db *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.h.QueryContext(ctx, query, args...)
}

// QueryRow is the single-row form of Query.
func (db *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return db.h.QueryRowContext(ctx, query, args...)
}

// Close closes the underlying connection pool; safe on a transaction wrapper
// (which owns no pool) and on a nil receiver.
func (db *DB) Close() error {
	if db == nil || db.pool == nil {
		return nil
	}
	return db.pool.Close()
}
