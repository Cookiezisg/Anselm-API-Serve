// Package model holds the OpenAI-compatible /v1/models catalog shapes (the
// discovery-driven model-list事实源). PURE domain: stdlib only, no config/infra/
// transport — these structs ARE the wire contract the app layer assembles and a
// transport handler later serializes verbatim.
package model

// Model is one entry of the OpenAI /v1/models list. created is deliberately
// OMITTED: the gateway has no per-model creation time, and an absent field is
// valid OpenAI shape — better than inventing a meaningless timestamp.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`   // always "model"
	OwnedBy string `json:"owned_by"` // always "anselm-gateway"
}

// ListEnvelope is the OpenAI list wrapper: {"object":"list","data":[...]}.
type ListEnvelope struct {
	Object string  `json:"object"` // always "list"
	Data   []Model `json:"data"`
}

// These are the fixed wire constants of the catalog shape. Every declared model
// is brokered by the gateway itself (one managed upstream), so OwnedBy is fixed.
const (
	ObjectList  = "list"
	ObjectModel = "model"
	OwnedBy     = "anselm-gateway"
)
