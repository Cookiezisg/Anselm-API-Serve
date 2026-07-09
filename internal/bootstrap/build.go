// Package bootstrap is the composition root: the ONE package allowed to import
// across domain + app + infra + transport (depguard targets no rule at
// internal/bootstrap). It loads config, opens the DB, constructs every store +
// app service through the small structural adapters in adapters.go, builds the
// three HTTP handlers via the router, and returns an *App the lifecycle drives.
// cmd/gateway imports ONLY this package.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	appchat "github.com/sunweilin/anselm/gateway/internal/app/chat"
	appdash "github.com/sunweilin/anselm/gateway/internal/app/dashboard"
	apphealth "github.com/sunweilin/anselm/gateway/internal/app/health"
	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	appmodel "github.com/sunweilin/anselm/gateway/internal/app/model"
	appquota "github.com/sunweilin/anselm/gateway/internal/app/quota"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/diskguard"
	"github.com/sunweilin/anselm/gateway/internal/infra/metrics"
	"github.com/sunweilin/anselm/gateway/internal/infra/ratelimit"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/installstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/quotastore"
	"github.com/sunweilin/anselm/gateway/internal/infra/store/settingsstore"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
	"github.com/sunweilin/anselm/gateway/internal/infra/webassets"
	"github.com/sunweilin/anselm/gateway/internal/pkg/alert"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
	dashhandler "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/handlers/dashboard"
	mwdash "github.com/sunweilin/anselm/gateway/internal/transport/httpapi/middleware/dashboard"
	"github.com/sunweilin/anselm/gateway/internal/transport/httpapi/router"
)

// App is the fully-wired gateway the lifecycle (Run) drives. It holds the three
// servers, the background-write WaitGroup, the DB (closed LAST, GW-INV-24), the
// provider + guards the background loops feed, and the cancel for those loops.
type App struct {
	cfg *configprovider.Provider
	db  *sqlite.DB

	bizSrv   *http.Server // business :8080
	adminSrv *http.Server // admin/metrics :9090 (loopback-only)
	dashSrv  *http.Server // dashboard :8081 (nil when auth not configured)

	bgWG       *sync.WaitGroup
	loopCtx    context.Context
	loopCancel context.CancelFunc

	// Background-loop collaborators.
	quota            *appquota.Service
	upstream         upstream.Client
	disk             *diskguard.Guard
	prober           *upstreamProber
	mx               *metrics.Metrics
	alerter          *alert.Alerter
	rate             *ratesample.Sampler
	rl               *ratelimit.RateLimiter
	throttle         *tokenThrottle
	openReservations *quotastore.Store
	log              *slog.Logger

	addrs addrs // resolved listen addresses for the lifecycle log.
}

type addrs struct {
	business  string
	admin     string
	dashboard string
}

// Build is the composition root. It loads env → fail-fast on bad config (the
// 3-addr-distinct + admin-loopback checks) → opens + migrates SQLite → assembles
// the overlay config → constructs stores, app services (via adapters), the
// upstream client + breaker, the shared rate limiter, the chat saga, the
// dashboard, the metrics/alert/ratesample bundle → builds the 3 HTTP handlers.
func Build(ctx context.Context, getenv func(string) string) (*App, error) {
	// 1) Env-only base config (defaults / secrets / startup-hard). A bad value or a
	// missing required secret fails fast here, before anything else logs.
	base, err := configprovider.LoadBase(getenv)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Logging foundation (JSON → stdout for journald). Installed right after config
	// so every subsequent line is structured + redaction-floored (OBS-1).
	logx.InitDefault()
	log := slog.Default()

	// 2) Listener fail-fast: the three addrs must differ (LoadBase already checks
	// dashboard≠business/admin; assert business≠admin too), and BOTH ADMIN_ADDR and
	// DASHBOARD_ADDR must be loopback (GW-INV-13: the metrics/pprof surface and the
	// dashboard are off-box only; only the business listener is public, behind Caddy).
	if base.ListenAddr == base.AdminAddr {
		return nil, fmt.Errorf("LISTEN_ADDR %q must not equal ADMIN_ADDR %q (three listeners must be physically isolated)", base.ListenAddr, base.AdminAddr)
	}
	if err := requireLoopback("ADMIN_ADDR", base.AdminAddr); err != nil {
		return nil, err
	}
	if err := requireLoopback("DASHBOARD_ADDR", base.DashboardAddr); err != nil {
		return nil, err
	}

	// PERF-2 heap soft-limit: GC harder as the heap approaches the budget rather
	// than being OOM-killed (the last line under abuse). env-only, set before the DB
	// opens; the worst-case RSS was already self-checked in LoadBase.
	if base.GoMemLimitMiB > 0 {
		debug.SetMemoryLimit(int64(base.GoMemLimitMiB) * 1024 * 1024)
		log.Info("gomemlimit_set", "mib", base.GoMemLimitMiB)
	}

	// 3) Open + migrate SQLite (write/read pools). Closed LAST in the lifecycle.
	db, err := sqlite.Open(sqlite.Config{
		Path:              base.DBPath,
		CacheKiB:          base.SQLiteCacheKiB,
		MmapBytes:         base.SQLiteMmapBytes,
		WalAutocheckpoint: base.SQLiteAutockptPage,
		ReadPoolMaxConns:  base.ReadPoolMaxConns,
		ConnMaxLifetime:   30 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// 4) Settings overlay: env defaults ← DB overlay (dashboard edits persisted
	// across restarts), re-running cross-field semantics. A corrupt overlay fails
	// fast (never a silent env fallback). Then hold the effective config in the
	// hot-swappable Provider.
	ss := settingsstore.New(db.Writer, db.Reader)
	effective, err := configprovider.LoadWithOverlay(ctx, base, ss)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load overlay: %w", err)
	}
	cfgP := configprovider.New(effective)
	log.Info("config_snapshot", cfgP.Snapshot()...)

	// 5) Stores.
	quotaStore := quotastore.New(db.Writer, db.Reader)
	installStore := installstore.New(db.Writer, db.Reader)

	// 6) Metrics bundle (RED + golden signals). Mounted loopback-only on admin.
	mx := metrics.New()
	mx.BudgetLimit.Set(float64(effective.GlobalDailyBudget))
	inflight := &atomic.Int64{} // shared mirror: chat writes, dashboard reads.

	// 7) App services over their PORTS (adapters in adapters.go).
	quotaSvc := appquota.New(quotaStore, quotaCfgSource{p: cfgP})
	nonces := noncecache.New(10 * time.Minute) // PoW replay-once (inert unless PoW on).
	installSvc := appinstall.New(cfgP, installStore, nonces,
		installsCreatedCounter{m: mx}, powCounter{m: mx})
	modelCat := appmodel.New(cfgP)

	// 8) Upstream client + breaker, fed the metrics hook (key cooldown + breaker
	// gauge). Wrapped in the covariance adapter so chat sees chat.UpstreamStream.
	upClient := upstream.New(effective.DeepSeekAPIKeys, effective.UpstreamHeaderTimeout,
		log, upstreamHook{m: mx})

	// 9) Shared rate limiter: the SAME bucket the chat RL gate AND the M2 throttle
	// reach through, so SetKeyLimit tightens the bucket Allow meters (B1). An LRU
	// eviction bumps the churn counter (rate-limit-skirt signal).
	rl := ratelimit.New(effective.RatePerMin)
	rl.SetOnEvict(func(string) { mx.RateLimiterEvictions.Inc() })
	throttle := newTokenThrottle(cfgP, rl, mx)

	// 10) REL-6 disk guard over the data disk (the DB path). Primed synchronously
	// before serving in the lifecycle so the first request sees the true state.
	dg := diskguard.New(diskguard.Config{
		Path:       effective.DBPath,
		MinMB:      effective.DiskMinMB,
		MinPercent: effective.DiskMinPercent,
		Logger:     log,
	})

	// 11) Chat saga. The N_global cap is fixed from the startup config inside New.
	bgWG := &sync.WaitGroup{}
	chatSvc := appchat.New(appchat.Deps{
		Auth:     installSvc,
		Quota:    quotaSvc,
		Upstream: upstreamAdapter{c: upClient},
		RL:       rl,
		Throttle: throttle,
		Disk:     dg,
		Config:   cfgP,
		Clock:    systemClock{},
		Logger:   chatLogger{},
		Metrics:  chatMetrics{m: mx, inflight: inflight},
		BgWG:     bgWG,
	})

	// 12) Health checker (DB writable + cached upstream probe + disk). freshFor
	// defaults to 90s (3× the 30s prober).
	prober := newUpstreamProber(upClient, effective.DeepSeekBaseURL)
	health := apphealth.New(dbChecker{w: db.Writer}, prober, dg, 0)

	// 13) Alert state tracker + recent-window QPS sampler (dashboard reads both).
	alerter := alert.New(alert.Thresholds{})
	sampler := ratesample.New(60)

	// 14) Business + admin handlers.
	bizHandler := router.BuildHandler(router.Deps{
		Install: installSvc,
		Chat:    chatSvc,
		Quota:   quotaSvc,
		Models:  modelCat,
		Mx:      mx,
		OnPanic: mx.Panics,
		// Boot snapshot: MAX_BODY_BYTES is RestartRequired (the chain is assembled
		// once), same contract as the N_global semaphore capacity.
		MaxBodyBytes: effective.MaxBodyBytes,
	})
	adminHandler := router.BuildAdminHandler(router.AdminDeps{
		Metrics: mx.Handler(),
		Ready:   health,
	})

	bizSrv := &http.Server{
		Handler:           bizHandler,
		ReadHeaderTimeout: 10 * time.Second, // anti-Slowloris
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: streaming uses per-frame ResponseController deadlines.
	}
	adminSrv := &http.Server{
		Addr:              effective.AdminAddr,
		Handler:           adminHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 15) Dashboard — only when BOTH auth secrets are set (a half-config already
	// fails fast in LoadBase). The Static SPA handler is the embedded React build
	// (infra/webassets), serving /static/ assets + the SPA shell for non-API GETs.
	var dashSrv *http.Server
	if effective.DashboardUser != "" && effective.DashboardPassword != "" {
		dashSvc := appdash.New(appdash.Deps{
			Budget:       budgetSource{ds: dashStore{w: db.Writer, r: db.Reader, loc: effective.Location}, getLimit: func() int64 { return cfgP.Load().GlobalDailyBudget }},
			Reservations: quotaStore,
			Inflight:     inflightSource{v: inflight},
			Rate:         sampler,
			Alerts:       alerter,
			Installs:     installSvc,
			Config:       dashCfgSource{p: cfgP, ss: ss},
			Lister:       dashStore{w: db.Writer, r: db.Reader, loc: effective.Location},
			Mutator:      dashStore{w: db.Writer, r: db.Reader, loc: effective.Location},
			Export:       dashStore{w: db.Writer, r: db.Reader, loc: effective.Location},
			Logger:       dashLogger{l: log},
		})
		gate := &mwdash.Gate{
			Sessions:     mwdash.NewSessionStore(0, time.Now),
			SecureCookie: !effective.DashboardDevInsecureCookie,
		}
		logins := mwdash.NewLoginLimiter(time.Now)
		dashH, derr := dashhandler.New(dashhandler.Config{
			Service:      dashSvc,
			Gate:         gate,
			Logins:       logins,
			User:         effective.DashboardUser,
			Password:     effective.DashboardPassword,
			BreakerOpen:  upClient.BreakerOpen,
			DiskDegraded: dg.Degraded,
		})
		if derr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("dashboard init: %w", derr)
		}
		dashHandler := router.BuildDashboardHandler(router.DashboardDeps{
			Handler: dashH,
			Gate:    gate,
			Static:  webassets.Handler(), // embedded React SPA (ui/dist): /static/ + SPA shell.
		})
		dashSrv = &http.Server{
			Addr:              effective.DashboardAddr,
			Handler:           dashHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
	} else {
		log.Info("dashboard_disabled", "reason", "DASHBOARD_USER/PASSWORD not configured")
	}

	// 16) REL-6 floors are runtime-tunable: hot-reload the guard + rate limiter on
	// the matching overrides so a dashboard edit takes effect without a restart.
	// (configprovider has no OnReload hook; the loops re-read live config instead —
	// see notes. SetRate is driven from the throttle's per-call snapshot.)

	loopCtx, loopCancel := context.WithCancel(context.Background())
	return &App{
		cfg:              cfgP,
		db:               db,
		bizSrv:           bizSrv,
		adminSrv:         adminSrv,
		dashSrv:          dashSrv,
		bgWG:             bgWG,
		loopCtx:          loopCtx,
		loopCancel:       loopCancel,
		quota:            quotaSvc,
		upstream:         upClient,
		disk:             dg,
		prober:           prober,
		mx:               mx,
		alerter:          alerter,
		rate:             sampler,
		rl:               rl,
		throttle:         throttle,
		openReservations: quotaStore,
		log:              log,
		addrs: addrs{
			business:  effective.ListenAddr,
			admin:     effective.AdminAddr,
			dashboard: effective.DashboardAddr,
		},
	}, nil
}
