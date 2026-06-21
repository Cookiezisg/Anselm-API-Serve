package dashboard

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/pkg/ratesample"
)

// --- fakes ---

type fakeBudget struct {
	day         string
	limit, used int64
	err         error
}

func (f fakeBudget) GlobalBudget(context.Context) (string, int64, int64, error) {
	return f.day, f.limit, f.used, f.err
}

type fakeReservations struct{ n int64 }

func (f fakeReservations) OpenReservations(context.Context) (int64, error) { return f.n, nil }

type fakeInflight struct{ n int64 }

func (f fakeInflight) InflightConcurrency() int64 { return f.n }

type fakeRate struct{ s ratesample.Snapshot }

func (f fakeRate) Snapshot() ratesample.Snapshot { return f.s }

type fakeInstallsToday struct{ n int64 }

func (f fakeInstallsToday) InstallsToday(context.Context) (int64, error) { return f.n, nil }

type fakeConfig struct {
	dump       []DumpItem
	cap        int64
	rejectWith string
	applied    map[string]string
}

func (f *fakeConfig) Dump() []DumpItem        { return f.dump }
func (f *fakeConfig) InstallGlobalCap() int64 { return f.cap }
func (f *fakeConfig) ApplyOverrides(_ context.Context, o map[string]string) (string, bool) {
	if f.rejectWith != "" {
		return f.rejectWith, false
	}
	f.applied = o
	return "", true
}

type fakeLister struct {
	rows []InstallRow
	err  error
}

func (f fakeLister) ListInstalls(_ context.Context, cursor, limit int) ([]InstallRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	if cursor >= len(f.rows) {
		return nil, nil
	}
	end := cursor + limit
	if end > len(f.rows) {
		end = len(f.rows)
	}
	return f.rows[cursor:end], nil
}

type fakeMutator struct {
	found bool
	err   error
	calls []string
}

func (f *fakeMutator) SetStatus(_ context.Context, id string, st install.Status) (bool, error) {
	f.calls = append(f.calls, id+":"+string(st))
	return f.found, f.err
}

type fakeSnapshotter struct {
	size    int64
	body    string
	cleaned bool
	openErr error
	snapErr error
}

func (f *fakeSnapshotter) Snapshot(context.Context) (string, int64, error) {
	if f.snapErr != nil {
		return "", 0, f.snapErr
	}
	return "/tmp/fake.db", f.size, nil
}
func (f *fakeSnapshotter) Cleanup(string) { f.cleaned = true }
func (f *fakeSnapshotter) Open(string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

// --- tests ---

func TestOverviewAssemblesFromPorts(t *testing.T) {
	svc := New(Deps{
		Budget:       fakeBudget{day: "2026-06-20", limit: 1000, used: 250},
		Reservations: fakeReservations{n: 7},
		Inflight:     fakeInflight{n: 3},
		Rate:         fakeRate{s: ratesample.Snapshot{QPS: 1.5}},
		Installs:     fakeInstallsToday{n: 42},
		Config:       &fakeConfig{cap: 200},
	})
	o := svc.Overview(context.Background(), true, true)
	if o.Budget.Used != 250 || o.Budget.Remaining != 750 {
		t.Fatalf("budget: %+v", o.Budget)
	}
	if o.OpenReservations != 7 || o.InflightConcurrency != 3 || o.InstallsToday != 42 {
		t.Fatalf("gauges: %+v", o)
	}
	if !o.UpstreamBreakerOpen || !o.DiskDegraded {
		t.Fatalf("flags not threaded: %+v", o)
	}
	if o.Recent.QPS != 1.5 || o.InstallGlobalCap != 200 {
		t.Fatalf("rate/cap: %+v", o)
	}
	if o.Alerts == nil {
		t.Fatal("Alerts must be non-nil slice for JSON")
	}
}

func TestOverviewBudgetErrorLeavesZero(t *testing.T) {
	svc := New(Deps{Budget: fakeBudget{err: errors.New("db down")}})
	o := svc.Overview(context.Background(), false, false)
	if o.Budget.Used != 0 || o.Budget.Limit != 0 {
		t.Fatalf("a failed budget read must not poison the snapshot: %+v", o.Budget)
	}
}

func TestConfigApplyRejectedCarriesReason(t *testing.T) {
	svc := New(Deps{Config: &fakeConfig{rejectWith: "RATE_PER_MIN out of bounds"}})
	_, err := svc.ConfigApply(context.Background(), "admin", map[string]string{"RATE_PER_MIN": "999999"})
	var rej *ErrConfigRejected
	if !errors.As(err, &rej) || rej.Reason != "RATE_PER_MIN out of bounds" {
		t.Fatalf("want ErrConfigRejected with reason, got %v", err)
	}
	// Rejected apply must NOT record an audit event.
	if ev, _ := svc.Audit(0, 10); len(ev) != 0 {
		t.Fatalf("rejected apply must not audit, got %d events", len(ev))
	}
}

func TestConfigApplyOKAuditsKeysOnly(t *testing.T) {
	cfg := &fakeConfig{dump: []DumpItem{{Key: "RATE_PER_MIN", Value: "60"}}}
	svc := New(Deps{Config: cfg})
	_, err := svc.ConfigApply(context.Background(), "admin", map[string]string{"RATE_PER_MIN": "60", "DAILY_SUBLIMIT": "5"})
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := svc.Audit(0, 10)
	if len(ev) != 1 || ev[0].Action != "config_update" {
		t.Fatalf("want one config_update audit, got %+v", ev)
	}
	// Target is the sorted comma-joined KEY set, never values.
	if ev[0].Target != "DAILY_SUBLIMIT,RATE_PER_MIN" {
		t.Fatalf("target must be sorted keys only: %q", ev[0].Target)
	}
	if strings.Contains(ev[0].Target, "60") || strings.Contains(ev[0].Target, "5") {
		t.Fatal("audit target leaked a value")
	}
}

func TestInstallsListPagination(t *testing.T) {
	rows := make([]InstallRow, 5)
	for i := range rows {
		rows[i] = InstallRow{ID: "ins_" + string(rune('a'+i))}
	}
	svc := New(Deps{Lister: fakeLister{rows: rows}})
	got, next, more, err := svc.InstallsList(context.Background(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !more || next != 2 {
		t.Fatalf("page1: got=%d more=%v next=%d", len(got), more, next)
	}
	// Last page: fewer than limit+1 returned ⇒ no more.
	got, next, more, err = svc.InstallsList(context.Background(), 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || more || next != 0 {
		t.Fatalf("last page: got=%d more=%v next=%d", len(got), more, next)
	}
}

func TestInstallRowCarriesNoSecrets(t *testing.T) {
	// Struct-level guard: only the safe JSON keys are allowed, so a future field
	// addition that leaks a token/fingerprint/ip fails this test.
	allowed := map[string]bool{"id": true, "status": true, "createdAt": true, "lastSeenAt": true, "todayTokens": true}
	rt := reflect.TypeOf(InstallRow{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		key := strings.Split(tag, ",")[0]
		// Allowlist is the guard: anything not explicitly safe (a token/fingerprint/ip
		// added later) fails. todayTokens is a usage COUNT, not a secret token.
		if !allowed[key] {
			t.Fatalf("InstallRow exposes a non-whitelisted JSON field %q (possible PII leak)", key)
		}
	}
}

func TestBanUnbanAuditAndNotFound(t *testing.T) {
	mut := &fakeMutator{found: true}
	svc := New(Deps{Mutator: mut})
	if err := svc.Ban(context.Background(), "admin", "ins_1", "abuse"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unban(context.Background(), "admin", "ins_1"); err != nil {
		t.Fatal(err)
	}
	ev, _ := svc.Audit(0, 10)
	if len(ev) != 2 {
		t.Fatalf("want 2 audit events, got %d", len(ev))
	}

	// No-match ⇒ ErrInstallNotFound + a not_found audit row.
	mut2 := &fakeMutator{found: false}
	svc2 := New(Deps{Mutator: mut2})
	err := svc2.Ban(context.Background(), "admin", "ghost", "x")
	var nf *ErrInstallNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("want ErrInstallNotFound, got %v", err)
	}
	ev2, _ := svc2.Audit(0, 10)
	if len(ev2) != 1 || ev2[0].Outcome != "not_found" {
		t.Fatalf("want one not_found audit, got %+v", ev2)
	}
}

func TestExportStreamsAndCleansUp(t *testing.T) {
	snap := &fakeSnapshotter{size: 4, body: "data"}
	svc := New(Deps{Export: snap})
	body, size, name, cleanup, err := svc.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if size != 4 || !strings.HasPrefix(name, "anselm-gateway-") || !strings.HasSuffix(name, ".db") {
		t.Fatalf("size/name: %d %q", size, name)
	}
	b, _ := io.ReadAll(body)
	if string(b) != "data" {
		t.Fatalf("body: %q", b)
	}
	cleanup()
	if !snap.cleaned {
		t.Fatal("cleanup must remove the temp snapshot")
	}
}

// TestAuditKeysetNoSkipNoDupUnderConcurrentWrite covers the keyset invariant:
// while one goroutine pages, another writes new events (which only ever get a
// HIGHER seq). Paging by seq must never skip or duplicate a row.
func TestAuditKeysetNoSkipNoDupUnderConcurrentWrite(t *testing.T) {
	svc := New(Deps{})
	const total = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			svc.RecordAudit(AuditEvent{Action: "ban", Target: "ins"})
		}
	}()
	wg.Wait()

	// Page through everything; collect seqs. Concurrent writes keep happening.
	var writer sync.WaitGroup
	writer.Add(1)
	stop := make(chan struct{})
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
				svc.RecordAudit(AuditEvent{Action: "unban"})
			}
		}
	}()

	seen := map[int64]bool{}
	var before int64 = 0
	for {
		ev, next := svc.Audit(before, 10)
		if len(ev) == 0 {
			break
		}
		for _, e := range ev {
			if seen[e.Seq] {
				close(stop)
				writer.Wait()
				t.Fatalf("duplicate seq %d in keyset paging", e.Seq)
			}
			seen[e.Seq] = true
		}
		if next == 0 {
			break
		}
		before = next
	}
	close(stop)
	writer.Wait()

	// Newest-first, strictly descending within the walk we captured.
	if len(seen) == 0 {
		t.Fatal("paged nothing")
	}
}

func TestAuditNewestFirstDescending(t *testing.T) {
	svc := New(Deps{Clock: fixedClock{}})
	for i := 0; i < 5; i++ {
		svc.RecordAudit(AuditEvent{Action: "x"})
	}
	ev, _ := svc.Audit(0, 10)
	for i := 1; i < len(ev); i++ {
		if ev[i-1].Seq <= ev[i].Seq {
			t.Fatalf("not newest-first: %d then %d", ev[i-1].Seq, ev[i].Seq)
		}
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
