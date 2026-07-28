package bootstrap

// lifecycle.go runs a built App: bind the three listeners, start the background
// loops on bgWG, sd_notify READY, then on SIGINT/SIGTERM perform the strict
// ordered shutdown (GW-INV-24 / REL-4): cancel loops → Shutdown the servers →
// drain bgWG (bounded) → close the DB LAST (so no detached settle/rollback is
// cut off mid-write, which would corrupt accounting).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/pkg/alert"
)

// shutdownTimeout bounds both the HTTP drain and the bgWG wait so a stuck stream
// or write can never hang shutdown forever; a timed-out reservation is backstopped
// by the next startup's orphan reconcile.
const shutdownTimeout = 30 * time.Second

// Run drives the App to serving + ordered shutdown. It blocks until a signal.
func Run(app *App) error {
	// 1) Bind listeners. Business prefers a socket-activation fd; admin + dashboard
	// self-bind. A bind failure here is fatal (return; cmd exits non-zero).
	bizLn, activated, err := businessListener(app.addrs.business)
	if err != nil {
		_ = app.db.Close()
		return err
	}
	adminLn, err := selfBind(app.addrs.admin)
	if err != nil {
		_ = bizLn.Close()
		_ = app.db.Close()
		return err
	}
	var dashLn net.Listener

	// 2) Startup sweep + prime BEFORE serving: finalize crash-left reservations at
	// their full reserved spend (never refund an unknown provider charge), and
	// prime the disk-degrade flag synchronously so the first request sees the true
	// disk state (not the optimistic false default) on an already-full disk.
	if n, rerr := app.quota.ReconcileOrphans(context.Background(), 10*time.Minute, time.Now()); rerr != nil {
		app.log.Error("startup_reconcile_failed", "error", rerr.Error())
	} else if n > 0 {
		app.log.Info("startup_reconciled", "count", n)
	}
	if app.media != nil {
		if n, merr := app.media.Recover(context.Background()); merr != nil {
			app.log.Error("media_startup_recovery_failed", "error", merr.Error())
		} else if n > 0 {
			app.log.Info("media_startup_recovered", "removed", n)
		}
	}
	app.disk.Check()
	app.prober.probe(app.loopCtx) // prime upstream readiness before READY.

	// 3) Start background loops on bgWG so graceful shutdown drains them.
	app.startLoops()

	// 4) Serve. Each Serve runs in its own goroutine; a non-ErrServerClosed error
	// is fatal and signals via errCh.
	errCh := make(chan error, 3)
	go serve(app.bizSrv, bizLn, errCh)
	app.log.Info("listening", "addr", bizLn.Addr().String(), "socket_activated", activated)
	go serve(app.adminSrv, adminLn, errCh)
	app.log.Info("admin_listening", "addr", app.addrs.admin)
	if app.dashSrv != nil {
		dl, derr := selfBind(app.addrs.dashboard)
		if derr != nil {
			app.shutdown(bizLn, adminLn, nil)
			return derr
		}
		dashLn = dl
		go serve(app.dashSrv, dl, errCh)
		app.log.Info("dashboard_listening", "addr", app.addrs.dashboard)
	}

	// 5) sd_notify READY only now: DB open+migrated and all listeners serving. With
	// Type=notify this keeps the old instance's business socket holding the backlog
	// until the new one is up (OPS-2). No-op outside systemd.
	if ok, nerr := daemon.SdNotify(false, daemon.SdNotifyReady); nerr != nil {
		app.log.Warn("sd_notify_failed", "error", nerr.Error())
	} else if ok {
		app.log.Info("sd_notify_ready")
	}

	// 6) Block until a signal OR a serve error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		app.log.Info("shutting_down")
	case serr := <-errCh:
		app.log.Error("serve_failed", "error", serr.Error())
		app.shutdown(bizLn, adminLn, dashLn)
		return serr
	}

	app.shutdown(bizLn, adminLn, dashLn)
	return nil
}

// serve runs one server's Serve, forwarding a non-ErrServerClosed error.
func serve(srv *http.Server, ln net.Listener, errCh chan<- error) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- err
	}
}

// startLoops spins up the periodic background work on bgWG: the orphan reconciler
// (5min), the upstream prober (30s feeding /readyz), the disk-guard check loop,
// and the metrics-refresh loop (15s; publishes DB-derived gauges + drives OBS-4
// alert state + follows the RATE_PER_MIN hot-reload).
func (app *App) startLoops() {
	add := func(f func(context.Context)) {
		app.bgWG.Add(1)
		go func() {
			defer app.bgWG.Done()
			f(app.loopCtx)
		}()
	}

	// Orphan reconciler — 5min.
	add(func(ctx context.Context) {
		tick(ctx, 5*time.Minute, func() {
			if n, err := app.quota.ReconcileOrphans(ctx, 10*time.Minute, time.Now()); err != nil {
				app.log.Error("reconcile_failed", "error", err.Error())
			} else if n > 0 {
				app.log.Info("reconciled", "count", n)
			}
		})
	})

	// Upstream prober — 30s; refreshes the cached /readyz upstream state.
	add(func(ctx context.Context) {
		tick(ctx, 30*time.Second, func() { app.prober.probe(ctx) })
	})

	// Disk guard — its own Run primes + ticks (REL-6).
	add(func(ctx context.Context) { app.disk.Run(ctx, 30*time.Second) })

	// Durable media retention — expiry is committed before a private staging file
	// is removed, and any interrupted cleanup is retried at the next tick/start.
	if app.media != nil {
		add(func(ctx context.Context) {
			tick(ctx, time.Minute, func() {
				if n, err := app.media.Recover(ctx); err != nil {
					app.log.Error("media_recovery_failed", "error", err.Error())
				} else if n > 0 {
					app.log.Info("media_expired_removed", "count", n)
				}
			})
		})
	}

	// Metrics-refresh + OBS-4 alert state — 15s.
	add(func(ctx context.Context) { app.metricsRefresh(ctx) })
}

// metricsRefresh publishes DB-derived gauges + drives the OBS-4 alert thresholds
// off the hot path. configprovider exposes no OnReload hook, so the loop re-reads
// the live snapshot each round (the RATE_PER_MIN hot-reload follows here, and the
// throttle reads live config per-call) — see notes. RATE_PER_MIN is pushed into
// the shared limiter so a dashboard edit takes effect without a restart.
func (app *App) metricsRefresh(ctx context.Context) {
	refresh := func() {
		c := app.cfg.Load()
		app.rl.SetRate(c.RatePerMin) // follow the RATE_PER_MIN hot-reload.

		_, budgetLimit, budgetUsed, budgetErr := app.monthlyBudget.GlobalBudget(ctx)
		if budgetErr == nil {
			const microUSDPerUSD = 1_000_000
			app.mx.BudgetLimit.Set(float64(budgetLimit) / microUSDPerUSD)
			app.mx.BudgetUsed.Set(float64(budgetUsed) / microUSDPerUSD)
		}

		open, haveOpen := int64(0), false
		if o, err := app.openReservations.OpenReservations(ctx); err == nil {
			open, haveOpen = o, true
			app.mx.ReservationsOpen.Set(float64(open))
		}

		throttledNow := app.throttle.TokensThrottledNow() // sweeps + republishes the gauge.

		if haveOpen && budgetErr == nil {
			app.alerter.Check(ctx, alert.Signals{
				BudgetUsed:       budgetUsed,
				BudgetLimit:      budgetLimit,
				ReservationsOpen: open,
				// Only the routed provider's breaker can degrade this deployment. DeepSeek's
				// breaker used to count here because text routed there; nothing does now, so
				// including it would let an outage on an idle path raise an alert nobody can act on.
				// **只有被路由到的那家**的熔断器能拖垮本部署。DeepSeek 的熔断器此前算数是因为文本
				// 走那儿;现在什么也不走,再算它就是让一条闲置路径的故障拉响一个没人能处理的告警。
				BreakerOpen:     app.providers.BreakerOpen(billing.ProviderQwen),
				DiskDegraded:    app.disk.Degraded(),
				TokensThrottled: int64(throttledNow),
			})
		}
	}
	refresh()
	tick(ctx, 15*time.Second, refresh)
}

// shutdown is the strict ordered teardown (REL-4): cancel loops → Shutdown the
// servers (drain HTTP, let long streams finish or hit the deadline) → wait for
// detached settle/rollback + loops to drain (bounded) → close the DB LAST.
func (app *App) shutdown(bizLn, adminLn, dashLn net.Listener) {
	// ① Stop the background loops; their bgWG slots drain in the wait below.
	app.loopCancel()

	// ② Drain HTTP: refuse new conns, let in-flight handlers finish/timeout. The
	// handlers spawn detached settle/rollback (tracked by bgWG) that may outlive
	// Shutdown's return.
	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := app.bizSrv.Shutdown(shCtx); err != nil {
		app.log.Error("shutdown_failed", "error", err.Error())
	}
	if err := app.adminSrv.Shutdown(shCtx); err != nil {
		app.log.Error("admin_shutdown_failed", "error", err.Error())
	}
	if app.dashSrv != nil {
		if err := app.dashSrv.Shutdown(shCtx); err != nil {
			app.log.Error("dashboard_shutdown_failed", "error", err.Error())
		}
	}
	closeAll(bizLn, adminLn, dashLn)

	// ③ Wait for detached settle/rollback + loops to finish (bounded).
	if !waitWithTimeout(app.bgWG, shutdownTimeout) {
		app.log.Warn("shutdown_drain_timeout", "note", "orphan reconcile will backstop")
	}

	// ④ Only now is it safe to close the DB (GW-INV-24 / REL-4 red line).
	if err := app.db.Close(); err != nil {
		app.log.Error("store_close_failed", "error", err.Error())
	}
}

// tick runs f on an interval until ctx is canceled (no initial call — callers
// that need a prime call f once before tick).
func tick(ctx context.Context, every time.Duration, f func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

// waitWithTimeout waits for wg to reach zero or the timeout, returning whether it
// drained. A timed-out background write is backstopped by the next startup's
// orphan reconcile (leaving state=open never corrupts the ledger; reconciliation
// later finalizes it at the full reserved spend).
func waitWithTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// closeAll best-effort closes the listeners (Shutdown already closed the active
// ones; this is the belt-and-suspenders for a serve that never started).
func closeAll(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close()
		}
	}
}
