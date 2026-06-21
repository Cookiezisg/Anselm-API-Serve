// Package sqlite owns the SQLite connection pools + the versioned migration
// runner. Pool split (蓝图 §6): a single-conn Writer (SetMaxOpenConns(1),
// _txlock=immediate so every tx is BEGIN IMMEDIATE) serializes all mutations,
// and a bounded Reader pool (WAL allows concurrent readers) caps per-connection
// cache/mmap memory. All PRAGMAs are injected per-connection via the DSN so both
// pools apply the identical set. Migrations run on the Writer before serving.
package sqlite

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/glebarez/go-sqlite" // pure-Go (no CGO) driver, registered as "sqlite".

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// Config carries the DB path plus the tunable SQLite knobs. The hardcoded PRAGMAs
// (journal_mode/busy_timeout/foreign_keys/synchronous) are not represented here —
// only the env-driven, memory-budget-coupled knobs are.
type Config struct {
	Path string // sqlite file path.

	CacheKiB          int           // per-conn page-cache ceiling, KiB (DSN cache_size negative = KiB).
	MmapBytes         int64         // mmap_size bytes (read path saves a copy; 0 disables).
	WalAutocheckpoint int           // WAL auto-checkpoint trigger page count (bounds WAL growth).
	ReadPoolMaxConns  int           // read-pool concurrency ceiling (must be > 0; bounds memory).
	ConnMaxLifetime   time.Duration // max conn lifetime (recycle to avoid pinning mmap/fd; 0 = no cap).
}

// DefaultConfig returns conservative defaults matching the legacy DefaultTuning,
// so callers (and tests) keep a near-zero-config call site while exercising the
// tuned DSN path. Path is left empty for the caller to set.
func DefaultConfig() Config {
	return Config{
		CacheKiB:          32768, // 32 MiB/conn.
		MmapBytes:         256 * 1024 * 1024,
		WalAutocheckpoint: 4000,
		ReadPoolMaxConns:  4,
		ConnMaxLifetime:   30 * time.Minute,
	}
}

// DB holds the separated write and read pools over the same file. Writer is the
// single serialized writer (BEGIN IMMEDIATE); Reader is the bounded concurrent
// read pool.
type DB struct {
	Writer *orm.DB
	Reader *orm.DB

	w *sql.DB // underlying pools, retained for Close.
	r *sql.DB
}

// dsn builds the per-connection DSN. Every PRAGMA is injected here so both pools
// apply the identical set. The write pool also takes _txlock=immediate so each
// BeginTx grabs the write lock up front (BEGIN IMMEDIATE), independent of the
// MaxOpenConns(1) cap (蓝图 §7.3). cache_size is negative = KiB (page-count would
// be unstable across page sizes), keeping the memory ceiling controllable.
func dsn(cfg Config, immediate bool) string {
	d := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
			"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"+
			"&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)"+
			"&_pragma=wal_autocheckpoint(%d)",
		cfg.Path, cfg.CacheKiB, cfg.MmapBytes, cfg.WalAutocheckpoint,
	)
	if immediate {
		d += "&_txlock=immediate"
	}
	return d
}

// Open opens the write and read pools applying the tuning, runs pending
// migrations on the writer, and returns the wrapped pools. The writer is opened
// and migrated before the reader so DDL only ever touches the single-writer pool.
func Open(cfg Config) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(cfg, true))
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	// Single writer serializes all mutations; avoids SQLITE_BUSY churn (蓝图 §6).
	w.SetMaxOpenConns(1)
	if cfg.ConnMaxLifetime > 0 {
		w.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if err := w.Ping(); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("ping write pool: %w", err)
	}

	writer := orm.Open(w)
	if err := migrate(writer); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	r, err := sql.Open("sqlite", dsn(cfg, false))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	// Bound the read pool: a few concurrent readers saturate a 1-vCPU box; an
	// unbounded pool would let a burst spawn hundreds of conns, each carrying its
	// own per-connection cache/mmap budget → memory blowup.
	if cfg.ReadPoolMaxConns > 0 {
		r.SetMaxOpenConns(cfg.ReadPoolMaxConns)
		idle := cfg.ReadPoolMaxConns / 2
		if idle < 1 {
			idle = 1
		}
		r.SetMaxIdleConns(idle)
	}
	if cfg.ConnMaxLifetime > 0 {
		r.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if err := r.Ping(); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, fmt.Errorf("ping read pool: %w", err)
	}

	return &DB{Writer: writer, Reader: orm.Open(r), w: w, r: r}, nil
}

// Close closes both pools. Idempotent: database/sql treats Close on an
// already-closed pool as a no-op, so a second call is harmless.
func (db *DB) Close() error {
	var first error
	if err := db.w.Close(); err != nil {
		first = err
	}
	if err := db.r.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
