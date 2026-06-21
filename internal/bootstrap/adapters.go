package bootstrap

// adapters.go holds the small structural adapters bootstrap injects so each app
// service sees ONLY its declared port (interface) — never a concrete infra type.
// Bootstrap is the one package allowed to reach across layers, so all the
// "make infra X satisfy app port Y" glue lives here, one place, WHY-commented.

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/settingsstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// --- upstream covariance adapter (deliverable 1) -----------------------------

// upstreamAdapter wraps the concrete *upstream.Client so Do returns the *Stream
// as the app's infra-free chat.UpstreamStream. Go has no return-type covariance,
// so a returned *upstream.Stream cannot directly satisfy a method declared to
// return chat.UpstreamStream — this thin wrap performs the widening (and maps the
// nil-stream case so a nil *Stream never becomes a non-nil interface).
type upstreamAdapter struct{ c upstream.Client }

func (a upstreamAdapter) Do(ctx context.Context, payload []byte, stream bool, cfg *config.Config) (appchat.UpstreamStream, *apierr.APIError) {
	s, e := a.c.Do(ctx, payload, stream, cfg)
	if e != nil {
		return nil, e
	}
	return s, nil // *upstream.Stream satisfies chat.UpstreamStream via Read+Close.
}

func (a upstreamAdapter) BreakerOpen() bool { return a.c.BreakerOpen() }

// --- quota ConfigSource adapter ----------------------------------------------

// quotaCfgSource adapts the live configprovider into app/quota.ConfigSource: the
// guardrail snapshot + the reset-tz, both read off ONE atomic Load so a hot-edit
// never splits a reservation's view.
type quotaCfgSource struct{ p *configprovider.Provider }

func (q quotaCfgSource) Limits() appquota.Limits {
	c := q.p.Load()
	return appquota.Limits{
		MonthlyQuota:         c.MonthlyQuota,
		InstallDailyTokenCap: c.InstallDailyTokenCap,
		GlobalDailyBudget:    c.GlobalDailyBudget,
		DailySublimit:        c.DailySublimit,
	}
}

func (q quotaCfgSource) Location() *time.Location { return q.p.Load().Location }

// --- chat Clock / Logger / Metrics adapters ----------------------------------

// systemClock is the production time source for chat (tests inject their own).
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// chatLogger adapts the request-scoped logx logger into chat.Logger. WARN is
// never sampled (security/B2), so it goes straight through the ctx logger.
type chatLogger struct{}

func (chatLogger) Warn(ctx context.Context, msg string, args ...any) {
	logx.From(ctx).Warn(msg, args...)
}

// chatMetrics adapts the Prometheus bundle into chat.Metrics. The bundle exposes
// the raw collectors; this maps the use case's intent (inflight gauge, upstream
// outcome by label, B2 failure counters) onto them. inflight mirrors the gauge
// into an atomic the dashboard can READ BACK (prometheus gauges have no getter).
type chatMetrics struct {
	m        *metrics.Metrics
	inflight *atomic.Int64
}

func (c chatMetrics) Inflight(n int) {
	c.m.InflightConc.Set(float64(n))
	c.inflight.Store(int64(n))
}
func (c chatMetrics) Upstream(outcome string) { c.m.UpstreamRequests.WithLabelValues(outcome).Inc() }
func (c chatMetrics) SettleFailure()          { c.m.SettleFailures.Inc() }
func (c chatMetrics) RollbackFailure()        { c.m.RollbackFailures.Inc() }

// inflightSource satisfies dashboard.InflightSource by reading the atomic mirror
// chatMetrics keeps in lock-step with the prometheus gauge.
type inflightSource struct{ v *atomic.Int64 }

func (s inflightSource) InflightConcurrency() int64 { return s.v.Load() }

// --- dashboard ConfigSource adapter ------------------------------------------

// dashCfgSource adapts configprovider into app/dashboard.ConfigSource: the
// secret-free dump (mapping the infra DumpItem to the app's identical-shape one),
// the validate→persist→swap apply bound to the settings store, and the live
// install coarse-cap. The store binding lives here so the dashboard never names
// settingsstore.
type dashCfgSource struct {
	p  *configprovider.Provider
	ss *settingsstore.Store
}

func (d dashCfgSource) Dump() []appdash.DumpItem {
	in := d.p.Dump()
	out := make([]appdash.DumpItem, 0, len(in))
	for _, it := range in {
		out = append(out, appdash.DumpItem{
			Key:             it.Key,
			Value:           it.Value,
			Editable:        it.Editable,
			RestartRequired: it.RestartRequired,
			Min:             it.Min,
			Max:             it.Max,
		})
	}
	return out
}

func (d dashCfgSource) ApplyOverrides(ctx context.Context, overrides map[string]string) (string, bool) {
	if _, err := d.p.ApplyOverrides(ctx, overrides, d.ss); err != nil {
		return err.Error(), false // the precise, client-safe reason (never a secret).
	}
	return "", true
}

func (d dashCfgSource) InstallGlobalCap() int64 { return d.p.Load().InstallGlobalDailyCap }

// --- dashboard Logger adapter ------------------------------------------------

// dashLogger adapts a *slog.Logger into app/dashboard.Logger (audit facts only).
type dashLogger struct{ l *slog.Logger }

func (d dashLogger) Info(msg string, args ...any)  { d.l.Info(msg, args...) }
func (d dashLogger) Warn(msg string, args ...any)  { d.l.Warn(msg, args...) }
func (d dashLogger) Error(msg string, args ...any) { d.l.Error(msg, args...) }

// --- health adapters ---------------------------------------------------------

// dbChecker satisfies health.DBChecker over the write pool: a bounded probe that
// the DB can currently accept a write. SELECT 1 against the single-writer pool
// honors ctx; it never opens a transaction (a readiness probe must not mutate).
type dbChecker struct{ w *orm.DB }

func (d dbChecker) Writable(ctx context.Context) error {
	var one int
	return d.w.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// --- upstream MetricsHook adapter --------------------------------------------

// upstreamHook adapts the metrics bundle into upstream.MetricsHook (key cooldown
// counter + breaker-state gauge), so upstream needn't import the metrics package.
type upstreamHook struct{ m *metrics.Metrics }

func (h upstreamHook) KeyCooldown() { h.m.KeyCooldowns.Inc() }

func (h upstreamHook) BreakerStateChange(open bool) {
	if open {
		h.m.BreakerState.Set(2) // 0=closed 1=half-open 2=open
	} else {
		h.m.BreakerState.Set(0)
	}
}

// --- install counter adapters ------------------------------------------------

// installsCreatedCounter / powCounter adapt the metrics bundle into the install
// service's optional counter ports (nil-safe in the service; we wire real ones).
type installsCreatedCounter struct{ m *metrics.Metrics }

func (c installsCreatedCounter) Inc() { c.m.InstallsCreated.Inc() }

type powCounter struct{ m *metrics.Metrics }

func (c powCounter) Inc(result string) { c.m.InstallPoW.WithLabelValues(result).Inc() }
