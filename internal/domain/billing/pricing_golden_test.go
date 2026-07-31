package billing

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pricing golden table.
//
// Every other test in this package asserts a RULE ("a cache hit costs less", "a
// contradictory usage vector is not refunded"). This one asserts the NUMBERS —
// every reservation quote and every settlement this gateway can currently
// produce, spelled out in pUSD.
//
// It exists for the refactor that follows: six near-identical NewXxxPlan/XxxCost
// pairs are about to collapse into one table-driven path, and "the tests still
// pass" is not evidence that nobody's bill changed. A rule-based suite can stay
// green while a unit price moves. A frozen table cannot.
//
// Regenerating it is therefore a deliberate act, and reviewing that diff is how
// a pricing change gets approved:
//
//	go test ./internal/domain/billing -run TestPricingMatchesGolden -update-golden
//
// 定价 golden 表。
//
// 本包其余测试断言的是**规则**(「缓存命中更便宜」「自相矛盾的 usage 不退款」)。这一份断言的是
// **数字**——本网关当前能产生的每一个预留报价与每一次结算,以 pUSD 逐条写开。
//
// 它是为紧接着的重构而存在的:六组几乎逐字相同的 NewXxxPlan/XxxCost 即将折成一条表驱动的路,
// 而「测试还是绿的」**不是**「没有人的账单变了」的证据——一套基于规则的测试完全可以在某个单价
// 移动之后依然全绿。一张冻结的表不会。
//
// 故重新生成它是一个**刻意的动作**,而审阅那份 diff 正是定价改动获得批准的方式。
var updateGolden = flag.Bool("update-golden", false, "rewrite the pricing golden table")

const goldenPath = "testdata/pricing.golden.txt"

// unitCase is one non-token capability priced by its own unit.
type unitCase struct {
	label string
	model string
	class InputClass
	plan  func(units int64) (Plan, error)
	cost  func(p Plan, units int64) (int64, error)
	units []int64
}

func unitCases() []unitCase {
	return []unitCase{
		{
			label: "audio-seconds (realtime ASR)", model: Qwen3ASRFlashRealtime, class: InputAudioSeconds,
			plan: func(u int64) (Plan, error) {
				return NewUnitPlan(ProviderQwen, Qwen3ASRFlashRealtime, InputAudioSeconds, u)
			},
			cost: func(p Plan, u int64) (int64, error) { return p.UnitCost(p.InputClass, u) },
			// 1s, a short clip, and the session ceiling.
			units: []int64{1, 30, QwenASRInputLimit},
		},
		{
			label: "images", model: QwenImage20, class: InputImages,
			plan: func(u int64) (Plan, error) { return NewUnitPlan(ProviderQwen, QwenImage20, InputImages, u) },
			cost: func(p Plan, u int64) (int64, error) { return p.UnitCost(p.InputClass, u) },
			// The wire fixes n=1; the headroom only bounds the card.
			units: []int64{1, 2, QwenImageInputLimit},
		},
		{
			label: "characters (speech synthesis)", model: QwenAudio30TTSFlash, class: InputCharacters,
			plan:  func(u int64) (Plan, error) { return NewUnitPlan(ProviderQwen, QwenAudio30TTSFlash, InputCharacters, u) },
			cost:  func(p Plan, u int64) (int64, error) { return p.UnitCost(p.InputClass, u) },
			units: []int64{1, 1_000, QwenTTSInputLimit},
		},
		{
			label: "video-seconds", model: Wan27T2V, class: InputVideoSeconds,
			plan: func(u int64) (Plan, error) { return NewUnitPlan(ProviderQwen, Wan27T2V, InputVideoSeconds, u) },
			cost: func(p Plan, u int64) (int64, error) { return p.UnitCost(p.InputClass, u) },
			// The most expensive thing this gateway can be asked to do.
			units: []int64{1, 5, QwenVideoInputLimit},
		},
		{
			label: "voices (clone enrollment)", model: QwenTTSClone, class: InputVoices,
			plan:  func(u int64) (Plan, error) { return NewUnitPlan(ProviderQwen, QwenTTSClone, InputVoices, u) },
			cost:  func(p Plan, u int64) (int64, error) { return p.UnitCost(p.InputClass, u) },
			units: []int64{1, QwenVoiceInputLimit},
		},
	}
}

// tokenCases pin the chat path: the reservation quote at the tier boundary and
// above it, plus what an authoritative usage vector settles to.
type tokenCase struct {
	label          string
	prompt, output int64
	usage          Usage
}

func tokenCases() []tokenCase {
	return []tokenCase{
		{"small request", 100, 20,
			Usage{Present: true, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}},
		{"small request, cache hit", 100, 20,
			Usage{Present: true, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedPromptTokens: 80}},
		{"at the 256K tier boundary", 256_000, 1,
			Usage{Present: true, PromptTokens: 256_000, CompletionTokens: 100, TotalTokens: 256_100}},
		{"one token past the boundary", 256_001, 1,
			Usage{Present: true, PromptTokens: 256_001, CompletionTokens: 100, TotalTokens: 256_101}},
		{"full hard-limit reservation", Qwen37InputLimit, Qwen37OutputLimit,
			Usage{Present: true, PromptTokens: Qwen37InputLimit, CompletionTokens: 1_000, TotalTokens: Qwen37InputLimit + 1_000}},
		{"reasoning beyond visible completion", 1_000, 100,
			Usage{Present: true, PromptTokens: 1_000, CompletionTokens: 10, TotalTokens: 1_500, ReasoningTokens: 400}},
	}
}

func renderPricing(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Pricing golden table — every quote and settlement in pUSD (1 USD = 10^12 pUSD).\n")
	b.WriteString("# Regenerate deliberately: go test ./internal/domain/billing -run TestPricingMatchesGolden -update-golden\n")

	b.WriteString("\n## token path (chat)\n")
	fmt.Fprintf(&b, "model=%s\n", Qwen37Plus)
	for _, tc := range tokenCases() {
		p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, tc.prompt, tc.output)
		if err != nil {
			t.Fatalf("NewPlan(%s): %v", tc.label, err)
		}
		cost, ok, err := p.Cost(tc.usage)
		if err != nil {
			t.Fatalf("Cost(%s): %v", tc.label, err)
		}
		fmt.Fprintf(&b, "%-34s prompt=%-9d output=%-6d reserved=%-20d settled=%-20d refundable=%v\n",
			tc.label, tc.prompt, tc.output, p.ReservedPUSD, cost, ok)
	}

	b.WriteString("\n## unit paths (generation capabilities)\n")
	for _, uc := range unitCases() {
		fmt.Fprintf(&b, "\n%s  model=%s class=%d\n", uc.label, uc.model, uc.class)
		for _, u := range uc.units {
			p, err := uc.plan(u)
			if err != nil {
				t.Fatalf("plan(%s,%d): %v", uc.label, u, err)
			}
			cost, err := uc.cost(p, u)
			if err != nil {
				t.Fatalf("cost(%s,%d): %v", uc.label, u, err)
			}
			fmt.Fprintf(&b, "  units=%-8d reserved=%-20d settled=%-20d card=%s\n",
				u, p.ReservedPUSD, cost, p.RateCardID)
		}
	}
	return b.String()
}

// TestPricingMatchesGolden freezes every price this gateway can currently quote
// or settle. A refactor that preserves behavior leaves this file untouched.
func TestPricingMatchesGolden(t *testing.T) {
	got := renderPricing(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("pricing golden rewritten (%d bytes)", len(got))
		return
	}

	want, err := os.ReadFile(goldenPath) // #nosec G304 — fixed repo-relative path
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	if got != string(want) {
		t.Errorf("a price changed.\n"+
			"If that is intended, re-approve it explicitly:\n"+
			"  go test ./internal/domain/billing -run TestPricingMatchesGolden -update-golden\n\n"+
			"--- golden ---\n%s\n--- live ---\n%s", want, got)
	}
}

// TestEveryPricedCardAppearsInTheGolden closes the loop the golden file cannot:
// the table only freezes what it was told to render, so a NEW rate card could be
// added and priced with nothing pinning it. This walks the compiled cards and
// requires each one to be covered by a case above.
//
// 这条闭上 golden 文件自己闭不上的环:那张表只冻结「被要求渲染的东西」,故新增一张费率卡完全可能
// 上线而没有任何东西钉住它。这里遍历**已编译的全部卡**,要求每一张都被上面的用例覆盖到。
func TestEveryPricedCardAppearsInTheGolden(t *testing.T) {
	covered := map[string]bool{Qwen37Plus: true}
	for _, uc := range unitCases() {
		covered[uc.model] = true
	}
	for provider, models := range rateCards {
		for model := range models {
			if !covered[model] {
				t.Errorf("rate card %s/%s is priced but not pinned by the golden table", provider, model)
			}
		}
	}
}
