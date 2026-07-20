// Package billing owns the provider-aware cost model used by quota accounting.
// Raw token counts are never added across providers. They are converted through
// an immutable, exact-model rate card into pico-US dollars (pUSD), then only the
// resulting integer cost enters the shared install/global wallet.
//
// pUSD is intentionally finer than nano-USD: DeepSeek's cache-hit price is
// $0.0028 / 1M tokens = 2,800 pUSD/token, which is exact in this unit. All
// arithmetic is checked and rounds toward higher spend when division is needed.
package billing

import (
	"errors"
	"math"
)

// Provider is a closed, stable identity used in ledgers and low-cardinality
// metrics. It is never supplied by a client.
type Provider string

const (
	ProviderDeepSeek Provider = "deepseek"
	ProviderGemini   Provider = "gemini"
)

const (
	DeepSeekV4Flash       = "deepseek-v4-flash"
	Gemini31FlashLite     = "gemini-3.1-flash-lite"
	PicoUSDPerMicroUSD    = int64(1_000_000)
	PicoUSDPerUSD         = int64(1_000_000_000_000)
	DeepSeekInputLimit    = int64(1_000_000)
	DeepSeekOutputLimit   = int64(384_000)
	GeminiInputLimit      = int64(1_048_576)
	GeminiOutputLimit     = int64(65_536)
	legacyMaxPUSDPerToken = int64(280_000)
)

var (
	ErrUnknownRateCard = errors.New("billing: no rate card for provider/model")
	ErrCostOverflow    = errors.New("billing: cost arithmetic overflow")
)

// LegacyMaxPUSDPerToken is the conservative conversion factor used by the v1
// token-ledger migration. It is the highest historical DeepSeek V4 Flash token
// dimension (output), so migrated balances can only overstate old spend.
func LegacyMaxPUSDPerToken() int64 { return legacyMaxPUSDPerToken }

// InputClass captures the only request fact that changes the Gemini input rate.
// Image/video share the standard rate; audio has its own higher rate.
type InputClass uint8

const (
	InputStandard InputClass = iota
	InputAudio
)

// Usage is a provider-neutral token vector extracted from an upstream usage
// snapshot. Fields absent from a compatibility response remain zero. Present is
// distinct from an all-zero usage object and lets callers conservatively retain
// the reservation when usage is missing or malformed.
type Usage struct {
	Present               bool
	Malformed             bool
	PromptTokens          int64
	CompletionTokens      int64
	TotalTokens           int64
	PromptCacheHitTokens  int64
	PromptCacheMissTokens int64
	CachedPromptTokens    int64
	ReasoningTokens       int64
}

// MergeSnapshot keeps the greatest cumulative value seen for each field. Both
// providers expose usage as cumulative snapshots; summing streaming frames would
// double-charge when more than one frame carries usage. Malformed is sticky:
// clamping a negative snapshot and later refunding from it would erase the very
// evidence that requires conservative full-quote settlement.
func (u Usage) MergeSnapshot(v Usage) Usage {
	u.Present = u.Present || v.Present
	u.Malformed = u.Malformed || v.Malformed || usageHasNegative(v)
	u.PromptTokens = max(u.PromptTokens, nonNegative(v.PromptTokens))
	u.CompletionTokens = max(u.CompletionTokens, nonNegative(v.CompletionTokens))
	u.TotalTokens = max(u.TotalTokens, nonNegative(v.TotalTokens))
	u.PromptCacheHitTokens = max(u.PromptCacheHitTokens, nonNegative(v.PromptCacheHitTokens))
	u.PromptCacheMissTokens = max(u.PromptCacheMissTokens, nonNegative(v.PromptCacheMissTokens))
	u.CachedPromptTokens = max(u.CachedPromptTokens, nonNegative(v.CachedPromptTokens))
	u.ReasoningTokens = max(u.ReasoningTokens, nonNegative(v.ReasoningTokens))
	return u
}

// RateCard is an immutable pricing snapshot. Prices are exact pUSD/token.
type RateCard struct {
	ID                string
	Provider          Provider
	Model             string
	InputLimit        int64
	OutputLimit       int64
	InputPUSD         int64
	AudioInputPUSD    int64
	CacheHitInputPUSD int64
	OutputPUSD        int64
}

var rateCards = map[Provider]map[string]RateCard{
	ProviderDeepSeek: {
		DeepSeekV4Flash: {
			ID: "deepseek-v4-flash-2026-07-20", Provider: ProviderDeepSeek, Model: DeepSeekV4Flash,
			InputLimit: DeepSeekInputLimit, OutputLimit: DeepSeekOutputLimit,
			// Official standard pricing per 1M tokens: hit $0.0028,
			// cache miss $0.14, output $0.28.
			InputPUSD: 140_000, CacheHitInputPUSD: 2_800, OutputPUSD: 280_000,
		},
	},
	ProviderGemini: {
		Gemini31FlashLite: {
			ID: "gemini-3.1-flash-lite-2026-05-07", Provider: ProviderGemini, Model: Gemini31FlashLite,
			InputLimit: GeminiInputLimit, OutputLimit: GeminiOutputLimit,
			// Official standard pricing per 1M tokens: text/image/video
			// input $0.25, audio input $0.50, output incl. thinking $1.50.
			InputPUSD: 250_000, AudioInputPUSD: 500_000,
			CacheHitInputPUSD: 25_000, OutputPUSD: 1_500_000,
		},
	},
}

// Lookup returns a copy of an exact provider/model rate card. Unknown models
// fail closed so a model hot-swap can never run with stale prices.
func Lookup(provider Provider, model string) (RateCard, error) {
	models, ok := rateCards[provider]
	if !ok {
		return RateCard{}, ErrUnknownRateCard
	}
	r, ok := models[model]
	if !ok {
		return RateCard{}, ErrUnknownRateCard
	}
	return r, nil
}

// Plan is snapshotted once before Reserve and then carried unchanged through
// settle/rollback. ReservedPUSD is a provable upper quote for the supplied token
// bounds under this frozen card.
type Plan struct {
	Provider     Provider
	Model        string
	RateCardID   string
	InputClass   InputClass
	PromptQuote  int64
	OutputQuote  int64
	ReservedPUSD int64
	card         RateCard
}

// Validate proves that a Plan is exactly one produced by NewPlan. This lets a
// persistence boundary reject a zero-value or manually forged rate-card/amount
// combination even though Plan is a transportable domain value.
func (p Plan) Validate() error {
	want, err := NewPlan(p.Provider, p.Model, p.InputClass, p.PromptQuote, p.OutputQuote)
	if err != nil {
		return err
	}
	if p != want {
		return ErrUnknownRateCard
	}
	return nil
}

// NewPlan prices a conservative input/output token bound. The caller chooses
// the bounds from the canonical request/model contract; this function clamps no
// value silently and rejects anything beyond the exact model limits.
func NewPlan(provider Provider, model string, inputClass InputClass, promptBound, outputBound int64) (Plan, error) {
	card, err := Lookup(provider, model)
	if err != nil {
		return Plan{}, err
	}
	switch inputClass {
	case InputStandard:
	case InputAudio:
		// Audio is a Gemini request class. Accepting it on another provider would
		// make a forged/misrouted plan appear self-validating under an unrelated
		// rate card.
		if provider != ProviderGemini {
			return Plan{}, ErrUnknownRateCard
		}
	default:
		return Plan{}, ErrUnknownRateCard
	}
	if promptBound < 0 || outputBound < 0 || promptBound > card.InputLimit || outputBound > card.OutputLimit {
		return Plan{}, ErrUnknownRateCard
	}
	inputRate := card.InputPUSD
	if inputClass == InputAudio && card.AudioInputPUSD > inputRate {
		inputRate = card.AudioInputPUSD
	}
	inCost, err := checkedMul(promptBound, inputRate)
	if err != nil {
		return Plan{}, err
	}
	outCost, err := checkedMul(outputBound, card.OutputPUSD)
	if err != nil {
		return Plan{}, err
	}
	total, err := checkedAdd(inCost, outCost)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Provider: provider, Model: model, RateCardID: card.ID,
		InputClass: inputClass, PromptQuote: promptBound, OutputQuote: outputBound,
		ReservedPUSD: total, card: card,
	}, nil
}

// Cost converts an authoritative cumulative usage snapshot under the frozen
// card. ok=false means the caller must keep the full reservation. Compatibility
// responses that omit cache details are conservatively priced as cache misses.
func (p Plan) Cost(u Usage) (cost int64, ok bool, err error) {
	// A syntactically present but empty usage object is not authoritative. Every
	// accepted completion has a non-empty prompt; refunding it as a zero-cost call
	// would turn a malformed compatibility response into systematic underbilling.
	if !u.Present || u.Malformed || usageHasNegative(u) || u.PromptTokens <= 0 {
		return 0, false, nil
	}
	prompt := nonNegative(u.PromptTokens)
	completion := nonNegative(u.CompletionTokens)
	// A refundable usage vector must be internally self-consistent. In
	// particular, total_tokens is the only compatibility field that bounds
	// Gemini's hidden thinking output; a visible completion count alone cannot
	// prove the provider's bill. Treat absence, overflow, or a total smaller than
	// prompt+completion as malformed evidence and retain the full quote.
	if u.TotalTokens <= 0 {
		return 0, false, nil
	}
	visibleTotal, addErr := checkedAdd(prompt, completion)
	if addErr != nil || u.TotalTokens < visibleTotal {
		return 0, false, nil
	}
	if p.Provider == ProviderDeepSeek && u.TotalTokens != visibleTotal {
		// DeepSeek formally defines total=prompt+completion. A contradictory
		// vector is malformed, so retaining the full quote is safer than refunding.
		return 0, false, nil
	}
	if u.CachedPromptTokens > prompt || u.PromptCacheHitTokens > prompt || u.PromptCacheMissTokens > prompt {
		return 0, false, nil
	}
	derivedOutput := u.TotalTokens - prompt
	if u.ReasoningTokens > derivedOutput {
		return 0, false, nil
	}
	if derivedOutput > completion {
		// Gemini compatibility does not formally specify how thinking tokens map
		// onto completion_tokens. total-prompt is the conservative output view.
		completion = derivedOutput
	}

	var inputCost int64
	switch p.Provider {
	case ProviderDeepSeek:
		hit := nonNegative(u.PromptCacheHitTokens)
		miss := nonNegative(u.PromptCacheMissTokens)
		cacheTotal, e := checkedAdd(hit, miss)
		if e != nil || cacheTotal > prompt {
			return 0, false, nil
		}
		if cacheTotal < prompt {
			miss = prompt - hit
		}
		hitCost, e := checkedMul(hit, p.card.CacheHitInputPUSD)
		if e != nil {
			return 0, false, e
		}
		missCost, e := checkedMul(miss, p.card.InputPUSD)
		if e != nil {
			return 0, false, e
		}
		inputCost, e = checkedAdd(hitCost, missCost)
		if e != nil {
			return 0, false, e
		}
	case ProviderGemini:
		// The OpenAI compatibility usage contract does not reliably expose
		// modality/cache splits. Price every prompt token at the request's highest
		// possible input class; this is conservative and wallet-safe.
		rate := p.card.InputPUSD
		if p.InputClass == InputAudio && p.card.AudioInputPUSD > rate {
			rate = p.card.AudioInputPUSD
		}
		var e error
		inputCost, e = checkedMul(prompt, rate)
		if e != nil {
			return 0, false, e
		}
	default:
		return 0, false, ErrUnknownRateCard
	}
	outputCost, err := checkedMul(completion, p.card.OutputPUSD)
	if err != nil {
		return 0, false, err
	}
	cost, err = checkedAdd(inputCost, outputCost)
	if err != nil {
		return 0, false, err
	}
	return cost, true, nil
}

func usageHasNegative(u Usage) bool {
	return u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 ||
		u.PromptCacheHitTokens < 0 || u.PromptCacheMissTokens < 0 ||
		u.CachedPromptTokens < 0 || u.ReasoningTokens < 0
}

func checkedMul(a, b int64) (int64, error) {
	if a < 0 || b < 0 || (a != 0 && b > math.MaxInt64/a) {
		return 0, ErrCostOverflow
	}
	return a * b, nil
}

func checkedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrCostOverflow
	}
	return a + b, nil
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
