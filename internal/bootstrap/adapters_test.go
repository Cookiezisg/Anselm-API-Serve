package bootstrap

import (
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
)

// This guard exists because of a bug it would have caught.
//
// SPEECH_DAILY_LIMIT was read from env, bounded, advertised in /v1/models, given
// its own quota.Category, its own install_category_daily ledger, its own
// categoryCap branch and its own wire sentinel — and then NOT copied into the
// Limits snapshot here. Every layer was individually correct; the wire between
// two of them was simply absent, so the store read a zero cap and the gate never
// fired once. No test was red, because no test asserted the whole chain.
//
// The lesson is what this test asserts: a per-category cap is only real if it
// SURVIVES the adapter. A test that checks the config parses, or that the store
// enforces a cap it was handed, cannot see the missing line between them.
//
// 这条守卫的存在,是因为它本可以抓住的一个 bug。
//
// SPEECH_DAILY_LIMIT 从 env 读出、设了界、在 /v1/models 里对外宣告、有自己的 quota.Category、
// 自己的 install_category_daily 账本、自己的 categoryCap 分支、自己的 wire sentinel——然后**没有**
// 被抄进这里的 Limits 快照。每一层各自都对;只是其中两层之间的那根线**根本不存在**,于是 store 读到
// 的上限恒为 0,那道闸**一次都没生效过**。没有测试是红的,因为没有测试断言过**整条链**。
//
// 教训正是本测试所断言的:一个品类上限只有**活着穿过适配器**才是真的。「配置解析得对」或者
// 「store 会执行别人交给它的上限」这两种测试,都看不见它们中间缺的那一行。
func TestQuotaLimitsCarryEveryCategoryCap(t *testing.T) {
	t.Parallel()
	// Distinct values so a copy-paste that maps the wrong field is as visible as
	// a missing one. 取互不相同的值,好让「抄错字段」与「漏抄」一样刺眼。
	base := config.Config{
		MonthlyQuota:           1,
		GlobalMonthlySpendPUSD: 2,
		DailySublimit:          3,
		ImageDailyLimit:        11,
		SpeechDailyLimit:       22,
		VideoDailyLimit:        33,
	}
	got := quotaCfgSource{p: configprovider.New(base)}.Limits()

	if got.MonthlyQuota != 1 || got.GlobalMonthlySpendPUSD != 2 || got.DailySublimit != 3 {
		t.Fatalf("wallet limits lost in transit: %+v", got)
	}
	for _, tc := range []struct {
		category string
		want     int64
	}{
		{domquota.CategoryImage, 11},
		{domquota.CategorySpeech, 22},
		{domquota.CategoryVideo, 33},
	} {
		if cap := capFor(got, tc.category); cap != tc.want {
			t.Fatalf("category %q cap = %d, want %d — the adapter dropped it", tc.category, cap, tc.want)
		}
	}
}

// capFor mirrors the store's categoryCap switch. It is duplicated deliberately:
// calling the store's own (unexported) function would let a category that BOTH
// sides forgot pass silently, which is exactly the failure mode being guarded.
//
// capFor 镜像 store 的 categoryCap switch。刻意重复一遍:调用 store 自己那个(未导出的)函数,会让
// 一个**两边都忘了**的品类静默通过,而那恰恰是本守卫要防的失败模式。
func capFor(l domquota.Limits, category string) int64 {
	switch category {
	case domquota.CategoryImage:
		return l.ImageDailyLimit
	case domquota.CategorySpeech:
		return l.SpeechDailyLimit
	case domquota.CategoryVideo:
		return l.VideoDailyLimit
	default:
		return 0
	}
}
