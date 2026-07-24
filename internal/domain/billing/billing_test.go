package billing

import (
	"errors"
	"testing"
)

func TestDeepSeekPlanAndCacheAwareCost(t *testing.T) {
	p, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	wantReserve := int64(100*140_000 + 20*280_000)
	if p.ReservedPUSD != wantReserve {
		t.Fatalf("reserve=%d want %d", p.ReservedPUSD, wantReserve)
	}
	cost, ok, err := p.Cost(Usage{
		Present: true, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		PromptCacheHitTokens: 80, PromptCacheMissTokens: 20,
	})
	if err != nil || !ok {
		t.Fatalf("Cost err=%v ok=%v", err, ok)
	}
	want := int64(80*2_800 + 20*140_000 + 10*280_000)
	if cost != want {
		t.Fatalf("cost=%d want %d", cost, want)
	}
}

func TestQwenTieredPlanReservesWorstTierAndSettlesActualTier(t *testing.T) {
	// Inline image/video input cannot prove its visual-token count from bytes.
	// The caller therefore freezes the full 1M/64K worst case before Open.
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, Qwen37InputLimit, Qwen37OutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	wantReserve := Qwen37InputLimit*4_800_000 + Qwen37OutputLimit*4_800_000
	if p.ReservedPUSD != wantReserve {
		t.Fatalf("reserve=%d want %d", p.ReservedPUSD, wantReserve)
	}

	// Model Studio selects the tier from total prompt tokens in THIS request.
	// At the inclusive 256K boundary all prompt and output tokens use the low
	// tier, while an implicit cache hit receives its documented 20% input rate.
	cost, ok, err := p.Cost(Usage{
		Present: true, PromptTokens: 256_000, CompletionTokens: 100, TotalTokens: 256_100,
		CachedPromptTokens: 10_000,
	})
	if err != nil || !ok {
		t.Fatalf("Cost err=%v ok=%v", err, ok)
	}
	want := int64(10_000*320_000 + 246_000*1_600_000 + 100*1_600_000)
	if cost != want {
		t.Fatalf("low-tier actual=%d want %d", cost, want)
	}

	// Crossing the boundary by one token changes the unit price for every
	// input/output token in the request; it is not progressive pricing.
	cost, ok, err = p.Cost(Usage{
		Present: true, PromptTokens: 256_001, CompletionTokens: 100, TotalTokens: 256_101,
	})
	if err != nil || !ok {
		t.Fatalf("Cost err=%v ok=%v", err, ok)
	}
	want = int64(256_001*4_800_000 + 100*4_800_000)
	if cost != want {
		t.Fatalf("high-tier actual=%d want %d", cost, want)
	}
}

func TestQwenQuoteUsesInclusiveLowTierBelow256K(t *testing.T) {
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 256_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(256_000*1_600_000 + 1*1_600_000)
	if p.ReservedPUSD != want {
		t.Fatalf("reserve=%d want %d", p.ReservedPUSD, want)
	}
}

func TestMissingUsageKeepsReservation(t *testing.T) {
	p, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Cost(Usage{}); err != nil || ok {
		t.Fatalf("missing usage: ok=%v err=%v", ok, err)
	}
	for name, usage := range map[string]Usage{
		"empty object":           {Present: true},
		"missing prompt":         {Present: true, CompletionTokens: 1, TotalTokens: 1},
		"DeepSeek missing total": {Present: true, PromptTokens: 1, CompletionTokens: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := p.Cost(usage); err != nil || ok {
				t.Fatalf("malformed usage refunded: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestContradictoryUsageKeepsReservation(t *testing.T) {
	ds, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, usage := range map[string]Usage{
		"cache split exceeds prompt": {
			Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101,
			PromptCacheHitTokens: 80, PromptCacheMissTokens: 30,
		},
		"cache hit exceeds prompt": {
			Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101,
			PromptCacheHitTokens: 101,
		},
		"cached detail exceeds prompt": {
			Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101,
			CachedPromptTokens: 101,
		},
	} {
		t.Run("deepseek/"+name, func(t *testing.T) {
			if _, ok, err := ds.Cost(usage); err != nil || ok {
				t.Fatalf("contradictory usage refunded: ok=%v err=%v usage=%+v", ok, err, usage)
			}
		})
	}

	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, usage := range map[string]Usage{
		"missing total and empty completion": {Present: true, PromptTokens: 1},
		"missing total with visible completion": {
			Present: true, PromptTokens: 1, CompletionTokens: 2,
		},
		"total below visible sum": {
			Present: true, PromptTokens: 10, CompletionTokens: 3, TotalTokens: 12,
		},
		"reasoning exceeds derived output": {
			Present: true, PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, ReasoningTokens: 3,
		},
	} {
		t.Run("qwen/"+name, func(t *testing.T) {
			if _, ok, err := p.Cost(usage); err != nil || ok {
				t.Fatalf("ambiguous Qwen usage refunded: ok=%v err=%v usage=%+v", ok, err, usage)
			}
		})
	}
}

func TestUnknownModelFailsClosed(t *testing.T) {
	_, err := NewPlan(ProviderQwen, "qwen-latest", InputStandard, 1, 1)
	if !errors.Is(err, ErrUnknownRateCard) {
		t.Fatalf("err=%v", err)
	}
}

func TestInputClassIsClosedAndProviderCompatible(t *testing.T) {
	for name, tc := range map[string]struct {
		provider Provider
		model    string
		class    InputClass
	}{
		"unknown class": {ProviderQwen, Qwen37Plus, InputClass(255)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPlan(tc.provider, tc.model, tc.class, 1, 1); !errors.Is(err, ErrUnknownRateCard) {
				t.Fatalf("NewPlan(%s,%s,class=%d) err=%v", tc.provider, tc.model, tc.class, err)
			}
		})
	}
}

func TestValidateRejectsExternallyForgedPlanWithoutPrivateRateCard(t *testing.T) {
	want, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	forged := Plan{
		Provider: want.Provider, Model: want.Model, RateCardID: want.RateCardID,
		InputClass: want.InputClass, PromptQuote: want.PromptQuote,
		OutputQuote: want.OutputQuote, ReservedPUSD: want.ReservedPUSD,
	}
	if err := forged.Validate(); !errors.Is(err, ErrUnknownRateCard) {
		t.Fatalf("forged plan validation err=%v", err)
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("NewPlan value must validate: %v", err)
	}
}

func TestMergeSnapshotUsesMaxNotSum(t *testing.T) {
	u := Usage{Present: true, PromptTokens: 10, CompletionTokens: 2}
	u = u.MergeSnapshot(Usage{Present: true, PromptTokens: 10, CompletionTokens: 4})
	if u.PromptTokens != 10 || u.CompletionTokens != 4 {
		t.Fatalf("merged=%+v", u)
	}
}

func TestMergeSnapshotPreservesMalformedEvidence(t *testing.T) {
	u := Usage{}.MergeSnapshot(Usage{
		Present: true, PromptTokens: 100, CompletionTokens: -1, TotalTokens: 100,
	})
	// Max-merge may normalize the numeric view for accumulation, but it must not
	// normalize away the fact that a provider supplied an impossible negative.
	if !u.Malformed {
		t.Fatalf("negative snapshot lost malformed evidence: %+v", u)
	}
	p, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Cost(u); err != nil || ok {
		t.Fatalf("malformed merged usage must keep full quote: ok=%v err=%v usage=%+v", ok, err, u)
	}
}

func TestCostDirectlyRejectsEveryNegativeUsageDimension(t *testing.T) {
	p, err := NewPlan(ProviderDeepSeek, DeepSeekV4Flash, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	base := Usage{Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101}
	mutations := map[string]func(*Usage){
		"prompt":        func(u *Usage) { u.PromptTokens = -1 },
		"completion":    func(u *Usage) { u.CompletionTokens = -1 },
		"total":         func(u *Usage) { u.TotalTokens = -1 },
		"cache hit":     func(u *Usage) { u.PromptCacheHitTokens = -1 },
		"cache miss":    func(u *Usage) { u.PromptCacheMissTokens = -1 },
		"cached detail": func(u *Usage) { u.CachedPromptTokens = -1 },
		"reasoning":     func(u *Usage) { u.ReasoningTokens = -1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			u := base
			mutate(&u)
			if _, ok, err := p.Cost(u); err != nil || ok {
				t.Fatalf("negative usage authorized refund: ok=%v err=%v usage=%+v", ok, err, u)
			}
		})
	}
}
