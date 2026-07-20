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
	"io"
	"strings"
	"unicode/utf8"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// BodyDecodeLimit is the contract body-size cap (256KiB, §5.3): transport wraps
// the request in a MaxBytesReader at this bound, and the handler re-caps to it
// defensively. Exported so the one number has a single home both layers cite.
const BodyDecodeLimit int64 = 256 * 1024

// InboundRequest mirrors ONLY the whitelisted top-level fields. Unknown fields
// are dropped by NOT being declared (strict whitelist, §5.2). n is decoded only
// to reject n>1. Tools/ToolChoice are kept as RawMessage and forwarded verbatim.
type InboundRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	MaxTokens   *int64          `json:"max_tokens"`
	N           *int            `json:"n"`
	Tools       json.RawMessage `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
}

// CompletionRequest is the sanitized, provider-independent Chat Completions
// body. It intentionally has no model: deterministic routing chooses a provider
// first, and the provider adapter owns its real model id. This prevents a client
// model string (or one provider's model namespace) from leaking into routing.
type CompletionRequest struct {
	Messages      []Message       `json:"messages"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature,omitempty"`
	MaxTokens     int64           `json:"max_tokens"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
}

// UpstreamRequest is the compatibility wire wrapper for an OpenAI-compatible
// provider. New code should keep CompletionRequest canonical until its provider
// adapter calls WithModel.
type UpstreamRequest struct {
	Model string `json:"model"`
	CompletionRequest
}

// WithModel attaches a provider-owned model id to the canonical request.
func (r CompletionRequest) WithModel(model string) UpstreamRequest {
	return UpstreamRequest{Model: model, CompletionRequest: r}
}

// StreamOptions asks an OpenAI-compatible provider to include a final usage
// frame, which the accounting saga needs for settlement.
type StreamOptions struct {
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
	// encoding/json historically replaces malformed UTF-8 inside decoded string
	// fields with U+FFFD, while json.RawMessage keeps the original bytes. That
	// split would make opaque tools/tool calls cost fewer estimated bytes than the
	// provider may tokenize after normalization (one invalid byte can become the
	// three-byte replacement scalar). Require the JSON transport itself to be
	// valid UTF-8 so decode, forwarding, and byte-level billing share one meaning.
	if !utf8.Valid(body) {
		return InboundRequest{}, apierr.ErrBadRequest
	}

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
	// Exactly one JSON value is the wire contract. json.Decoder.Decode alone
	// would accept a valid object followed by arbitrary bytes, creating an
	// ambiguous request that proxies and audit tooling may parse differently.
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return InboundRequest{}, apierr.ErrBadRequest
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
func CheckMessageShape(msgs []Message, maxMessages, maxChars int) string {
	if len(msgs) > maxMessages {
		return "too many messages"
	}
	for _, m := range msgs {
		if m.Content.textRunes() > maxChars {
			return "message content too large"
		}
	}
	return ""
}

// EstimateRawTokens returns a byte-level upper bound for an opaque JSON value.
// A tokenizer may split one multi-byte Unicode scalar into several byte tokens,
// so rune count and bytes/3 are only heuristics; one token per input byte is the
// conservative fallback bound. The provider parses this JSON before applying
// its chat template, therefore punctuation/escaping bytes only over-reserve.
// 0 is retained for an absent field.
func EstimateRawTokens(raw json.RawMessage) int64 {
	return int64(len(raw))
}

// EstimatePromptTokens conservatively over-estimates the prompt token count. It
// is the SHARED foundation of three guardrails (input cap, reserve est, over-cap
// bound), so it must stay ≥ the real tokenizer — over-reject is acceptable,
// under-estimate is not. Every accepted text value is valid UTF-8 after JSON
// decode, so its byte length bounds byte-fallback tokenization. We add a large,
// explicit per-message allowance for provider-owned role/chat framing. Opaque
// tool_calls, tool_call_id, name, and reasoning_content bytes are all included.
func EstimatePromptTokens(msgs []Message) int64 {
	return estimatePromptTokens(msgs, true)
}

// EstimateTextPromptTokens estimates only text/tool context. It deliberately
// excludes inline media's base64 payload: Gemini tokenizes decoded media by
// image tiles or audio duration, not as base64 text. Media has separate strict
// part/decoded-byte limits and the Gemini billing plan reserves the complete
// model input hard limit, so treating encoded bytes as text would only create
// false rejections for ordinary screenshots and audio clips.
func EstimateTextPromptTokens(msgs []Message) int64 {
	return estimatePromptTokens(msgs, false)
}

func estimatePromptTokens(msgs []Message, includeMediaPayload bool) int64 {
	// Current OpenAI-style templates use single-digit framing tokens per turn.
	// 64 leaves substantial room for provider template evolution while keeping
	// the reservation useful for concurrent admission control.
	const perMessageFramingUpperBound int64 = 64
	const requestFramingUpperBound int64 = 64

	bytes := requestFramingUpperBound
	for _, m := range msgs {
		contentBytes, _ := m.Content.estimateBytesAndRunes(includeMediaPayload)
		bytes += int64(len(m.Role)) + contentBytes + int64(len(m.Name)) +
			int64(len(m.ToolCalls)) + int64(len(m.ToolCallID)) +
			int64(len(m.ReasoningContent)) + perMessageFramingUpperBound
	}
	return bytes
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

// Sanitize builds the provider-independent body from a decoded inbound request:
// ONLY whitelisted fields survive (messages / stream / temperature / clamped
// max_tokens / tools / tool_choice) plus the gateway-forced
// stream_options.include_usage on a stream (so the upstream emits a final usage
// frame we settle against). Everything else the client sent is dropped by
// construction — this is the single sanitization path the app + fuzz share.
// Tools/ToolChoice pass through verbatim (GW); they are NOT inspected or reshaped.
func Sanitize(in InboundRequest, maxTok int64) CompletionRequest {
	out := CompletionRequest{
		Messages:    canonicalMessages(in.Messages),
		Stream:      in.Stream,
		Temperature: in.Temperature,
		MaxTokens:   maxTok,
		Tools:       in.Tools,
		ToolChoice:  in.ToolChoice,
	}
	if in.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	return out
}

// canonicalMessages collapses text-only part arrays to their equivalent string
// form. That keeps the canonical request provider-independent: text providers
// which only accept string content receive the Text route without a second
// provider-specific interpretation, while any array containing media remains an
// ordered parts array for the multimodal provider.
func canonicalMessages(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i := range out {
		if out[i].Content.kind != ContentKindParts {
			continue
		}
		var text strings.Builder
		textOnly := true
		for _, part := range out[i].Content.parts {
			if part.Type != PartTypeText {
				textOnly = false
				break
			}
			text.WriteString(part.Text)
		}
		if textOnly {
			out[i].Content = StringContent(text.String())
		}
	}
	return out
}

// SanitizeUpstream is the compatibility wrapper for the existing app path.
// Provider-aware callers should use Sanitize and attach a model only inside the
// selected adapter.
func SanitizeUpstream(in InboundRequest, model string, maxTok int64) UpstreamRequest {
	return Sanitize(in, maxTok).WithModel(model)
}

// ShapeError runs CheckMessageShape against the request's own messages — a thin
// accessor so app/chat needn't reach into the unexported slice.
func (in InboundRequest) ShapeError(maxMessages, maxChars int) string {
	return CheckMessageShape(in.Messages, maxMessages, maxChars)
}

// PromptEstimate is the full input-side estimate: prompt tokens (messages) plus
// both opaque tool-definition fields. Both the input cap check and the reserve
// estimate use THIS value so no forwarded tool JSON is silently free.
func (in InboundRequest) PromptEstimate() int64 {
	return EstimatePromptTokens(in.Messages) + EstimateRawTokens(in.Tools) + EstimateRawTokens(in.ToolChoice)
}

// TextPromptEstimate is the gateway-side context estimate used for a
// multimodal request's INPUT_TOKEN_CAP/model precheck. Binary media is governed
// by MediaLimits and provider tokenization; text and tool schemas remain fully
// counted here.
func (in InboundRequest) TextPromptEstimate() int64 {
	return EstimateTextPromptTokens(in.Messages) + EstimateRawTokens(in.Tools) + EstimateRawTokens(in.ToolChoice)
}

// HasAudio reports whether the validated request contains any input_audio part.
// It is used only to select the conservative Gemini input price class; routing
// itself remains the complete-history Modality returned by ValidateAndClassify.
func (in InboundRequest) HasAudio() bool {
	for _, message := range in.Messages {
		parts, ok := message.Content.Parts()
		if !ok {
			continue
		}
		for _, part := range parts {
			if part.Type == PartTypeInputAudio {
				return true
			}
		}
	}
	return false
}
