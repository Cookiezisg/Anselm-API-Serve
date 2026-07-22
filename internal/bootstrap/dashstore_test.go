package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/quotastore"
)

func newDashStoreTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	cfg := sqlite.DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sqlite.Open(cfg)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBudgetSourceReadsNewSpendTableAndConvertsPUSD(t *testing.T) {
	db := newDashStoreTestDB(t)
	ctx := context.Background()
	month := time.Now().UTC().Format("2006-01")
	const (
		usedPUSD  = 12*billing.PicoUSDPerMicroUSD + 999_999
		limitPUSD = 42*billing.PicoUSDPerMicroUSD + 999_999
	)
	if _, err := db.Writer.Exec(ctx,
		`INSERT INTO global_spend_monthly(period_month, spend_pusd, requests) VALUES (?, ?, 3)`,
		month, usedPUSD); err != nil {
		t.Fatalf("insert global spend: %v", err)
	}
	// A stale v1 value must not influence the dashboard after accounting v2.
	if _, err := db.Writer.Exec(ctx,
		`INSERT INTO budget(period, tokens_used, requests) VALUES (?, 999999999, 99)`, month+"-01"); err != nil {
		t.Fatalf("insert legacy budget: %v", err)
	}

	source := budgetSource{
		ds:       dashStore{w: db.Writer, r: db.Reader, loc: time.UTC},
		getLimit: func() int64 { return limitPUSD },
	}
	gotDay, limitMicroUSD, usedMicroUSD, err := source.GlobalBudget(ctx)
	if err != nil {
		t.Fatalf("GlobalBudget: %v", err)
	}
	if gotDay != month || limitMicroUSD != 42 || usedMicroUSD != 12 {
		t.Fatalf("GlobalBudget = (%q, %d, %d), want (%q, 42, 12)",
			gotDay, limitMicroUSD, usedMicroUSD, month)
	}
}

func TestBudgetSourceMissingRowIsZeroButReadFailurePropagates(t *testing.T) {
	db := newDashStoreTestDB(t)
	ctx := context.Background()
	source := budgetSource{
		ds: dashStore{w: db.Writer, r: db.Reader, loc: time.UTC},
		getLimit: func() int64 {
			return 7 * billing.PicoUSDPerMicroUSD
		},
	}

	_, limit, used, err := source.GlobalBudget(ctx)
	if err != nil || limit != 7 || used != 0 {
		t.Fatalf("missing row = (limit=%d used=%d err=%v), want (7, 0, nil)", limit, used, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	if _, _, _, err := source.GlobalBudget(ctx); err == nil {
		t.Fatal("closed reader failure must propagate instead of masquerading as zero spend")
	}
}

func TestDashStoreListsTodaySpendInMicroUSDFromNewTable(t *testing.T) {
	db := newDashStoreTestDB(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")
	store := dashStore{w: db.Writer, r: db.Reader, loc: time.UTC}

	for _, row := range []struct {
		id, keyID, created string
	}{
		{id: "ins_old", keyID: "key-old", created: "2026-01-01T00:00:00Z"},
		{id: "ins_new", keyID: "key-new", created: "2026-01-02T00:00:00Z"},
	} {
		if _, err := db.Writer.Exec(ctx, `
			INSERT INTO installs(id, public_key, key_thumbprint, status, created_at)
			VALUES (?, ?, ?, 'active', ?)`, row.id, []byte(row.keyID), row.keyID, row.created); err != nil {
			t.Fatalf("insert install %s: %v", row.id, err)
		}
	}
	if _, err := db.Writer.Exec(ctx, `
		INSERT INTO install_spend_daily(install_id, period_day, spend_pusd, requests)
		VALUES ('ins_new', ?, ?, 1)`, day, 5*billing.PicoUSDPerMicroUSD+999_999); err != nil {
		t.Fatalf("insert install spend: %v", err)
	}
	// Old usage.tokens is intentionally contradictory: the dashboard must ignore
	// it and read only install_spend_daily after the accounting migration.
	if _, err := db.Writer.Exec(ctx, `
		INSERT INTO usage(install_id, period, count, tokens)
		VALUES ('ins_new', ?, 1, 999999999)`, day); err != nil {
		t.Fatalf("insert legacy usage: %v", err)
	}

	rows, err := store.ListInstalls(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListInstalls: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ID != "ins_new" || rows[0].TodaySpendMicroUSD != 5 {
		t.Fatalf("newest row = %+v, want ins_new with 5 micro-USD", rows[0])
	}
	if rows[1].ID != "ins_old" || rows[1].TodaySpendMicroUSD != 0 {
		t.Fatalf("older row = %+v, want ins_old with zero spend", rows[1])
	}
}

func TestQuotaResetSourceResetsOnlyTheCurrentMonth(t *testing.T) {
	db := newDashStoreTestDB(t)
	ctx := context.Background()
	if _, err := db.Writer.Exec(ctx, `
		INSERT INTO quota_monthly(install_id, period_month, requests) VALUES
		('ins_current_1', '2026-07', 7),
		('ins_current_2', '2026-07', 3),
		('ins_previous', '2026-06', 9)`); err != nil {
		t.Fatalf("seed monthly quota: %v", err)
	}
	source := quotaResetSource{
		store: quotastore.New(db.Writer, db.Reader),
		loc:   time.UTC,
		now:   func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) },
	}
	got, err := source.ResetAllMonthlyQuota(ctx)
	if err != nil {
		t.Fatalf("reset monthly quota: %v", err)
	}
	if got.Period != "2026-07" || got.ResetInstalls != 2 {
		t.Fatalf("reset result = %+v, want current period / two installs", got)
	}
	for _, tc := range []struct {
		id, period string
		want       int64
	}{
		{"ins_current_1", "2026-07", 0},
		{"ins_current_2", "2026-07", 0},
		{"ins_previous", "2026-06", 9},
	} {
		var requests int64
		if err := db.Reader.QueryRow(ctx,
			`SELECT requests FROM quota_monthly WHERE install_id = ? AND period_month = ?`, tc.id, tc.period).Scan(&requests); err != nil {
			t.Fatalf("read %s/%s: %v", tc.id, tc.period, err)
		}
		if requests != tc.want {
			t.Fatalf("%s/%s requests = %d, want %d", tc.id, tc.period, requests, tc.want)
		}
	}
}
