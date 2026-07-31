// Package model holds the OpenAI-compatible /v1/models catalog shapes (the
// discovery-driven model-list source of truth). PURE domain: stdlib only, no config/infra/
// transport — these structs ARE the wire contract the app layer assembles and a
// transport handler later serializes verbatim.
package model

// Model is one entry of the OpenAI /v1/models list. created is deliberately
// OMITTED: the gateway has no per-model creation time, and an absent field is
// valid OpenAI shape — better than inventing a meaningless timestamp.
type Model struct {
	ID                 string              `json:"id"`
	Object             string              `json:"object"`   // always "model"
	OwnedBy            string              `json:"owned_by"` // always "anselm-gateway"
	AnselmCapabilities *AnselmCapabilities `json:"anselm_capabilities,omitempty"`
}

// RouteProfile publishes the exact input/output envelope for one deterministic
// gateway route. InputLimit is intentionally separate from OutputLimit: clients
// must not guess whether a provider's marketing "context window" includes the
// reserved completion.
type RouteProfile struct {
	InputLimit  int64 `json:"input_limit"`
	OutputLimit int64 `json:"output_limit"`
	Available   bool  `json:"available"`
}

// GenProfile publishes one generation capability. It deliberately does NOT
// reuse RouteProfile: token limits are meaningless for per-artifact generation —
// what a client needs is availability and the daily per-install cap (0 =
// uncapped). The cap's UNIT is the capability's own (images for image,
// characters for speech), which is why each capability carries its own profile
// instead of sharing one number.
//
// GenProfile 发布一项生成能力。刻意不复用 RouteProfile:token 上限对按件生成无意义——客户端要的
// 是可用性与日 per-install 上限(0=不设)。上限的**单位**随能力本身(图像按张、语音按字符),这
// 正是每项能力各带一份 profile、而非共用一个数的原因。
type GenProfile struct {
	Available  bool  `json:"available"`
	DailyLimit int64 `json:"daily_limit"`
}

// AnselmCapabilities is a namespaced, backwards-compatible extension on the
// OpenAI model object. Unknown fields are ignored by generic clients; Anselm
// uses it to select a context budget per concrete outbound request.
// ImageGeneration is additive (version stays 1): an old desktop ignores it, an
// old gateway omits it and the new desktop reads nil = unavailable — rolling
// compatibility in both directions.
//
// ImageGeneration 为增量字段(version 仍 1):旧桌面忽略之,旧网关缺席之而新桌面读 nil=不可用
// ——双向滚动兼容。
type AnselmCapabilities struct {
	Version          int          `json:"version"`
	Routing          string       `json:"routing"`
	Text             RouteProfile `json:"text"`
	Multimodal       RouteProfile `json:"multimodal"`
	ImageGeneration  *GenProfile  `json:"image_generation,omitempty"`
	SpeechGeneration *GenProfile  `json:"speech_generation,omitempty"`
	VideoGeneration  *GenProfile  `json:"video_generation,omitempty"`
}

// ListEnvelope is the OpenAI list wrapper: {"object":"list","data":[...]}.
type ListEnvelope struct {
	Object string  `json:"object"` // always "list"
	Data   []Model `json:"data"`
}

// These are the fixed wire constants of the catalog shape. Every declared model
// is brokered by the gateway itself (one managed upstream), so OwnedBy is fixed.
const (
	ObjectList       = "list"
	ObjectModel      = "model"
	OwnedBy          = "anselm-gateway"
	RoutingByContent = "content"
)
