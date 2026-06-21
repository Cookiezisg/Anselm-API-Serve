// Package logx is the structured-logging foundation: one slog JSON handler to
// stdout (systemd → journald), a request-scoped logger carried in ctx, a hard
// redaction floor so secrets/PII can NEVER reach a log line (GW-INV-11/17), and
// INFO 10% sampling to keep journald volume sane (WARN/ERROR never sampled).
//
// 红线:Authorization / 上游 key / prompt·completion 正文 / 上游原始 body 绝不
// 出日志。脱敏在 handler 层兜底,既抹敏感 key 名,也整体抹掉任何复合值。
//
// 约定:禁止对可能含密的结构体/map 用 slog.Any —— slog 经 JSON 编码器逐字段
// 渲染复合值,ReplaceAttr 够不着其内部字段;redactAttr 把任何非白名单 key 下的
// 复合值整体抹成 [REDACTED] 兜底。要落日志的字段一律拆成标量 attr 逐个传。
package logx

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"time"
)

// levelVar lets LOG_LEVEL be parsed once at boot and held for the process.
var levelVar = new(slog.LevelVar)

type ctxKey struct{}

// loggerKey carries the per-request derived logger through ctx.
var loggerKey = ctxKey{}

// redactKeys are attribute keys whose VALUE is always dropped regardless of
// caller intent — the last line of defense against logging a secret/PII.
var redactKeys = map[string]struct{}{
	"authorization": {},
	"api_key":       {},
	"apikey":        {},
	"key":           {},
	"token":         {},
	"prompt":        {},
	"completion":    {},
	"content":       {},
	"messages":      {},
	"body":          {},
	"upstream_body": {},
	"raw":           {},
}

// redactValue is the marker substituted for any redacted attribute.
const redactValue = "[REDACTED]"

// compositeAllowlist names the ONLY top-level keys under which a composite value
// (struct/map/slice) may be serialized. Everything else composite is redacted
// wholesale, because slog's ReplaceAttr never reaches the INNER fields of a
// struct/map logged via slog.Any — a secret bundled under a benign key would
// otherwise be encoded verbatim. No legitimate composite payload exists today.
var compositeAllowlist = map[string]struct{}{}

// infoSampleRate is the fraction of INFO records emitted; WARN/ERROR are never
// sampled. Sampling keeps journald volume sane under load.
const infoSampleRate = 0.10

// sampleFn is the sampling decision seam, injectable in tests for determinism.
// 默认随机;测试可注入恒真/恒假以断言采样路径,避免依赖随机数。
var sampleFn = func() bool {
	return rand.Float64() < infoSampleRate //nolint:gosec // log-sampling, not security
}

// Init parses LOG_LEVEL (debug|info|warn|error, default info) and installs a
// JSON handler to w (stdout in production) as the slog default.
func Init(w io.Writer, logLevel string) {
	levelVar.Set(parseLevel(logLevel))
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       levelVar,
		ReplaceAttr: redactAttr,
	})
	slog.SetDefault(slog.New(h))
}

// InitDefault initializes from the environment to stdout.
func InitDefault() {
	Init(os.Stdout, os.Getenv("LOG_LEVEL"))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redactAttr is the hard floor, run on EVERY attribute of EVERY record so even a
// careless caller cannot leak a secret. Two defenses: (1) key-name floor blanks
// any value under a sensitive key; (2) composite floor blanks any struct/map/
// slice logged via slog.Any under a non-allowlisted key — ReplaceAttr is NOT
// re-invoked on the inner fields of such a value, so a nested secret under a
// benign key would otherwise be JSON-encoded verbatim.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if _, hit := redactKeys[strings.ToLower(a.Key)]; hit {
		a.Value = slog.StringValue(redactValue)
		return a
	}
	// slog.Group members recurse back through ReplaceAttr, so leave them be;
	// only guard composite *values* (KindAny from slog.Any).
	if a.Value.Kind() == slog.KindAny && isComposite(a.Value.Any()) {
		if _, ok := compositeAllowlist[strings.ToLower(a.Key)]; !ok {
			a.Value = slog.StringValue(redactValue)
		}
	}
	return a
}

// isComposite reports whether v renders field-by-field via the JSON encoder
// (struct/map/slice/array, or a pointer to one) — out of ReplaceAttr's reach, so
// it must be redacted whole. error / fmt.Stringer / time values render to a
// single scalar and must survive.
func isComposite(v any) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case error, interface{ String() string }, time.Time, time.Duration:
		return false
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// SampledInfo emits an INFO record only when the sampler fires (default 10%).
// Use for high-volume access lines; security/WARN events must use the logger's
// Warn directly so they are NEVER dropped.
func SampledInfo(ctx context.Context, msg string, args ...any) {
	if sampleFn() {
		From(ctx).Info(msg, args...)
	}
}

// Into returns a child ctx carrying logger for the request scope.
func Into(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// From returns the request-scoped logger, falling back to the default so a
// caller never has to nil-check.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
