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
	ProviderQwen     Provider = "qwen"
)

const (
	DeepSeekV4Flash       = "deepseek-v4-flash"
	Qwen37Plus            = "qwen3.7-plus"
	Qwen3ASRFlashRealtime = "qwen3-asr-flash-realtime"
	QwenImage20           = "qwen-image-2.0"
	Qwen3TTSFlash         = "qwen3-tts-flash"
	Wan27T2V              = "wan2.7-t2v"
	// QwenTTSClone is the voice-ENROLLMENT model — the `model` field of the customization call, not
	// the synthesis model. It is a separate card because it is a separate purchase: enrollment buys
	// a persistent registration once, synthesis buys characters every time.
	// QwenTTSClone 是音色**登记**模型——customization 调用的 `model` 字段,不是合成模型。它是一张
	// 单独的卡,因为它是一笔**单独的购买**:登记一次性买下一份长存的登记,合成每次买字符。
	QwenTTSClone        = "qwen-tts-clone"
	PicoUSDPerMicroUSD  = int64(1_000_000)
	PicoUSDPerUSD       = int64(1_000_000_000_000)
	DeepSeekInputLimit  = int64(1_000_000)
	DeepSeekOutputLimit = int64(384_000)
	Qwen37InputLimit    = int64(1_000_000)
	Qwen37OutputLimit   = int64(65_536)
	QwenASRInputLimit   = int64(120) // seconds; bounded by the speech WebSocket session cap.
	QwenASROutputLimit  = int64(0)
	// QwenImageInputLimit bounds images per reservation. The gateway request contract fixes n=1
	// (WRK-082 P12); the small headroom only bounds the card, it does not widen the wire.
	// QwenImageInputLimit 界定单次预留的图张数。网关请求契约钉 n=1(P12);小余量只界卡、不放宽线缆。
	QwenImageInputLimit  = int64(6)
	QwenImageOutputLimit = int64(0)
	// QwenTTSInputLimit bounds characters per reservation. The gateway wire caps one request at
	// maxInputChars (WRK-082 代拍 C5: the desktop chunks long text, the gateway stays one
	// request = one reservation = one settle); the headroom only bounds the card.
	// QwenTTSInputLimit 界定单次预留的字符数。网关线缆单请求上限为 maxInputChars(代拍 C5:长文本由
	// 桌面端切块,网关恒守「一请求=一预留=一结算」);余量只界卡。
	QwenTTSInputLimit  = int64(4_000)
	QwenTTSOutputLimit = int64(0)
	// QwenVideoInputLimit bounds SECONDS per reservation. The wire caps one clip at
	// maxDurationSec; the headroom only bounds the card.
	// QwenVideoInputLimit 界定单次预留的**秒数**。线缆把单条封在 maxDurationSec;余量只界卡。
	QwenVideoInputLimit  = int64(15)
	QwenVideoOutputLimit = int64(0)
	// QwenVoiceInputLimit bounds VOICES per reservation. One enrollment is always exactly one voice
	// (the wire has no batch form); the headroom only bounds the card.
	// QwenVoiceInputLimit 界定单次预留的**音色个数**。一次登记恒为一个音色(线缆没有批量形);余量只界卡。
	QwenVoiceInputLimit   = int64(2)
	QwenVoiceOutputLimit  = int64(0)
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

// InputClass is retained in the frozen Plan wire shape. Standard covers token
// plans; AudioSeconds is used only by realtime ASR, where the provider bills
// elapsed audio duration rather than chat tokens; Images is used only by image
// generation, where the provider bills per successfully generated image;
// Characters is used only by speech synthesis, where the provider bills the
// INPUT text length rather than the produced audio's duration; Voices is used
// only by voice cloning, where the provider bills once per registration created
// — the only class whose purchase OUTLIVES the request that made it.
type InputClass uint8

const (
	InputStandard InputClass = iota
	InputAudioSeconds
	InputImages
	InputCharacters
	InputVideoSeconds
	InputVoices
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

// pricingTier is one request-wide pricing band. Model Studio selects a tier from
// the total input tokens in a request and charges every input token at that
// tier's price; Qwen3.7 also prices output at the selected tier. It stays
// private so callers cannot manufacture a rate that differs from the card.
type pricingTier struct {
	InputUpperBound   int64
	InputPUSD         int64
	CacheHitInputPUSD int64
	OutputPUSD        int64
}

// RateCard is an immutable pricing snapshot. The legacy scalar fields are
// intentionally replaced by ordered tiers: future provider tariffs must not be
// flattened into a rate that under-reserves a whole request.
type RateCard struct {
	ID          string
	Provider    Provider
	Model       string
	InputLimit  int64
	OutputLimit int64
	tiers       []pricingTier
}

var rateCards = map[Provider]map[string]RateCard{
	ProviderDeepSeek: {
		DeepSeekV4Flash: {
			ID: "deepseek-v4-flash-2026-07-20", Provider: ProviderDeepSeek, Model: DeepSeekV4Flash,
			InputLimit: DeepSeekInputLimit, OutputLimit: DeepSeekOutputLimit,
			// Official standard pricing per 1M tokens: hit $0.0028,
			// cache miss $0.14, output $0.28.
			tiers: []pricingTier{{
				InputUpperBound: DeepSeekInputLimit,
				InputPUSD:       140_000, CacheHitInputPUSD: 2_800, OutputPUSD: 280_000,
			}},
		},
	},
	ProviderQwen: {
		Qwen37Plus: {
			ID: "qwen3.7-plus-sg-thinking-2026-07-24", Provider: ProviderQwen, Model: Qwen37Plus,
			InputLimit: Qwen37InputLimit, OutputLimit: Qwen37OutputLimit,
			// Singapore list pricing, thinking enabled, per 1M tokens. The first
			// tier is 0 < input <= 256K; the second is 256K < input <= 1M.
			// Implicit context-cache hits are billed at 20% of standard input.
			tiers: []pricingTier{
				{InputUpperBound: 256_000, InputPUSD: 1_600_000, CacheHitInputPUSD: 320_000, OutputPUSD: 1_600_000},
				{InputUpperBound: Qwen37InputLimit, InputPUSD: 4_800_000, CacheHitInputPUSD: 960_000, OutputPUSD: 4_800_000},
			},
		},
		Qwen3ASRFlashRealtime: {
			ID: "qwen3-asr-flash-realtime-sg-2026-07-24", Provider: ProviderQwen, Model: Qwen3ASRFlashRealtime,
			InputLimit: QwenASRInputLimit, OutputLimit: QwenASROutputLimit,
			// Singapore realtime ASR list price: $0.00009 / second.
			tiers: []pricingTier{{InputUpperBound: QwenASRInputLimit, InputPUSD: 90_000_000, OutputPUSD: 0}},
		},
		QwenImage20: {
			ID: "qwen-image-2.0-assumed-2026-07-27", Provider: ProviderQwen, Model: QwenImage20,
			InputLimit: QwenImageInputLimit, OutputLimit: QwenImageOutputLimit,
			// WORKING ASSUMPTION (WRK-082 代拍 B3): ¥0.25/image ≈ $0.035 = 35e9 pUSD. This card
			// only budgets the operator's own wallet gate (reserve == settle, deterministic);
			// the upstream bills its真实 list price regardless. MUST be reconciled against the
			// official pricing page before launch — the "assumed" ID keeps that debt visible.
			// 工作假设(代拍 B3):¥0.25/张≈$0.035=35e9 pUSD。此卡只作 operator 自家钱包预算闸
			// (reserve==settle,确定性成本);上游按真实价目计费不受影响。上线前必须对官方价页
			// 对账——ID 里的 "assumed" 让这笔债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenImageInputLimit, InputPUSD: 35_000_000_000, OutputPUSD: 0}},
		},
		Qwen3TTSFlash: {
			ID: "qwen3-tts-flash-assumed-2026-07-27", Provider: ProviderQwen, Model: Qwen3TTSFlash,
			InputLimit: QwenTTSInputLimit, OutputLimit: QwenTTSOutputLimit,
			// WORKING ASSUMPTION (WRK-082 代拍 C2): ¥1 / 10K characters ≈ $0.0000139 per character
			// = 14e6 pUSD. The official price page renders its table in JS and could not be read
			// verbatim; the third-party figure is what this card encodes. Same discipline as the
			// image card: this budgets the operator's OWN wallet gate (reserve == settle, cost is
			// deterministic in the request's own character count) while the upstream bills its real
			// list price regardless. The "assumed" ID keeps the reconciliation debt visible.
			// 工作假设(代拍 C2):¥1/万字符≈$0.0000139/字符=14e6 pUSD。官方价目表由 JS 渲染、取不到
			// 逐字数值,此卡编码的是第三方数字。与图像卡同纪律:它只作 operator 自家钱包闸(reserve==
			// settle,成本在本请求字符数上确定),上游按真实价目计费不受影响。ID 里的 "assumed" 让这
			// 笔对账债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenTTSInputLimit, InputPUSD: 14_000_000, OutputPUSD: 0}},
		},
		Wan27T2V: {
			ID: "wan2.7-t2v-assumed-2026-07-27", Provider: ProviderQwen, Model: Wan27T2V,
			InputLimit: QwenVideoInputLimit, OutputLimit: QwenVideoOutputLimit,
			// WORKING ASSUMPTION (WRK-082 H1): ¥0.6 per second at 720P ≈ $0.083 = 83e9 pUSD.
			// Video is the most expensive thing this gateway can be asked to do — a single 5-second
			// clip costs more than a whole day's image allowance — so this card is also the one
			// whose reconciliation matters most. The "assumed" ID keeps that debt visible.
			// 工作假设(H1):720P ¥0.6/秒 ≈ $0.083 = 83e9 pUSD。视频是本网关能被要求做的最贵的事——
			// 一条 5 秒片子比一整天的图像额度还贵——故这张卡也是最该对账的一张。ID 里的 "assumed" 让
			// 这笔债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenVideoInputLimit, InputPUSD: 83_000_000_000, OutputPUSD: 0}},
		},
		QwenTTSClone: {
			ID: "qwen-tts-clone-2026-07-28", Provider: ProviderQwen, Model: QwenTTSClone,
			InputLimit: QwenVoiceInputLimit, OutputLimit: QwenVoiceOutputLimit,
			// $0.2 per voice created = 200e9 pUSD. NOT an "assumed" card: this figure is printed
			// verbatim on the official pricing page (H9 第0步核准 2026-07-28), which is why the ID
			// carries no `assumed-` marker — the other three generation cards do, and the absence
			// here is the claim that this one is reconciled.
			//
			// The reservation is what makes this capability bounded at all. Its two ceilings are
			// INVENTORY (how many a person may hold), and inventory bounds the CONCURRENT count,
			// never the cumulative spend: delete frees a slot, so an enroll→delete cycle would burn
			// money without limit. The wallet gate and the daily category gate are the two things
			// that actually bound it.
			//
			// 每创建一个音色 $0.2 = 200e9 pUSD。**不是** assumed 卡:这个数字逐字印在官方价目页上
			// (H9 第0步核准 2026-07-28),故 ID 里没有 `assumed-` 标记——另外三张生成卡都有,而这里
			// 的**缺席**本身就是「这一张已对账」的断言。
			//
			// 预留才是让这个能力**有界**的东西。它那两条上限是**库存**(一个人能持有几个),而库存界的
			// 是**同时**的个数、从来不是**累计**的花费:删除会腾出位置,故 enroll→delete 循环可以无界
			// 烧钱。钱包闸与品类日闸才是真正界住它的那两样。
			tiers: []pricingTier{{InputUpperBound: QwenVoiceInputLimit, InputPUSD: 200_000_000_000, OutputPUSD: 0}},
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

// tierForInput returns the one request-wide tariff selected by the input-token
// count. A zero input bound is only possible while constructing a quote; it
// selects the first tier. Actual usage still requires prompt_tokens > 0.
func (r RateCard) tierForInput(tokens int64) (pricingTier, bool) {
	if tokens < 0 || tokens > r.InputLimit || len(r.tiers) == 0 {
		return pricingTier{}, false
	}
	for _, tier := range r.tiers {
		if tier.InputUpperBound <= 0 || tier.InputUpperBound > r.InputLimit ||
			tier.InputPUSD <= 0 || tier.OutputPUSD < 0 || tier.CacheHitInputPUSD < 0 ||
			(r.OutputLimit > 0 && tier.OutputPUSD == 0) {
			return pricingTier{}, false
		}
		if tokens <= tier.InputUpperBound {
			return tier, true
		}
	}
	return pricingTier{}, false
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
	tier         pricingTier
}

// Validate proves that a Plan is exactly one produced by NewPlan. This lets a
// persistence boundary reject a zero-value or manually forged rate-card/amount
// combination even though Plan is a transportable domain value.
func (p Plan) Validate() error {
	want, err := NewPlan(p.Provider, p.Model, p.InputClass, p.PromptQuote, p.OutputQuote)
	if err != nil {
		return err
	}
	if !samePlan(p, want) {
		return ErrUnknownRateCard
	}
	return nil
}

func samePlan(a, b Plan) bool {
	return a.Provider == b.Provider && a.Model == b.Model && a.RateCardID == b.RateCardID &&
		a.InputClass == b.InputClass && a.PromptQuote == b.PromptQuote &&
		a.OutputQuote == b.OutputQuote && a.ReservedPUSD == b.ReservedPUSD && a.tier == b.tier
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
		if card.OutputLimit == 0 || (outputBound == 0 && provider != ProviderQwen) {
			return Plan{}, ErrUnknownRateCard
		}
	case InputAudioSeconds:
		if provider != ProviderQwen || model != Qwen3ASRFlashRealtime || outputBound != 0 {
			return Plan{}, ErrUnknownRateCard
		}
	case InputImages:
		if provider != ProviderQwen || model != QwenImage20 || outputBound != 0 || promptBound < 1 {
			return Plan{}, ErrUnknownRateCard
		}
	case InputCharacters:
		if provider != ProviderQwen || model != Qwen3TTSFlash || outputBound != 0 || promptBound < 1 {
			return Plan{}, ErrUnknownRateCard
		}
	case InputVideoSeconds:
		if provider != ProviderQwen || model != Wan27T2V || outputBound != 0 || promptBound < 1 {
			return Plan{}, ErrUnknownRateCard
		}
	case InputVoices:
		if provider != ProviderQwen || model != QwenTTSClone || outputBound != 0 || promptBound < 1 {
			return Plan{}, ErrUnknownRateCard
		}
	default:
		return Plan{}, ErrUnknownRateCard
	}
	if promptBound < 0 || outputBound < 0 || promptBound > card.InputLimit || outputBound > card.OutputLimit {
		return Plan{}, ErrUnknownRateCard
	}
	tier, ok := card.tierForInput(promptBound)
	if !ok {
		return Plan{}, ErrUnknownRateCard
	}
	inCost, err := checkedMul(promptBound, tier.InputPUSD)
	if err != nil {
		return Plan{}, err
	}
	outCost, err := checkedMul(outputBound, tier.OutputPUSD)
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
		ReservedPUSD: total, card: card, tier: tier,
	}, nil
}

// NewAudioSecondsPlan prices a realtime ASR reservation in whole seconds. The
// gateway rounds partial audio seconds up before calling it, so cost accounting
// stays conservative without pretending audio duration is a text token count.
func NewAudioSecondsPlan(provider Provider, model string, seconds int64) (Plan, error) {
	return NewPlan(provider, model, InputAudioSeconds, seconds, 0)
}

// AudioSecondsCost converts an authoritative audio duration under a frozen ASR
// plan. It is intentionally separate from token Usage so chat and speech
// accounting cannot be accidentally mixed.
func (p Plan) AudioSecondsCost(seconds int64) (int64, error) {
	if p.InputClass != InputAudioSeconds || seconds < 0 || seconds > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(seconds)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(seconds, tier.InputPUSD)
}

// NewImagesPlan prices an image-generation reservation by image count (n=1 on
// the gateway wire, WRK-082 P12). Deterministic per-image pricing means reserve
// equals settle for a successful generation.
//
// NewImagesPlan 按图张数定价图像生成预留(网关线缆 n=1,P12)。按张确定性定价意味着成功生成时
// reserve == settle。
func NewImagesPlan(provider Provider, model string, images int64) (Plan, error) {
	return NewPlan(provider, model, InputImages, images, 0)
}

// ImagesCost converts an authoritative generated-image count under a frozen
// image plan — the AudioSecondsCost twin, kept separate from token Usage so
// image accounting cannot be mixed with chat.
//
// ImagesCost 在冻结的图像 plan 下换算权威已生成张数——AudioSecondsCost 的孪生,与 token Usage
// 刻意分离,图像账不与 chat 混。
func (p Plan) ImagesCost(images int64) (int64, error) {
	if p.InputClass != InputImages || images < 0 || images > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(images)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(images, tier.InputPUSD)
}

// NewVoicesPlan prices a voice-ENROLLMENT reservation by voice count (always 1 on the wire). The
// price is fixed and known before the call, so reserve equals settle for a successful enrollment —
// the same deterministic shape as images, but for a purchase that persists after the request ends.
//
// NewVoicesPlan 按**音色个数**定价一次音色登记预留(线缆上恒为 1)。价格固定、调用前就已知,故登记成功
// 时 reserve == settle——与图像同一种确定性形状,只是这笔购买在请求结束之后仍然存在。
func NewVoicesPlan(provider Provider, model string, voices int64) (Plan, error) {
	return NewPlan(provider, model, InputVoices, voices, 0)
}

// VoicesCost converts an authoritative created-voice count under a frozen enrollment plan.
//
// VoicesCost 在冻结的登记 plan 下换算权威已创建音色数。
func (p Plan) VoicesCost(voices int64) (int64, error) {
	if p.InputClass != InputVoices || voices < 0 || voices > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(voices)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(voices, tier.InputPUSD)
}

// NewCharactersPlan prices a speech-synthesis reservation by INPUT character
// count. The count is known exactly before the call (it is the request's own
// text), so reserve equals settle for a successful synthesis — the same
// deterministic shape as images, and the reason speech needs no usage feedback
// from the upstream to close its books.
//
// NewCharactersPlan 按**输入**字符数定价语音合成预留。字符数在调用前就精确已知(它就是请求自带的
// 文本),故合成成功时 reserve == settle——与图像同一种确定性形状,也正因如此语音不需要上游回报
// usage 就能平账。
func NewCharactersPlan(provider Provider, model string, characters int64) (Plan, error) {
	return NewPlan(provider, model, InputCharacters, characters, 0)
}

// CharactersCost converts an authoritative character count under a frozen speech
// plan — the ImagesCost twin, kept separate from token Usage so speech
// accounting cannot be mixed with chat.
//
// CharactersCost 在冻结的语音 plan 下换算权威字符数——ImagesCost 的孪生,与 token Usage 刻意
// 分离,语音账不与 chat 混。
func (p Plan) CharactersCost(characters int64) (int64, error) {
	if p.InputClass != InputCharacters || characters < 0 || characters > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(characters)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(characters, tier.InputPUSD)
}

// Cost converts an authoritative cumulative usage snapshot under the frozen
// card. ok=false means the caller must keep the full reservation. Compatibility
// responses that omit cache details are conservatively priced as cache misses.
func (p Plan) Cost(u Usage) (cost int64, ok bool, err error) {
	if p.InputClass != InputStandard {
		return 0, false, ErrUnknownRateCard
	}
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
	// Qwen's hidden thinking output; a visible completion count alone cannot
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
		// Qwen compatibility does not formally specify how thinking tokens map
		// onto completion_tokens. total-prompt is the conservative output view.
		completion = derivedOutput
	}
	tier, ok := p.card.tierForInput(prompt)
	if !ok {
		return 0, false, nil
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
		hitCost, e := checkedMul(hit, tier.CacheHitInputPUSD)
		if e != nil {
			return 0, false, e
		}
		missCost, e := checkedMul(miss, tier.InputPUSD)
		if e != nil {
			return 0, false, e
		}
		inputCost, e = checkedAdd(hitCost, missCost)
		if e != nil {
			return 0, false, e
		}
	case ProviderQwen:
		// The compatibility response may omit cache details. Treat such prompt
		// tokens as cache misses; when it reports cached tokens, charge the exact
		// provider cache-hit rate without ever undercharging unknown tokens.
		hit := nonNegative(u.CachedPromptTokens)
		if hit > prompt {
			return 0, false, nil
		}
		hitCost, e := checkedMul(hit, tier.CacheHitInputPUSD)
		if e != nil {
			return 0, false, e
		}
		missCost, e := checkedMul(prompt-hit, tier.InputPUSD)
		if e != nil {
			return 0, false, e
		}
		inputCost, e = checkedAdd(hitCost, missCost)
		if e != nil {
			return 0, false, e
		}
	default:
		return 0, false, ErrUnknownRateCard
	}
	outputCost, err := checkedMul(completion, tier.OutputPUSD)
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

// NewVideoSecondsPlan prices a video reservation by clip SECONDS. Like images and characters the
// cost is deterministic before the call (the requested duration IS the billed quantity), so
// reserve equals settle for a successful generation — even though the generation itself is
// asynchronous and minutes long.
//
// NewVideoSecondsPlan 按片长**秒数**定价视频预留。与图像、字符一样,成本在调用前即确定(请求的时长
// **就是**计费量),故成功生成时 reserve == settle——尽管生成本身是异步且分钟级的。
func NewVideoSecondsPlan(provider Provider, model string, seconds int64) (Plan, error) {
	return NewPlan(provider, model, InputVideoSeconds, seconds, 0)
}

// VideoSecondsCost converts an authoritative clip length under a frozen video plan.
//
// VideoSecondsCost 在冻结的视频 plan 下换算权威片长。
func (p Plan) VideoSecondsCost(seconds int64) (int64, error) {
	if p.InputClass != InputVideoSeconds || seconds < 0 || seconds > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(seconds)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(seconds, tier.InputPUSD)
}
