package bootstrap

// dashstore.go implements the dashboard's read/mutate/export ports DIRECTLY over
// the orm pools. These were NOT provided by infra/store/{quota,install}store
// (slices left them unbuilt — see notes), and app/dashboard.Service does NOT
// nil-check Lister/Mutator/Export, so they must be non-nil or the handler panics.
// Bootstrap is the cross-layer composition root, so wiring the SQL here keeps the
// gateway runnable; if a later slice adds these to the stores, swap the wiring.
//
// Every query returns ONLY safe, low-cardinality facts (opaque id/status/time) —
// never a token/fingerprint/IP (dashboard privacy red line).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// dashStore bundles the write+read pools the dashboard ports query through.
type dashStore struct {
	w   *orm.DB
	r   *orm.DB
	loc *time.Location
}

// GlobalBudget (BudgetSource): today's RESET_TZ window day plus global spend
// limit and usage in integer micro-USD. The database and live config retain exact
// pUSD; this adapter is the single conversion boundary for the dashboard wire.
// A missing spend row reads used=0. The limit closure is wired from live config.
type budgetSource struct {
	ds       dashStore
	getLimit func() int64
}

func (b budgetSource) GlobalBudget(ctx context.Context) (day string, limit, used int64, err error) {
	day = time.Now().In(b.ds.loc).Format("2006-01-02")
	limit = b.getLimit() / billing.PicoUSDPerMicroUSD
	var usedPUSD int64
	err = b.ds.r.QueryRow(ctx,
		`SELECT spend_pusd FROM global_spend_daily WHERE period_day = ?`, day).Scan(&usedPUSD)
	switch {
	case err == nil:
		return day, limit, usedPUSD / billing.PicoUSDPerMicroUSD, nil
	case errors.Is(err, sql.ErrNoRows):
		return day, limit, 0, nil
	default:
		return day, limit, 0, fmt.Errorf("read global spend: %w", err)
	}
}

// ListInstalls (InstallLister): newest-first, offset-paged, plus today's spend
// for each install in integer micro-USD. NEVER returns token/fingerprint/IP.
func (d dashStore) ListInstalls(ctx context.Context, cursor, limit int) ([]appdash.InstallRow, error) {
	day := time.Now().In(d.loc).Format("2006-01-02")
	rows, err := d.r.Query(ctx, `
		SELECT i.id, i.status, i.created_at, COALESCE(i.last_seen_at, ''),
		       COALESCE(s.spend_pusd, 0)
		FROM installs i
		LEFT JOIN install_spend_daily s ON s.install_id = i.id AND s.period_day = ?
		ORDER BY i.created_at DESC, i.id DESC
		LIMIT ? OFFSET ?`, day, limit, cursor)
	if err != nil {
		return nil, fmt.Errorf("list installs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []appdash.InstallRow
	for rows.Next() {
		var r appdash.InstallRow
		var spendPUSD int64
		if err := rows.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.LastSeenAt, &spendPUSD); err != nil {
			return nil, fmt.Errorf("scan install row: %w", err)
		}
		r.TodaySpendMicroUSD = spendPUSD / billing.PicoUSDPerMicroUSD
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetStatus (StatusMutator): flips installs.status on the write pool, the SAME
// write the CLI ban uses. found=false on no matching id (a 404 the operator sees).
func (d dashStore) SetStatus(ctx context.Context, id string, status dominstall.Status) (bool, error) {
	res, err := d.w.Exec(ctx, `UPDATE installs SET status = ? WHERE id = ?`, string(status), id)
	if err != nil {
		return false, fmt.Errorf("set status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// Snapshot (Snapshotter): VACUUM INTO a private temp file for a CONSISTENT
// on-disk copy (committed state only — never an in-memory secret). The caller
// streams it then calls Cleanup.
func (d dashStore) Snapshot(ctx context.Context) (string, int64, error) {
	f, err := os.CreateTemp("", "anselm-gateway-export-*.db")
	if err != nil {
		return "", 0, fmt.Errorf("create temp snapshot: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path) // VACUUM INTO requires the target NOT exist.

	// VACUUM INTO writes a consistent snapshot of committed pages to path.
	if _, err := d.w.Exec(ctx, `VACUUM INTO ?`, path); err != nil {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("vacuum into: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("stat snapshot: %w", err)
	}
	return path, fi.Size(), nil
}

// Cleanup removes a temp snapshot path (idempotent, best-effort).
func (d dashStore) Cleanup(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// Open opens a snapshot path for streaming (the use case never imports os).
func (d dashStore) Open(path string) (io.ReadCloser, error) {
	return os.Open(path) //nolint:gosec // path is our own freshly-minted temp file
}
