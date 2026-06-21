package installstore

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	appinstall "github.com/sunweilin/anselm/gateway/internal/app/install"
	dinstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
)

func newStore(t *testing.T) (*Store, *sqlite.DB) {
	t.Helper()
	cfg := sqlite.DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "install.db")
	db, err := sqlite.Open(cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db.Writer, db.Reader), db
}

// baseParams builds an admit-everything IssueParams (all M2 gates disabled, IP
// gate high) for the given fp + token + day/hour window.
func baseParams(id, tokenHash, fp string, now time.Time) dinstall.IssueParams {
	return dinstall.IssueParams{
		InstallID:   id,
		TokenSHA256: tokenHash,
		Fingerprint: fp,
		Now:         now,
		IPGate: dinstall.IPGate{
			Key:        "ipk-" + id,
			WindowHour: now.UTC().Format("2006-01-02T15"),
			Max:        1_000_000,
		},
	}
}

func TestIssueFreshTokenAndRow(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	p := baseParams("ins_a", dinstall.HashToken("gwk_one"), "fp-aaa", now)
	res, err := st.Issue(ctx, p)
	if err != nil || !res.Admitted {
		t.Fatalf("issue 1: admitted=%v err=%v", res.Admitted, err)
	}
	p2 := baseParams("ins_b", dinstall.HashToken("gwk_two"), "fp-aaa", now)
	p2.IPGate.Key = "ipk-other"
	res2, err := st.Issue(ctx, p2)
	if err != nil || !res2.Admitted {
		t.Fatalf("issue 2: admitted=%v err=%v", res2.Admitted, err)
	}

	var n int
	if err := db.Reader.QueryRow(ctx, `SELECT COUNT(*) FROM installs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("installs rows=%d want 2", n)
	}
}

func TestIssueStoresTokenAndFPAsHash(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const tok = "gwk_secret_plain"
	const fp = "fingerprint-plaintext-xyz"

	p := baseParams("ins_h", dinstall.HashToken(tok), fp, now)
	// Force the fp into install_fp_rate by enabling the fp gate.
	p.FPGate = dinstall.FPGate{
		Enabled:   true,
		FPSHA256:  dinstall.HashFingerprint(fp),
		WindowDay: now.Format("2006-01-02"),
		Cap:       100,
		Cutoff:    now.AddDate(1000, 0, 0),
	}
	if _, err := st.Issue(ctx, p); err != nil {
		t.Fatal(err)
	}

	var storedTok string
	if err := db.Reader.QueryRow(ctx, `SELECT token_sha256 FROM installs LIMIT 1`).Scan(&storedTok); err != nil {
		t.Fatal(err)
	}
	if storedTok == tok {
		t.Fatal("plaintext token stored")
	}
	if storedTok != dinstall.HashToken(tok) {
		t.Fatal("stored token != SHA-256")
	}

	var storedFP string
	if err := db.Reader.QueryRow(ctx, `SELECT fp_sha256 FROM install_fp_rate LIMIT 1`).Scan(&storedFP); err != nil {
		t.Fatal(err)
	}
	if storedFP == fp {
		t.Fatal("plaintext fingerprint stored in install_fp_rate")
	}
	if storedFP != dinstall.HashFingerprint(fp) {
		t.Fatal("stored fp != SHA-256")
	}
}

// TestGlobalCapConcurrentNoOversell: N+1 concurrent issuances with cap N → exactly
// N admitted, no oversell under -race.
func TestGlobalCapConcurrentNoOversell(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const cap = 5
	const total = cap + 1

	var wg sync.WaitGroup
	results := make([]dinstall.IssueResult, total)
	errs := make([]error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := baseParams("ins_g"+itoa(i), dinstall.HashToken("gwk_g"+itoa(i)), "", now)
			p.IPGate.Key = "ipk-g" + itoa(i)
			p.GlobalGate = dinstall.GlobalGate{Enabled: true, WindowDay: now.Format("2006-01-02"), Cap: cap}
			results[i], errs[i] = st.Issue(ctx, p)
		}(i)
	}
	wg.Wait()

	admitted := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("err[%d]=%v", i, errs[i])
		}
		if results[i].Admitted {
			admitted++
		} else if results[i].Gate != dinstall.GateGlobal {
			t.Fatalf("reject gate=%q want global", results[i].Gate)
		}
	}
	if admitted != cap {
		t.Fatalf("global oversell: admitted=%d want %d", admitted, cap)
	}
}

// TestB11NoCounterConsumedOnLaterGateReject: when the fp gate rejects, the per-IP
// and global counters must NOT be left bumped (atomic admit, counters = issuances).
func TestB11NoCounterConsumedOnLaterGateReject(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	const fp = "fpReject"
	fpHash := dinstall.HashFingerprint(fp)

	// First issuance fills the fp daily cap (cap=1).
	p1 := baseParams("ins_r1", dinstall.HashToken("gwk_r1"), fp, now)
	p1.IPGate.Key = "ipk-shared"
	p1.GlobalGate = dinstall.GlobalGate{Enabled: true, WindowDay: day, Cap: 100}
	p1.FPGate = dinstall.FPGate{Enabled: true, FPSHA256: fpHash, WindowDay: day, Cap: 1, Cutoff: now.AddDate(1000, 0, 0)}
	if r, err := st.Issue(ctx, p1); err != nil || !r.Admitted {
		t.Fatalf("issue 1: %v %v", r.Admitted, err)
	}

	// Read counters after the successful issuance.
	ipBefore := readCount(t, ctx, db, `SELECT count FROM install_ip_rate WHERE ip_key='ipk-shared'`)
	globalBefore := readCount(t, ctx, db, `SELECT count FROM install_global_rate WHERE window_day=?`, day)

	// Second issuance with same fp/IP/global trips the fp gate → reject.
	p2 := baseParams("ins_r2", dinstall.HashToken("gwk_r2"), fp, now)
	p2.IPGate.Key = "ipk-shared"
	p2.GlobalGate = dinstall.GlobalGate{Enabled: true, WindowDay: day, Cap: 100}
	p2.FPGate = dinstall.FPGate{Enabled: true, FPSHA256: fpHash, WindowDay: day, Cap: 1, Cutoff: now.AddDate(1000, 0, 0)}
	r, err := st.Issue(ctx, p2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Admitted || r.Gate != dinstall.GateFP {
		t.Fatalf("want fp reject, got admitted=%v gate=%q", r.Admitted, r.Gate)
	}

	ipAfter := readCount(t, ctx, db, `SELECT count FROM install_ip_rate WHERE ip_key='ipk-shared'`)
	globalAfter := readCount(t, ctx, db, `SELECT count FROM install_global_rate WHERE window_day=?`, day)
	if ipAfter != ipBefore {
		t.Fatalf("B11: per-IP counter consumed on fp reject: before=%d after=%d", ipBefore, ipAfter)
	}
	if globalAfter != globalBefore {
		t.Fatalf("B11: global counter consumed on fp reject: before=%d after=%d", globalBefore, globalAfter)
	}

	// And exactly one installs row exists (the reject wrote nothing).
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM installs`); n != 1 {
		t.Fatalf("installs rows=%d want 1 (reject must not insert)", n)
	}
}

// TestPerFPCooldownConcurrentNoDoublePass: two concurrent same-fp issuances under
// a cooldown → exactly one admitted (single conditional UPSERT, no RMW).
func TestPerFPCooldownConcurrentNoDoublePass(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	fpHash := dinstall.HashFingerprint("dave")
	cutoff := now.Add(-3600 * time.Second)

	const total = 8
	var wg sync.WaitGroup
	results := make([]dinstall.IssueResult, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := baseParams("ins_d"+itoa(i), dinstall.HashToken("gwk_d"+itoa(i)), "dave", now)
			p.IPGate.Key = "ipk-d" + itoa(i)
			p.FPGate = dinstall.FPGate{Enabled: true, FPSHA256: fpHash, WindowDay: day, Cap: fpSentinel(), Cutoff: cutoff}
			results[i], _ = st.Issue(ctx, p)
		}(i)
	}
	wg.Wait()
	admitted := 0
	for _, r := range results {
		if r.Admitted {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("cooldown double-pass: admitted=%d want 1", admitted)
	}
}

// TestEmptyFPNeverWritesRow is covered by the app layer (it decides Enabled
// false), but the store must also write no fp row when the fp gate is disabled.
func TestDisabledFPGateNoRow(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		p := baseParams("ins_e"+itoa(i), dinstall.HashToken("gwk_e"+itoa(i)), "", now)
		p.IPGate.Key = "ipk-e" + itoa(i)
		if r, err := st.Issue(ctx, p); err != nil || !r.Admitted {
			t.Fatalf("issue %d: %v %v", i, r.Admitted, err)
		}
	}
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM install_fp_rate`); n != 0 {
		t.Fatalf("install_fp_rate has %d rows; disabled gate must write none", n)
	}
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM install_global_rate`); n != 0 {
		t.Fatalf("install_global_rate has %d rows; disabled gate must write none", n)
	}
}

// TestB8RateBucketPrune: a stale per-IP window row is pruned inside the bump tx of
// the next window; only the current window survives.
func TestB8RateBucketPrune(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Hour) // different window_hour, same ip_key.

	p1 := baseParams("ins_p1", dinstall.HashToken("gwk_p1"), "", t1)
	p1.IPGate.Key = "ipk-prune"
	if r, err := st.Issue(ctx, p1); err != nil || !r.Admitted {
		t.Fatalf("issue 1: %v %v", r.Admitted, err)
	}
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM install_ip_rate WHERE ip_key='ipk-prune'`); n != 1 {
		t.Fatalf("after issue 1 ip_rate rows=%d want 1", n)
	}

	p2 := baseParams("ins_p2", dinstall.HashToken("gwk_p2"), "", t2)
	p2.IPGate.Key = "ipk-prune"
	if r, err := st.Issue(ctx, p2); err != nil || !r.Admitted {
		t.Fatalf("issue 2: %v %v", r.Admitted, err)
	}
	// The stale t1 window must have been pruned; only the current window remains.
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM install_ip_rate WHERE ip_key='ipk-prune'`); n != 1 {
		t.Fatalf("B8: after window roll ip_rate rows=%d want 1 (stale pruned)", n)
	}
	var win string
	if err := db.Reader.QueryRow(ctx, `SELECT window_hour FROM install_ip_rate WHERE ip_key='ipk-prune'`).Scan(&win); err != nil {
		t.Fatal(err)
	}
	if win != t2.Format("2006-01-02T15") {
		t.Fatalf("surviving window=%q want current %q", win, t2.Format("2006-01-02T15"))
	}
}

// TestB13IDRegenerateOnConflict: a pre-seeded install id collision is recovered by
// the single regenerate-on-conflict retry — admitted, not a hard error.
func TestB13IDRegenerateOnConflict(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Pre-seed a row with a known id so the next Issue with that same id collides.
	const dupID = "ins_dupe000000000000000000000000"
	if _, err := db.Writer.Exec(ctx,
		`INSERT INTO installs(id, token_sha256, status, created_at, last_seen_at) VALUES (?,?,?,?,?)`,
		dupID, dinstall.HashToken("gwk_seed"), "active", now, now); err != nil {
		t.Fatal(err)
	}

	p := baseParams(dupID, dinstall.HashToken("gwk_new"), "", now)
	p.IPGate.Key = "ipk-dupe"
	r, err := st.Issue(ctx, p)
	if err != nil {
		t.Fatalf("B13: id collision must be recovered, got err=%v", err)
	}
	if !r.Admitted {
		t.Fatal("B13: should admit after regenerating id")
	}
	// Two rows now (the seed + the regenerated one), both with distinct ids.
	if n := readCount(t, ctx, db, `SELECT COUNT(*) FROM installs`); n != 2 {
		t.Fatalf("installs rows=%d want 2", n)
	}
}

func TestLookupAndRefreshThrottle(t *testing.T) {
	st, db := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	p := baseParams("ins_lk", dinstall.HashToken("gwk_lk"), "", base)
	p.IPGate.Key = "ipk-lk"
	if _, err := st.Issue(ctx, p); err != nil {
		t.Fatal(err)
	}

	id, status, found, err := st.Lookup(ctx, dinstall.HashToken("gwk_lk"))
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if id != "ins_lk" || status != dinstall.StatusActive {
		t.Fatalf("lookup bad: id=%q status=%q", id, status)
	}

	// Pin last_seen far in the past, then a within-interval refresh is a no-op and a
	// cross-interval refresh updates (DB-side WHERE backstop).
	if _, err := db.Writer.Exec(ctx, `UPDATE installs SET last_seen_at=? WHERE id=?`, base, "ins_lk"); err != nil {
		t.Fatal(err)
	}
	within := base.Add(appinstall.LastSeenRefreshInterval - time.Minute)
	if err := st.RefreshLastSeen(ctx, "ins_lk", within, appinstall.LastSeenRefreshInterval); err != nil {
		t.Fatal(err)
	}
	if got := readSeen(t, ctx, db, "ins_lk"); !got.Equal(base) {
		t.Fatalf("within-interval refresh wrote: got=%v base=%v", got, base)
	}
	after := base.Add(appinstall.LastSeenRefreshInterval + time.Minute)
	if err := st.RefreshLastSeen(ctx, "ins_lk", after, appinstall.LastSeenRefreshInterval); err != nil {
		t.Fatal(err)
	}
	if got := readSeen(t, ctx, db, "ins_lk"); !got.Equal(after) {
		t.Fatalf("cross-interval refresh: got=%v want=%v", got, after)
	}

	// Missing token → not found.
	if _, _, found, _ := st.Lookup(ctx, dinstall.HashToken("nope")); found {
		t.Fatal("lookup of unknown token reported found")
	}
}

func TestInstallsToday(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := now.Format("2006-01-02")

	if n, err := st.InstallsToday(ctx, day); err != nil || n != 0 {
		t.Fatalf("before any issuance = (%d,%v) want (0,nil)", n, err)
	}
	for i := 0; i < 2; i++ {
		p := baseParams("ins_t"+itoa(i), dinstall.HashToken("gwk_t"+itoa(i)), "", now)
		p.IPGate.Key = "ipk-t" + itoa(i)
		p.GlobalGate = dinstall.GlobalGate{Enabled: true, WindowDay: day, Cap: 100}
		if _, err := st.Issue(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.InstallsToday(ctx, day); err != nil || n != 2 {
		t.Fatalf("after 2 issuances = (%d,%v) want (2,nil)", n, err)
	}
}

// --- helpers ---

func readCount(t *testing.T, ctx context.Context, db *sqlite.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Reader.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("readCount %q: %v", q, err)
	}
	return n
}

func readSeen(t *testing.T, ctx context.Context, db *sqlite.DB, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := db.Reader.QueryRow(ctx, `SELECT last_seen_at FROM installs WHERE id=?`, id).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts.UTC()
}

func fpSentinel() int64 { return 1 << 62 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
