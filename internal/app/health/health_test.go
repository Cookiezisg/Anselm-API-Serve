package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

// trackingDB fails the test if its probe is ever called (used to prove Live
// never touches deps) or returns a scripted result otherwise.
type trackingDB struct {
	called *bool
	err    error
}

func (d trackingDB) Writable(context.Context) error {
	if d.called != nil {
		*d.called = true
	}
	return d.err
}

type fakeUpstream struct {
	ok  bool
	age time.Duration
}

func (u fakeUpstream) LastOK() (bool, time.Duration) { return u.ok, u.age }

type fakeDisk struct{ degraded bool }

func (d fakeDisk) Degraded() bool { return d.degraded }

func TestLiveAlwaysOKAndNeverTouchesDeps(t *testing.T) {
	dbCalled := false
	upCalled := false
	diskCalled := false
	// Wire ports that flip a flag the instant they are touched, then assert
	// Live ignores them entirely (GW-INV-13: /healthz never touches the DB).
	_ = New(
		trackingDB{called: &dbCalled},
		probeSpy{called: &upCalled},
		diskSpy{called: &diskCalled},
		0,
	)
	if !Live() {
		t.Fatal("Live must always report ok")
	}
	if dbCalled || upCalled || diskCalled {
		t.Fatalf("Live touched a dependency: db=%v up=%v disk=%v", dbCalled, upCalled, diskCalled)
	}
}

type probeSpy struct{ called *bool }

func (p probeSpy) LastOK() (bool, time.Duration) {
	if p.called != nil {
		*p.called = true
	}
	return true, 0
}

type diskSpy struct{ called *bool }

func (d diskSpy) Degraded() bool {
	if d.called != nil {
		*d.called = true
	}
	return false
}

func TestReadyAllHealthy(t *testing.T) {
	c := New(trackingDB{}, fakeUpstream{ok: true, age: time.Second}, fakeDisk{}, 0)
	got := c.Ready(context.Background())
	want := Readiness{DB: StateOK, Upstream: StateOK, Disk: StateOK, OK: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadyDBDownIs503(t *testing.T) {
	c := New(trackingDB{err: errors.New("ping failed")}, fakeUpstream{ok: true, age: time.Second}, fakeDisk{}, 0)
	got := c.Ready(context.Background())
	if got.DB != StateDown {
		t.Fatalf("DB = %q, want down", got.DB)
	}
	if got.OK {
		t.Fatal("OK must be false when DB is down (503)")
	}
}

func TestReadyDiskDegradedReflectsDBDegradedAnd503(t *testing.T) {
	// DB pings fine but disk is low (REL-6 read-only): db must read degraded.
	c := New(trackingDB{}, fakeUpstream{ok: true, age: time.Second}, fakeDisk{degraded: true}, 0)
	got := c.Ready(context.Background())
	if got.Disk != StateDegraded {
		t.Fatalf("Disk = %q, want degraded", got.Disk)
	}
	if got.DB != StateDegraded {
		t.Fatalf("DB = %q, want degraded (REL-6 read-only reflects db:degraded)", got.DB)
	}
	if got.OK {
		t.Fatal("OK must be false when disk is degraded (503)")
	}
}

func TestReadyDBDownTakesPrecedenceOverDiskDegraded(t *testing.T) {
	// Unwritable DB while disk also degraded → db:down (hard) wins over degraded.
	c := New(trackingDB{err: errors.New("down")}, fakeUpstream{ok: true, age: time.Second}, fakeDisk{degraded: true}, 0)
	if got := c.Ready(context.Background()); got.DB != StateDown {
		t.Fatalf("DB = %q, want down", got.DB)
	}
}

func TestReadyUpstreamStaleIsDegradedAnd503(t *testing.T) {
	c := New(trackingDB{}, fakeUpstream{ok: true, age: 200 * time.Second}, fakeDisk{}, 90*time.Second)
	got := c.Ready(context.Background())
	if got.Upstream != StateDegraded {
		t.Fatalf("Upstream = %q, want degraded", got.Upstream)
	}
	if got.OK {
		t.Fatal("OK must be false when upstream is degraded (503)")
	}
}

func TestReadyUpstreamNeverProbedIsDegraded(t *testing.T) {
	c := New(trackingDB{}, fakeUpstream{ok: false}, fakeDisk{}, 0)
	if got := c.Ready(context.Background()); got.Upstream != StateDegraded {
		t.Fatalf("Upstream = %q, want degraded when never probed", got.Upstream)
	}
}

func TestReadyUpstreamAtFreshnessBoundaryIsOK(t *testing.T) {
	// age == freshFor is still fresh (≤ boundary).
	c := New(trackingDB{}, fakeUpstream{ok: true, age: 90 * time.Second}, fakeDisk{}, 90*time.Second)
	if got := c.Ready(context.Background()); got.Upstream != StateOK {
		t.Fatalf("Upstream = %q, want ok at the exact freshness boundary", got.Upstream)
	}
}

func TestReadyNilPortsFailClosed(t *testing.T) {
	// Partial wiring must never report ready.
	c := New(nil, nil, nil, 0)
	got := c.Ready(context.Background())
	if got.DB != StateDown {
		t.Fatalf("nil DB → DB = %q, want down", got.DB)
	}
	if got.Upstream != StateDegraded {
		t.Fatalf("nil upstream → Upstream = %q, want degraded", got.Upstream)
	}
	if got.Disk != StateDegraded {
		t.Fatalf("nil disk → Disk = %q, want degraded (fail closed)", got.Disk)
	}
	if got.OK {
		t.Fatal("nil ports must not be ready")
	}
}

func TestNewDefaultsFreshFor(t *testing.T) {
	c := New(trackingDB{}, fakeUpstream{ok: true, age: 89 * time.Second}, fakeDisk{}, 0)
	if c.freshFor != 90*time.Second {
		t.Fatalf("freshFor = %v, want 90s default", c.freshFor)
	}
	if got := c.Ready(context.Background()); got.Upstream != StateOK {
		t.Fatalf("Upstream = %q, want ok inside the default 90s window", got.Upstream)
	}
}
