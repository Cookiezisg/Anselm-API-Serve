package sqlite

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one embedded, version-tagged SQL file.
type migration struct {
	version  int
	name     string // full file name, e.g. "0001_init.sql".
	body     string
	checksum string // hex SHA-256 of body — drift detector.
}

// loadMigrations parses the embedded migrations/*.sql, deriving each version from
// the leading NNNN_ prefix and a SHA-256 checksum from the body. Result is sorted
// ascending by version; duplicate versions are a build-time authoring error.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[ver]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", ver, prev, e.Name())
		}
		seen[ver] = e.Name()

		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  ver,
			name:     e.Name(),
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseVersion extracts the integer version from a leading NNNN_ prefix.
func parseVersion(name string) (int, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("migration %q: missing NNNN_ version prefix", name)
	}
	ver, err := strconv.Atoi(name[:idx])
	if err != nil {
		return 0, fmt.Errorf("migration %q: bad version prefix: %w", name, err)
	}
	return ver, nil
}

// migrate applies pending migrations forward-only on the writer. It ensures the
// schema_migrations ledger, verifies the checksum of every already-applied
// migration (fail on drift), fails if the DB holds a version this binary doesn't
// know (downgrade guard), then applies each pending migration in its own
// transaction recording version+checksum+applied_at.
func migrate(writer *orm.DB) error {
	ctx := context.Background()
	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	if err := ensureMigrationsTable(ctx, writer); err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, writer)
	if err != nil {
		return err
	}

	known := map[int]migration{}
	maxKnown := 0
	for _, m := range migs {
		known[m.version] = m
		if m.version > maxKnown {
			maxKnown = m.version
		}
	}

	// Downgrade guard + checksum drift on already-applied rows.
	for ver, gotSum := range applied {
		m, ok := known[ver]
		if !ok {
			return fmt.Errorf("db at migration version %d unknown to this binary (max known %d): refusing to run", ver, maxKnown)
		}
		if gotSum != m.checksum {
			return fmt.Errorf("migration %s checksum drift: db has %s, embedded is %s", m.name, gotSum, m.checksum)
		}
	}

	// Apply pending in ascending order.
	for _, m := range migs {
		if _, done := applied[m.version]; done {
			continue
		}
		if err := applyOne(ctx, writer, m); err != nil {
			return err
		}
	}
	return nil
}

// ensureMigrationsTable creates the schema_migrations ledger if absent (its own
// existence is idempotent, so it predates the versioned migrations themselves).
func ensureMigrationsTable(ctx context.Context, writer *orm.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at DATETIME NOT NULL,
  checksum   TEXT NOT NULL
)`
	if _, err := writer.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

// appliedMigrations returns version → checksum for every recorded migration.
func appliedMigrations(ctx context.Context, writer *orm.DB) (map[int]string, error) {
	rows, err := writer.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int]string{}
	for rows.Next() {
		var ver int
		var sum string
		if err := rows.Scan(&ver, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[ver] = sum
	}
	return out, rows.Err()
}

// applyOne runs one migration body and records it atomically in a single
// transaction: a failed migration leaves no half-recorded row.
func applyOne(ctx context.Context, writer *orm.DB, m migration) error {
	return writer.Transaction(ctx, func(tx *orm.DB) error {
		if _, err := tx.Exec(ctx, m.body); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version, applied_at, checksum) VALUES (?, ?, ?)`,
			m.version, time.Now().UTC(), m.checksum,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
		return nil
	})
}
