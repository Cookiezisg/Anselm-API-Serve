package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeInbound_ToolsAndToolChoiceSurvive(t *testing.T) {
	body := []byte(`{
		"model":"x","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],
		"tool_choice":"auto"
	}`)
	in, ae := DecodeInbound(body)
	if ae != nil {
		t.Fatalf("unexpected reject: %v", ae)
	}
	if len(in.Tools) == 0 || len(in.ToolChoice) == 0 {
		t.Fatalf("tools/tool_choice dropped: tools=%q choice=%q", in.Tools, in.ToolChoice)
	}
	out := SanitizeUpstream(in, "real", 100)
	raw, _ := json.Marshal(out)
	s := string(raw)
	if !strings.Contains(s, `"tools"`) || !strings.Contains(s, `"tool_choice"`) {
		t.Fatalf("sanitize dropped tools/tool_choice: %s", s)
	}
	if !strings.Contains(s, `"include_usage":true`) {
		t.Fatalf("stream missing forced include_usage: %s", s)
	}
}

func TestSanitize_ToolCallsAndNamePreservedPerMessage(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function"}]},
		{"role":"tool","content":"result","tool_call_id":"c1","name":"f"}
	]}`)
	in, ae := DecodeInbound(body)
	if ae != nil {
		t.Fatalf("reject: %v", ae)
	}
	out := SanitizeUpstream(in, "real", 50)
	raw, _ := json.Marshal(out)
	s := string(raw)
	for _, want := range []string{`"tool_calls"`, `"tool_call_id":"c1"`, `"name":"f"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestSanitize_DangerFieldsStripped(t *testing.T) {
	body := []byte(`{
		"model":"x","messages":[{"role":"user","content":"hi"}],
		"logit_bias":{"1":5},"function_call":"auto",
		"response_format":{"type":"json"},"top_p":0.1,"frequency_penalty":2
	}`)
	in, ae := DecodeInbound(body)
	if ae != nil {
		t.Fatalf("reject: %v", ae)
	}
	out := SanitizeUpstream(in, "real", 50)
	raw, _ := json.Marshal(out)
	s := string(raw)
	for _, banned := range []string{"logit_bias", "function_call", "response_format", "top_p", "frequency_penalty"} {
		if strings.Contains(s, banned) {
			t.Fatalf("danger field %q leaked: %s", banned, s)
		}
	}
}

func TestDecodeInbound_NGreaterThanOneRejected(t *testing.T) {
	cases := []string{
		`{"model":"x","messages":[{"role":"user","content":"a"}],"n":2}`,
		// Trailing bytes after a valid object: json.Decoder (typed path) tolerates
		// the trailing tokens and decodes the first object, so the TYPED n>1 reject
		// must still fire on the n inside that first object.
		`{"model":"x","messages":[{"role":"user","content":"a"}],"n":3} trailing`,
	}
	for _, c := range cases {
		_, ae := DecodeInbound([]byte(c))
		if ae == nil || ae.Status != 400 {
			t.Fatalf("expected 400 n>1 reject for %q, got %v", c, ae)
		}
	}
	// n==1 is allowed.
	if _, ae := DecodeInbound([]byte(`{"model":"x","messages":[{"role":"user","content":"a"}],"n":1}`)); ae != nil {
		t.Fatalf("n==1 must pass: %v", ae)
	}
}

func TestDecodeInbound_RejectsTrailingJSONOrGarbage(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"a"}]} trailing`,
		`{"messages":[{"role":"user","content":"a"}]} {}`,
		`{"messages":[{"role":"user","content":"a"}]} null`,
	} {
		if _, ae := DecodeInbound([]byte(body)); ae == nil || ae.Status != 400 {
			t.Fatalf("trailing payload must be rejected: body=%q err=%v", body, ae)
		}
	}
}

func TestDecodeInboundRejectsInvalidUTF8BeforeOpaqueRawFields(t *testing.T) {
	body := append([]byte(`{"messages":[{"role":"user","content":"ok"}],"tools":[{"description":"`), 0xff)
	body = append(body, []byte(`"}]}`)...)
	if utf8.Valid(body) {
		t.Fatal("test fixture unexpectedly contains valid UTF-8")
	}
	if _, ae := DecodeInbound(body); ae == nil || ae.Status != 400 {
		t.Fatalf("invalid UTF-8 must fail closed before RawMessage passthrough: %v", ae)
	}
}

func TestCheckMessageShape(t *testing.T) {
	ok := []Message{{Role: "user", Content: StringContent("hi")}}
	if msg := CheckMessageShape(ok, 10, 100); msg != "" {
		t.Fatalf("expected accept, got %q", msg)
	}
	many := make([]Message, 11)
	if msg := CheckMessageShape(many, 10, 100); msg != "too many messages" {
		t.Fatalf("expected too many messages, got %q", msg)
	}
	big := []Message{{Role: "user", Content: StringContent(strings.Repeat("x", 101))}}
	if msg := CheckMessageShape(big, 10, 100); msg != "message content too large" {
		t.Fatalf("expected content too large, got %q", msg)
	}
	// Rune (not byte) semantics: 3 CJK chars = 9 bytes but 3 runes ≤ cap 5.
	cjk := []Message{{Role: "user", Content: StringContent("你好吗")}}
	if msg := CheckMessageShape(cjk, 10, 5); msg != "" {
		t.Fatalf("rune-count cap should accept 3 CJK runes ≤ 5, got %q", msg)
	}
}

func TestEstimate_CountsTools(t *testing.T) {
	msgs := []Message{{Role: "user", Content: StringContent("hello world")}}
	base := EstimatePromptTokens(msgs)
	in := InboundRequest{Messages: msgs, Tools: json.RawMessage(`[{"type":"function","function":{"name":"longtoolname","description":"a fairly long description here"}}]`)}
	withTools := in.PromptEstimate()
	if withTools <= base {
		t.Fatalf("tools must add to estimate: base=%d withTools=%d", base, withTools)
	}
	in.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"longtoolname"}}`)
	withChoice := in.PromptEstimate()
	if withChoice <= withTools {
		t.Fatalf("tool_choice must add to estimate: tools=%d withChoice=%d", withTools, withChoice)
	}
	if got := in.TextPromptEstimate(); got != withChoice {
		t.Fatalf("text request estimates diverged: PromptEstimate=%d TextPromptEstimate=%d", withChoice, got)
	}
}

func TestEstimateCountsToolCallID(t *testing.T) {
	base := EstimatePromptTokens([]Message{{Role: "tool", Content: StringContent("ok"), ToolCallID: "x"}})
	long := EstimatePromptTokens([]Message{{Role: "tool", Content: StringContent("ok"), ToolCallID: strings.Repeat("x", 300)}})
	if long <= base {
		t.Fatalf("tool_call_id must add to estimate: base=%d long=%d", base, long)
	}
}

func TestEstimate_ConservativeCJK(t *testing.T) {
	// A byte-fallback tokenizer can split one CJK scalar into three tokens. The
	// estimate must therefore bound UTF-8 bytes, not merely rune count.
	msgs := []Message{{Role: "user", Content: StringContent(strings.Repeat("中", 100))}}
	est := EstimatePromptTokens(msgs)
	if est < 300 {
		t.Fatalf("CJK estimate must be ≥ UTF-8 byte count (conservative): got %d", est)
	}
}

func TestEstimateBoundsEveryForwardedMessageByte(t *testing.T) {
	msg := Message{
		Role:             "assistant",
		Content:          StringContent("正文🙂"),
		Name:             "worker",
		ToolCalls:        json.RawMessage(`[{"id":"call_1","type":"function"}]`),
		ToolCallID:       "call_1",
		ReasoningContent: json.RawMessage(`"opaque"`),
	}
	payloadBytes := len(msg.Role) + len("正文🙂") + len(msg.Name) + len(msg.ToolCalls) + len(msg.ToolCallID) + len(msg.ReasoningContent)
	if got := EstimatePromptTokens([]Message{msg}); got < int64(payloadBytes) {
		t.Fatalf("estimate=%d does not bound forwarded bytes=%d", got, payloadBytes)
	}
}

func TestFixedMaxTokens(t *testing.T) {
	if got := FixedMaxTokens(100); got != 100 {
		t.Fatalf("gateway cap should be fixed: %d", got)
	}
}

func TestParseUsage(t *testing.T) {
	if got := ParseUsageLine([]byte(`data: {"usage":{"total_tokens":42}}`)); got != 42 {
		t.Fatalf("stream usage: %d", got)
	}
	if got := ParseUsageLine([]byte(`data: [DONE]`)); got != -1 {
		t.Fatalf("DONE → -1: %d", got)
	}
	if got := ParseUsageLine([]byte(`data: {"choices":[]}`)); got != -1 {
		t.Fatalf("content frame → -1: %d", got)
	}
	if got := ParseUsageBody([]byte(`{"usage":{"total_tokens":7}}`)); got != 7 {
		t.Fatalf("body usage: %d", got)
	}
	if got := ParseUsageBody([]byte(`garbage`)); got != -1 {
		t.Fatalf("bad body → -1: %d", got)
	}
	if got := ParseUsageSnapshotLine([]byte(`data: {"usage":{"prompt_tokens":3,"completion_tokens":-1,"total_tokens":3}}`)); !got.Malformed {
		t.Fatalf("negative stream usage must be marked malformed: %+v", got)
	}
	if got := ParseUsageSnapshotLine([]byte(`data: {"usage":{"prompt_tokens":"three"}}`)); !got.Present || !got.Malformed {
		t.Fatalf("wrong-typed usage must retain malformed evidence: %+v", got)
	}
}

func TestParseUsageRejectsDuplicateBillingKeys(t *testing.T) {
	for name, payload := range map[string]string{
		"duplicate top-level usage":       `{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11},"Usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`,
		"negative overwritten in usage":   `{"usage":{"prompt_tokens":10,"completion_tokens":-1,"Completion_Tokens":1,"total_tokens":11}}`,
		"negative overwritten in details": `{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":-1,"Reasoning_Tokens":1}}}`,
		"unicode simple-fold alias":       `{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":-1,"reaſoning_tokenſ":1}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseUsageSnapshotBody([]byte(payload))
			if !got.Malformed {
				t.Fatalf("duplicate billing key must be sticky malformed: %+v", got)
			}
		})
	}
}

func TestIsDONERequiresExactSSESentinel(t *testing.T) {
	if !IsDONE([]byte(" data:   [DONE]  ")) {
		t.Fatal("exact sentinel must terminate the stream")
	}
	for _, line := range [][]byte{
		[]byte(`data: {"choices":[{"delta":{"content":"[DONE]"}}]}`),
		[]byte(`event: [DONE]`),
		[]byte(`data: [DONE] trailing`),
	} {
		if IsDONE(line) {
			t.Fatalf("content/non-sentinel line must not terminate: %s", line)
		}
	}
}

// FuzzDecodeInbound asserts DecodeInbound never panics and never lets a
// non-whitelisted top-level field reach the sanitized upstream body (tools /
// tool_choice are the deliberate EXCEPTION — they pass through by design).
func FuzzDecodeInbound(f *testing.F) {
	f.Add([]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"n":3,"logit_bias":{}}`))
	f.Add([]byte(`{"tools":[1,2,3],"tool_choice":"none"}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"x"}}]}]}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		in, ae := DecodeInbound(body)
		if ae != nil {
			return // rejected, never forwarded
		}
		// Validation/classification shares the decoded canonical union and must
		// remain panic-free on arbitrary structurally accepted input.
		_, _ = in.ValidateAndClassify(MediaLimits{MaxParts: 8, MaxDecodedBytes: 1 << 20})
		out := SanitizeUpstream(in, "real-model", 64)
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("sanitized body must marshal: %v", err)
		}
		// Whitelist leak-proof: only these keys may appear at the top level.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("sanitized body must be a JSON object: %v", err)
		}
		allowed := map[string]bool{
			"model": true, "messages": true, "stream": true, "temperature": true,
			"max_tokens": true, "tools": true, "tool_choice": true, "stream_options": true,
		}
		for k := range m {
			if !allowed[k] {
				t.Fatalf("non-whitelisted key %q leaked into upstream body: %s", k, raw)
			}
		}
		// Model is always forced to the resolved value, never the client's.
		if mv, ok := m["model"]; ok && string(mv) != `"real-model"` {
			t.Fatalf("model not forced: %s", mv)
		}
	})
}
