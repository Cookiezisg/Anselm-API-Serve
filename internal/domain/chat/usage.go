package chat

import (
	"bytes"
	"encoding/json"
)

// usageObj is the minimal shape we parse from upstream output: only
// usage.total_tokens, never the completion text (privacy zero-body, OBS-1).
type usageObj struct {
	Usage *struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

// ParseUsageLine extracts total_tokens from one streaming SSE line of form
// `data: {...,"usage":{...}}`. Returns -1 when the line carries no usage (a
// content frame, the [DONE] sentinel, or unparseable). The gateway forces
// stream_options.include_usage so a well-behaved stream's final frame carries
// the authoritative count we settle against; absence falls back to full est.
func ParseUsageLine(line []byte) int64 {
	line = bytes.TrimSpace(line)
	const p = "data:"
	if !bytes.HasPrefix(line, []byte(p)) {
		return -1
	}
	payload := bytes.TrimSpace(line[len(p):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return -1
	}
	var u usageObj
	if err := json.Unmarshal(payload, &u); err != nil || u.Usage == nil {
		return -1
	}
	return u.Usage.TotalTokens
}

// ParseUsageBody extracts total_tokens from a non-streaming JSON body. -1 when
// absent/unparseable (the caller then settles full est, conservative).
func ParseUsageBody(body []byte) int64 {
	var u usageObj
	if err := json.Unmarshal(body, &u); err != nil || u.Usage == nil {
		return -1
	}
	return u.Usage.TotalTokens
}

// IsDONE reports whether an SSE line is the terminal [DONE] sentinel, after
// which the relay loop stops. Kept here so the frame protocol lives in one place.
func IsDONE(line []byte) bool { return bytes.Contains(line, []byte("[DONE]")) }
