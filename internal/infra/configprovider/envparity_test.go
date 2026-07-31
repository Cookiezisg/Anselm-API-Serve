package configprovider

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// This file is the config-surface parity gate. It exists because the three
// places that describe one env key drifted apart with nothing to catch it:
// ADR 0012 deleted MEDIA_PUBLIC_BASE_URL from the loader, and the .env.example
// line survived — advertising a knob that does nothing, with a real hostname in
// it. In the other direction GATEWAY_MODE, the master switch over every
// rationing gate, was never written into the template at all.
//
// Both failures are invisible to the compiler: env keys are strings. So they get
// a test instead.
//
// 本文件是配置面对账闸。它存在,是因为描述同一个 env key 的三处各自漂移而没有任何东西
// 接住:ADR 0012 从 loader 删掉了 MEDIA_PUBLIC_BASE_URL,而 .env.example 那一行活了下来
// ——一个什么也不做的旋钮,还带着真实主机名。反方向上,GATEWAY_MODE(所有配额闸的总开关)
// 压根没写进模板。两种失败编译器都看不见(env key 是字符串),故用测试接住。

// recordingEnv answers from m while recording every key LoadBase asks for. The
// loader takes its getenv injected, so the read set is observed rather than
// guessed from source — a key read through any helper is captured the same way.
//
// recordingEnv 按 m 作答,同时记录 LoadBase 问过的每一个 key。loader 的 getenv 是注入的,
// 故读取集合是**观测**到的、不是从源码猜的——经任何 helper 读取都一样被捕获。
func recordingEnv(m map[string]string, seen map[string]bool) func(string) string {
	return func(k string) string {
		seen[k] = true
		return m[k]
	}
}

// loaderKeys returns every env key LoadBase reads, unioned over the capability
// postures that gate conditional reads. A capability that is off never reaches
// its own keys, so an all-off load alone would under-report the surface.
//
// loaderKeys 返回 LoadBase 读到的全部 env key,在决定条件读取的各种能力姿态上取并集。
// 关着的能力不会走到自己那几个 key,故只跑一次全关的加载会少报整个面。
func loaderKeys(t *testing.T) map[string]bool {
	t.Helper()
	seen := map[string]bool{}

	base := func() map[string]string {
		return map[string]string{
			"DEEPSEEK_API_KEY":               "sk-a",
			"DASHSCOPE_API_KEY":              "qwen-key",
			"DASHSCOPE_WORKSPACE_ID":         "ws-test",
			"GLOBAL_MONTHLY_SPEND_MICRO_USD": "420000000",
		}
	}

	// All capabilities off (the shipped default), then all on. Every conditional
	// branch in LoadBase is gated by one of these flags.
	// 先全关(即出厂默认),再全开。LoadBase 里每个条件分支都由这几个开关之一把守。
	allOn := base()
	for k, v := range map[string]string{
		"MEDIA_ENABLED":        "true",
		"IMAGE_ENABLED":        "true",
		"SPEECH_ENABLED":       "true",
		"VIDEO_ENABLED":        "true",
		"MEDIA_SIGNING_SECRET": strings.Repeat("s", 48),
		"MEDIA_STAGING_ROOT":   t.TempDir(),
		"INSTALL_POW_MODE":     config.PowModeEnforce,
		"INSTALL_POW_SECRET":   strings.Repeat("p", 32),
		"TOKEN_ANOMALY_RPM":    "600",
	} {
		allOn[k] = v
	}

	for name, env := range map[string]map[string]string{"all-off": base(), "all-on": allOn} {
		if _, err := LoadBase(recordingEnv(env, seen)); err != nil {
			t.Fatalf("LoadBase(%s): %v", name, err)
		}
	}
	return seen
}

// exampleKeys parses the assignment keys out of .env.example. The template is the
// operator-facing contract, so a key that is not on a bare `KEY=` line is not
// documented, whatever a surrounding comment says about it.
//
// exampleKeys 解析 .env.example 里的赋值键。模板是面向运营者的契约,故没有出现在裸
// `KEY=` 行上的键就是没有被文档化——不论它周围的注释怎么提到它。
func exampleKeys(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "..", ".env.example")
	f, err := os.Open(path) // #nosec G304 — fixed repo-relative path, test-only
	if err != nil {
		t.Fatalf("open .env.example: %v", err)
	}
	defer func() { _ = f.Close() }()

	keys := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			keys[k] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan .env.example: %v", err)
	}
	return keys
}

func sortedDiff(have, want map[string]bool) []string {
	var out []string
	for k := range have {
		if !want[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestEnvExampleMatchesLoader pins .env.example to the loader's actual read set,
// in BOTH directions. A key the loader stopped reading must leave the template
// (it is a knob that does nothing), and a key the loader reads must appear in it
// (an operator cannot configure what nobody wrote down).
//
// TestEnvExampleMatchesLoader 把 .env.example 钉在 loader 的真实读取集上,**双向**。
// loader 不再读的键必须离开模板(那是个什么也不做的旋钮);loader 读的键必须出现在模板里
// (没写下来的东西运营者配不了)。
func TestEnvExampleMatchesLoader(t *testing.T) {
	loader := loaderKeys(t)
	example := exampleKeys(t)

	if stale := sortedDiff(example, loader); len(stale) > 0 {
		t.Errorf(".env.example documents %d key(s) the loader never reads — delete them: %v", len(stale), stale)
	}
	if missing := sortedDiff(loader, example); len(missing) > 0 {
		t.Errorf("loader reads %d key(s) absent from .env.example — document them: %v", len(missing), missing)
	}
}

// TestSpecsAreLoadable pins the dashboard-surfaced registry to the loader: every
// key an operator can see or edit in the dashboard must be one the process
// actually reads at boot. A registry row with no loader behind it would render an
// editable control whose value goes nowhere.
//
// TestSpecsAreLoadable 把后台可见的 registry 钉在 loader 上:运营者在后台看得见或改得动的
// 每一个键,都必须是进程启动时真的会读的那个。没有 loader 在后面的 registry 行,会在后台
// 渲染出一个「改了但值哪儿也没去」的控件。
func TestSpecsAreLoadable(t *testing.T) {
	loader := loaderKeys(t)
	specs := map[string]bool{}
	for _, s := range config.Specs() {
		specs[s.Key] = true
	}
	if orphan := sortedDiff(specs, loader); len(orphan) > 0 {
		t.Errorf("config.Specs() exposes %d key(s) the loader never reads: %v", len(orphan), orphan)
	}
}
