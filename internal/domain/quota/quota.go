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
	// (image count, speech characters) would be exceeded (gate 2c). It is the
	// UMBRELLA sentinel: the store always returns a *CategoryDailyExceededError
	// naming the category, and that value reports Is(ErrCategoryDailyExceeded)
	// true — so callers who only care "some category is out" keep working, while
	// the app layer reads the category to pick the wire sentinel.
	//
	// ErrCategoryDailyExceeded 是**伞** sentinel:store 恒返带品类名的
	// *CategoryDailyExceededError,而该值 Is(ErrCategoryDailyExceeded) 为真——只关心
	// 「某个品类满了」的调用方照常工作,app 层则读品类挑 wire sentinel。
	ErrCategoryDailyExceeded = errors.New("quota: per-category daily units exhausted")
)

// CategoryDailyExceededError is the gate-2c denial WITH its category. A denial
// that cannot say which ledger it came from forces every consumer to guess, and
// that guess was already wired as a constant once (every category rendering as
// IMAGE_QUOTA_EXHAUSTED). Carrying the parameter on the error is the Go answer;
// a second sentinel per category would declare the closed set twice.
//
// CategoryDailyExceededError 是**带品类**的 gate-2c 拒绝。一个说不出自己来自哪个账本的拒绝
// 会逼每个消费方去猜,而这个猜测已经被写死成常量过一次(所有品类都渲成 IMAGE_QUOTA_EXHAUSTED)。
// 把参数挂在错误上是 Go 的答案;每品类再加一个 sentinel 等于把封闭集声明两遍。
type CategoryDailyExceededError struct{ Category string }

func (e *CategoryDailyExceededError) Error() string {
	return "quota: per-category daily units exhausted: " + e.Category
}

// Is makes every category-specific denial satisfy errors.Is(err, ErrCategoryDailyExceeded).
//
// Is 让每个具体品类的拒绝都满足 errors.Is(err, ErrCategoryDailyExceeded)。
func (e *CategoryDailyExceededError) Is(target error) bool {
	return target == ErrCategoryDailyExceeded
}

// Category names the per-category daily unit ledgers (install_category_daily).
// A closed set: every new member is legislated together with its own Limits
// field and its app-layer wire mapping.
//
// Category 是品类日账本(install_category_daily)的名字。封闭集:每个新成员连同自己的
// Limits 字段与 app 层 wire 映射一起立法。
const (
	CategoryImage  = "image"
	CategorySpeech = "speech"
	CategoryVideo  = "video"
	// CategoryVoice rations voice ENROLLMENTS per day. It is the one category whose purchase
	// persists, and therefore the one where the daily gate does work no other ceiling does: the
	// inventory cap bounds how many voices a person HOLDS, and deleting one frees a slot, so
	// enroll→delete→enroll would spend without limit. This gate is what bounds the cumulative cost.
	// CategoryVoice 配给每天的音色**登记**次数。它是唯一一个购买会长存的品类,故也是那个日闸干着别的
	// 上限干不了的活的品类:库存上限界的是一个人**持有**几个,而删掉一个就腾出位置,于是
	// enroll→delete→enroll 可以无界花钱。**是这道闸界住了累计成本。**
	CategoryVoice = "voice"
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
	SpeechDailyLimit       int64 // 0 disables the per-install daily speech-character cap (WRK-082 P8: default 50000).
	// VideoDailyLimit counts CLIPS, not seconds — the user's cap is 一人一天 10 条 (WRK-082 H1).
	// Billing quotes video by the second, but the thing a person rations is whole videos, so the
	// category ledger and the money ledger deliberately count different units here.
	// VideoDailyLimit 数的是**条**、不是秒——用户定的额度是「一人一天 10 条」(H1)。计费按秒报价,
	// 但人心里配给的是**整条片子**,故品类账本与钱账本在此刻意数不同的单位。
	VideoDailyLimit int64 // 0 disables the per-install daily video-clip cap (default 10).
	// VoiceDailyLimit counts ENROLLMENTS per day. Default 2 = the inventory size, so a person can
	// fill an empty inventory in one day and a delete→re-enroll cycle costs them a day rather than
	// $0.2 per round. Unlike the other three this cap is not about fairness of a renewable
	// allowance; it is the only thing standing between a free-tier install and unbounded spend.
	// VoiceDailyLimit 数的是每天的**登记**次数。默认 2 = 库存大小,故一个人能在一天里填满空库存,而
	// delete→重登记 的循环代价是**一天**、不是每圈 $0.2。与另外三条不同,这条上限不是关于「可再生额度
	// 的公平」;它是免费档 install 与无界花费之间唯一站着的东西。
	VoiceDailyLimit int64 // 0 disables the per-install daily voice-enrollment cap (default 2).
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
