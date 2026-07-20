package chat

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

// rewriteClientModelJSON validates that payload is exactly one complete JSON
// object and rewrites its single top-level "model" value to the provider-neutral
// public model id. It deliberately edits only the value spans discovered by
// encoding/json: nested model fields and all choices/usage/tool-call bytes remain
// byte-for-byte unchanged. Invalid JSON, non-object JSON, or trailing values are
// rejected (ok=false), never returned to a public client.
//
// Duplicate or case-fold-equivalent top-level model keys are rejected. This keeps
// provider identity from reappearing under encoding/json's case-insensitive key
// matching and prevents duplicate-key output amplification.
func rewriteClientModelJSON(payload []byte, publicModelID string) ([]byte, bool) {
	type span struct{ start, end int }

	// encoding/json replaces invalid UTF-8 inside strings with U+FFFD instead of
	// rejecting it. The public wire is stricter: malformed provider bytes never
	// pass merely because the standard decoder can repair them internally.
	if !utf8.Valid(payload) {
		return nil, false
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false
	}

	var modelSpan span
	hasModel := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}
		keyEnd := int(dec.InputOffset())

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		valueEnd := int(dec.InputOffset())
		if !strings.EqualFold(key, "model") {
			continue
		}
		if key != "model" || hasModel {
			return nil, false
		}
		valueStart, ok := valueStartAfterKey(payload, keyEnd, valueEnd)
		if !ok {
			return nil, false
		}
		modelSpan = span{start: valueStart, end: valueEnd}
		hasModel = true
	}

	last, err := dec.Token()
	if err != nil || last != json.Delim('}') {
		return nil, false
	}
	// Decode once more to reject a trailing second JSON value or garbage. JSON
	// whitespace after the object is valid and remains part of the original bytes.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	if !hasModel {
		return payload, true
	}

	publicJSON, err := json.Marshal(publicModelID)
	if err != nil { // strings cannot fail to marshal; retain fail-closed behavior.
		return nil, false
	}
	out := make([]byte, 0, len(payload)-(modelSpan.end-modelSpan.start)+len(publicJSON))
	out = append(out, payload[:modelSpan.start]...)
	out = append(out, publicJSON...)
	out = append(out, payload[modelSpan.end:]...)
	return out, true
}

// valueStartAfterKey locates the start of the JSON value whose decoder-reported
// end is valueEnd. keyEnd is immediately after the decoded key's closing quote.
func valueStartAfterKey(payload []byte, keyEnd, valueEnd int) (int, bool) {
	if keyEnd < 0 || keyEnd > valueEnd || valueEnd > len(payload) {
		return 0, false
	}
	i := keyEnd
	for i < valueEnd && jsonSpace(payload[i]) {
		i++
	}
	if i >= valueEnd || payload[i] != ':' {
		return 0, false
	}
	i++
	for i < valueEnd && jsonSpace(payload[i]) {
		i++
	}
	return i, i < valueEnd
}

func jsonSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// rewriteClientModelSSELine applies the same closed-object boundary to an SSE
// data payload while preserving the event prefix and spacing. The exact [DONE]
// sentinel and blank event separators are accepted byte-for-byte. Comment
// heartbeats are normalized to a bare colon so provider-owned comment text can
// never expose a model/account identifier. event/id/retry fields are outside the
// data-only Chat Completions contract and fail closed. Every ordinary data
// payload must be exactly one JSON object.
func rewriteClientModelSSELine(line []byte, publicModelID string) ([]byte, bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r') {
		i++
	}
	if i == len(line) {
		return line, true
	}
	const prefix = "data:"
	// Per SSE parsing rules, a field line without ':' has an empty value. A bare
	// data field is therefore a data payload (and invalid here), not a control
	// line that may pass unchecked. Scanner leaves a CR on CRLF input.
	if bytes.Equal(bytes.TrimSuffix(line[i:], []byte{'\r'}), []byte("data")) {
		return nil, false
	}
	if !bytes.HasPrefix(line[i:], []byte(prefix)) {
		if line[i] == ':' {
			return []byte(":"), true
		}
		return nil, false
	}
	i += len(prefix)
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if bytes.Equal(bytes.TrimSpace(line[i:]), []byte("[DONE]")) {
		return line, true
	}

	payload, valid := rewriteClientModelJSON(line[i:], publicModelID)
	if !valid {
		return nil, false
	}
	if len(payload) == len(line[i:]) && bytes.Equal(payload, line[i:]) {
		return line, true
	}
	out := make([]byte, 0, i+len(payload))
	out = append(out, line[:i]...)
	out = append(out, payload...)
	return out, true
}
