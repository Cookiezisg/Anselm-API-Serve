package quota

import (
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

func TestSnapshotPeriodFormatsInLocation(t *testing.T) {
	// 2026-01-31 23:30 UTC is 2026-02-01 08:30 in Asia/Tokyo (+09): the snapshot
	// must reflect the configured tz, not UTC — month AND day both roll.
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Date(2026, 1, 31, 23, 30, 0, 0, time.UTC)
	p := SnapshotPeriod(now, loc)
	if p.Month != "2026-02" {
		t.Errorf("Month = %q, want 2026-02", p.Month)
	}
	if p.Day != "2026-02-01" {
		t.Errorf("Day = %q, want 2026-02-01", p.Day)
	}
}

func TestSnapshotPeriodUTC(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	p := SnapshotPeriod(now, time.UTC)
	if p.Month != "2026-06" || p.Day != "2026-06-20" {
		t.Errorf("got %+v, want {2026-06 2026-06-20}", p)
	}
}

func TestMonthResetAtNextMonth(t *testing.T) {
	loc := time.UTC
	p := Period{Month: "2026-12", Day: "2026-12-31"}
	got := MonthResetAt(p, loc)
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("MonthResetAt = %s, want %s", got, want)
	}
}

func TestMonthResetAtMalformed(t *testing.T) {
	if got := MonthResetAt(Period{Month: "garbage"}, time.UTC); !got.IsZero() {
		t.Errorf("malformed month should yield zero time, got %s", got)
	}
}

// The same snapshot reused across a simulated midnight crossing must keep
// pointing at the entry day, not a recomputed one (GW-INV-05 at the type level:
// Period is a plain value, so reuse is structurally guaranteed once captured).
func TestPeriodIsAPlainValueReusable(t *testing.T) {
	entry := SnapshotPeriod(time.Date(2026, 6, 20, 23, 59, 59, 0, time.UTC), time.UTC)
	// Time advances past midnight; entry must be unchanged (value semantics).
	_ = SnapshotPeriod(time.Date(2026, 6, 21, 0, 0, 1, 0, time.UTC), time.UTC)
	if entry.Day != "2026-06-20" {
		t.Errorf("entry period mutated to %q", entry.Day)
	}
}

func TestProviderDailyLimitFailsClosed(t *testing.T) {
	lim := Limits{ProviderDailySpendPUSD: map[billing.Provider]int64{
		billing.ProviderDeepSeek: 123,
		billing.ProviderKimi:   0,
	}}
	if got, ok := lim.ProviderDailyLimit(billing.ProviderDeepSeek); !ok || got != 123 {
		t.Fatalf("DeepSeek limit=(%d,%v), want (123,true)", got, ok)
	}
	if _, ok := lim.ProviderDailyLimit(billing.ProviderKimi); ok {
		t.Fatal("zero provider cap must fail closed")
	}
	if _, ok := lim.ProviderDailyLimit(billing.Provider("unknown")); ok {
		t.Fatal("missing provider cap must fail closed")
	}
}
