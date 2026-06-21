package diskguard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const mb = 1024 * 1024

// newWithFake builds a Guard whose statfs is driven by the test, so we can
// exercise the degrade/recover edges without a real low-disk filesystem.
func newWithFake(minMB, minPct int, fake func() (free, total int64, err error)) *Guard {
	g := New(Config{Path: "/x", MinMB: minMB, MinPercent: minPct})
	g.statfs = func(string) (int64, int64, error) { return fake() }
	return g
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake statfs error" }

var errFake = fakeErr{}

// TestAbsoluteFloorFlipAndRecover: below the MB floor → degraded; back above →
// cleared (REL-6 auto-recovery).
func TestAbsoluteFloorFlipAndRecover(t *testing.T) {
	free := int64(1000 * mb)
	g := newWithFake(500, 0, func() (int64, int64, error) { return free, 10000 * mb, nil })

	g.Check()
	if g.Degraded() {
		t.Fatal("1000MB free with 500MB floor must NOT be degraded")
	}

	free = 200 * mb
	g.Check()
	if !g.Degraded() {
		t.Fatal("200MB free under 500MB floor must be degraded")
	}

	free = 600 * mb
	g.Check()
	if g.Degraded() {
		t.Fatal("600MB free above 500MB floor must auto-recover")
	}
}

// TestSynchronousPrimeFromOptimisticDefault (REL-6): a freshly-built guard starts
// at the optimistic degraded=false default; a SINGLE synchronous Check() against
// an already-low disk must flip it BEFORE any Run loop — exactly what bootstrap
// does before serving, so the first request sees the true disk state.
func TestSynchronousPrimeFromOptimisticDefault(t *testing.T) {
	g := newWithFake(500, 0, func() (int64, int64, error) { return 100 * mb, 10000 * mb, nil })

	if g.Degraded() {
		t.Fatal("a fresh guard must start non-degraded (optimistic default)")
	}

	g.Check()
	if !g.Degraded() {
		t.Fatal("synchronous pre-serve Check() must flip an already-low disk to degraded")
	}
}

// TestPercentFloor: percent floor trips independently of the absolute floor.
func TestPercentFloor(t *testing.T) {
	// 4% free, 10% floor → degraded even though absolute MB floor is disabled.
	g := newWithFake(0, 10, func() (int64, int64, error) { return 4 * mb, 100 * mb, nil })
	g.Check()
	if !g.Degraded() {
		t.Fatal("4% free under 10% floor must be degraded")
	}
}

// TestProbeErrorFailOpen: a statfs error must NOT flip an otherwise-healthy guard
// into degraded (fail-open; the next Check re-evaluates).
func TestProbeErrorFailOpen(t *testing.T) {
	g := newWithFake(500, 0, func() (int64, int64, error) { return 0, 0, errFake })
	g.Check()
	if g.Degraded() {
		t.Fatal("a probe error must leave the guard non-degraded (fail-open)")
	}
}

// TestProbeErrorPreservesDegraded: fail-open must also preserve an EXISTING
// degraded state on a probe glitch (it leaves state untouched, not just false).
func TestProbeErrorPreservesDegraded(t *testing.T) {
	var fail atomic.Bool
	g := newWithFake(500, 0, func() (int64, int64, error) {
		if fail.Load() {
			return 0, 0, errFake
		}
		return 100 * mb, 10000 * mb, nil
	})
	g.Check()
	if !g.Degraded() {
		t.Fatal("100MB under 500MB floor must be degraded")
	}
	fail.Store(true)
	g.Check()
	if !g.Degraded() {
		t.Fatal("a probe error must preserve the prior degraded state (fail-open)")
	}
}

// TestSetFloorsHotChange: a hot floor change is reflected on the NEXT Check
// without rebuilding the guard (DISK_MIN_MB runtime-hot).
func TestSetFloorsHotChange(t *testing.T) {
	g := newWithFake(500, 0, func() (int64, int64, error) { return 1000 * mb, 10000 * mb, nil })
	g.Check()
	if g.Degraded() {
		t.Fatal("1000MB free with 500MB floor must NOT be degraded")
	}

	// Raise the floor above current free; next Check must degrade.
	g.SetFloors(2000, 0)
	g.Check()
	if !g.Degraded() {
		t.Fatal("after SetFloors(2000) with 1000MB free, next Check must degrade")
	}

	// Lower the floor below current free; next Check must recover.
	g.SetFloors(100, 0)
	g.Check()
	if g.Degraded() {
		t.Fatal("after SetFloors(100) with 1000MB free, next Check must recover")
	}
}

// TestRunPrimesBeforeFirstTick: Run() must Check() once synchronously before its
// ticker fires, so an already-low disk is reflected immediately on start.
func TestRunPrimesBeforeFirstTick(t *testing.T) {
	g := newWithFake(500, 0, func() (int64, int64, error) { return 100 * mb, 10000 * mb, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { g.Run(ctx, time.Hour); close(done) }() // long interval: only the prime can run

	deadline := time.After(time.Second)
	for !g.Degraded() {
		select {
		case <-deadline:
			t.Fatal("Run did not prime the degraded flag before the first tick")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

// TestConcurrentDegradedCheck exercises the atomic flag/floors under concurrent
// readers, checkers, and floor hot-swaps (run with -race).
func TestConcurrentDegradedCheck(t *testing.T) {
	var free atomic.Int64
	free.Store(1000 * mb)
	g := newWithFake(500, 0, func() (int64, int64, error) { return free.Load(), 10000 * mb, nil })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = g.Degraded()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				free.Store(int64(100*mb) + free.Load()%(2000*mb))
				g.Check()
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				g.SetFloors(500, 5)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestRealStatfs: the syscall-backed probe returns a plausible (non-zero) total
// for the working dir, proving the Bavail/Blocks/Bsize wiring compiles + runs on
// this platform.
func TestRealStatfs(t *testing.T) {
	free, total, err := statfsReal(".")
	if err != nil {
		t.Fatalf("statfsReal: %v", err)
	}
	if total <= 0 || free < 0 {
		t.Fatalf("implausible statfs: free=%d total=%d", free, total)
	}
}
