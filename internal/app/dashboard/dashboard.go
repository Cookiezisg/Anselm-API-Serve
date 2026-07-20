// Package dashboard is the admin-backend use-case layer: the operator surface
// (live overview, hot-reloadable config, install list/ban/unban, an in-process
// audit view, and a consistent DB-snapshot export) expressed over PORTS it
// declares (ports.go). It imports domain + pkg + those interfaces ONLY — never
// net/http, never infra, never database/sql (depguard §2). Transport adapts HTTP
// into these use cases; bootstrap wires the concrete infra adapters.
//
// Secrets never appear: ConfigDump is the secret-free dump, Export streams only
// committed on-disk DB state, and no use case returns a token/key/prompt/ip.
package dashboard

import (
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/alert"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
)

// Deps bundles the ports the dashboard use cases drive. Optional ports (Logger,
// Clock) default to safe no-ops in New so the hot path never nil-checks.
type Deps struct {
	Budget       BudgetSource
	Reservations ReservationsSource
	Inflight     InflightSource
	Providers    ProviderSource
	Rate         RateSource
	Alerts       AlertSource
	Installs     InstallsToday
	Config       ConfigSource
	Lister       InstallLister
	Mutator      StatusMutator
	Export       Snapshotter
	Logger       Logger
	Clock        Clock
}

// Service is the dashboard use-case façade + its in-process audit ring (the one
// piece of dashboard state that lives in the use-case layer, since it is process
// memory below the durable log, not an infra resource).
type Service struct {
	d     Deps
	audit *auditRing
}

// New wires the Service to its ports, defaulting the optional ones to no-ops.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = noopLogger{}
	}
	if d.Clock == nil {
		d.Clock = systemClock{}
	}
	return &Service{d: d, audit: newAuditRing(512, d.Clock.Now)}
}

// Overview is the GET /api/overview snapshot: aggregates only, no per-install /
// prompt / key / ip detail. Each port is best-effort — a single failed read
// leaves its field at zero rather than failing the whole snapshot.
type Overview struct {
	Budget struct {
		Day               string `json:"day"`
		UsedMicroUSD      int64  `json:"usedMicroUsd"`
		LimitMicroUSD     int64  `json:"limitMicroUsd"`
		RemainingMicroUSD int64  `json:"remainingMicroUsd"`
		Unit              string `json:"unit"`
	} `json:"budget"`
	InflightConcurrency int64               `json:"inflightConcurrency"`
	OpenReservations    int64               `json:"openReservations"`
	Providers           ProviderStatuses    `json:"providers"`
	UpstreamBreakerOpen bool                `json:"upstreamBreakerOpen"`
	DiskDegraded        bool                `json:"diskDegraded"`
	Alerts              []alert.AlertState  `json:"alerts"`
	Recent              ratesample.Snapshot `json:"recent"`
	InstallsToday       int64               `json:"installsToday"`
	InstallGlobalCap    int64               `json:"installGlobalCap"`
}

// ProviderStatus is the deliberately small, secret-free operational view for a
// fixed provider. Configured means a backend was constructed; it does not expose
// key count, key material, endpoint, model/account metadata, or probe details.
type ProviderStatus struct {
	Configured  bool `json:"configured"`
	BreakerOpen bool `json:"breakerOpen"`
}

// ProviderStatuses is a closed wire shape, not an open-ended map. Both stable
// keys are always serialized so operators and the SPA never infer capability
// from a field being absent.
type ProviderStatuses struct {
	DeepSeek ProviderStatus `json:"deepseek"`
	Kimi     ProviderStatus `json:"kimi"`
}

// Overview assembles the live snapshot. Provider state comes through the
// ProviderSource port; diskDegraded is passed by transport as the one remaining
// cheap in-process accessor it already holds.
func (s *Service) Overview(ctx context.Context, diskDegraded bool) Overview {
	var o Overview
	o.Budget.Unit = "micro_usd"
	if s.d.Budget != nil {
		if day, limit, used, err := s.d.Budget.GlobalBudget(ctx); err == nil {
			remaining := limit - used
			if remaining < 0 {
				remaining = 0
			}
			o.Budget.Day = day
			o.Budget.LimitMicroUSD = limit
			o.Budget.UsedMicroUSD = used
			o.Budget.RemainingMicroUSD = remaining
		}
	}
	if s.d.Reservations != nil {
		if open, err := s.d.Reservations.OpenReservations(ctx); err == nil {
			o.OpenReservations = open
		}
	}
	if s.d.Inflight != nil {
		o.InflightConcurrency = s.d.Inflight.InflightConcurrency()
	}
	o.Providers.DeepSeek = s.providerStatus(billing.ProviderDeepSeek)
	o.Providers.Kimi = s.providerStatus(billing.ProviderKimi)
	// Kept as a compatibility aggregate for older dashboard/API consumers. New
	// consumers use the provider-specific states above.
	o.UpstreamBreakerOpen = o.Providers.DeepSeek.BreakerOpen || o.Providers.Kimi.BreakerOpen
	o.DiskDegraded = diskDegraded
	if s.d.Alerts != nil {
		o.Alerts = s.d.Alerts.CurrentStatus()
	}
	if s.d.Rate != nil {
		o.Recent = s.d.Rate.Snapshot()
	}
	if s.d.Config != nil {
		o.InstallGlobalCap = s.d.Config.InstallGlobalCap()
	}
	if s.d.Installs != nil {
		if n, err := s.d.Installs.InstallsToday(ctx); err == nil {
			o.InstallsToday = n
		}
	}
	if o.Alerts == nil {
		o.Alerts = []alert.AlertState{}
	}
	return o
}

func (s *Service) providerStatus(provider billing.Provider) ProviderStatus {
	if s.d.Providers == nil || !s.d.Providers.Available(provider) {
		return ProviderStatus{}
	}
	return ProviderStatus{Configured: true, BreakerOpen: s.d.Providers.BreakerOpen(provider)}
}

// ConfigDump returns the secret-free config read model (GET /api/config).
func (s *Service) ConfigDump() []DumpItem {
	if s.d.Config == nil {
		return []DumpItem{}
	}
	return s.d.Config.Dump()
}

// ErrConfigRejected reports that an override batch was rejected by validation; the
// Reason is the precise, client-safe message (an out-of-bounds value / unknown or
// secret key / failed cross-field check) — never a secret. Transport renders it
// as 400 CONFIG_REJECTED.
type ErrConfigRejected struct{ Reason string }

func (e *ErrConfigRejected) Error() string { return e.Reason }

// ConfigApply validates + persists + hot-swaps an override batch and, on success,
// records an audit event (the changed KEYS only, never their values) + the fresh
// dump. A rejected batch returns *ErrConfigRejected with the precise reason.
func (s *Service) ConfigApply(ctx context.Context, actor string, overrides map[string]string) ([]DumpItem, error) {
	if reason, ok := s.d.Config.ApplyOverrides(ctx, overrides); !ok {
		return nil, &ErrConfigRejected{Reason: reason}
	}
	keys := keysOf(overrides)
	s.audit.record(AuditEvent{Action: "config_update", Target: keys, Outcome: "ok", Actor: actor})
	s.d.Logger.Warn("dashboard_config_update", "event", "dashboard_audit", "keys", keys, "outcome", "ok")
	return s.ConfigDump(), nil
}

// InstallsList lists installs newest-first with offset-cursor pagination. It
// fetches limit+1 internally to know whether a next page exists; nextCursor is the
// opaque offset to pass for the following page (empty when no more remain).
func (s *Service) InstallsList(ctx context.Context, cursor, limit int) (rows []InstallRow, nextCursor int, hasMore bool, err error) {
	cursor, limit = clampPagination(cursor, limit)
	rows, err = s.d.Lister.ListInstalls(ctx, cursor, limit+1)
	if err != nil {
		return nil, 0, false, err
	}
	if len(rows) > limit {
		return rows[:limit], cursor + limit, true, nil
	}
	return rows, 0, false, nil
}

// MaxReasonChars bounds the operator-supplied ban reason logged verbatim, so a
// single audit line cannot be bloated.
const MaxReasonChars = 256

// Ban flips an install to banned and records a durable WARN + an in-process audit
// event. A no-match id is a 404 (ErrInstallNotFound) — an operator mistake worth
// surfacing, not a silent success. found=false maps to that at transport.
func (s *Service) Ban(ctx context.Context, actor, id, reason string) error {
	return s.mutate(ctx, "ban", actor, id, reason, install.StatusBanned)
}

// Unban flips an install back to active (reason optional).
func (s *Service) Unban(ctx context.Context, actor, id string) error {
	return s.mutate(ctx, "unban", actor, id, "", install.StatusActive)
}

// ErrInstallNotFound is returned by Ban/Unban when no install matches the id.
type ErrInstallNotFound struct{ ID string }

func (e *ErrInstallNotFound) Error() string { return "no install with id " + e.ID }

func (s *Service) mutate(ctx context.Context, action, actor, id, reason string, status install.Status) error {
	if len(reason) > MaxReasonChars {
		reason = reason[:MaxReasonChars]
	}
	found, err := s.d.Mutator.SetStatus(ctx, id, status)
	if err != nil {
		s.d.Logger.Error("dashboard_status_failed", "event", "dashboard_audit",
			"action", action, "install_id", id, "error", err.Error())
		return err
	}
	if !found {
		s.audit.record(AuditEvent{Action: action, Target: id, Reason: reason, Outcome: "not_found", Actor: actor})
		s.d.Logger.Warn("dashboard_status_no_match", "event", "dashboard_audit",
			"action", action, "install_id", id, "reason", reason, "outcome", "not_found")
		return &ErrInstallNotFound{ID: id}
	}
	s.audit.record(AuditEvent{Action: action, Target: id, Reason: reason, Outcome: "ok", Actor: actor})
	s.d.Logger.Warn("dashboard_status_change", "event", "dashboard_audit",
		"action", action, "install_id", id, "reason", reason, "new_status", string(status), "outcome", "ok")
	return nil
}

// Audit returns recent audit events (newest first), keyset-paged by the monotonic
// seq. beforeSeq<=0 = newest; nextCursor is the seq to page below for the next
// page (0 when none remain). Keyset (not offset) so concurrent writes never
// skip/duplicate a row.
func (s *Service) Audit(beforeSeq int64, limit int) (events []AuditEvent, nextCursor int64) {
	events, next, more := s.audit.page(beforeSeq, limit)
	if !more {
		next = 0
	}
	return events, next
}

// RecordAudit is the hook transport uses to land non-use-case audit facts in the
// same ring (e.g. an export that succeeds or is cut short mid-stream). It stamps
// At+Seq itself, so a caller cannot forge ordering.
func (s *Service) RecordAudit(e AuditEvent) { s.audit.record(e) }

// Export produces a CONSISTENT on-disk snapshot, returns a ReadCloser + size + a
// suggested filename + a cleanup func. The caller streams the ReadCloser then
// calls cleanup (which closes the reader AND removes the temp file). The snapshot
// contains ONLY committed disk state — never an in-memory secret.
//
// The export audit event is NOT recorded here: success vs interrupted is only
// known after the stream finishes, so transport records the terminal outcome
// (ok/interrupted) via RecordAudit — every export attempt leaves a trail.
func (s *Service) Export(ctx context.Context) (body io.ReadCloser, size int64, filename string, cleanup func(), err error) {
	path, size, err := s.d.Export.Snapshot(ctx)
	if err != nil {
		s.d.Logger.Error("dashboard_export_failed", "event", "dashboard_audit", "error", err.Error())
		return nil, 0, "", func() {}, err
	}
	rc, err := s.d.Export.Open(path)
	if err != nil {
		s.d.Export.Cleanup(path)
		return nil, 0, "", func() {}, err
	}
	filename = "anselm-gateway-" + s.d.Clock.Now().UTC().Format("20060102-150405") + ".db"
	cleanup = func() {
		_ = rc.Close()
		s.d.Export.Cleanup(path)
	}
	return rc, size, filename, cleanup, nil
}

// keysOf returns the sorted, comma-joined key set of an override map for the audit
// target field — the keys changed, never their values (a value could be large but
// is never a secret here; keys alone are the safe summary).
func keysOf(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// clampPagination bounds the offset cursor (>=0) and limit ([1,200]).
func clampPagination(cursor, limit int) (int, int) {
	if cursor < 0 {
		cursor = 0
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	return cursor, limit
}

// noopLogger / systemClock are the default optional-port implementations.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
