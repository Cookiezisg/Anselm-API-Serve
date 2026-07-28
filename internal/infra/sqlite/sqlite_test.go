package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
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

// TestOpenCreatesSchema: Open runs migrations so the legacy and v2 tables exist
// queryable on both pools, and a write on the writer is visible on the reader
// (same file).
func TestOpenCreatesSchema(t *testing.T) {
	db := openT(t)
	ctx := context.Background()

	tables := []string{
		"installs", "usage", "budget", "ledger",
		"install_ip_rate", "install_global_rate", "install_fp_rate", "settings",
		"quota_monthly", "install_spend_daily", "provider_spend_daily",
		"global_spend_daily", "global_spend_monthly", "spend_ledger",
		"media_uploads", "media_leases",
	}
	for _, table := range tables {
		var name string
		err := db.Reader.QueryRow(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}

	// Both the read-only legacy ledger and active v2 ledger retain their indexes.
	var idx string
	if err := db.Reader.QueryRow(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_ledger_open'`).Scan(&idx); err != nil {
		t.Fatalf("idx_ledger_open not created: %v", err)
	}
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

// TestQwenMigrationCreatesCleanProviderAccounting proves the unshipped initial
// schema admits Qwen directly and does not retain a retired runtime provider.
func TestQwenMigrationCreatesCleanProviderAccounting(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	raw, err := sql.Open("sqlite", dsn(cfg, true))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	w := orm.Open(raw)
	if err := ensureMigrationsTable(ctx, w); err != nil {
		t.Fatalf("ensure migrations: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migs) != 7 {
		t.Fatalf("migration count=%d want 7", len(migs))
	}
	for _, migration := range migs {
		if err := applyOne(ctx, w, migration); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
	}
	if _, err := w.Exec(ctx, `INSERT INTO provider_spend_daily(provider,period_day,spend_pusd,requests) VALUES ('qwen','2026-07-24',11,1)`); err != nil {
		t.Fatalf("insert qwen provider spend: %v", err)
	}
	if _, err := w.Exec(ctx, `INSERT INTO provider_spend_daily(provider,period_day,spend_pusd,requests) VALUES ('retired','2026-07-24',7,1)`); err == nil {
		t.Fatal("retired provider unexpectedly accepted by clean schema")
	}
	if _, err := w.Exec(ctx, `INSERT INTO media_uploads(id,install_id,expected_sha256,mime_type,total_bytes,received_bytes,state,expires_at,created_at,updated_at)
		VALUES ('mup_1','ins_1','0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef','image/png',1,0,'open','2026-07-25','2026-07-24','2026-07-24')`); err != nil {
		t.Fatalf("insert media upload: %v", err)
	}
	if _, err := w.Exec(ctx, `INSERT INTO media_leases(id,install_id,upload_id,sha256,mime_type,size_bytes,fetch_token_hash,state,expires_at,created_at)
		VALUES ('mls_1','ins_1','mup_1','0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef','image/png',1,'secret-hash','active','2026-07-25','2026-07-24')`); err != nil {
		t.Fatalf("insert media lease: %v", err)
	}
	if _, err := w.Exec(ctx, `INSERT INTO media_leases(id,install_id,upload_id,sha256,mime_type,size_bytes,fetch_token_hash,state,expires_at,created_at)
		VALUES ('mls_2','ins_2','mup_1','0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef','image/png',1,'other-hash','active','2026-07-25','2026-07-24')`); err == nil {
		t.Fatal("same upload unexpectedly received a second lease")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
}

// TestPragmasWALAndForeignKeys: every connection on BOTH pools reports WAL +
// foreign_keys ON (DSN injects per-connection; a regressed DSN silently drops
// durability/integrity).
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

func TestProviderSpendMigrationBackfillsV1AndSettings(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	raw, err := sql.Open("sqlite", dsn(cfg, true))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	w := orm.Open(raw)
	if err := ensureMigrationsTable(ctx, w); err != nil {
		t.Fatalf("ensure migrations: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil || len(migs) < 2 {
		t.Fatalf("load migrations: len=%d err=%v", len(migs), err)
	}
	if err := applyOne(ctx, w, migs[0]); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	created := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO usage(install_id, period, count, tokens) VALUES ('ins_1','2026-06',3,0)`, nil},
		{`INSERT INTO usage(install_id, period, count, tokens) VALUES ('ins_1','2026-06-20',2,10)`, nil},
		{`INSERT INTO budget(period, tokens_used, requests) VALUES ('2026-06-20',20,4)`, nil},
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at) VALUES ('req_open','ins_1','2026-06-20',5,NULL,?)`, []any{created}},
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at) VALUES ('req_done','ins_1','2026-06-20',5,2,?)`, []any{created}},
		{`INSERT INTO settings(key,value,updated_at) VALUES ('GLOBAL_DAILY_BUDGET_TOKENS','101',?)`, []any{created}},
		{`INSERT INTO settings(key,value,updated_at) VALUES ('INSTALL_DAILY_TOKEN_CAP','1',?)`, []any{created}},
		{`INSERT INTO settings(key,value,updated_at) VALUES ('MODEL_ALLOWLIST','deepseek-v4-flash',?)`, []any{created}},
		{`INSERT INTO settings(key,value,updated_at) VALUES ('RATE_PER_MIN','20',?)`, []any{created}},
	}
	for _, item := range seed {
		if _, err := w.Exec(ctx, item.q, item.args...); err != nil {
			_ = raw.Close()
			t.Fatalf("seed %q: %v", item.q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var requests, spend int64
	if err := db.Reader.QueryRow(ctx,
		`SELECT requests FROM quota_monthly WHERE install_id='ins_1' AND period_month='2026-06'`).Scan(&requests); err != nil || requests != 3 {
		t.Fatalf("monthly requests=%d err=%v", requests, err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT spend_pusd, requests FROM install_spend_daily WHERE install_id='ins_1' AND period_day='2026-06-20'`).Scan(&spend, &requests); err != nil || spend != 2_800_000 || requests != 2 {
		t.Fatalf("install=(%d,%d) err=%v", spend, requests, err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT spend_pusd, requests FROM global_spend_daily WHERE period_day='2026-06-20'`).Scan(&spend, &requests); err != nil || spend != 5_600_000 || requests != 4 {
		t.Fatalf("global=(%d,%d) err=%v", spend, requests, err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT spend_pusd, requests FROM global_spend_monthly WHERE period_month='2026-06'`).Scan(&spend, &requests); err != nil || spend != 5_600_000 || requests != 4 {
		t.Fatalf("global month=(%d,%d) err=%v", spend, requests, err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT spend_pusd FROM provider_spend_daily WHERE provider='deepseek' AND period_day='2026-06-20'`).Scan(&spend); err != nil || spend != 5_600_000 {
		t.Fatalf("provider=%d err=%v", spend, err)
	}

	var state string
	var reserved int64
	var charged sql.NullInt64
	if err := db.Reader.QueryRow(ctx,
		`SELECT state,reserved_pusd,charged_pusd FROM spend_ledger WHERE request_id='req_open'`).Scan(&state, &reserved, &charged); err != nil || state != "open" || reserved != 1_400_000 || charged.Valid {
		t.Fatalf("open ledger=(%s,%d,%v) err=%v", state, reserved, charged, err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT state,reserved_pusd,charged_pusd FROM spend_ledger WHERE request_id='req_done'`).Scan(&state, &reserved, &charged); err != nil || state != "settled" || reserved != 1_400_000 || !charged.Valid || charged.Int64 != 560_000 {
		t.Fatalf("done ledger=(%s,%d,%v) err=%v", state, reserved, charged, err)
	}

	settings := map[string]string{}
	rows, err := db.Reader.Query(ctx, `SELECT key,value FROM settings`)
	if err != nil {
		t.Fatalf("settings query: %v", err)
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("settings scan: %v", err)
		}
		settings[key] = value
	}
	_ = rows.Close()
	for _, removed := range []string{
		"MODEL_ALLOWLIST",
		"GLOBAL_DAILY_BUDGET_TOKENS",
		"INSTALL_DAILY_TOKEN_CAP",
		"GLOBAL_DAILY_SPEND_MICRO_USD",
		"INSTALL_DAILY_SPEND_MICRO_USD",
		"DEEPSEEK_DAILY_SPEND_MICRO_USD",
		"QWEN_DAILY_SPEND_MICRO_USD",
	} {
		if _, ok := settings[removed]; ok {
			t.Fatalf("legacy setting %s was not removed", removed)
		}
	}
	if settings["RATE_PER_MIN"] != "20" {
		t.Fatalf("unrelated setting lost: %v", settings)
	}

	// v1 tables are retained byte-for-byte as read-only audit history.
	var legacyTokens int64
	if err := db.Reader.QueryRow(ctx,
		`SELECT tokens_used FROM budget WHERE period='2026-06-20'`).Scan(&legacyTokens); err != nil || legacyTokens != 20 {
		t.Fatalf("legacy budget=%d err=%v", legacyTokens, err)
	}
}

func TestProviderSpendMigrationFloorsWalletsFromChargeableLegacyLedger(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	raw, err := sql.Open("sqlite", dsn(cfg, true))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	w := orm.Open(raw)
	if err := ensureMigrationsTable(ctx, w); err != nil {
		t.Fatalf("ensure migrations: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil || len(migs) < 2 {
		t.Fatalf("load migrations: len=%d err=%v", len(migs), err)
	}
	if err := applyOne(ctx, w, migs[0]); err != nil {
		t.Fatalf("apply v1: %v", err)
	}

	created := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO usage(install_id,period,count,tokens) VALUES ('ins_1','2026-07',4,0)`, nil},
		// Five tokens is the copied v1 wallet. The chargeable ledger floor below is
		// ten for ins_1 and fourteen globally, so MAX must raise rather than add.
		{`INSERT INTO usage(install_id,period,count,tokens) VALUES ('ins_1','2026-07-20',3,5)`, nil},
		{`INSERT INTO budget(period,tokens_used,requests) VALUES ('2026-07-20',5,3)`, nil},
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
		  VALUES ('req_done','ins_1','2026-07-20',9,2,?)`, []any{created}},
		// This is the exact terminal encoding used by the v1 orphan reconciler:
		// settled=reserved while its five-token wallet reservation was refunded.
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
		  VALUES ('req_old_orphan','ins_1','2026-07-20',5,5,?)`, []any{created}},
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
		  VALUES ('req_open','ins_1','2026-07-20',3,NULL,?)`, []any{created}},
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
		  VALUES ('req_rolled_back','ins_1','2026-07-20',4,0,?)`, []any{created}},
		// No matching usage row: the ledger floor must create the install wallet.
		{`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
		  VALUES ('req_missing_wallet','ins_2','2026-07-20',4,4,?)`, []any{created}},
	}
	for _, item := range seed {
		if _, err := w.Exec(ctx, item.query, item.args...); err != nil {
			_ = raw.Close()
			t.Fatalf("seed %q: %v", item.query, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	assertWallets := func(db *DB) {
		t.Helper()
		for installID, want := range map[string]int64{
			"ins_1": 10 * 280_000,
			"ins_2": 4 * 280_000,
		} {
			var spend, requests int64
			if err := db.Reader.QueryRow(ctx,
				`SELECT spend_pusd,requests FROM install_spend_daily WHERE install_id=? AND period_day='2026-07-20'`,
				installID).Scan(&spend, &requests); err != nil || spend != want {
				t.Fatalf("install %s=(%d,%d) err=%v want spend=%d", installID, spend, requests, err, want)
			}
			if installID == "ins_1" && requests != 3 {
				t.Fatalf("copied daily sublimit count changed: got %d want 3", requests)
			}
			if installID == "ins_2" && requests != 0 {
				t.Fatalf("ledger-created install row invented a sublimit count: got %d", requests)
			}
		}
		for table, query := range map[string]string{
			"provider": `SELECT spend_pusd,requests FROM provider_spend_daily
			              WHERE provider='deepseek' AND period_day='2026-07-20'`,
			"global": `SELECT spend_pusd,requests FROM global_spend_daily
			            WHERE period_day='2026-07-20'`,
		} {
			var spend, requests int64
			if err := db.Reader.QueryRow(ctx, query).Scan(&spend, &requests); err != nil ||
				spend != 14*280_000 || requests != 4 {
				t.Fatalf("%s=(%d,%d) err=%v want (%d,4)", table, spend, requests, err, 14*280_000)
			}
		}
	}
	assertWallets(db)

	var openState, rollbackState string
	var openCharged, rollbackCharged sql.NullInt64
	if err := db.Reader.QueryRow(ctx,
		`SELECT state,charged_pusd FROM spend_ledger WHERE request_id='req_open'`).Scan(&openState, &openCharged); err != nil {
		t.Fatalf("read migrated open: %v", err)
	}
	if err := db.Reader.QueryRow(ctx,
		`SELECT state,charged_pusd FROM spend_ledger WHERE request_id='req_rolled_back'`).Scan(&rollbackState, &rollbackCharged); err != nil {
		t.Fatalf("read migrated rollback: %v", err)
	}
	if openState != "open" || openCharged.Valid {
		t.Fatalf("open migration=(%s,%v)", openState, openCharged)
	}
	if rollbackState != "rolled_back" || !rollbackCharged.Valid || rollbackCharged.Int64 != 0 {
		t.Fatalf("rollback migration=(%s,%v)", rollbackState, rollbackCharged)
	}

	// Re-opening must skip the checksummed migration rather than applying the floor
	// a second time. The values remain byte-for-byte unchanged.
	if err := db.Close(); err != nil {
		t.Fatalf("close migrated: %v", err)
	}
	db, err = Open(cfg)
	if err != nil {
		t.Fatalf("reopen migrated: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertWallets(db)
}

func TestProviderSpendMigrationRejectsUnsafeLegacyLedger(t *testing.T) {
	const maxConvertibleTokens = int64(32_940_614_417_338)
	cases := []struct {
		name string
		seed func(context.Context, *orm.DB) error
	}{
		{
			name: "aggregate multiplication overflow",
			seed: func(ctx context.Context, w *orm.DB) error {
				if _, err := w.Exec(ctx,
					`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
					 VALUES ('req_max','ins_1','2026-07-20',?,NULL,?)`,
					maxConvertibleTokens, time.Now().UTC()); err != nil {
					return err
				}
				_, err := w.Exec(ctx,
					`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
					 VALUES ('req_one_more','ins_2','2026-07-20',1,1,?)`, time.Now().UTC())
				return err
			},
		},
		{
			name: "negative settlement",
			seed: func(ctx context.Context, w *orm.DB) error {
				_, err := w.Exec(ctx,
					`INSERT INTO ledger(request_id,install_id,period_day,reserved,settled,created_at)
					 VALUES ('req_negative','ins_1','2026-07-20',1,-1,?)`, time.Now().UTC())
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			ctx := context.Background()
			raw, err := sql.Open("sqlite", dsn(cfg, true))
			if err != nil {
				t.Fatalf("open raw: %v", err)
			}
			raw.SetMaxOpenConns(1)
			w := orm.Open(raw)
			if err := ensureMigrationsTable(ctx, w); err != nil {
				t.Fatalf("ensure migrations: %v", err)
			}
			migs, err := loadMigrations()
			if err != nil || len(migs) < 2 {
				t.Fatalf("load migrations: len=%d err=%v", len(migs), err)
			}
			if err := applyOne(ctx, w, migs[0]); err != nil {
				t.Fatalf("apply v1: %v", err)
			}
			if err := tc.seed(ctx, w); err != nil {
				t.Fatalf("seed unsafe v1: %v", err)
			}
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}

			if db, err := Open(cfg); err == nil {
				_ = db.Close()
				t.Fatal("unsafe legacy ledger must fail migration")
			}

			// applyOne wraps the SQL body and migration row in one transaction: a
			// rejected floor must leave neither v2 schema nor version 2 behind.
			check, err := sql.Open("sqlite", dsn(cfg, true))
			if err != nil {
				t.Fatalf("reopen raw: %v", err)
			}
			t.Cleanup(func() { _ = check.Close() })
			var versionRows, v2Tables int64
			if err := check.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE version=2`).Scan(&versionRows); err != nil {
				t.Fatalf("query migration ledger: %v", err)
			}
			if err := check.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='spend_ledger'`).Scan(&v2Tables); err != nil {
				t.Fatalf("query v2 schema: %v", err)
			}
			if versionRows != 0 || v2Tables != 0 {
				t.Fatalf("failed migration partially committed: version=%d tables=%d", versionRows, v2Tables)
			}
		})
	}
}

func TestGlobalMonthlyMigrationDropsRetiredDailySettings(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	raw, err := sql.Open("sqlite", dsn(cfg, true))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	w := orm.Open(raw)
	if err := ensureMigrationsTable(ctx, w); err != nil {
		t.Fatalf("ensure migrations: %v", err)
	}
	migs, _ := loadMigrations()
	if err := applyOne(ctx, w, migs[0]); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	now := time.Now().UTC()
	for _, kv := range [][2]string{
		{"GLOBAL_DAILY_BUDGET_TOKENS", "100"},
		{"GLOBAL_DAILY_SPEND_MICRO_USD", "777"},
	} {
		if _, err := w.Exec(ctx, `INSERT INTO settings(key,value,updated_at) VALUES (?,?,?)`, kv[0], kv[1], now); err != nil {
			t.Fatalf("seed setting: %v", err)
		}
	}
	_ = raw.Close()
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var n int64
	if err := db.Reader.QueryRow(ctx,
		`SELECT COUNT(*) FROM settings WHERE key IN ('GLOBAL_DAILY_BUDGET_TOKENS','GLOBAL_DAILY_SPEND_MICRO_USD')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("retired settings rows=%d err=%v", n, err)
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
