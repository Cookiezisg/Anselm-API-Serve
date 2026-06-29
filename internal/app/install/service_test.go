package install

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/install"
)

// --- fakes ---

type fakeCfg struct{ c *config.Config }

func (f fakeCfg) Load() *config.Config { return f.c }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{MonthlyQuota: 5000, Location: loc, InstallPerIPHour: 1000}
}

// fakeStore records the IssueParams it sees and returns a programmable result.
type fakeStore struct {
	mu          sync.Mutex
	lastParams  install.IssueParams
	issueResult install.IssueResult
	issueErr    error
	issueCalls  int

	lookupID     string
	lookupStatus install.Status
	lookupFound  bool
	lookupErr    error

	refreshCalls int
	today        int64
}

func (s *fakeStore) Issue(_ context.Context, p install.IssueParams) (install.IssueResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueCalls++
	s.lastParams = p
	return s.issueResult, s.issueErr
}

func (s *fakeStore) Lookup(_ context.Context, _ string) (string, install.Status, bool, error) {
	return s.lookupID, s.lookupStatus, s.lookupFound, s.lookupErr
}

func (s *fakeStore) RefreshLastSeen(_ context.Context, _ string, _ time.Time, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls++
	return nil
}

func (s *fakeStore) InstallsToday(_ context.Context, _ string) (int64, error) {
	return s.today, nil
}

func (s *fakeStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCalls
}

// fakeNonces never blocks (PoW tests inject their own when needed).
type fakeNonces struct{}

func (fakeNonces) UseOnce(string) bool { return true }

type countCounter struct {
	mu sync.Mutex
	n  int
}

func (c *countCounter) Inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }

type labelCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newLabelCounter() *labelCounter { return &labelCounter{counts: map[string]int{}} }
func (c *labelCounter) Inc(result string) {
	c.mu.Lock()
	c.counts[result]++
	c.mu.Unlock()
}
func (c *labelCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := 0
	for _, v := range c.counts {
		t += v
	}
	return t
}

// --- Issue tests ---

func TestIssueFreshTokenEachCall(t *testing.T) {
	store := &fakeStore{issueResult: install.IssueResult{Admitted: true}}
	mxNew := &countCounter{}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, mxNew, nil)

	req := install.NewRequest("fp-aaa", "anselm/0.1")
	v1, gate, ae := s.Issue(context.Background(), req, "ipk1", false)
	if ae != nil || gate != install.GateNone {
		t.Fatalf("issue 1: gate=%q ae=%v", gate, ae)
	}
	v2, _, ae2 := s.Issue(context.Background(), req, "ipk1", false)
	if ae2 != nil {
		t.Fatal(ae2)
	}
	if v1.Token == "" || v2.Token == "" || v1.Token == v2.Token {
		t.Fatalf("tokens must be fresh+distinct: %q %q", v1.Token, v2.Token)
	}
	if !strings.HasPrefix(v1.Token, "gwk_") {
		t.Fatalf("token missing gwk_ prefix: %q", v1.Token)
	}
	if v1.MonthlyQuota != 5000 {
		t.Fatalf("monthlyQuota=%d want 5000", v1.MonthlyQuota)
	}
	if mxNew.n != 2 {
		t.Fatalf("installs_created inc=%d want 2", mxNew.n)
	}
	// The store stores only the hash, never the plaintext token.
	if store.lastParams.TokenSHA256 == v2.Token {
		t.Fatal("plaintext token passed to store")
	}
	if store.lastParams.TokenSHA256 != install.HashToken(v2.Token) {
		t.Fatal("store did not receive SHA-256(token)")
	}
}

func TestIssueGateRejectMapsDistinctCodes(t *testing.T) {
	cases := []struct {
		gate install.Gate
		code string
	}{
		{install.GateIP, "INSTALL_RATE_LIMITED"},
		{install.GateGlobal, "INSTALL_CAP_REACHED"},
		{install.GateFP, "INSTALL_FP_LIMITED"},
	}
	for _, tc := range cases {
		store := &fakeStore{issueResult: install.IssueResult{Admitted: false, Gate: tc.gate}}
		s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
		_, gate, ae := s.Issue(context.Background(), install.NewRequest("fp", ""), "ipk", false)
		if ae == nil || ae.Code != tc.code {
			t.Fatalf("gate %q: code=%v want %s", tc.gate, ae, tc.code)
		}
		if gate != tc.gate {
			t.Fatalf("returned gate=%q want %q", gate, tc.gate)
		}
	}
}

// TestIssueGatesDefaultDisabledParams: with all M2 gates at 0 (default), the params
// handed to the store have GlobalGate/FPGate disabled (dormant short-circuit; the
// store then never touches those tables).
func TestIssueGatesDefaultDisabledParams(t *testing.T) {
	store := &fakeStore{issueResult: install.IssueResult{Admitted: true}}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
	if _, _, ae := s.Issue(context.Background(), install.NewRequest("fp-aaa", ""), "ipk", false); ae != nil {
		t.Fatal(ae)
	}
	if store.lastParams.GlobalGate.Enabled {
		t.Fatal("global gate must be disabled by default")
	}
	if store.lastParams.FPGate.Enabled {
		t.Fatal("fp gate must be disabled by default")
	}
}

// TestIssueEmptyFPDisablesFPGate: even with the fp gate configured, an empty
// fingerprint disables it (never bucketed).
func TestIssueEmptyFPDisablesFPGate(t *testing.T) {
	cfg := testConfig(t)
	cfg.InstallPerFPDaily = 1
	store := &fakeStore{issueResult: install.IssueResult{Admitted: true}}
	s := New(fakeCfg{cfg}, store, fakeNonces{}, nil, nil)
	if _, _, ae := s.Issue(context.Background(), install.NewRequest("", ""), "ipk", false); ae != nil {
		t.Fatal(ae)
	}
	if store.lastParams.FPGate.Enabled {
		t.Fatal("empty fp must disable the fp gate")
	}
}

// TestIssueFPGateResolvesSentinelsForDisabledSubgates: cap-off → sentinel cap;
// cooldown-off → far-future cutoff.
func TestIssueFPGateResolvesSentinels(t *testing.T) {
	cfg := testConfig(t)
	cfg.InstallPerFPDaily = 3 // cap on, cooldown off
	store := &fakeStore{issueResult: install.IssueResult{Admitted: true}}
	s := New(fakeCfg{cfg}, store, fakeNonces{}, nil, nil)
	now := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	if _, _, ae := s.Issue(context.Background(), install.NewRequest("fp", ""), "ipk", false); ae != nil {
		t.Fatal(ae)
	}
	g := store.lastParams.FPGate
	if !g.Enabled || g.Cap != 3 {
		t.Fatalf("cap on: enabled=%v cap=%d want 3", g.Enabled, g.Cap)
	}
	if !g.Cutoff.After(now.AddDate(900, 0, 0)) {
		t.Fatalf("cooldown off → cutoff must be far future, got %v", g.Cutoff)
	}

	cfg.InstallPerFPDaily = 0
	cfg.InstallPerFPCooldownSec = 60 // cap off, cooldown on
	if _, _, ae := s.Issue(context.Background(), install.NewRequest("fp", ""), "ipk", false); ae != nil {
		t.Fatal(ae)
	}
	g = store.lastParams.FPGate
	if g.Cap != fpDailyCapDisabledSentinel {
		t.Fatalf("cap off → sentinel, got %d", g.Cap)
	}
	if !g.Cutoff.Equal(now.Add(-60 * time.Second)) {
		t.Fatalf("cooldown cutoff = %v want %v", g.Cutoff, now.Add(-60*time.Second))
	}
}

// TestIssueUnmeteredSkipsGates: with ALL Sybil gates configured, an unmetered
// (whitelisted) re-mint sets IssueParams.Unmetered and leaves every gate struct
// zero (the store then runs no gate at all) — a fresh token is still minted.
func TestIssueUnmeteredSkipsGates(t *testing.T) {
	cfg := testConfig(t)
	cfg.InstallPerIPHour = 1 // would normally bite immediately
	cfg.InstallGlobalDailyCap = 1
	cfg.InstallPerFPDaily = 1
	cfg.InstallPerFPCooldownSec = 3600
	store := &fakeStore{issueResult: install.IssueResult{Admitted: true}}
	s := New(fakeCfg{cfg}, store, fakeNonces{}, nil, nil)

	v, gate, ae := s.Issue(context.Background(), install.NewRequest("fp", "anselm"), "ipk", true)
	if ae != nil || gate != install.GateNone {
		t.Fatalf("unmetered issue: gate=%q ae=%v", gate, ae)
	}
	if v.Token == "" {
		t.Fatal("unmetered issue must still mint a fresh token")
	}
	p := store.lastParams
	if !p.Unmetered {
		t.Fatal("IssueParams.Unmetered must be true on the unmetered path")
	}
	if p.IPGate.Max != 0 || p.GlobalGate.Enabled || p.FPGate.Enabled {
		t.Fatalf("unmetered path must leave all gates zero, got %+v", p)
	}
}

// TestIsUnmetered: only a whitelisted token-hash matches; empty token / dormant
// (empty allowlist) / non-whitelisted all return false.
func TestIsUnmetered(t *testing.T) {
	const tok = "gwk_operator_device_token"
	cfg := testConfig(t)

	// Dormant (no allowlist): always false, even for any token.
	dormant := New(fakeCfg{cfg}, &fakeStore{}, fakeNonces{}, nil, nil)
	if dormant.IsUnmetered(tok) {
		t.Fatal("dormant allowlist must never report unmetered")
	}

	cfg.WhitelistTokenSHA256 = map[string]struct{}{install.HashToken(tok): {}}
	s := New(fakeCfg{cfg}, &fakeStore{}, fakeNonces{}, nil, nil)
	if !s.IsUnmetered(tok) {
		t.Fatal("whitelisted token must be unmetered")
	}
	if s.IsUnmetered("gwk_some_other_token") {
		t.Fatal("non-whitelisted token must not be unmetered")
	}
	if s.IsUnmetered("") {
		t.Fatal("empty token must not be unmetered")
	}
}

// --- LookupInstall throttle (B9) ---

func TestLookupThrottleNotWritePerCall(t *testing.T) {
	store := &fakeStore{lookupID: "ins_x", lookupStatus: install.StatusActive, lookupFound: true}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
	base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })

	// First lookup refreshes (no prior stamp).
	if _, _, found, err := s.LookupInstall(context.Background(), "gwk_tok"); err != nil || !found {
		t.Fatalf("lookup 1: found=%v err=%v", found, err)
	}
	// Many more within the interval (all < 10min from base): NO additional refresh
	// write (the in-Go throttle gates the write pool).
	s.SetClock(func() time.Time { return base.Add(5 * time.Second) })
	for i := 0; i < 50; i++ {
		if _, _, _, err := s.LookupInstall(context.Background(), "gwk_tok"); err != nil {
			t.Fatal(err)
		}
	}
	if store.calls() != 1 {
		t.Fatalf("B9: refresh writes=%d want 1 (throttled in-Go, not per call)", store.calls())
	}

	// After the interval elapses, a refresh happens again.
	s.SetClock(func() time.Time { return base.Add(LastSeenRefreshInterval + time.Minute) })
	if _, _, _, err := s.LookupInstall(context.Background(), "gwk_tok"); err != nil {
		t.Fatal(err)
	}
	if store.calls() != 2 {
		t.Fatalf("cross-interval refresh writes=%d want 2", store.calls())
	}
}

func TestLookupEmptyTokenNotFound(t *testing.T) {
	store := &fakeStore{}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
	if _, _, found, err := s.LookupInstall(context.Background(), ""); found || err != nil {
		t.Fatalf("empty token: found=%v err=%v want false,nil", found, err)
	}
	if store.calls() != 0 {
		t.Fatal("empty token must not touch the store refresh")
	}
}

func TestLookupConcurrentThrottleRaceSafe(t *testing.T) {
	store := &fakeStore{lookupID: "ins_c", lookupStatus: install.StatusActive, lookupFound: true}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
	base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = s.LookupInstall(context.Background(), "gwk_tok")
		}()
	}
	wg.Wait()
	// At most one refresh within the interval (the in-Go throttle is the gate).
	if c := store.calls(); c != 1 {
		t.Fatalf("concurrent throttle: refresh writes=%d want 1", c)
	}
}

func TestInstallsToday(t *testing.T) {
	store := &fakeStore{today: 7}
	s := New(fakeCfg{testConfig(t)}, store, fakeNonces{}, nil, nil)
	n, err := s.InstallsToday(context.Background())
	if err != nil || n != 7 {
		t.Fatalf("InstallsToday = (%d,%v) want (7,nil)", n, err)
	}
}
