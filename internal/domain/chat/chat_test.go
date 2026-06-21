package chat

import (
	"encoding/json"
	"strings"
	"testing"
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

func TestCheckMessageShape(t *testing.T) {
	ok := []chatMessage{{Role: "user", Content: "hi"}}
	if msg := CheckMessageShape(ok, 10, 100); msg != "" {
		t.Fatalf("expected accept, got %q", msg)
	}
	many := make([]chatMessage, 11)
	if msg := CheckMessageShape(many, 10, 100); msg != "too many messages" {
		t.Fatalf("expected too many messages, got %q", msg)
	}
	big := []chatMessage{{Role: "user", Content: strings.Repeat("x", 101)}}
	if msg := CheckMessageShape(big, 10, 100); msg != "message content too large" {
		t.Fatalf("expected content too large, got %q", msg)
	}
	// Rune (not byte) semantics: 3 CJK chars = 9 bytes but 3 runes ≤ cap 5.
	cjk := []chatMessage{{Role: "user", Content: "你好吗"}}
	if msg := CheckMessageShape(cjk, 10, 5); msg != "" {
		t.Fatalf("rune-count cap should accept 3 CJK runes ≤ 5, got %q", msg)
	}
}

func TestEstimate_CountsTools(t *testing.T) {
	msgs := []chatMessage{{Role: "user", Content: "hello world"}}
	base := EstimatePromptTokens(msgs)
	in := InboundRequest{Messages: msgs, Tools: json.RawMessage(`[{"type":"function","function":{"name":"longtoolname","description":"a fairly long description here"}}]`)}
	withTools := in.PromptEstimate()
	if withTools <= base {
		t.Fatalf("tools must add to estimate: base=%d withTools=%d", base, withTools)
	}
}

func TestEstimate_ConservativeCJK(t *testing.T) {
	// CJK estimate should approach ~1 token/char (rune-dominated), an upper bound.
	msgs := []chatMessage{{Role: "user", Content: strings.Repeat("中", 100)}}
	est := EstimatePromptTokens(msgs)
	if est < 100 {
		t.Fatalf("CJK estimate must be ≥ rune count (conservative): got %d", est)
	}
}

func TestClampMaxTokens(t *testing.T) {
	five := int64(5)
	big := int64(9999)
	if got := ClampMaxTokens(&five, 100); got != 5 {
		t.Fatalf("smaller client request should win: %d", got)
	}
	if got := ClampMaxTokens(&big, 100); got != 100 {
		t.Fatalf("larger client request must clamp to cap: %d", got)
	}
	if got := ClampMaxTokens(nil, 100); got != 100 {
		t.Fatalf("nil → cap: %d", got)
	}
	zero := int64(0)
	if got := ClampMaxTokens(&zero, 100); got != 100 {
		t.Fatalf("zero/negative → cap: %d", got)
	}
}

func TestResolveModel(t *testing.T) {
	allow := []string{"deepseek-chat", "deepseek-reasoner"}
	if got := ResolveModel(allow, "deepseek-chat", "deepseek-reasoner"); got != "deepseek-reasoner" {
		t.Fatalf("allowlisted model must pass: %s", got)
	}
	if got := ResolveModel(allow, "deepseek-chat", "gpt-4"); got != "deepseek-chat" {
		t.Fatalf("unknown → default: %s", got)
	}
	if got := ResolveModel(allow, "deepseek-chat", ""); got != "deepseek-chat" {
		t.Fatalf("empty → default: %s", got)
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
}

// FuzzDecodeInbound asserts DecodeInbound never panics and never lets a
// non-whitelisted top-level field reach the sanitized upstream body (tools /
// tool_choice are the deliberate EXCEPTION — they pass through by design).
func FuzzDecodeInbound(f *testing.F) {
	f.Add([]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"n":3,"logit_bias":{}}`))
	f.Add([]byte(`{"tools":[1,2,3],"tool_choice":"none"}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		in, ae := DecodeInbound(body)
		if ae != nil {
			return // rejected, never forwarded
		}
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
