package billing

import (
	"errors"
	"testing"
)

// A small request sits in the FIRST pricing tier, and a reported cache hit is
// charged at that tier's discounted input rate while every unreported prompt
// token stays at full price. The arithmetic is spelled out rather than pasted
// so a rate-card edit has to be re-derived here, not copied from the failure.
//
// 小请求落在**第一档**,已报告的缓存命中按该档折后输入价计费,而每一个**未**报告的 prompt token
// 仍按全价。算式**写开**而不是贴一个常数:改费率卡时必须在这里重新推导,而不是把失败信息里的数
// 抄回来。
func TestPlanAndCacheAwareCost(t *testing.T) {
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	const (
		tier1Input    = 1_600_000 // $1.60 / 1M tokens
		tier1CacheHit = 320_000   // 20% of standard input
		tier1Output   = 1_600_000
	)
	wantReserve := int64(100*tier1Input + 20*tier1Output)
	if p.ReservedPUSD != wantReserve {
		t.Fatalf("reserve=%d want %d", p.ReservedPUSD, wantReserve)
	}
	cost, ok, err := p.Cost(Usage{
		Present: true, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		CachedPromptTokens: 80,
	})
	if err != nil || !ok {
		t.Fatalf("Cost err=%v ok=%v", err, ok)
	}
	want := int64(80*tier1CacheHit + 20*tier1Input + 10*tier1Output)
	if cost != want {
		t.Fatalf("cost=%d want %d", cost, want)
	}
	if cost >= p.ReservedPUSD {
		t.Fatalf("a cache-discounted settle must come in under the reservation: cost=%d reserve=%d", cost, p.ReservedPUSD)
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

func TestQwenRealtimeASRPlanPricesAudioSeconds(t *testing.T) {
	p, err := NewUnitPlan(ProviderQwen, Qwen3ASRFlashRealtime, InputAudioSeconds, 120)
	if err != nil {
		t.Fatal(err)
	}
	if p.InputClass != InputAudioSeconds || p.OutputQuote != 0 {
		t.Fatalf("bad ASR plan shape: %+v", p)
	}
	wantReserve := int64(120 * 90_000_000)
	if p.ReservedPUSD != wantReserve {
		t.Fatalf("reserve=%d want %d", p.ReservedPUSD, wantReserve)
	}
	actual, err := p.UnitCost(InputAudioSeconds, 2)
	if err != nil {
		t.Fatalf("AudioSecondsCost: %v", err)
	}
	if actual != 2*90_000_000 {
		t.Fatalf("actual=%d", actual)
	}
	if _, _, err := p.Cost(Usage{Present: true, PromptTokens: 2, TotalTokens: 2}); err == nil {
		t.Fatalf("ASR plan must not settle through token usage")
	}
}

func TestMissingUsageKeepsReservation(t *testing.T) {
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Cost(Usage{}); err != nil || ok {
		t.Fatalf("missing usage: ok=%v err=%v", ok, err)
	}
	for name, usage := range map[string]Usage{
		"empty object":   {Present: true},
		"missing prompt": {Present: true, CompletionTokens: 1, TotalTokens: 1},
		"missing total":  {Present: true, PromptTokens: 1, CompletionTokens: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := p.Cost(usage); err != nil || ok {
				t.Fatalf("malformed usage refunded: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestContradictoryUsageKeepsReservation(t *testing.T) {
	ds, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, usage := range map[string]Usage{
		"cached detail exceeds prompt": {
			Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101,
			CachedPromptTokens: 101,
		},
		"reasoning exceeds derived output": {
			Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101,
			ReasoningTokens: 50,
		},
		"total below prompt+completion": {
			Present: true, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 100,
		},
	} {
		t.Run(name, func(t *testing.T) {
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
		"unknown class":         {ProviderQwen, Qwen37Plus, InputClass(255)},
		"audio seconds on chat": {ProviderQwen, Qwen37Plus, InputAudioSeconds},
		"standard on ASR model": {ProviderQwen, Qwen3ASRFlashRealtime, InputStandard},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPlan(tc.provider, tc.model, tc.class, 1, 1); !errors.Is(err, ErrUnknownRateCard) {
				t.Fatalf("NewPlan(%s,%s,class=%d) err=%v", tc.provider, tc.model, tc.class, err)
			}
		})
	}
}

func TestValidateRejectsExternallyForgedPlanWithoutPrivateRateCard(t *testing.T) {
	want, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 10, 2)
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
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Cost(u); err != nil || ok {
		t.Fatalf("malformed merged usage must keep full quote: ok=%v err=%v usage=%+v", ok, err, u)
	}
}

func TestCostDirectlyRejectsEveryNegativeUsageDimension(t *testing.T) {
	p, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	base := Usage{Present: true, PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101}
	mutations := map[string]func(*Usage){
		"prompt":        func(u *Usage) { u.PromptTokens = -1 },
		"completion":    func(u *Usage) { u.CompletionTokens = -1 },
		"total":         func(u *Usage) { u.TotalTokens = -1 },
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

// TestImagesPlanAndCost pins the image rate-card pair: a frozen per-image plan whose reserve
// equals its settle for the same count (deterministic pricing), the Validate roundtrip that the
// persistence boundary relies on, and the closed-set rejections (wrong provider/model, zero
// images, out-of-card counts, cross-class cost calls).
func TestImagesPlanAndCost(t *testing.T) {
	p, err := NewUnitPlan(ProviderQwen, QwenImage20, InputImages, 1)
	if err != nil {
		t.Fatalf("images plan: %v", err)
	}
	if p.InputClass != InputImages || p.ReservedPUSD != 35_000_000_000 {
		t.Fatalf("plan = class %d reserved %d, want images/35e9", p.InputClass, p.ReservedPUSD)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("frozen plan fails Validate roundtrip: %v", err)
	}
	cost, err := p.UnitCost(InputImages, 1)
	if err != nil || cost != p.ReservedPUSD {
		t.Fatalf("ImagesCost(1) = %d,%v — want reserve==settle", cost, err)
	}
	if _, err := NewUnitPlan(ProviderQwen, Qwen37Plus, InputImages, 1); err == nil {
		t.Fatal("images plan on a text card must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenImage20, InputImages, 0); err == nil {
		t.Fatal("zero-image plan must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenImage20, InputImages, QwenImageInputLimit+1); err == nil {
		t.Fatal("over-card image count must fail closed")
	}
	if _, err := p.UnitCost(InputImages, QwenImageInputLimit+1); err == nil {
		t.Fatal("over-card ImagesCost must fail closed")
	}
	chat, err := NewPlan(ProviderQwen, Qwen37Plus, InputStandard, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.UnitCost(InputImages, 1); err == nil {
		t.Fatal("ImagesCost on a token plan must fail closed (accounts must not mix)")
	}
}

// TestCharactersPlanAndCost pins the speech rate-card pair: a frozen per-character
// plan whose reserve equals its settle for the same count, the Validate roundtrip the persistence
// boundary relies on, and the closed-set rejections. The cross-class assertions matter most: a
// characters plan must not answer ImagesCost and an images plan must not answer CharactersCost —
// the two are priced in different units and a silent crossover would bill audio at 35e9 per unit.
//
// TestCharactersPlanAndCost 钉语音卡对。最要紧的是**跨类**断言:字符 plan 不得答 ImagesCost、
// 图像 plan 不得答 CharactersCost——两者单位不同,静默串线会把音频按每单位 35e9 计费。
func TestCharactersPlanAndCost(t *testing.T) {
	p, err := NewUnitPlan(ProviderQwen, QwenAudio30TTSFlash, InputCharacters, 100)
	if err != nil {
		t.Fatalf("characters plan: %v", err)
	}
	if p.InputClass != InputCharacters || p.ReservedPUSD != 100*14_000_000 {
		t.Fatalf("plan = class %d reserved %d, want characters/1.4e9", p.InputClass, p.ReservedPUSD)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("frozen plan fails Validate roundtrip: %v", err)
	}
	cost, err := p.UnitCost(InputCharacters, 100)
	if err != nil || cost != p.ReservedPUSD {
		t.Fatalf("CharactersCost(100) = %d,%v — want reserve==settle", cost, err)
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenImage20, InputCharacters, 100); err == nil {
		t.Fatal("characters plan on the image card must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, Qwen37Plus, InputCharacters, 100); err == nil {
		t.Fatal("characters plan on a text card must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenAudio30TTSFlash, InputCharacters, 0); err == nil {
		t.Fatal("zero-character plan must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenAudio30TTSFlash, InputCharacters, QwenTTSInputLimit+1); err == nil {
		t.Fatal("over-card character count must fail closed")
	}
	if _, err := p.UnitCost(InputImages, 1); err == nil {
		t.Fatal("a characters plan must refuse ImagesCost (unit crossover)")
	}
	img, err := NewUnitPlan(ProviderQwen, QwenImage20, InputImages, 1)
	if err != nil {
		t.Fatalf("images plan: %v", err)
	}
	if _, err := img.UnitCost(InputCharacters, 1); err == nil {
		t.Fatal("an images plan must refuse CharactersCost (unit crossover)")
	}
}

func TestVideoSecondsPlanAndCost(t *testing.T) {
	p, err := NewUnitPlan(ProviderQwen, Wan27T2V, InputVideoSeconds, 5)
	if err != nil {
		t.Fatalf("video plan: %v", err)
	}
	if p.InputClass != InputVideoSeconds || p.ReservedPUSD != 5*83_000_000_000 {
		t.Fatalf("plan = class %d reserved %d, want video-seconds/415e9", p.InputClass, p.ReservedPUSD)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("frozen plan fails Validate roundtrip: %v", err)
	}
	cost, err := p.UnitCost(InputVideoSeconds, 5)
	if err != nil || cost != p.ReservedPUSD {
		t.Fatalf("VideoSecondsCost(5) = %d,%v — want reserve==settle", cost, err)
	}
	if _, err := NewUnitPlan(ProviderQwen, QwenImage20, InputVideoSeconds, 5); err == nil {
		t.Fatal("video plan on the image card must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, Qwen37Plus, InputVideoSeconds, 5); err == nil {
		t.Fatal("video plan on a text card must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, Wan27T2V, InputVideoSeconds, 0); err == nil {
		t.Fatal("zero-second plan must fail closed")
	}
	if _, err := NewUnitPlan(ProviderQwen, Wan27T2V, InputVideoSeconds, QwenVideoInputLimit+1); err == nil {
		t.Fatal("over-card duration must fail closed")
	}
	// Unit crossover in BOTH directions: a per-second plan must never be settled
	// as if its quantity were characters or images, and vice versa. Video is the
	// most expensive card here, so a crossover mistake is also the costliest.
	// **两个方向**的单位穿越:按秒的 plan 绝不能被当成字符或张来结算,反之亦然。视频是这里最贵的
	// 卡,故穿越错得也最贵。
	if _, err := p.UnitCost(InputCharacters, 5); err == nil {
		t.Fatal("a video plan must refuse CharactersCost")
	}
	if _, err := p.UnitCost(InputImages, 1); err == nil {
		t.Fatal("a video plan must refuse ImagesCost")
	}
	tts, err := NewUnitPlan(ProviderQwen, QwenAudio30TTSFlash, InputCharacters, 100)
	if err != nil {
		t.Fatalf("characters plan: %v", err)
	}
	if _, err := tts.UnitCost(InputVideoSeconds, 5); err == nil {
		t.Fatal("a characters plan must refuse VideoSecondsCost")
	}
}
