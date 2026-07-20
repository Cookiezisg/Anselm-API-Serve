package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// usageObj is the minimal shape we parse from upstream output: only
// usage.total_tokens, never the completion text (privacy zero-body, OBS-1).
type usageObj struct {
	Usage *usageFields `json:"usage"`
}

type usageFields struct {
	PromptTokens          int64 `json:"prompt_tokens"`
	CompletionTokens      int64 `json:"completion_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u usageObj) vector() billing.Usage {
	if u.Usage == nil {
		return billing.Usage{}
	}
	v := billing.Usage{
		Present: true, PromptTokens: u.Usage.PromptTokens,
		CompletionTokens: u.Usage.CompletionTokens, TotalTokens: u.Usage.TotalTokens,
		PromptCacheHitTokens:  u.Usage.PromptCacheHitTokens,
		PromptCacheMissTokens: u.Usage.PromptCacheMissTokens,
	}
	if u.Usage.PromptTokensDetails != nil {
		v.CachedPromptTokens = u.Usage.PromptTokensDetails.CachedTokens
	}
	if u.Usage.CompletionTokensDetails != nil {
		v.ReasoningTokens = u.Usage.CompletionTokensDetails.ReasoningTokens
	}
	v.Malformed = v.PromptTokens < 0 || v.CompletionTokens < 0 || v.TotalTokens < 0 ||
		v.PromptCacheHitTokens < 0 || v.PromptCacheMissTokens < 0 ||
		v.CachedPromptTokens < 0 || v.ReasoningTokens < 0
	return v
}

// ParseUsageLine extracts total_tokens from one streaming SSE line of form
// `data: {...,"usage":{...}}`. Returns -1 when the line carries no usage (a
// content frame, the [DONE] sentinel, or unparseable). The gateway forces
// stream_options.include_usage so a well-behaved stream's final frame carries
// the authoritative count we settle against; absence falls back to full est.
func ParseUsageLine(line []byte) int64 {
	v := ParseUsageSnapshotLine(line)
	if !v.Present || v.Malformed {
		return -1
	}
	return v.TotalTokens
}

// ParseUsageSnapshotLine extracts the complete cumulative billing vector from
// one SSE data frame. Unknown fields/content are ignored and never logged.
func ParseUsageSnapshotLine(line []byte) billing.Usage {
	line = bytes.TrimSpace(line)
	const p = "data:"
	if !bytes.HasPrefix(line, []byte(p)) {
		return billing.Usage{}
	}
	payload := bytes.TrimSpace(line[len(p):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return billing.Usage{}
	}
	return parseUsagePayload(payload)
}

// ParseUsageBody extracts total_tokens from a non-streaming JSON body. -1 when
// absent/unparseable (the caller then settles full est, conservative).
func ParseUsageBody(body []byte) int64 {
	v := ParseUsageSnapshotBody(body)
	if !v.Present || v.Malformed {
		return -1
	}
	return v.TotalTokens
}

// ParseUsageSnapshotBody is the non-stream sibling returning the structured
// cumulative billing vector.
func ParseUsageSnapshotBody(body []byte) billing.Usage {
	return parseUsagePayload(body)
}

// parseUsagePayload first isolates the usage member so a syntactically present
// but malformed object becomes sticky malformed evidence. Decoding the whole
// completion directly would collapse a bad usage field into "absent" and a later
// streaming frame could then incorrectly authorize a refund.
func parseUsagePayload(payload []byte) billing.Usage {
	rawUsage, present, malformed := extractUsage(payload)
	if malformed {
		return billing.Usage{Present: present, Malformed: true}
	}
	if !present {
		return billing.Usage{}
	}
	raw := bytes.TrimSpace(rawUsage)
	if bytes.Equal(raw, []byte("null")) {
		return billing.Usage{}
	}
	// encoding/json is last-key-wins, including case-insensitive struct-field
	// matches. A duplicate could hide a negative token count in the same frame,
	// defeating sticky malformed evidence. Reject duplicate keys recursively in
	// the usage subtree before typed decoding.
	if !uniqueJSONKeys(raw) {
		return billing.Usage{Present: true, Malformed: true}
	}
	var fields usageFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return billing.Usage{Present: true, Malformed: true}
	}
	return usageObj{Usage: &fields}.vector()
}

// extractUsage reads exactly one top-level object and isolates its usage member
// without last-key-wins semantics. Case variants are equivalent because
// encoding/json matches tagged struct fields case-insensitively.
func extractUsage(payload []byte) (raw json.RawMessage, present, malformed bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false, true
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, present, true
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, present, true
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, present, true
		}
		if !strings.EqualFold(key, "usage") {
			continue
		}
		if present {
			return nil, true, true
		}
		present = true
		raw = append(raw[:0], value...)
	}
	last, err := dec.Token()
	if err != nil || last != json.Delim('}') {
		return nil, present, true
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, present, true
	}
	return raw, present, false
}

var errDuplicateJSONKey = errors.New("duplicate JSON object key")

func uniqueJSONKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeUniqueJSONValue(dec); err != nil {
		return false
	}
	var trailing json.RawMessage
	return dec.Decode(&trailing) == io.EOF
}

func consumeUniqueJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errDuplicateJSONKey
			}
			// Compatibility usage fields are ASCII. Reject non-ASCII keys so
			// encoding/json's Unicode simple-fold aliases (for example long-s)
			// cannot assign two spellings to the same billing field last-key-wins.
			for i := 0; i < len(key); i++ {
				if key[i] >= 0x80 {
					return errDuplicateJSONKey
				}
			}
			folded := strings.ToLower(key)
			if _, exists := seen[folded]; exists {
				return errDuplicateJSONKey
			}
			seen[folded] = struct{}{}
			if err := consumeUniqueJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return errDuplicateJSONKey
		}
		return nil
	case '[':
		for dec.More() {
			if err := consumeUniqueJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return errDuplicateJSONKey
		}
		return nil
	default:
		return errDuplicateJSONKey
	}
}

// IsDONE reports whether an SSE line is exactly the terminal [DONE] sentinel.
// A model is allowed to emit the literal text "[DONE]" inside a JSON delta; a
// substring check would truncate that valid response and lose its usage frame.
func IsDONE(line []byte) bool {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(line[len("data:"):]), []byte("[DONE]"))
}
