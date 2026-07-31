package apierr

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The wire error surface, frozen.
//
// Every one of these is a promise to a client that already shipped: a desktop
// build in someone's hands branches on these codes and shows these statuses. A
// code that silently changes spelling, or a 429 that quietly becomes a 503, is
// not caught by any test that only asserts "an error was returned".
//
// This table is also the closed set itself. error-codes.md documents it for
// humans; this pins it for the compiler's blind spot — a code is a string.
//
//	go test ./internal/domain/apierr -run TestErrorEnvelopeMatchesGolden -update-golden
//
// 线缆错误面,冻结。
//
// 这里每一条都是对**已经发出去的**客户端的承诺:某个人手里的桌面版正按这些 code 分支、按这些
// status 显示。一个悄悄改了拼写的 code、或一个从 429 静默变成 503 的状态,任何只断言「返回了
// 一个错误」的测试都接不住。
//
// 这张表本身也是那个封闭集。error-codes.md 给人看;这一份钉住编译器的盲区——code 是字符串。
var updateGolden = flag.Bool("update-golden", false, "rewrite the error envelope golden file")

const goldenPath = "testdata/envelope.golden.txt"

// prebuilt is every package-level error value a handler can return directly.
// Adding one here is part of adding one at all — see TestEveryPrebuiltErrorIsPinned.
func prebuilt() map[string]*APIError {
	return map[string]*APIError{
		"ErrInvalidInstall":          ErrInvalidInstall,
		"ErrDeviceProofRequired":     ErrDeviceProofRequired,
		"ErrDeviceProofInvalid":      ErrDeviceProofInvalid,
		"ErrDeviceProofNonceInvalid": ErrDeviceProofNonceInvalid,
		"ErrDeviceProofReplayed":     ErrDeviceProofReplayed,
		"ErrAccountBanned":           ErrAccountBanned,
		"ErrRateLimited":             ErrRateLimited,
		"ErrInstallRateLimited":      ErrInstallRateLimited,
		"ErrRequestBodyTooLarge":     ErrRequestBodyTooLarge,
		"ErrQuotaExhausted":          ErrQuotaExhausted,
		"ErrBudgetExhausted":         ErrBudgetExhausted,
		"ErrUpstreamBusy":            ErrUpstreamBusy,
		"ErrBadRequest":              ErrBadRequest,
		"ErrUpstreamError":           ErrUpstreamError,
		"ErrUpstreamTimeout":         ErrUpstreamTimeout,
		"ErrMultimodalUnavailable":   ErrMultimodalUnavailable,
		"ErrAudioUnavailable":        ErrAudioUnavailable,
		"ErrSpeechUnavailable":       ErrSpeechUnavailable,
		"ErrMediaUnavailable":        ErrMediaUnavailable,
		"ErrImageUnavailable":        ErrImageUnavailable,
		"ErrImageSourceInvalid":      ErrImageSourceInvalid,
		"ErrImageQuotaExhausted":     ErrImageQuotaExhausted,
		"ErrTTSUnavailable":          ErrTTSUnavailable,
		"ErrTTSQuotaExhausted":       ErrTTSQuotaExhausted,
		"ErrVideoUnavailable":        ErrVideoUnavailable,
		"ErrVideoQuotaExhausted":     ErrVideoQuotaExhausted,
		"ErrVideoFrameInvalid":       ErrVideoFrameInvalid,
		"ErrVideoTaskNotFound":       ErrVideoTaskNotFound,
		"ErrVoiceUnavailable":        ErrVoiceUnavailable,
		"ErrVoiceQuotaExhausted":     ErrVoiceQuotaExhausted,
		"ErrVoiceSampleInvalid":      ErrVoiceSampleInvalid,
		"ErrVoiceInventoryFull":      ErrVoiceInventoryFull,
		"ErrVoiceNameTaken":          ErrVoiceNameTaken,
		"ErrVoiceCapacityReached":    ErrVoiceCapacityReached,
		"ErrVoiceNotFound":           ErrVoiceNotFound,
		"ErrMediaUploadInvalid":      ErrMediaUploadInvalid,
		"ErrMediaUploadNotFound":     ErrMediaUploadNotFound,
		"ErrMediaLeaseNotFound":      ErrMediaLeaseNotFound,
		"ErrMediaUploadConflict":     ErrMediaUploadConflict,
		"ErrMediaIntegrityFailed":    ErrMediaIntegrityFailed,
		"ErrDiskLow":                 ErrDiskLow,
		"ErrInstallCapReached":       ErrInstallCapReached,
		"ErrInstallFPLimited":        ErrInstallFPLimited,
		"ErrInstallPoWInvalid":       ErrInstallPoWInvalid,
		"ErrInstallPoWRequired":      ErrInstallPoWRequired,
		"ErrQuotaResetBusy":          ErrQuotaResetBusy,
	}
}

func renderEnvelope(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Wire error envelope — status + code + message, frozen.\n")
	b.WriteString("# Regenerate deliberately: go test ./internal/domain/apierr -run TestErrorEnvelopeMatchesGolden -update-golden\n\n")

	names := make([]string, 0, len(prebuilt()))
	for name := range prebuilt() {
		names = append(names, name)
	}
	sort.Strings(names)

	table := prebuilt()
	for _, name := range names {
		e := table[name]
		if e == nil {
			t.Fatalf("%s is nil — a prebuilt error must never be", name)
		}
		fmt.Fprintf(&b, "%-28s %3d %-26s %s\n", name, e.Status, e.Code, e.Message)
	}

	// Constructed errors take a parameter, so they are rendered from a fixed one.
	// 构造型错误带参数,故用固定入参渲染。
	b.WriteString("\n# constructed\n")
	internal := Internal()
	fmt.Fprintf(&b, "%-28s %3d %-26s %s\n", "Internal()", internal.Status, internal.Code, internal.Message)
	return b.String()
}

func TestErrorEnvelopeMatchesGolden(t *testing.T) {
	got := renderEnvelope(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("envelope golden rewritten (%d bytes)", len(got))
		return
	}

	want, err := os.ReadFile(goldenPath) // #nosec G304 — fixed repo-relative path
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	if got != string(want) {
		t.Errorf("the wire error surface changed — a shipped client branches on these.\n"+
			"If that is intended, re-approve it explicitly:\n"+
			"  go test ./internal/domain/apierr -run TestErrorEnvelopeMatchesGolden -update-golden\n\n"+
			"--- golden ---\n%s\n--- live ---\n%s", want, got)
	}
}

// TestPrebuiltCodesAreUnique: two errors sharing a code make them
// indistinguishable to a client that branches on it, which defeats the entire
// point of a closed enum.
//
// 两个错误共用一个 code,会让按 code 分支的客户端根本分不开它们——那正好废掉了封闭枚举的全部意义。
func TestPrebuiltCodesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for name, e := range prebuilt() {
		if prev, dup := seen[e.Code]; dup {
			t.Errorf("code %q is used by both %s and %s", e.Code, prev, name)
			continue
		}
		seen[e.Code] = name
	}
}

// TestEveryPrebuiltErrorIsPinned enumerates the package's own source rather than
// trusting the hand-written map above. The first draft of that map was missing
// six errors — a hand-maintained list of everything is exactly the artifact that
// silently falls behind, and a golden built from an incomplete list freezes an
// incomplete promise.
//
// TestEveryPrebuiltErrorIsPinned **解析本包自己的源码**来枚举,而不是相信上面那张手写的表。那张表
// 的初稿漏了六个错误——「一份手工维护的全集清单」正是那种会悄悄落后的产物,而用一份不完整的清单
// 建出来的 golden,冻结的是一份不完整的承诺。
func TestEveryPrebuiltErrorIsPinned(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	declared := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if strings.HasPrefix(name.Name, "Err") {
							declared[name.Name] = true
						}
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Err* declarations — the AST walk is broken, not the package")
	}

	pinned := prebuilt()
	var missing []string
	for name := range declared {
		if _, ok := pinned[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d prebuilt error(s) are declared but not pinned by the golden: %v", len(missing), missing)
	}
	for name := range pinned {
		if !declared[name] {
			t.Errorf("%s is pinned but no longer declared — remove it from the golden table", name)
		}
	}
}
