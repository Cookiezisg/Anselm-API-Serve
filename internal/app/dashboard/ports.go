package dashboard

import (
	"context"
	"io"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/alert"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
)

// The dashboard use-case layer declares EVERY infra need as a port here; it
// imports domain + pkg + these interfaces ONLY — never infra, net/http, or
// database/sql (depguard §2). Bootstrap wires concrete infra adapters
// (configprovider, quotastore, installstore, metrics, alerter, ratesample) that
// satisfy these structurally, so app/dashboard stays unit-testable with fakes.

// BudgetSource reads the GLOBAL monthly spend budget for the overview. The quota
// layer's per-install View is not the global figure, so the dashboard declares
// its own narrow port: current period, limit and used spend, all expressed
// as integer micro-USD at this display boundary. Reserves/bills nothing.
type BudgetSource interface {
	// GlobalBudget returns the current RESET_TZ month and the global limit/used
	// amounts in micro-USD. A missing spend row reads usedMicroUSD=0.
	GlobalBudget(ctx context.Context) (day string, limitMicroUSD, usedMicroUSD int64, err error)
}

// ReservationsSource counts spend-ledger rows still in the open state — the
// missed-settle backlog gauge on the overview. Satisfied by
// *infra/store/quotastore.Store.
type ReservationsSource interface {
	OpenReservations(ctx context.Context) (int64, error)
}

// InflightSource reads the live N_global occupancy gauge for the overview.
// Satisfied by an infra/metrics adapter over the InflightConc gauge.
type InflightSource interface {
	InflightConcurrency() int64
}

// ProviderSource exposes only safe construction/health facts for the two fixed
// upstreams. It deliberately has no key-count or credential accessor: the
// dashboard may say whether a provider is configured, never how or with what.
// Satisfied by *infra/chatprovider.Registry at the composition root.
type ProviderSource interface {
	Available(provider billing.Provider) bool
	BreakerOpen(provider billing.Provider) bool
}

// RateSource is the B16 server-side recent-window QPS/error-rate. Concurrent
// /api/overview polls each get an independent, correct Snapshot (no shared
// last-point to steal). Satisfied by *pkg/ratesample.Sampler.
type RateSource interface {
	Snapshot() ratesample.Snapshot
}

// AlertSource is the in-process alert state view. Satisfied by *pkg/alert.Alerter.
type AlertSource interface {
	CurrentStatus() []alert.AlertState
}

// InstallsToday reports today's global install issuance count for the overview
// (0 when the coarse valve is dormant). Satisfied by an installstore adapter.
type InstallsToday interface {
	InstallsToday(ctx context.Context) (int64, error)
}

// ConfigSource is the hot-reloadable config read/write surface. Dump is the
// secret-free read model; ApplyOverrides validates + persists + hot-swaps and
// returns the precise reason on a rejected key/value. Satisfied by
// *infra/configprovider.Provider (its ApplyOverrides binds the settings store).
type ConfigSource interface {
	Dump() []DumpItem
	ApplyOverrides(ctx context.Context, overrides map[string]string) (rejectReason string, ok bool)
	// InstallGlobalCap is the configured coarse install valve for the overview
	// (INSTALL_GLOBAL_DAILY_CAP; 0 = disabled). Read off the live snapshot.
	InstallGlobalCap() int64
}

// DumpItem mirrors the configprovider dump row shape so the use case can carry
// it to transport without importing infra. The JSON tags are the wire contract.
type DumpItem struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	Editable        bool   `json:"editable"`
	RestartRequired bool   `json:"restartRequired"`
	Min             *int64 `json:"min,omitempty"`
	Max             *int64 `json:"max,omitempty"`
}

// InstallLister lists installs newest-first with offset-cursor pagination for the
// admin table. It returns ONLY safe, low-cardinality facts — opaque id, status,
// lifecycle timestamps and today's spend — never a token/fingerprint/IP.
// Satisfied by an installstore adapter (the dashboard never opens its own DB).
type InstallLister interface {
	ListInstalls(ctx context.Context, cursor, limit int) ([]InstallRow, error)
}

// InstallRow is one admin-table row. It deliberately carries NO token, NO
// fingerprint, NO IP — id is the opaque ins_<hex> handle (safe to surface).
type InstallRow struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	CreatedAt          string `json:"createdAt"`
	LastSeenAt         string `json:"lastSeenAt,omitempty"`
	TodaySpendMicroUSD int64  `json:"todaySpendMicroUsd"`
}

// StatusMutator flips an install's lifecycle status (ban/unban). A no-match id
// returns found=false (a 404 the operator should see, not a silent success).
// Satisfied by an installstore adapter; the SAME write the CLI ban uses.
type StatusMutator interface {
	SetStatus(ctx context.Context, id string, status install.Status) (found bool, err error)
}

// Snapshotter produces a CONSISTENT on-disk DB snapshot (VACUUM INTO a temp file)
// and returns the path; the caller streams it then calls Cleanup. The snapshot
// contains ONLY committed disk state — never an in-memory secret. Satisfied by an
// installstore/quotastore export adapter on the write pool.
type Snapshotter interface {
	// Snapshot writes a consistent copy to a private temp file and returns its path
	// + size. The caller owns streaming + Cleanup.
	Snapshot(ctx context.Context) (path string, size int64, err error)
	// Cleanup removes a temp snapshot path. Idempotent + best-effort.
	Cleanup(path string)
	// Open opens a snapshot path for streaming. Kept on the port so the use case
	// never imports os (stays infra-free); the adapter returns an io.ReadCloser.
	Open(path string) (io.ReadCloser, error)
}

// Logger is the structured audit logger port (WARN/INFO never sampled). The use
// case never logs a token/key/prompt/ip — only safe audit facts.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Clock is the injectable time source (tests drive the audit ring + export name).
type Clock interface {
	Now() time.Time
}
