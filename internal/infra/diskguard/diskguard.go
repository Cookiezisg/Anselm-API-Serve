// Package diskguard implements REL-6 (GW-INV-29) low-disk read-only degradation:
// a statfs probe of the data disk flips the gateway into degraded mode when free
// space drops below a floor, so the proxy can shed new writes with 503 DISK_LOW
// BEFORE any reservation — rather than letting SQLite fail a mid-write on a full
// disk, which would silently corrupt the conservative quota accounting. Recovery
// auto-clears once the disk frees up.
//
// 红线(REL-6):磁盘满让 SQLite 静默写失败 = 预占/结算无声损坏;故剩余低于阈值即
// 主动只读降级(预占前 shed 503 DISK_LOW),盘恢复自动解除。探测用真实 Statfs。
package diskguard

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"syscall"
	"time"
)

// Guard holds the atomic degraded flag plus the hot-reloadable floors. Degraded
// is read on the proxy hot path (pre-reserve), so it is a plain atomic, never a
// lock; the floors are atomics too so a config hot-reload can adjust them while
// the periodic Check reads them.
//
// degraded 在请求热路径被读,故 atomic 无锁;floors 也 atomic 以便热调与 Check 并发安全。
type Guard struct {
	path string

	minBytes   atomic.Int64 // DISK_MIN_MB absolute free-bytes floor (0=disabled)
	minPercent atomic.Int64 // DISK_MIN_PERCENT free-percent floor 0..100 (0=disabled)

	degraded atomic.Bool

	log *slog.Logger

	// statfs is injected so tests can drive the flip/clear transitions without a
	// real low-disk filesystem; production uses the syscall-backed default.
	statfs func(path string) (freeBytes, totalBytes int64, err error)
}

// Config configures a Guard.
type Config struct {
	Path       string       // a path on the data disk (the DB file is ideal)
	MinMB      int          // DISK_MIN_MB absolute floor (0=disable)
	MinPercent int          // DISK_MIN_PERCENT percent floor 0..100 (0=disable)
	Logger     *slog.Logger // nil → slog.Default()
}

// New builds a Guard. The degraded flag starts at the optimistic false default;
// bootstrap MUST call Check() synchronously before it begins serving (so the very
// first request sees the true disk state, not the optimistic default — otherwise a
// request could slip through on an already-full disk before the first tick).
//
// 🔴 degraded 起始 false 是「乐观默认」:必须在 serve 前同步 Check 一次,否则首请求
// 可能在满盘上被放行。Run 的 prime 是冗余兜底。
func New(c Config) *Guard {
	log := c.Logger
	if log == nil {
		log = slog.Default()
	}
	g := &Guard{
		path:   c.Path,
		log:    log,
		statfs: statfsReal,
	}
	g.minBytes.Store(int64(c.MinMB) * 1024 * 1024)
	g.minPercent.Store(int64(c.MinPercent))
	return g
}

// SetFloors hot-swaps the low-disk floors (DISK_MIN_MB / DISK_MIN_PERCENT
// override). Race-safe with a concurrent Check; the next Check re-evaluates
// against the new floors. Call Check() if an immediate re-evaluation is wanted.
//
// 热调磁盘阈值:与周期 Check 并发安全(atomic 存),下一次 Check 用新阈值重判。
func (g *Guard) SetFloors(minMB, minPercent int) {
	g.minBytes.Store(int64(minMB) * 1024 * 1024)
	g.minPercent.Store(int64(minPercent))
}

// Degraded reports whether the gateway is currently in read-only degradation.
// Hot-path safe (atomic load).
func (g *Guard) Degraded() bool { return g.degraded.Load() }

// Check samples the data disk once and updates the degraded flag, logging the
// edges (enter/leave). A statfs error is treated as NON-degraded (FAIL-OPEN): a
// transient probe glitch must not wedge the whole service read-only — leave the
// current state and let the next Check re-evaluate.
//
// 探测失败 fail-open(不因瞬时探测抖动把服务整体卡成只读);仅真·低于阈值才降级。
func (g *Guard) Check() {
	free, total, err := g.statfs(g.path)
	if err != nil {
		g.log.Warn("diskguard_probe_failed", "event", "diskguard", "error", err.Error())
		return
	}

	minBytes := g.minBytes.Load()
	minPercent := g.minPercent.Load()
	low := minBytes > 0 && free < minBytes
	if minPercent > 0 && total > 0 {
		// Integer-safe percent: free*100/total < floor.
		if free*100/total < minPercent {
			low = true
		}
	}

	prev := g.degraded.Swap(low)
	switch {
	case low && !prev:
		g.log.Warn("disk_low_degraded", "event", "diskguard",
			"free_mb", free/(1024*1024), "total_mb", total/(1024*1024),
			"min_mb", minBytes/(1024*1024), "min_percent", minPercent)
	case !low && prev:
		g.log.Info("disk_recovered", "event", "diskguard",
			"free_mb", free/(1024*1024), "total_mb", total/(1024*1024))
	}
}

// Run drives the periodic probe on a ticker until ctx is canceled. It primes via
// Check() before the first tick (redundant with bootstrap's synchronous pre-serve
// prime). interval <= 0 falls back to 30s. Meant to run on bootstrap's shutdown
// WaitGroup so it drains on graceful stop (like the reconciler / prober, REL-4).
func (g *Guard) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	g.Check() // prime before the first tick
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Check()
		}
	}
}

// statfsReal returns (freeBytes, totalBytes) for the filesystem holding path via
// syscall.Statfs (portable across linux/darwin: Bavail/Blocks/Bsize fields).
//
// Bavail = blocks free to an unprivileged user (the honest headroom), not Bfree.
// Block counts/size are unsigned; we compute in uint64 then saturate to int64 max
// — an exabyte-scale FS is implausible, and saturation can only AVOID a low-disk
// shed (the safe direction), never falsely trigger one.
func statfsReal(path string) (int64, int64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	bsize := uint64(s.Bsize) //nolint:gosec // block size is a small positive value; range-safe
	return satInt64(s.Bavail * bsize), satInt64(s.Blocks * bsize), nil
}

// satInt64 clamps a uint64 byte count to the int64 range (no overflow/negative
// wrap on absurdly large filesystems).
func satInt64(v uint64) int64 {
	if v > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(v)
}
