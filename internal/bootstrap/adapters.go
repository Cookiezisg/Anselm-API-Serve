package bootstrap

// adapters.go holds the small structural adapters bootstrap injects so each app
// service sees ONLY its declared port (interface) — never a concrete infra type.
// Bootstrap is the one package allowed to reach across layers, so all the
// "make infra X satisfy app port Y" glue lives here, one place, WHY-commented.

import (
	"context"
	"github.com/sunweilin/anselm/gateway/internal/pkg/idgen"
	"log/slog"
	"sync/atomic"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	appvideo "github.com/sunweilin/anselm/gateway/internal/app/video"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/infra/chatprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/settingsstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// --- upstream covariance adapter (deliverable 1) -----------------------------

// upstreamAdapter wraps the provider registry so Open returns its concrete
// stream as the app's infra-free chat.UpstreamStream. Go has no return-type
// covariance, so a returned concrete stream cannot directly satisfy a method declared to
// return chat.UpstreamStream — this thin wrap performs the widening (and maps the
// nil-stream case so a nil *Stream never becomes a non-nil interface).
type upstreamAdapter struct{ r *chatprovider.Registry }

func (a upstreamAdapter) Open(ctx context.Context, provider billing.Provider, model string, req domchat.CompletionRequest, firstByteTimeout time.Duration) (appchat.UpstreamStream, *appchat.UpstreamFailure) {
	s, failure := a.r.Open(ctx, provider, model, req, firstByteTimeout)
	if failure != nil {
		return nil, &appchat.UpstreamFailure{APIError: failure.APIError, Exposure: failure.Exposure}
	}
	return s, nil // *upstream.Stream satisfies chat.UpstreamStream via Read+Close.
}

func (a upstreamAdapter) Available(provider billing.Provider) bool { return a.r.Available(provider) }
func (a upstreamAdapter) BreakerOpen(provider billing.Provider) bool {
	return a.r.BreakerOpen(provider)
}

// --- quota ConfigSource adapter ----------------------------------------------

// quotaCfgSource adapts the live configprovider into app/quota.ConfigSource: the
// guardrail snapshot + the reset-tz, both read off ONE atomic Load so a hot-edit
// never splits a reservation's view.
type quotaCfgSource struct{ p *configprovider.Provider }

func (q quotaCfgSource) Limits() appquota.Limits {
	c := q.p.Load()
	return appquota.Limits{
		MonthlyQuota:           c.MonthlyQuota,
		GlobalMonthlySpendPUSD: c.GlobalMonthlySpendPUSD,
		DailySublimit:          c.DailySublimit,
		ImageDailyLimit:        c.ImageDailyLimit,
		// SPEECH was configured, advertised in /v1/models, and given both a category
		// ledger and a categoryCap case — but it was never copied here, so the store
		// read a zero cap and the daily character gate has never once fired. Nothing
		// was red: every layer was individually correct and the wire between two of
		// them was simply absent (找到的真 bug).
		// SPEECH 有配置、在 /v1/models 里对外宣告、有品类账本、也有 categoryCap 分支——**唯独没有
		// 被抄到这里**,于是 store 读到的上限恒为 0,那道日字符闸**一次都没生效过**。没有任何东西
		// 是红的:每一层各自都对,只是其中两层之间的那根线**根本不存在**(找到的真 bug)。
		SpeechDailyLimit: c.SpeechDailyLimit,
		VideoDailyLimit:  c.VideoDailyLimit,
		VoiceDailyLimit:  c.VoiceDailyLimit,
	}
}

func (q quotaCfgSource) Location() *time.Location { return q.p.Load().Location }

// --- chat Clock / Logger / Metrics adapters ----------------------------------

// systemClock is the production time source for chat (tests inject their own).
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// chatLogger adapts the request-scoped logx logger into chat.Logger. WARN is
// never sampled (security/), so it goes straight through the ctx logger.
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
func (c chatMetrics) Upstream(provider billing.Provider, outcome string) {
	c.m.UpstreamRequests.WithLabelValues(string(provider), outcome).Inc()
}
func (c chatMetrics) BillingDrift(provider billing.Provider) {
	c.m.BillingDrifts.WithLabelValues(string(provider)).Inc()
}
func (c chatMetrics) SettleFailure()   { c.m.SettleFailures.Inc() }
func (c chatMetrics) RollbackFailure() { c.m.RollbackFailures.Inc() }

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
type upstreamHook struct {
	m        *metrics.Metrics
	provider billing.Provider
}

func (h upstreamHook) KeyCooldown() { h.m.KeyCooldowns.WithLabelValues(string(h.provider)).Inc() }

func (h upstreamHook) BreakerStateChange(state upstream.BreakerState) {
	h.m.BreakerState.WithLabelValues(string(h.provider)).Set(float64(state))
}

func (h upstreamHook) CallLatency(elapsed time.Duration) {
	h.m.UpstreamLatency.WithLabelValues(string(h.provider)).Observe(elapsed.Seconds())
}

// --- install counter adapters ------------------------------------------------

// installsCreatedCounter / powCounter adapt the metrics bundle into the install
// service's optional counter ports (nil-safe in the service; we wire real ones).
type installsCreatedCounter struct{ m *metrics.Metrics }

func (c installsCreatedCounter) Inc() { c.m.InstallsCreated.Inc() }

type powCounter struct{ m *metrics.Metrics }

func (c powCounter) Inc(result string) { c.m.InstallPoW.WithLabelValues(result).Inc() }

// videoUpstream adapts the infra video client's own status struct into the app
// port's. The two are field-identical on purpose and stay separate on purpose:
// the app layer must be able to name its port types without importing infra, and
// a shared struct would be exactly that import (S3 依赖单向).
//
// videoUpstream 把 infra 视频 client 自己的状态结构适配成 app 端口的。两者刻意逐字段相同、也刻意
// 保持分离:app 层必须能在不 import infra 的前提下给自己的端口类型起名,而共用一个 struct 恰恰
// 就是那个 import(依赖单向)。
type videoUpstream struct{ g *upstream.VideoGen }

func (v videoUpstream) SubmitVideo(ctx context.Context, model, prompt string, seconds int, ratio, resolution, firstFrame string) (string, bool, error) {
	return v.g.SubmitVideo(ctx, model, prompt, seconds, ratio, resolution, firstFrame)
}

func (v videoUpstream) PollVideo(ctx context.Context, taskID string) (appvideo.VideoStatus, error) {
	st, err := v.g.PollVideo(ctx, taskID)
	if err != nil {
		return appvideo.VideoStatus{}, err
	}
	return appvideo.VideoStatus{Phase: st.Phase, URL: st.URL}, nil
}

// --- voice adapters ---------------------------------------------

// voiceIDs mints voice ids.
type voiceIDs struct{}

func (voiceIDs) New() string { return idgen.VoiceID() }

// voiceLogger renders the two conditions an operator must act on. Both are WARN and neither is
// sampled: one says our shared account is full, the other says a paid registration is stranded
// where nothing automatic can reach it. A dropped line here is a line nobody will ever see again.
//
// voiceLogger 渲染运营者**必须**处理的那两个状况。两条都是 WARN 且都不采样:一条说我们的共享账号满了,
// 另一条说有一份已付费的登记搁浅在自动机制够不着的地方。这里丢掉一行,就是永远不会再有人看见的一行。
// It reuses the SAME Prometheus counters the other three capabilities feed, because an unclosed
// book is one condition regardless of which capability left it open — the operator's alert must
// fire on the total, not on four independently-named metrics nobody thought to graph together.
//
// 它喂的是另外三个能力**同一批** Prometheus 计数器,因为「账没平」是**一个**状况、与是哪个能力留下的
// 无关——运营者的告警该对着**总数**响,而不是对着四个各自命名、没人想到要放一起看的指标。
type voiceLogger struct {
	log *slog.Logger
	m   *metrics.Metrics
}

func (l voiceLogger) AccountVoiceCeilingReached(total, ceiling int) {
	l.log.Warn("voice_account_ceiling_reached", "total", total, "ceiling", ceiling)
}

func (l voiceLogger) VoiceOrphaned(upstreamID string) {
	l.log.Warn("voice_orphaned_upstream", "upstream_id", upstreamID)
}

func (l voiceLogger) SampleNotRevoked(leaseID string) {
	l.log.Warn("voice_sample_not_revoked", "lease_id", leaseID)
}

func (l voiceLogger) VoiceSlowToDeploy(upstreamID string) {
	l.log.Warn("voice_slow_to_deploy", "upstream_id", upstreamID)
}

func (l voiceLogger) SettleFailure() {
	if l.m != nil {
		l.m.SettleFailures.Inc()
	}
	l.log.Warn("voice_settle_failed")
}

func (l voiceLogger) RollbackFailure() {
	if l.m != nil {
		l.m.RollbackFailures.Inc()
	}
	l.log.Warn("voice_rollback_failed")
}
