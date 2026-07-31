package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "t.db")
	return cfg
}

func openT(t *testing.T) *DB {
	t.Helper()
	db, err := Open(testConfig(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestOpenCreatesSchema: Open runs the migration so every table exists and is
// queryable on both pools, and a write on the writer is visible on the reader
// (proving the two pools really are the same file).
func TestOpenCreatesSchema(t *testing.T) {
	db := openT(t)
	ctx := context.Background()

	tables := []string{
		"installs", "install_ip_rate", "install_global_rate", "install_fp_rate", "settings",
		"quota_monthly", "install_spend_daily", "provider_spend_daily",
		"global_spend_daily", "global_spend_monthly", "spend_ledger",
		"install_category_daily", "media_uploads", "media_leases", "install_voices",
	}
	for _, table := range tables {
		var name string
		err := db.Reader.QueryRow(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}

	var idx string
	if err := db.Reader.QueryRow(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_spend_ledger_open'`).Scan(&idx); err != nil {
		t.Fatalf("idx_spend_ledger_open not created: %v", err)
	}

	// Writer → Reader visibility on the same file.
	if _, err := db.Writer.Exec(ctx,
		`INSERT INTO global_spend_daily(period_day, spend_pusd, requests) VALUES ('2026-01-01', 5, 1)`); err != nil {
		t.Fatalf("write: %v", err)
	}
	var used int64
	if err := db.Reader.QueryRow(ctx,
		`SELECT spend_pusd FROM global_spend_daily WHERE period_day='2026-01-01'`).Scan(&used); err != nil {
		t.Fatalf("read: %v", err)
	}
	if used != 5 {
		t.Fatalf("reader sees %d want 5 (pools not on the same file?)", used)
	}
}

// TestRetiredTablesAreGone pins the SQUASH itself: the v1 token ledger must not
// come back. Naming the three tables explicitly means a future migration that
// resurrects one fails here rather than quietly reintroducing an accounting
// surface nothing reads.
//
// TestRetiredTablesAreGone 钉住**压平本身**:v1 token 账本不能回来。把三张表指名写出,
// 意味着将来某个迁移复活其中一张会在这里失败,而不是悄悄重新引入一个没人读的账务面。
func TestRetiredTablesAreGone(t *testing.T) {
	db := openT(t)
	for _, table := range []string{"usage", "budget", "ledger"} {
		var name string
		err := db.Reader.QueryRow(context.Background(),
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err == nil {
			t.Fatalf("retired v1 table %q is present again", table)
		}
	}
}

func TestPragmasWALAndForeignKeys(t *testing.T) {
	db := openT(t)
	ctx := context.Background()

	check := func(name string, q func(string) *strScanner, want string) {
		got := q(name).str
		if !strings.EqualFold(got, want) {
			t.Fatalf("%s = %q want %q", name, got, want)
		}
	}
	wStr := func(q string) *strScanner {
		s := &strScanner{}
		if err := db.Writer.QueryRow(ctx, q).Scan(&s.str); err != nil {
			t.Fatalf("writer %s: %v", q, err)
		}
		return s
	}
	rStr := func(q string) *strScanner {
		s := &strScanner{}
		if err := db.Reader.QueryRow(ctx, q).Scan(&s.str); err != nil {
			t.Fatalf("reader %s: %v", q, err)
		}
		return s
	}
	for _, q := range []func(string) *strScanner{wStr, rStr} {
		check(`PRAGMA journal_mode`, q, "wal")
		check(`PRAGMA foreign_keys`, q, "1")
	}
}

type strScanner struct{ str string }

// TestTuningPragmasApplied: cache_size/mmap_size/wal_autocheckpoint land on both
// pools via the DSN (cache_size reported back as negative KiB).
func TestTuningPragmasApplied(t *testing.T) {
	cfg := testConfig(t)
	cfg.CacheKiB = 32768
	cfg.MmapBytes = 128 * 1024 * 1024
	cfg.WalAutocheckpoint = 2000
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	checkInt := func(label, q string, scan func(context.Context, string) (int64, error), want int64) {
		got, err := scan(ctx, q)
		if err != nil {
			t.Fatalf("%s %s: %v", label, q, err)
		}
		if got != want {
			t.Fatalf("%s %s = %d want %d", label, q, got, want)
		}
	}
	wInt := func(ctx context.Context, q string) (int64, error) {
		var v int64
		err := db.Writer.QueryRow(ctx, q).Scan(&v)
		return v, err
	}
	rInt := func(ctx context.Context, q string) (int64, error) {
		var v int64
		err := db.Reader.QueryRow(ctx, q).Scan(&v)
		return v, err
	}
	for label, scan := range map[string]func(context.Context, string) (int64, error){"W": wInt, "R": rInt} {
		checkInt(label, `PRAGMA mmap_size`, scan, 128*1024*1024)
		checkInt(label, `PRAGMA wal_autocheckpoint`, scan, 2000)
		checkInt(label, `PRAGMA cache_size`, scan, -32768)
	}
}

// TestWritePoolSingleConn: the writer is MaxOpenConns=1 (single serialized
// writer); the reader must NOT be capped to 1 (WAL allows concurrent readers).
func TestWritePoolSingleConn(t *testing.T) {
	db := openT(t)
	if got := db.w.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool MaxOpenConnections=%d want 1", got)
	}
	if got := db.r.Stats().MaxOpenConnections; got == 1 {
		t.Fatal("read pool capped to 1 — should allow concurrent reads")
	}
}

// TestReadPoolBounded: the reader honors the configured MaxOpenConns ceiling.
func TestReadPoolBounded(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReadPoolMaxConns = 5
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := db.r.Stats().MaxOpenConnections; got != 5 {
		t.Fatalf("read pool MaxOpenConnections=%d want 5", got)
	}
	if got := db.w.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool MaxOpenConnections=%d want 1", got)
	}
}

// TestMigrateIdempotent: a second Open on the same file is a no-op (no new
// schema_migrations rows, no error).
func TestMigrateIdempotent(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	db1, err := Open(cfg)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	var n1 int
	if err := db1.Reader.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n1); err != nil {
		t.Fatalf("count 1: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	db2, err := Open(cfg)
	if err != nil {
		t.Fatalf("open 2 (should be idempotent): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	var n2 int
	if err := db2.Reader.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n2); err != nil {
		t.Fatalf("count 2: %v", err)
	}
	if n1 != n2 {
		t.Fatalf("schema_migrations rows changed on re-open: %d → %d", n1, n2)
	}
	if n1 == 0 {
		t.Fatal("no migrations recorded")
	}
}

// TestSchemaMigrationsRecordsVersions: every embedded migration is recorded with
// its version + a non-empty checksum.
func TestSchemaMigrationsRecordsVersions(t *testing.T) {
	db := openT(t)
	ctx := context.Background()

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations")
	}
	for _, m := range migs {
		var sum string
		if err := db.Reader.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version=?`, m.version).Scan(&sum); err != nil {
			t.Fatalf("version %d not recorded: %v", m.version, err)
		}
		if sum != m.checksum {
			t.Fatalf("version %d checksum = %q want %q", m.version, sum, m.checksum)
		}
	}
}

// TestChecksumDriftFails: if an already-applied migration's recorded checksum
// no longer matches the embedded body, re-opening fails (tamper detection).
func TestChecksumDriftFails(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Writer.Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(cfg)
	if err == nil {
		t.Fatal("Open must fail on checksum drift")
	}
	if !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("error = %v, want checksum drift", err)
	}
}

// TestFutureVersionFails: a DB recording a version newer than this binary knows
// is refused (downgrade guard — old binary, new schema must not run blindly).
func TestFutureVersionFails(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Writer.Exec(ctx,
		`INSERT INTO schema_migrations(version, applied_at, checksum) VALUES (9999, ?, 'x')`,
		time.Now().UTC()); err != nil {
		t.Fatalf("insert future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(cfg)
	if err == nil {
		t.Fatal("Open must fail on a future/unknown version")
	}
	if !strings.Contains(err.Error(), "unknown to this binary") {
		t.Fatalf("error = %v, want unknown-version failure", err)
	}
}

// TestOpenBadPathFails: an uncreatable DB path fails fast on Open (writer ping).
func TestOpenBadPathFails(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "no", "such", "dir", "s.db")
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open on an uncreatable path must error")
	}
}

// TestCloseIdempotent: Close twice does not panic and the second call is a
// harmless no-op.
func TestCloseIdempotent(t *testing.T) {
	db, err := Open(testConfig(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("second Close panicked: %v", rec)
		}
	}()
	if err := db.Close(); err != nil {
		t.Fatalf("second close should be a harmless no-op, got %v", err)
	}
}

// TestParseVersion: the NNNN_ prefix parser accepts well-formed names and
// rejects malformed ones (build-time authoring guard).
func TestParseVersion(t *testing.T) {
	if v, err := parseVersion("0001_init.sql"); err != nil || v != 1 {
		t.Fatalf("parseVersion(0001_init.sql) = %d,%v want 1,nil", v, err)
	}
	if v, err := parseVersion("0042_add_col.sql"); err != nil || v != 42 {
		t.Fatalf("parseVersion(0042_add_col.sql) = %d,%v want 42,nil", v, err)
	}
	for _, bad := range []string{"init.sql", "_init.sql", "xx_init.sql"} {
		if _, err := parseVersion(bad); err == nil {
			t.Fatalf("parseVersion(%q) should error", bad)
		}
	}
}
