package settingsstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/glebarez/go-sqlite" // pure-Go sqlite driver, registered as "sqlite".

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// newTestStore opens a real on-disk sqlite file (t.TempDir) with the production
// `settings` schema plus a CHECK that lets a test inject an UPSERT failure on a
// sentinel value — the only way to exercise the rollback path through the fixed
// map[string]string API. Writer is single-conn (matches the production write
// pool); reader is a separate pool over the same file.
const failValue = "__FAIL__"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	w.SetMaxOpenConns(1) // single serialized writer, as in production.
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Exec(`CREATE TABLE settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL CHECK (value <> '` + failValue + `'),
		updated_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	return New(orm.Open(w), orm.Open(r))
}

func loadAll(t *testing.T, s *Store) map[string]string {
	t.Helper()
	got, err := s.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return got
}

// TestLoadAllRoundTripsPersistAll: what PersistAll writes, LoadAll reads back
// verbatim across the writer→reader pool boundary (same file).
func TestLoadAllRoundTripsPersistAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := map[string]string{"a": "1", "b": "two", "c": "3.5"}
	if err := s.PersistAll(ctx, want); err != nil {
		t.Fatalf("PersistAll: %v", err)
	}
	if got := loadAll(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %v, want %v", got, want)
	}
}

// TestLoadAllEmpty: an untouched table yields an empty, non-nil map.
func TestLoadAllEmpty(t *testing.T) {
	s := newTestStore(t)
	got := loadAll(t, s)
	if got == nil {
		t.Fatal("LoadAll returned nil map")
	}
	if len(got) != 0 {
		t.Fatalf("LoadAll on empty table = %v, want empty", got)
	}
}

// TestPersistAllEmptyNoOp: an empty map opens no transaction and persists nothing.
func TestPersistAllEmptyNoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Seed a row, then PersistAll(nil/empty) must leave it untouched.
	if err := s.PersistAll(ctx, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.PersistAll(ctx, nil); err != nil {
		t.Fatalf("PersistAll(nil): %v", err)
	}
	if err := s.PersistAll(ctx, map[string]string{}); err != nil {
		t.Fatalf("PersistAll(empty): %v", err)
	}
	if got := loadAll(t, s); !reflect.DeepEqual(got, map[string]string{"k": "v"}) {
		t.Fatalf("after no-op persists = %v, want {k:v}", got)
	}
}

// TestPersistAllOverwrites: re-persisting an existing key replaces its value.
func TestPersistAllOverwrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.PersistAll(ctx, map[string]string{"k": "old"}); err != nil {
		t.Fatalf("persist old: %v", err)
	}
	if err := s.PersistAll(ctx, map[string]string{"k": "new", "x": "y"}); err != nil {
		t.Fatalf("persist new: %v", err)
	}
	want := map[string]string{"k": "new", "x": "y"}
	if got := loadAll(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("after overwrite = %v, want %v", got, want)
	}
}

// TestPersistAllAtomicRollback: if any key fails mid-transaction, the whole
// batch rolls back — neither the good key nor any prior table state changes.
func TestPersistAllAtomicRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Pre-existing state that must survive the failed batch unchanged.
	if err := s.PersistAll(ctx, map[string]string{"keep": "stable"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// One good key + one key whose value trips the CHECK → tx must abort whole.
	bad := map[string]string{"good": "ok", "bad": failValue}
	if err := s.PersistAll(ctx, bad); err == nil {
		t.Fatal("PersistAll with failing key: want error, got nil")
	}

	// Nothing from the failed batch persisted; prior state intact.
	want := map[string]string{"keep": "stable"}
	if got := loadAll(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("after rollback = %v, want %v (no partial writes)", got, want)
	}
}
