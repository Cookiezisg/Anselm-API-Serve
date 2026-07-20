package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool-call assistant turn with empty content must marshal to the OpenAI-
// canonical {role, tool_calls} (content omitted), NOT "content":"" — stricter
// upstreams (DeepSeek v4-flash) 400 on an empty content beside tool_calls, which
// would break the multi-turn agentic loop.
func TestToolCallTurnOmitsEmptyContent(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"w","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"26C"}` +
		`]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	out, err := json.Marshal(SanitizeUpstream(in, "m", ptrInt64(64)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"content":""`) {
		t.Fatalf("tool-call turn still emits empty content: %s", s)
	}
	// The assistant turn keeps its tool_calls; the tool result keeps its content + id.
	for _, want := range []string{`"tool_calls":[`, `"tool_call_id":"c1"`, `"content":"26C"`, `"content":"weather?"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
}

// A normal assistant turn WITH text content (no tool_calls) keeps its content.
func TestNormalAssistantKeepsContent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"hello"}]}`)
	in, _ := DecodeInbound(body)
	out, _ := json.Marshal(SanitizeUpstream(in, "m", ptrInt64(64)))
	if !strings.Contains(string(out), `"content":"hello"`) {
		t.Fatalf("normal assistant lost content: %s", out)
	}
}

// reasoning_content on an echoed assistant tool-call turn must pass through
// verbatim — deepseek-v4-flash (thinking model) requires it on the next request
// or the multi-turn tool loop 400s.
func TestReasoningContentPassesThrough(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"reasoning_content":"I should call the weather tool.","tool_calls":[{"id":"c1","type":"function","function":{"name":"w","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"26C"}` +
		`]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	out, _ := json.Marshal(SanitizeUpstream(in, "m", ptrInt64(64)))
	if !strings.Contains(string(out), `"reasoning_content":"I should call the weather tool."`) {
		t.Fatalf("reasoning_content stripped: %s", out)
	}
}
