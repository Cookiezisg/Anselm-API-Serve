// Package chat is the PURE chat-completions request domain: the inbound/upstream
// body shapes, the strict whitelist decode + n>1 reject, the SEC-1 shape gate,
// the conservative token estimate, the max_tokens clamp, model resolution, and
// the upstream-body sanitizer. It has ZERO I/O — stdlib + encoding/json only, NO
// net/http, NO os, NO sql. The app layer (app/chat) drives the saga over these
// pure transforms; transport renders the result. Keeping the decode/sanitize/
// estimate here — the innermost layer — means the production path and the fuzz
// target share ONE whitelist with no twin to drift (the §5.2 security boundary).
//
// ⚠ tools passthrough (GW): unlike the pre-fix _legacy proxy which STRIPPED
// tools, the whitelist here passes tools/tool_choice/tool_calls VERBATIM so the
// multi-turn tool loop works end-to-end (this shipped to prod). Everything NOT
// on the whitelist (logit_bias / function_call / response_format / top_p / n>1)
// is still dropped or rejected by construction.
package chat

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// BodyDecodeLimit is the contract body-size cap (256KiB, §5.3): transport wraps
// the request in a MaxBytesReader at this bound, and the handler re-caps to it
// defensively. Exported so the one number has a single home both layers cite.
const BodyDecodeLimit int64 = 256 * 1024

// chatMessage mirrors only the whitelisted message fields. tool_calls /
// tool_call_id / name are preserved verbatim (multi-turn tool loop, GW): a tool
// turn carries tool_calls on the assistant message and tool_call_id+name on the
// tool result, so stripping them breaks the loop. ToolCalls stays json.RawMessage
// so the gateway neither parses nor reshapes the provider's tool-call schema.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// ReasoningContent is the thinking-model trace. deepseek-v4-flash REQUIRES the
	// assistant turn's reasoning_content be passed back on the next request when
	// that turn made a tool call ("reasoning_content in the thinking mode must be
	// passed back"), so the gateway whitelists it and forwards it verbatim — else
	// the multi-turn agentic loop 400s. Opaque RawMessage (string|null), omitempty.
	ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
}

// MarshalJSON omits content on an assistant tool-call turn that carries no text:
// the OpenAI-canonical echo of a tool call is {role, tool_calls} with content
// null/absent (the model emitted null content). A literal "content":"" alongside
// tool_calls is rejected by stricter upstreams (DeepSeek v4-flash → 400), which
// breaks the multi-turn agentic loop. content stays a plain string field for the
// estimate/shape guards; only the wire form drops the empty value here. All other
// messages marshal normally (content is required for user/tool turns).
func (m chatMessage) MarshalJSON() ([]byte, error) {
	if len(m.ToolCalls) > 0 && m.Content == "" {
		return json.Marshal(struct {
			Role             string          `json:"role"`
			Name             string          `json:"name,omitempty"`
			ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
			ToolCallID       string          `json:"tool_call_id,omitempty"`
			ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
		}{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, ReasoningContent: m.ReasoningContent})
	}
	type wire chatMessage // alias breaks the MarshalJSON recursion
	return json.Marshal(wire(m))
}

// InboundRequest mirrors ONLY the whitelisted top-level fields. Unknown fields
// are dropped by NOT being declared (strict whitelist, §5.2). n is decoded only
// to reject n>1. Tools/ToolChoice are kept as RawMessage and forwarded verbatim.
type InboundRequest struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	MaxTokens   *int64          `json:"max_tokens"`
	N           *int            `json:"n"`
	Tools       json.RawMessage `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
}

// UpstreamRequest is the sanitized body forwarded upstream: ONLY whitelisted
// fields + gateway-forced stream_options. Tools/ToolChoice are omitempty so a
// non-tool request forwards an identical body to before.
type UpstreamRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature,omitempty"`
	MaxTokens     int64           `json:"max_tokens"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// DecodeInbound is the strict public-facing whitelist gate, shared by the app
// path and the fuzz target so both exercise IDENTICAL reject logic (no twin to
// drift). It rejects n>1 via the UNION of two checks because they disagree on
// edge inputs: (a) a strict json.Unmarshal probe catches n>1 in a clean
// single-object body; (c) the streaming json.Decoder typed path catches n>1 even
// when trailing tokens follow the object (Unmarshal would error and skip the
// probe there). (b) strictly decodes into the whitelist struct — unknown fields
// are dropped by not being declared. Any non-nil APIError ⇒ REJECTED, never
// forwarded. Never panics on any input (fuzz-safe).
func DecodeInbound(body []byte) (InboundRequest, *apierr.APIError) {
	// (a) Strict n-probe: catches n>1 in a clean single-object body.
	var probe struct {
		N *int `json:"n"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.N != nil && *probe.N > 1 {
		return InboundRequest{}, errNAllowed()
	}

	// (b) Strict decode into the whitelist struct. A field absent from the struct
	// is silently dropped — that IS the whitelist (logit_bias / function_call /
	// response_format / top_p never survive because they are never declared).
	var in InboundRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&in); err != nil {
		return InboundRequest{}, apierr.ErrBadRequest
	}

	// (c) Typed n>1 reject.
	if in.N != nil && *in.N > 1 {
		return InboundRequest{}, errNAllowed()
	}
	return in, nil
}

// errNAllowed is the explicit n>1 reject (a distinct message from the generic
// bad-body, so the client knows precisely what to fix).
func errNAllowed() *apierr.APIError {
	return apierr.NewError(apierr.ErrBadRequest.Status, "BAD_REQUEST", "n>1 is not allowed")
}

// MessagesEmpty reports whether the decoded request carries no messages — a
// distinct 400 ("messages is required"), kept here so the gate order in app/chat
// reads as a list of pure predicates.
func (in InboundRequest) MessagesEmpty() bool { return len(in.Messages) == 0 }

// CheckMessageShape is the SEC-1 deep input guardrail: reject a messages array
// exceeding maxMessages elements, or any single message whose content exceeds
// maxChars RUNES (consistent with EstimatePromptTokens' char semantics). Returns
// "" when acceptable, else a client-safe BAD_REQUEST message. These two O(1)/
// O(len) checks run BEFORE the full token estimate so a hostile array/message is
// rejected cheaply (海量 tiny message 在计费边界放大成本 OWASP-API4;单巨消息挡单条).
func CheckMessageShape(msgs []chatMessage, maxMessages, maxChars int) string {
	if len(msgs) > maxMessages {
		return "too many messages"
	}
	for _, m := range msgs {
		if utf8.RuneCountInString(m.Content) > maxChars {
			return "message content too large"
		}
	}
	return ""
}

// EstimateRawTokens conservatively over-estimates the token cost of an opaque
// raw JSON blob (the tools array): max(bytes/3, runes) + a 20% margin, rounded
// up. Used to fold the tools payload into the prompt estimate so the input cap
// and the reservation both account for a large tool schema (a 50-tool definition
// is real prompt weight upstream bills for). 0 for empty/nil.
func EstimateRawTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	b := int64(len(raw))
	r := int64(utf8.RuneCount(raw))
	est := (b + 2) / 3 // ceil(bytes/3)
	if r > est {
		est = r
	}
	est = (est*12 + 9) / 10 // +20%, ceil
	return est
}

// EstimatePromptTokens conservatively over-estimates the prompt token count. It
// is the SHARED foundation of three guardrails (input cap, reserve est, over-cap
// bound), so it must stay ≥ the real tokenizer — over-reject is acceptable,
// under-estimate is not. Strategy: per message count UTF-8 runes (CJK ≈ 1 token/
// char, denser than the byte/3 Latin heuristic) AND bytes/3, take the max, add 8
// framing tokens/msg, then a 20% margin rounded up. tool_calls + name are folded
// in per message (they are real prompt weight in a multi-turn tool loop).
func EstimatePromptTokens(msgs []chatMessage) int64 {
	var runes, bytes int64
	for _, m := range msgs {
		bytes += int64(len(m.Role)) + int64(len(m.Content)) + int64(len(m.Name)) + int64(len(m.ToolCalls)) + int64(len(m.ReasoningContent))
		runes += int64(utf8.RuneCountInString(m.Role)) + int64(utf8.RuneCountInString(m.Content))
		runes += int64(utf8.RuneCountInString(m.Name)) + int64(utf8.RuneCount(m.ToolCalls)) + int64(utf8.RuneCount(m.ReasoningContent))
		runes += 8 // per-message structural overhead (role + framing), conservative.
	}
	byteEst := (bytes + 2) / 3 // ceil(bytes/3)
	est := byteEst
	if runes > est {
		est = runes
	}
	est = (est*12 + 9) / 10 // +20%, ceil
	if est < 1 {
		est = 1
	}
	return est
}

// ClampMaxTokens returns the upstream max_tokens: the gateway cap, lowered to a
// positive client request only when that request is strictly smaller. So a
// client can ask for fewer output tokens but never more than the cap.
func ClampMaxTokens(client *int64, capTok int64) int64 {
	if client != nil && *client > 0 && *client < capTok {
		return *client
	}
	return capTok
}

// ResolveModel forces the client model to a whitelisted real model id; an
// unknown/empty model resolves to the default (allowlist[0]). It resolves
// against the request's OWN config snapshot so allowlist membership and the
// default can never disagree mid-request after a MODEL_ALLOWLIST hot-reload. The
// allowlist is a handful of entries, so a linear scan is intrinsically
// consistent and cheaper than a separately-swapped membership set.
func ResolveModel(allowlist []string, defaultModel, client string) string {
	for _, m := range allowlist {
		if m == client {
			return client
		}
	}
	return defaultModel
}

// SanitizeUpstream builds the forwarded body from a decoded inbound request: ONLY
// whitelisted fields survive (model[rewritten] / messages / stream / temperature
// / clamped max_tokens / tools / tool_choice) plus the gateway-forced
// stream_options.include_usage on a stream (so the upstream emits a final usage
// frame we settle against). Everything else the client sent is dropped by
// construction — this is the single sanitization path the app + fuzz share.
// Tools/ToolChoice pass through verbatim (GW); they are NOT inspected or reshaped.
func SanitizeUpstream(in InboundRequest, model string, maxTok int64) UpstreamRequest {
	out := UpstreamRequest{
		Model:       model,
		Messages:    in.Messages,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		MaxTokens:   maxTok,
		Tools:       in.Tools,
		ToolChoice:  in.ToolChoice,
	}
	if in.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return out
}

// ShapeError runs CheckMessageShape against the request's own messages — a thin
// accessor so app/chat needn't reach into the unexported slice.
func (in InboundRequest) ShapeError(maxMessages, maxChars int) string {
	return CheckMessageShape(in.Messages, maxMessages, maxChars)
}

// PromptEstimate is the full input-side estimate: prompt tokens (messages) plus
// the tools-array cost. Both the input cap check and the reserve est use THIS
// value so the tools payload is never silently free.
func (in InboundRequest) PromptEstimate() int64 {
	return EstimatePromptTokens(in.Messages) + EstimateRawTokens(in.Tools)
}
