package logx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// setSample swaps the sampling seam for a deterministic test result and restores
// it on cleanup, so sampling-path assertions never depend on randomness.
func setSample(t *testing.T, fire bool) {
	t.Helper()
	prev := sampleFn
	sampleFn = func() bool { return fire }
	t.Cleanup(func() { sampleFn = prev })
}

// TestRedactionNeverLeaksSecrets: whatever a careless caller passes under a
// sensitive key, output shows [REDACTED], never the real value (GW-INV-11/17).
func TestRedactionNeverLeaksSecrets(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")

	secrets := map[string]string{
		"authorization": "Bearer sk-super-secret-deepseek-key",
		"api_key":       "sk-another-key",
		"token":         "gwk_install_token_value",
		"prompt":        "the user's private prompt text",
		"completion":    "the model's private completion",
		"content":       "message content that is private",
		"messages":      "[{role:user}]",
		"body":          "raw upstream body json",
		"upstream_body": "raw deepseek error body",
	}
	args := make([]any, 0, len(secrets)*2)
	for k, v := range secrets {
		args = append(args, k, v)
	}
	From(context.Background()).Info("test", args...)

	out := buf.String()
	for _, v := range secrets {
		if strings.Contains(out, v) {
			t.Fatalf("secret value leaked into log: %q in %s", v, out)
		}
	}
	if !strings.Contains(out, redactValue) {
		t.Fatalf("expected redaction marker in output: %s", out)
	}
}

// TestRedactionDefendsAgainstStructAny: key-name redaction sees only the TOP
// key, so slog.Any of a struct/map under a benign key would let the JSON encoder
// serialize nested secret fields verbatim. The composite floor must redact any
// struct/map/slice under a non-allowlisted key (GW-INV-17).
func TestRedactionDefendsAgainstStructAny(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")

	type leaky struct {
		Authorization string
		APIKey        string
		Note          string
	}
	v := leaky{
		Authorization: "Bearer sk-super-secret-deepseek-key",
		APIKey:        "sk-another-key",
		Note:          "benign-but-bundled",
	}
	From(context.Background()).Info("test", "extra", v)

	out := buf.String()
	for _, secret := range []string{"sk-super-secret-deepseek-key", "sk-another-key", "Bearer "} {
		if strings.Contains(out, secret) {
			t.Fatalf("nested secret leaked via slog.Any of a struct: %q in %s", secret, out)
		}
	}

	buf.Reset()
	From(context.Background()).Info("test", "payload", map[string]string{
		"token": "gwk_secret_token_value",
	})
	if strings.Contains(buf.String(), "gwk_secret_token_value") {
		t.Fatalf("nested secret leaked via slog.Any of a map: %s", buf.String())
	}
}

// TestRedactionKeepsScalarAny: scalar values under benign keys (the common case)
// must survive — the composite floor must not nuke ordinary numbers/strings.
func TestRedactionKeepsScalarAny(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")
	From(context.Background()).Info("test", "count", 42, "label", "hello-world")
	out := buf.String()
	if !strings.Contains(out, "42") || !strings.Contains(out, "hello-world") {
		t.Fatalf("benign scalar attrs were wrongly redacted: %s", out)
	}
}

// TestRequestScopedLogger: From returns the ctx-bound logger, and the rid it was
// derived with appears on every line.
func TestRequestScopedLogger(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "debug")

	ctx := Into(context.Background(), From(context.Background()).With("rid", "abc123"))
	From(ctx).Info("hello")

	out := buf.String()
	if !strings.Contains(out, `"rid":"abc123"`) {
		t.Fatalf("rid not bound to scoped logger: %s", out)
	}
}

// TestLevelGate: a level below the configured threshold is dropped.
func TestLevelGate(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "warn")
	From(context.Background()).Info("should-not-appear")
	if strings.Contains(buf.String(), "should-not-appear") {
		t.Fatalf("info logged despite warn level: %s", buf.String())
	}
	From(context.Background()).Warn("should-appear")
	if !strings.Contains(buf.String(), "should-appear") {
		t.Fatalf("warn not logged at warn level: %s", buf.String())
	}
}

// TestSampledInfoEmitsWhenSamplerFires: with the deterministic seam set to fire,
// the INFO line is written.
func TestSampledInfoEmitsWhenSamplerFires(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")
	setSample(t, true)
	SampledInfo(context.Background(), "access", "endpoint", "/v1/chat")
	if !strings.Contains(buf.String(), "access") {
		t.Fatalf("SampledInfo dropped a line the sampler chose to keep: %s", buf.String())
	}
}

// TestSampledInfoDropsWhenSamplerDeclines: with the seam set to decline, the
// INFO line is suppressed (volume control).
func TestSampledInfoDropsWhenSamplerDeclines(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")
	setSample(t, false)
	SampledInfo(context.Background(), "access", "endpoint", "/v1/chat")
	if buf.Len() != 0 {
		t.Fatalf("SampledInfo emitted despite sampler declining: %s", buf.String())
	}
}

// TestWarnNeverSampled: WARN/ERROR are emitted via the logger directly and must
// never pass through the sampler — even with the seam declining, WARN appears.
func TestWarnNeverSampled(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")
	setSample(t, false)
	From(context.Background()).Warn("security_event", "error_code", "RATE_LIMITED")
	if !strings.Contains(buf.String(), "security_event") {
		t.Fatalf("WARN was dropped — security events must never be sampled: %s", buf.String())
	}
}

// TestSampledInfoRedactsSecrets: the sampling helper still routes through the
// redaction floor — secrets under sensitive keys never survive sampling.
func TestSampledInfoRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, "info")
	setSample(t, true)
	SampledInfo(context.Background(), "access", "authorization", "Bearer sk-leak")
	if strings.Contains(buf.String(), "sk-leak") {
		t.Fatalf("secret leaked through sampled INFO line: %s", buf.String())
	}
}
