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

func TestGeminiAudioAndThinkingConservativeCost(t *testing.T) {
	p, err := NewPlan(ProviderGemini, Gemini31FlashLite, InputAudio, 100, GeminiOutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	cost, ok, err := p.Cost(Usage{
		Present: true, PromptTokens: 100, CompletionTokens: 5, TotalTokens: 120,
	})
	if err != nil || !ok {
		t.Fatalf("Cost err=%v ok=%v", err, ok)
	}
	// total-prompt=20 is greater than completion_tokens=5, so it protects
	// compatibility responses whose thinking tokens appear only in total.
	want := int64(100*500_000 + 20*1_500_000)
	if cost != want {
		t.Fatalf("cost=%d want %d", cost, want)
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

	p, err := NewPlan(ProviderGemini, Gemini31FlashLite, InputStandard, 10, 10)
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
		t.Run("gemini/"+name, func(t *testing.T) {
			if _, ok, err := p.Cost(usage); err != nil || ok {
				t.Fatalf("ambiguous Gemini usage refunded: ok=%v err=%v usage=%+v", ok, err, usage)
			}
		})
	}
}

func TestUnknownModelFailsClosed(t *testing.T) {
	_, err := NewPlan(ProviderGemini, "gemini-latest", InputStandard, 1, 1)
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
		"unknown class":          {ProviderGemini, Gemini31FlashLite, InputClass(255)},
		"audio on text provider": {ProviderDeepSeek, DeepSeekV4Flash, InputAudio},
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
