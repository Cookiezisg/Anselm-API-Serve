package chatprovider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
)

func ptrInt64(v int64) *int64 { return &v }

func canonicalRequest(t *testing.T) domchat.CompletionRequest {
	t.Helper()
	in, ae := domchat.DecodeInbound([]byte(`{
      "model":"client-must-not-select-provider",
      "messages":[
        {"role":"assistant","content":"x","reasoning_content":"private-ds-trace"},
        {"role":"user","content":"hello"}
      ],"stream":true}`))
	if ae != nil {
		t.Fatal(ae)
	}
	return domchat.Sanitize(in, ptrInt64(64))
}

func TestDeepSeekEncodingPreservesReasoningAndForcesModel(t *testing.T) {
	raw, err := encode(billing.ProviderDeepSeek, billing.DeepSeekV4Flash, canonicalRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"model":"deepseek-v4-flash"`) || !strings.Contains(s, `"reasoning_content":"private-ds-trace"`) {
		t.Fatalf("DeepSeek wire=%s", s)
	}
	if !strings.Contains(s, `"thinking":{"type":"enabled"}`) || !strings.Contains(s, `"reasoning_effort":"high"`) {
		t.Fatalf("DeepSeek product knobs missing: %s", s)
	}
	if strings.Contains(s, "client-must-not-select-provider") {
		t.Fatalf("client model leaked: %s", s)
	}
}

func TestQwenEncodingDropsDeepSeekTraceAndEnablesThinking(t *testing.T) {
	req := canonicalRequest(t)
	raw, err := encode(billing.ProviderQwen, billing.Qwen37Plus, req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "private-ds-trace") || strings.Contains(s, "reasoning_content") {
		t.Fatalf("DeepSeek extension leaked to Qwen: %s", s)
	}
	if !strings.Contains(s, `"model":"qwen3.7-plus"`) {
		t.Fatalf("Qwen wire=%s", s)
	}
	if !strings.Contains(s, `"enable_thinking":true`) || strings.Contains(s, "reasoning_effort") || strings.Contains(s, `"thinking":`) {
		t.Fatalf("Qwen product knobs wrong: %s", s)
	}
	// The caller-owned canonical value was cloned, not mutated.
	original, _ := json.Marshal(req)
	if !strings.Contains(string(original), "private-ds-trace") {
		t.Fatalf("encoder mutated canonical request: %s", original)
	}
}

func TestQwenEncodingPreservesThoughtSignatureInsideToolCall(t *testing.T) {
	in, ae := domchat.DecodeInbound([]byte(`{
      "messages":[
        {"role":"user","content":"inspect"},
        {"role":"assistant","tool_calls":[{
          "id":"function-call-1",
          "type":"function",
          "function":{"name":"inspect","arguments":"{}"},
          "extra_content":{"google":{"thought_signature":"signed-state"}}
        }]},
        {"role":"tool","tool_call_id":"function-call-1","content":"ok"}
      ]}`))
	if ae != nil {
		t.Fatal(ae)
	}
	raw, err := encode(billing.ProviderQwen, billing.Qwen37Plus, domchat.Sanitize(in, ptrInt64(64)))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			ToolCalls []struct {
				ExtraContent struct {
					Google struct {
						ThoughtSignature string `json:"thought_signature"`
					} `json:"google"`
				} `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.Messages[1].ToolCalls[0].ExtraContent.Google.ThoughtSignature; got != "signed-state" {
		t.Fatalf("thought signature=%q", got)
	}
}

func TestUnknownProviderFailsClosed(t *testing.T) {
	if _, err := encode(billing.Provider("future"), "model", canonicalRequest(t)); err == nil {
		t.Fatal("unknown provider must fail")
	}
}
