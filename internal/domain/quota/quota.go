// Package quota is the PURE accounting domain: the period value type, the
// reservation aggregate threaded through the reserve→settle/rollback lifecycle,
// and the pure period math. It has ZERO I/O — no os/sql/http. The atomic write
// transactions live in infra/store/quotastore (UoW, ADR-005); the app Service
// (app/quota) maps typed repo errors to apierr sentinels and owns the reconciler
// policy. This package only owns the types + the pure math so every layer shares
// one source of truth for how a period is computed and what a reservation
// carries (GW-INV-01..10).
package quota

import (
	"errors"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// Typed denial sentinels for the four conditional-UPDATE gates in Reserve. They
// live in domain (not app) so the infra store can return them WITHOUT importing
// the app layer (clean-arch: infra → domain only). The app Service maps each to
// its apierr wire sentinel in spec order — the deny→wire mapping is app policy,
// the deny CATEGORY is a domain fact. Distinct values let errors.Is discriminate.
var (
	// ErrMonthlyExhausted — usage.count would reach MonthlyQuota (gate 1).
	ErrMonthlyExhausted = errors.New("quota: monthly count exhausted")
	// ErrSublimitExceeded — day-row count would reach DailySublimit (gate 2b).
	ErrSublimitExceeded = errors.New("quota: daily request sublimit exceeded")
	// ErrBudgetExceeded — the shared monthly pUSD wallet would be exceeded.
	ErrBudgetExceeded = errors.New("quota: global monthly spend budget exceeded")
	// ErrMonthlyResetBlocked — a manual reset of the current month's request
	// entitlement cannot proceed while any reservation is still open. The
	// maintenance operation waits for all settlements, keeping its boundary
	// simple and avoiding an overlap with an unfinished accounting mutation.
	ErrMonthlyResetBlocked = errors.New("quota: monthly reset blocked by open reservations")
	// ErrCategoryDailyExceeded — the per-install per-category daily unit cap
	// (image count today; speech characters in a later batch) would be exceeded
	// (gate 2c). The category rides the plan's InputClass; the app layer maps it
	// to the category-specific wire sentinel.
	ErrCategoryDailyExceeded = errors.New("quota: per-category daily units exhausted")
)

// Category names the per-category daily unit ledgers (install_category_daily).
// A closed set: every new member is legislated together with its own Limits
// field and wire sentinel.
//
// Category 是品类日账本(install_category_daily)的名字。封闭集:每个新成员连同自己的
// Limits 字段与 wire sentinel 一起立法。
const (
	CategoryImage = "image"
)

// Period is the entry snapshot of the month + day buckets. It is computed ONCE
// at request entry (SnapshotPeriod) and threaded UNCHANGED through
// reserve/settle/rollback; it is NEVER recomputed downstream so a midnight
// rollover under concurrency can never settle against a different day/month row
// (GW-INV-05). Month is 'YYYY-MM', Day is 'YYYY-MM-DD'.
type Period struct {
	Month string
	Day   string
}

// Reservation is the outcome of a successful Reserve, carrying everything
// settle/rollback need: the request id, the install, the entry-snapshot period,
// the reserved est, and the B1 flag.
type Reservation struct {
	RequestID    string
	InstallID    string
	Period       Period
	Plan         billing.Plan
	ReservedPUSD int64

	// SublimitApplied records whether the optional daily-sublimit +1 actually
	// fired at reserve time (B1). Rollback reverses that +1 IFF this flag is set,
	// rather than re-reading live config — so a DailySublimit hot-reload between
	// reserve and rollback can never make rollback reverse a count it never took
	// (or skip one it did). Same entry-snapshot discipline as Period.
	SublimitApplied bool

	// CategoryApplied / CategoryUnits record the per-category daily units this
	// reservation actually consumed ("" / 0 = none). Rollback reverses exactly
	// these recorded units — the SublimitApplied discipline extended to the
	// category ledger: never re-read live config to decide what to reverse.
	//
	// CategoryApplied / CategoryUnits 记录本次预留真实消耗的品类日 units(""/0=无)。
	// rollback 恰按记录反转——SublimitApplied 纪律延伸到品类账本:绝不重读 live 配置决定反转量。
	CategoryApplied string
	CategoryUnits   int64
}

// Limits is the consistency snapshot of the live runtime guardrails a single
// reserve/view needs. Snapshotting them together (not field-by-field) means a
// concurrent hot-reload can never mix an old budget with a new cap mid-operation
// (the generalized B1 fix: one config snapshot per request). It lives in domain
// so the app port and the infra store agree on the type without infra importing
// app.
type Limits struct {
	MonthlyQuota           int64
	GlobalMonthlySpendPUSD int64
	DailySublimit          int64 // 0 disables the per-install daily request sublimit.
	ImageDailyLimit        int64 // 0 disables the per-install daily image-count cap (WRK-082 P8: default 10).
}

// SnapshotPeriod computes the month/day buckets for now in loc. Pure: the caller
// (the app entry point) passes the configured Location, so this package stays
// I/O-free. The result must be reused for the whole request lifecycle.
func SnapshotPeriod(now time.Time, loc *time.Location) Period {
	t := now.In(loc)
	return Period{
		Month: t.Format("2006-01"),
		Day:   t.Format("2006-01-02"),
	}
}

// MonthResetAt returns the start of the month AFTER p.Month, in loc — the
// RFC3339 resetAt the client sees in /v1/quota. Pure period math; on a malformed
// month string it returns the zero time (the caller only ever passes a Period
// produced by SnapshotPeriod, so this is defensive).
func MonthResetAt(p Period, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01", p.Month, loc)
	if err != nil {
		return time.Time{}
	}
	return t.AddDate(0, 1, 0)
}
