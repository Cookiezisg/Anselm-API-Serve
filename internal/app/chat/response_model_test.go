package chat

import (
	"bytes"
	"testing"
)

func TestRewriteClientModelJSONPreservesEveryOtherByte(t *testing.T) {
	in := []byte(" \n{\"id\":\"cmpl-1\", \"model\" : \"deepseek-v4-flash\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"arguments\":\"{\\\"x\\\":1}\"}}]}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}} \t")
	want := []byte(" \n{\"id\":\"cmpl-1\", \"model\" : \"anselm-auto\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"arguments\":\"{\\\"x\\\":1}\"}}]}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}} \t")
	got, ok := rewriteClientModelJSON(in, "anselm-auto")
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("rewrite changed bytes outside top-level model\n got: %s\nwant: %s", got, want)
	}
}

func TestRewriteClientModelJSONHandlesEscapedTopLevelKey(t *testing.T) {
	in := []byte(`{"mo\u0064el":"deepseek-v4-flash","nested":{"model":"keep-me"}}`)
	want := []byte(`{"mo\u0064el":"public","nested":{"model":"keep-me"}}`)
	got, ok := rewriteClientModelJSON(in, "public")
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestRewriteClientModelJSONRejectsDuplicateOrCaseFoldModelKeys(t *testing.T) {
	inputs := [][]byte{
		[]byte(`{"model":"deepseek-v4-flash","model":"gemini-3.1-flash-lite"}`),
		[]byte(`{"model":"deepseek-v4-flash","Model":"gemini-3.1-flash-lite"}`),
		[]byte(`{"Model":"gemini-3.1-flash-lite","model":"deepseek-v4-flash"}`),
		[]byte(`{"MODEL":"gemini-3.1-flash-lite"}`),
	}
	for _, in := range inputs {
		got, ok := rewriteClientModelJSON(in, "public")
		if ok || got != nil {
			t.Fatalf("ambiguous model key accepted: in=%q got=%q ok=%v", in, got, ok)
		}
	}
}

func TestRewriteClientModelJSONAcceptsModelFreeObjectUnchanged(t *testing.T) {
	in := []byte(" \n{\"choices\":[{\"message\":{\"model\":\"nested-only\"}}]} \t")
	got, ok := rewriteClientModelJSON(in, "public")
	if !ok || !bytes.Equal(got, in) {
		t.Fatalf("model-free object changed or rejected: in=%q got=%q ok=%v", in, got, ok)
	}
}

func TestRewriteClientModelJSONRejectsAnythingButOneCompleteObject(t *testing.T) {
	inputs := [][]byte{
		[]byte(`["model","deepseek-v4-flash"]`),
		[]byte(`null`),
		[]byte(`"deepseek-v4-flash"`),
		[]byte(`42`),
		[]byte(`true`),
		[]byte(``),
		[]byte(" \t\r\n"),
		[]byte(`{"model":"deepseek-v4-flash"} trailing`),
		[]byte(`{"model":"deepseek-v4-flash"}{"id":"second"}`),
		[]byte(`{"model":`),
		{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
	}
	for _, in := range inputs {
		got, ok := rewriteClientModelJSON(in, "public")
		if ok || got != nil {
			t.Fatalf("invalid/non-object payload accepted: in=%q got=%q ok=%v", in, got, ok)
		}
	}
}

func TestRewriteClientModelSSELinePreservesSentinelAndBlankLines(t *testing.T) {
	for _, in := range [][]byte{
		[]byte(`data: [DONE]`), []byte(" data:\t[DONE]  "), nil, []byte("\r"),
	} {
		got, ok := rewriteClientModelSSELine(in, "anselm-auto")
		if !ok || !bytes.Equal(got, in) {
			t.Fatalf("sentinel/separator changed: in=%q got=%q", in, got)
		}
	}
	for _, in := range [][]byte{
		[]byte(`: keep-alive model=deepseek-v4-flash`),
		[]byte(`  : provider=gemini-3.1-flash-lite`),
	} {
		got, ok := rewriteClientModelSSELine(in, "anselm-auto")
		if !ok || !bytes.Equal(got, []byte(":")) {
			t.Fatalf("comment heartbeat was not normalized: in=%q got=%q ok=%v", in, got, ok)
		}
	}

	in := []byte(`  data:  {"model":"gemini-3.1-flash-lite","choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}  `)
	want := []byte(`  data:  {"model":"anselm-auto","choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}  `)
	got, ok := rewriteClientModelSSELine(in, "anselm-auto")
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("data frame got %q want %q", got, want)
	}
}

func TestRewriteClientModelSSELineAcceptsModelFreeObject(t *testing.T) {
	in := []byte("data:\t {\"choices\":[]} \t")
	got, ok := rewriteClientModelSSELine(in, "anselm-auto")
	if !ok || !bytes.Equal(got, in) {
		t.Fatalf("model-free data object changed or rejected: got=%q ok=%v", got, ok)
	}
}

func TestRewriteClientModelSSELineRejectsMalformedAndNonObjectData(t *testing.T) {
	inputs := [][]byte{
		[]byte(`event: message`),
		[]byte(`id: deepseek-v4-flash`),
		[]byte(`retry: 1000`),
		[]byte(`data`),
		[]byte(" data\r"),
		[]byte(`data:`),
		[]byte(`data: null`),
		[]byte(`data: ["gemini-3.1-flash-lite"]`),
		[]byte(`data: "deepseek-v4-flash"`),
		[]byte(`data: {"model":"deepseek-v4-flash"`),
		[]byte(`data: {"model":"deepseek-v4-flash"} garbage`),
		[]byte(`data: {"model":"deepseek-v4-flash"}{"id":2}`),
		[]byte(`data: [DONE] trailing`),
	}
	for _, in := range inputs {
		got, ok := rewriteClientModelSSELine(in, "anselm-auto")
		if ok || got != nil {
			t.Fatalf("invalid SSE data accepted: in=%q got=%q ok=%v", in, got, ok)
		}
	}
}
