package alert

import (
	"context"
	"testing"
	"time"
)

// statusByReason indexes a CurrentStatus snapshot for assertions.
func statusByReason(a *Alerter) map[string]AlertState {
	m := make(map[string]AlertState)
	for _, st := range a.CurrentStatus() {
		m[st.Reason] = st
	}
	return m
}

// firingReasons returns the reasons currently firing.
func firingReasons(a *Alerter) []string {
	var out []string
	for _, st := range a.CurrentStatus() {
		if st.Firing {
			out = append(out, st.Reason)
		}
	}
	return out
}

// TestBudgetThresholds: <80% silent, >=80% warning, >=100% critical, with 100%
// and 80% mutually exclusive.
func TestBudgetThresholds(t *testing.T) {
	cases := []struct {
		used, limit int64
		wantReason  string // "" = no budget alert firing
	}{
		{700, 1000, ""},
		{800, 1000, "budget_high"},
		{990, 1000, "budget_high"},
		{1000, 1000, "budget_exhausted"},
		{1500, 1000, "budget_exhausted"},
	}
	for _, tc := range cases {
		a := New(Thresholds{})
		a.Check(context.Background(), Signals{BudgetUsed: tc.used, BudgetLimit: tc.limit})
		got := firingReasons(a)
		if tc.wantReason == "" {
			if len(got) != 0 {
				t.Fatalf("used=%d/%d: want no alert firing, got %v", tc.used, tc.limit, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != tc.wantReason {
			t.Fatalf("used=%d/%d: want %q firing, got %v", tc.used, tc.limit, tc.wantReason, got)
		}
	}
}

// TestReservationsBacklogFires: open reservations >= threshold trips its alert.
func TestReservationsBacklogFires(t *testing.T) {
	a := New(Thresholds{ReservationsOpen: 50})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, ReservationsOpen: 50})
	if !statusByReason(a)["reservations_open_backlog"].Firing {
		t.Fatal("reservations_open_backlog must be firing")
	}
}

// TestBreakerOpenFires: an OPEN upstream breaker trips its alert.
func TestBreakerOpenFires(t *testing.T) {
	a := New(Thresholds{})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, BreakerOpen: true})
	if !statusByReason(a)["upstream_breaker_open"].Firing {
		t.Fatal("upstream_breaker_open must be firing")
	}
}

// TestDiskDegradedFires: read-only degradation trips its alert.
func TestDiskDegradedFires(t *testing.T) {
	a := New(Thresholds{})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, DiskDegraded: true})
	if !statusByReason(a)["disk_read_only"].Firing {
		t.Fatal("disk_read_only must be firing")
	}
}

// TestUpstreamErrorBurstFires: errors at/above the burst ceiling fire, below it
// stays clear.
func TestUpstreamErrorBurstFires(t *testing.T) {
	a := New(Thresholds{UpstreamErrorBurst: 10})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, UpstreamErrors: 9})
	if statusByReason(a)["upstream_error_burst"].Firing {
		t.Fatal("below burst ceiling must stay clear")
	}
	a.Check(context.Background(), Signals{BudgetLimit: 1000, UpstreamErrors: 10})
	if !statusByReason(a)["upstream_error_burst"].Firing {
		t.Fatal("at/above burst ceiling must fire")
	}
}

// TestInstallBurstFires: installs at/above the burst ceiling fire as a neutral
// warning (organic spike or Sybil wave), below it stays clear.
func TestInstallBurstFires(t *testing.T) {
	a := New(Thresholds{InstallBurst: 200})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, InstallsThisWindow: 199})
	if statusByReason(a)["install_burst"].Firing {
		t.Fatal("below install burst ceiling must stay clear")
	}
	a.Check(context.Background(), Signals{BudgetLimit: 1000, InstallsThisWindow: 200})
	st := statusByReason(a)["install_burst"]
	if !st.Firing {
		t.Fatal("at/above install burst ceiling must fire")
	}
	if st.Severity != "warning" {
		t.Fatalf("install_burst severity = %q want warning (neutral operational signal)", st.Severity)
	}
}

// TestTokenThrottleFires: the per-token anomaly auto-throttle signal fires (as a
// neutral warning) when installs are currently throttled.
func TestTokenThrottleFires(t *testing.T) {
	a := New(Thresholds{TokensThrottled: 1})
	a.Check(context.Background(), Signals{BudgetLimit: 1000, TokensThrottled: 0})
	if statusByReason(a)["token_throttle"].Firing {
		t.Fatal("zero throttled installs must stay clear")
	}
	a.Check(context.Background(), Signals{BudgetLimit: 1000, TokensThrottled: 1})
	st := statusByReason(a)["token_throttle"]
	if !st.Firing {
		t.Fatal("at/above throttled ceiling must fire")
	}
	if st.Severity != "warning" {
		t.Fatalf("token_throttle severity = %q want warning (neutral signal)", st.Severity)
	}
}

// TestEachFailureFiresIndependently: every dimension tripped in one Check is
// reflected firing, with no cross-suppression (except the budget mutual exclusion).
func TestEachFailureFiresIndependently(t *testing.T) {
	a := New(Thresholds{ReservationsOpen: 10, UpstreamErrorBurst: 10})
	a.Check(context.Background(), Signals{
		BudgetUsed: 1000, BudgetLimit: 1000, ReservationsOpen: 10,
		BreakerOpen: true, DiskDegraded: true, UpstreamErrors: 10,
	})
	want := map[string]bool{
		"budget_exhausted": true, "reservations_open_backlog": true,
		"upstream_breaker_open": true, "disk_read_only": true, "upstream_error_burst": true,
	}
	for _, r := range firingReasons(a) {
		if !want[r] {
			t.Fatalf("unexpected firing reason %q", r)
		}
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("conditions not firing: %v", want)
	}
	// budget_high must NOT fire when exhausted (mutual exclusion).
	if statusByReason(a)["budget_high"].Firing {
		t.Fatal("budget_high must not fire while exhausted")
	}
}

// TestStateClearsAndKeepsLastFired: a cleared condition reports Firing=false but
// retains its LastFiredAt (the most recent trigger time).
func TestStateClearsAndKeepsLastFired(t *testing.T) {
	fixed := time.Now()
	a := New(Thresholds{}, WithClock(func() time.Time { return fixed }))

	a.Check(context.Background(), Signals{BudgetUsed: 1000, BudgetLimit: 1000})
	st := statusByReason(a)["budget_exhausted"]
	if !st.Firing || st.LastFiredAt.IsZero() {
		t.Fatalf("budget_exhausted should be firing with a LastFiredAt, got %+v", st)
	}
	firedAt := st.LastFiredAt

	// Recovery: budget back under threshold → not firing, but LastFiredAt kept.
	a.Check(context.Background(), Signals{BudgetUsed: 100, BudgetLimit: 1000})
	st = statusByReason(a)["budget_exhausted"]
	if st.Firing {
		t.Fatal("budget_exhausted should have cleared")
	}
	if !st.LastFiredAt.Equal(firedAt) {
		t.Fatalf("LastFiredAt must be retained after clear: want %v got %v", firedAt, st.LastFiredAt)
	}
}

// TestCurrentStatusStableOrderAndNoSecrets: CurrentStatus returns a stable order
// and each state carries only aggregate reason/severity/message (fixed strings).
func TestCurrentStatusStableOrderAndNoSecrets(t *testing.T) {
	a := New(Thresholds{})
	a.Check(context.Background(), Signals{BudgetUsed: 1000, BudgetLimit: 1000})
	got := a.CurrentStatus()
	if len(got) == 0 {
		t.Fatal("status must be populated after a Check")
	}
	wantOrder := []string{
		"budget_exhausted", "budget_high", "reservations_open_backlog",
		"upstream_breaker_open", "disk_read_only", "upstream_error_burst",
		"install_burst", "token_throttle",
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("want %d conditions, got %d", len(wantOrder), len(got))
	}
	for i, st := range got {
		if st.Reason != wantOrder[i] {
			t.Fatalf("order[%d] = %q want %q", i, st.Reason, wantOrder[i])
		}
		if st.Reason == "" || st.Severity == "" || st.Message == "" {
			t.Fatalf("state missing fields: %+v", st)
		}
	}
}

// TestCurrentStatusBeforeCheck: nothing is reported until Check has run.
func TestCurrentStatusBeforeCheck(t *testing.T) {
	a := New(Thresholds{})
	if got := a.CurrentStatus(); len(got) != 0 {
		t.Fatalf("want empty status before any Check, got %v", got)
	}
}

// TestConcurrentCheckAndStatus exercises the mutex under -race: a writer loop
// (Check) and reader loops (CurrentStatus) must not race.
func TestConcurrentCheckAndStatus(t *testing.T) {
	a := New(Thresholds{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			a.Check(context.Background(), Signals{BudgetUsed: int64(i), BudgetLimit: 1000, BreakerOpen: i%2 == 0})
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			_ = a.CurrentStatus()
		}
	}
}
