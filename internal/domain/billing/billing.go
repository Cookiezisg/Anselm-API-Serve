// Package billing owns the provider-aware cost model used by quota accounting.
// Raw token counts are never added across providers. They are converted through
// an immutable, exact-model rate card into pico-US dollars (pUSD), then only the
// resulting integer cost enters the shared install/global wallet.
//
// pUSD is intentionally finer than nano-USD so a per-token price stays EXACT
// rather than rounded: the cheapest dimension currently priced here is Qwen's
// cache-hit input at $0.32 / 1M tokens = 320,000 pUSD/token, and the unit leaves
// room for an order of magnitude below that. All arithmetic is checked and
// rounds toward higher spend when division is needed.
//
// The provider set is a closed enum of ONE. That is not an oversight: every
// route this gateway serves goes to Qwen, so a second identity would describe
// traffic that does not exist. The Provider type stays because the ledger, the
// metrics labels, and the rate-card key are all provider-scoped — the shape is
// what keeps a future second provider from being bolted on as a special case.
//
// provider 集合是一个**只有一个成员**的封闭枚举。这不是疏忽:本网关服务的每一条路由都去
// Qwen,故第二个身份描述的是不存在的流量。Provider 类型仍然保留,因为账本、指标 label 与
// 费率卡键都是按 provider 收窄的——正是这个形状,让将来真要接第二家时不必把它当特例硬塞。
package billing

import (
	"errors"
	"math"
)

// Provider is a closed, stable identity used in ledgers and low-cardinality
// metrics. It is never supplied by a client.
type Provider string

const (
	ProviderQwen Provider = "qwen"
)

const (
	Qwen37Plus            = "qwen3.7-plus"
	Qwen3ASRFlashRealtime = "qwen3-asr-flash-realtime"
	QwenImage20           = "qwen-image-2.0"
	QwenAudio30TTSFlash   = "qwen-audio-3.0-tts-flash"
	Wan27T2V              = "wan2.7-t2v"
	Wan27I2V              = "wan2.7-i2v-2026-04-25"
	// QwenTTSClone is the voice-ENROLLMENT model — the `model` field of the customization call, not
	// the synthesis model. It is a separate card because it is a separate purchase: enrollment buys
	// a persistent registration once, synthesis buys characters every time.
	// QwenTTSClone 是音色**登记**模型——customization 调用的 `model` 字段,不是合成模型。它是一张
	// 单独的卡,因为它是一笔**单独的购买**:登记一次性买下一份长存的登记,合成每次买字符。
	QwenTTSClone       = "qwen-tts-clone"
	PicoUSDPerMicroUSD = int64(1_000_000)
	PicoUSDPerUSD      = int64(1_000_000_000_000)
	Qwen37InputLimit   = int64(1_000_000)
	Qwen37OutputLimit  = int64(65_536)
	QwenASRInputLimit  = int64(120) // seconds; bounded by the speech WebSocket session cap.
	QwenASROutputLimit = int64(0)
	// QwenImageInputLimit bounds images per reservation. The gateway request contract fixes n=1;
	// the small headroom only bounds the card, it does not widen the wire.
	// QwenImageInputLimit 界定单次预留的图张数。网关请求契约钉 n=1;小余量只界卡、不放宽线缆。
	QwenImageInputLimit  = int64(6)
	QwenImageOutputLimit = int64(0)
	// QwenTTSInputLimit bounds characters per reservation. The gateway wire caps one request at
	// maxInputChars (the desktop chunks long text, the gateway stays one
	// request = one reservation = one settle); the headroom only bounds the card.
	// QwenTTSInputLimit 界定单次预留的字符数。网关线缆单请求上限为 maxInputChars(长文本由
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
	QwenVoiceInputLimit  = int64(2)
	QwenVoiceOutputLimit = int64(0)
)

var (
	ErrUnknownRateCard = errors.New("billing: no rate card for provider/model")
	ErrCostOverflow    = errors.New("billing: cost arithmetic overflow")
)

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

// unitClass is everything that distinguishes one non-token pricing class from
// another: the exact model it prices, and the smallest reservation that means
// anything for it.
//
// This table replaced five near-identical validation branches and five
// near-identical constructor/cost pairs. Collapsing them made ONE asymmetry
// visible that had been invisible while it was spread across five copies: audio
// alone admits a ZERO-unit reservation. A realtime session that opens and sends
// no PCM has nothing to price, and refusing to build its plan would turn a legal
// no-op into an error. The other four bill a discrete artifact — zero images,
// zero characters, zero seconds of video, zero voices are all malformed asks.
//
// That difference was real before this table existed; it was just spelled as
// "the audio branch happens not to have the `< 1` check", which reads like an
// oversight rather than a decision. Here it is a column.
//
// unitClass 是「一个非 token 计费品类区别于另一个」的全部内容:它定价的确切模型,以及对它而言
// 有意义的最小预留量。
//
// 这张表取代了五段几乎逐字相同的校验分支与五组几乎逐字相同的构造/计价函数。合并让**一处不对称**
// 显形——它此前分散在五份拷贝里所以看不见:**只有音频允许 0 单位预留**。一个开了会话却没发 PCM
// 的实时连接没有东西可定价,拒绝为它建 plan 等于把一次合法的空操作变成错误。另外四个计的是离散
// 产物——零张图、零个字符、零秒视频、零个音色,都是畸形的请求。
//
// 这个差别在本表存在之前就是真的,只是它被写成「音频那个分支碰巧没有 `< 1` 检查」——那读起来像
// 疏忽,而不像决定。在这里它是一列。
type unitClass struct {
	models   map[string]struct{}
	minUnits int64
}

var unitClasses = map[InputClass]unitClass{
	InputAudioSeconds: {models: modelSet(Qwen3ASRFlashRealtime), minUnits: 0},
	InputImages:       {models: modelSet(QwenImage20), minUnits: 1},
	InputCharacters:   {models: modelSet(QwenAudio30TTSFlash), minUnits: 1},
	InputVideoSeconds: {models: modelSet(Wan27T2V, Wan27I2V), minUnits: 1},
	InputVoices:       {models: modelSet(QwenTTSClone), minUnits: 1},
}

func modelSet(models ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		set[model] = struct{}{}
	}
	return set
}

// Usage is a provider-neutral token vector extracted from an upstream usage
// snapshot. Fields absent from a compatibility response remain zero. Present is
// distinct from an all-zero usage object and lets callers conservatively retain
// the reservation when usage is missing or malformed.
type Usage struct {
	Present            bool
	Malformed          bool
	PromptTokens       int64
	CompletionTokens   int64
	TotalTokens        int64
	CachedPromptTokens int64
	ReasoningTokens    int64
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
			// WORKING ASSUMPTION: ¥0.25/image ≈ $0.035 = 35e9 pUSD. This card
			// only budgets the operator's own wallet gate (reserve == settle, deterministic);
			// the upstream bills its real list price regardless. MUST be reconciled against the
			// official pricing page before launch — the "assumed" ID keeps that debt visible.
			// 工作假设:¥0.25/张≈$0.035=35e9 pUSD。此卡只作 operator 自家钱包预算闸
			// (reserve==settle,确定性成本);上游按真实价目计费不受影响。上线前必须对官方价页
			// 对账——ID 里的 "assumed" 让这笔债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenImageInputLimit, InputPUSD: 35_000_000_000, OutputPUSD: 0}},
		},
		QwenAudio30TTSFlash: {
			ID: "qwen-audio-3.0-tts-flash-assumed-2026-07-28", Provider: ProviderQwen, Model: QwenAudio30TTSFlash,
			InputLimit: QwenTTSInputLimit, OutputLimit: QwenTTSOutputLimit,
			// WORKING ASSUMPTION (model swapped): ¥1 / 10K characters ≈ $0.0000139/char
			// = 14e6 pUSD. The official price page renders its table in JS and could not be read
			// verbatim; the third-party figure is what this card encodes. Same discipline as the
			// image card: this budgets the operator's OWN wallet gate (reserve == settle, cost is
			// deterministic in the request's own character count) while the upstream bills its real
			// list price regardless. The "assumed" ID keeps the reconciliation debt visible.
			// 工作假设:¥1/万字符≈$0.0000139/字符=14e6 pUSD。官方价目表由 JS 渲染、取不到
			// 逐字数值,此卡编码的是第三方数字。与图像卡同纪律:它只作 operator 自家钱包闸(reserve==
			// settle,成本在本请求字符数上确定),上游按真实价目计费不受影响。ID 里的 "assumed" 让这
			// 笔对账债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenTTSInputLimit, InputPUSD: 14_000_000, OutputPUSD: 0}},
		},
		Wan27T2V: {
			ID: "wan2.7-t2v-assumed-2026-07-27", Provider: ProviderQwen, Model: Wan27T2V,
			InputLimit: QwenVideoInputLimit, OutputLimit: QwenVideoOutputLimit,
			// WORKING ASSUMPTION: ¥0.6 per second at 720P ≈ $0.083 = 83e9 pUSD.
			// Video is the most expensive thing this gateway can be asked to do — a single 5-second
			// clip costs more than a whole day's image allowance — so this card is also the one
			// whose reconciliation matters most. The "assumed" ID keeps that debt visible.
			// 工作假设:720P ¥0.6/秒 ≈ $0.083 = 83e9 pUSD。视频是本网关能被要求做的最贵的事——
			// 一条 5 秒片子比一整天的图像额度还贵——故这张卡也是最该对账的一张。ID 里的 "assumed" 让
			// 这笔债保持可见。
			tiers: []pricingTier{{InputUpperBound: QwenVideoInputLimit, InputPUSD: 83_000_000_000, OutputPUSD: 0}},
		},
		Wan27I2V: {
			ID: "wan2.7-i2v-2026-04-25-720p-conservative-2026-08-06", Provider: ProviderQwen, Model: Wan27I2V,
			InputLimit: QwenVideoInputLimit, OutputLimit: QwenVideoOutputLimit,
			// Official list prices verified 2026-08-06: CNY 0.6/s in Beijing and
			// CNY 0.74942/s in Singapore at 720P. The card uses a conservative
			// USD conversion of the higher regional price ($0.105/s). Input frames
			// are free; only output duration is billed. Managed animation is held
			// to 720P until resolution becomes part of the frozen price identity.
			tiers: []pricingTier{{InputUpperBound: QwenVideoInputLimit, InputPUSD: 105_000_000_000, OutputPUSD: 0}},
		},
		QwenTTSClone: {
			ID: "qwen-tts-clone-2026-07-28", Provider: ProviderQwen, Model: QwenTTSClone,
			InputLimit: QwenVoiceInputLimit, OutputLimit: QwenVoiceOutputLimit,
			// $0.2 per voice created = 200e9 pUSD. NOT an "assumed" card: this figure is printed
			// verbatim on the official pricing page (官方价目表核对 2026-07-28), which is why the ID
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
			// (官方价目表核对 2026-07-28),故 ID 里没有 `assumed-` 标记——另外三张生成卡都有,而这里
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
	if inputClass == InputStandard {
		if card.OutputLimit == 0 {
			return Plan{}, ErrUnknownRateCard
		}
	} else {
		u, known := unitClasses[inputClass]
		_, modelAllowed := u.models[model]
		if !known || provider != ProviderQwen || !modelAllowed ||
			outputBound != 0 || promptBound < u.minUnits {
			return Plan{}, ErrUnknownRateCard
		}
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

// NewUnitPlan prices a non-token capability by ITS OWN unit — seconds of audio,
// images, characters, seconds of video, enrolled voices. The unit is whatever the
// provider actually bills, which is why none of these can be expressed as tokens.
//
// Every one of them is deterministic before the call: the requested quantity IS
// the billed quantity, so a successful capability settles for exactly what it
// reserved. That is the property that lets four different products share one
// reservation shape.
//
// NewUnitPlan 按**各自的单位**为非 token 能力定价——音频秒数、图张数、字符数、视频秒数、已登记
// 音色个数。单位取的是 provider 真正计费的那个东西,这正是它们都表达不成 token 的原因。
//
// 它们全都在调用**之前**就已确定:请求的量**就是**计费的量,故一次成功的能力调用结算额恰等于它
// 预留的额。正是这个性质,让四个不同的产品共用同一种预留形状。
func NewUnitPlan(provider Provider, model string, class InputClass, units int64) (Plan, error) {
	return NewPlan(provider, model, class, units, 0)
}

// UnitCost converts an authoritative unit count under a frozen unit plan.
//
// The caller must NAME the unit it is passing, and it must match the plan's own
// class. That parameter is not ceremony: a bare count carries no unit, and the
// five methods this replaced each encoded theirs in the method name — so
// dropping it would have let a video reservation be settled as if the number
// were characters. Video is the most expensive card here, which makes the
// crossover both the easiest to make and the costliest to make.
//
// It is also deliberately separate from Cost(Usage), so a token vector and a
// unit count can never be settled through each other's arithmetic.
//
// UnitCost 在冻结的单位 plan 下换算权威单位数。
//
// 调用方**必须说出**自己传的是什么单位,且它必须与 plan 自己的品类一致。这个参数不是客套:一个
// 裸计数**不携带单位**,而它取代的那五个方法各自把单位编码在方法名里——去掉它,就会让一次视频预留
// 被当成字符数来结算。视频是这里最贵的卡,这让穿越既最容易犯、也最贵。
//
// 它同样与 Cost(Usage) **刻意分开**,使 token 向量与单位计数永远走不进对方的算术。
func (p Plan) UnitCost(class InputClass, units int64) (int64, error) {
	if _, ok := unitClasses[class]; !ok || p.InputClass != class {
		return 0, ErrUnknownRateCard
	}
	if units < 0 || units > p.card.InputLimit {
		return 0, ErrUnknownRateCard
	}
	tier, ok := p.card.tierForInput(units)
	if !ok {
		return 0, ErrUnknownRateCard
	}
	return checkedMul(units, tier.InputPUSD)
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
	if u.CachedPromptTokens > prompt {
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

	// The compatibility response may omit cache details. Treat such prompt tokens
	// as cache misses; when it reports cached tokens, charge the exact provider
	// cache-hit rate without ever undercharging unknown tokens.
	hit := nonNegative(u.CachedPromptTokens)
	hitCost, err := checkedMul(hit, tier.CacheHitInputPUSD)
	if err != nil {
		return 0, false, err
	}
	missCost, err := checkedMul(prompt-hit, tier.InputPUSD)
	if err != nil {
		return 0, false, err
	}
	inputCost, err := checkedAdd(hitCost, missCost)
	if err != nil {
		return 0, false, err
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
