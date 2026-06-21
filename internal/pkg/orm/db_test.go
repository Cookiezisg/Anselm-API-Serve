package orm_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/glebarez/go-sqlite" // driver, registered as "sqlite"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// openMem opens a private in-memory database and the schema the tests share.
func openMem(t *testing.T) *orm.DB {
	t.Helper()
	// Single shared conn so the in-memory DB survives across the pool's lifetime.
	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	pool.SetMaxOpenConns(1)
	db := orm.Open(pool)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func count(t *testing.T, db *orm.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestTransactionCommitPersists(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	err := db.Transaction(ctx, func(tx *orm.DB) error {
		_, err := tx.Exec(ctx, `INSERT INTO t (v) VALUES ('a')`)
		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if got := count(t, db); got != 1 {
		t.Fatalf("after commit count = %d, want 1", got)
	}
}

func TestTransactionRollbackReverts(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	sentinel := errors.New("boom")
	err := db.Transaction(ctx, func(tx *orm.DB) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t (v) VALUES ('a')`); err != nil {
			return err
		}
		return sentinel // forces rollback
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transaction err = %v, want sentinel", err)
	}
	if got := count(t, db); got != 0 {
		t.Fatalf("after rollback count = %d, want 0", got)
	}
}

// Nested Transaction must reuse the outer tx: no new BeginTx, and the whole unit
// is atomic — an inner failure rolls back inserts made before it in the outer.
func TestNestedTransactionReusesOuter(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	t.Run("commit", func(t *testing.T) {
		err := db.Transaction(ctx, func(tx *orm.DB) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t (v) VALUES ('outer')`); err != nil {
				return err
			}
			return tx.Transaction(ctx, func(inner *orm.DB) error {
				_, err := inner.Exec(ctx, `INSERT INTO t (v) VALUES ('inner')`)
				return err
			})
		})
		if err != nil {
			t.Fatalf("Transaction: %v", err)
		}
		if got := count(t, db); got != 2 {
			t.Fatalf("count = %d, want 2", got)
		}
	})

	t.Run("inner_failure_rolls_back_outer", func(t *testing.T) {
		// Reset.
		if _, err := db.Exec(ctx, `DELETE FROM t`); err != nil {
			t.Fatalf("reset: %v", err)
		}
		sentinel := errors.New("inner boom")
		err := db.Transaction(ctx, func(tx *orm.DB) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t (v) VALUES ('outer')`); err != nil {
				return err
			}
			return tx.Transaction(ctx, func(inner *orm.DB) error {
				if _, err := inner.Exec(ctx, `INSERT INTO t (v) VALUES ('inner')`); err != nil {
					return err
				}
				return sentinel
			})
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if got := count(t, db); got != 0 {
			t.Fatalf("after inner failure count = %d, want 0 (atomic)", got)
		}
	})
}

func TestCloseIdempotentAndNilSafe(t *testing.T) {
	var nilDB *orm.DB
	if err := nilDB.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}

	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db := orm.Open(pool)
	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// database/sql Close is idempotent; the wrapper must not panic on a re-close.
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A tx-wrapper owns no pool, so Close on it is a no-op and never touches the
// root pool.
func TestCloseOnTxWrapperIsNoop(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	err := db.Transaction(ctx, func(tx *orm.DB) error {
		return tx.Close() // must be a no-op, returns nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	// Root pool still usable afterward.
	if _, err := db.Exec(ctx, `INSERT INTO t (v) VALUES ('x')`); err != nil {
		t.Fatalf("pool still usable: %v", err)
	}
}
