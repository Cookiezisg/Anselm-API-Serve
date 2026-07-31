package main

// Forbidden-token ratchet. The docs linter above governs how docs are WRITTEN;
// this governs what the repository is still allowed to MENTION.
//
// It exists because deletions in this repo kept going half-done and nothing
// noticed: DeepSeek left the router but stayed in 37 files (including a hard
// requirement in the deploy script), `gemini` outlived the provider it named by
// a whole generation, and a config key ADR 0012 deleted survived in
// .env.example with a real hostname in it. All of it invisible to the compiler,
// because every one of them is a string.
//
// The ratchet is exact in BOTH directions: more occurrences than the baseline is
// a regression, FEWER is also a failure. That asymmetry is the point — a
// shrinking baseline must be committed alongside the removal, so the file is
// always an honest, current list of what is left. Its endgame is empty.
//
// 禁词棘轮。上面的 docs linter 管文档**怎么写**;本闸管仓库**还允许提到什么**。
//
// 它存在,是因为本仓的删除反复只做了一半而没有任何东西发现:DeepSeek 退出了路由却留在 37 个
// 文件里(包括部署脚本里的一条硬要求),`gemini` 比它命名的那个 provider 多活了整整一代,而
// ADR 0012 删掉的一个配置键在 .env.example 里活了下来、还带着真实主机名。这些编译器全都看不
// 见,因为它们每一个都是字符串。
//
// 棘轮**双向**精确:命中多于 baseline 是回归,**少于**也是失败。这个不对称正是要点——收缩后的
// baseline 必须与删除同一提交落地,故该文件永远是一份诚实的、当前的「还剩什么」清单。它的终局
// 是空。

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// forbidden is the closed set of tokens under ratchet, each with the phase that
// takes it to zero. Adding a row is how a future cleanup gets mechanical
// enforcement; removing one is how it graduates.
//
// forbidden 是受棘轮管辖的封闭禁词集,每条注明由哪个阶段清零。加一行 = 让未来某次清理获得机械
// 强制;删一行 = 它毕业了。
var forbidden = []struct {
	Name string
	Re   *regexp.Regexp
	Why  string
}{
	{"deepseek", regexp.MustCompile(`(?i)deepseek`),
		"provider retired from routing 2026-07-28; goes to zero in 阶段 2"},
	{"gemini", regexp.MustCompile(`(?i)gemini`),
		"provider that never shipped; survives only in DB CHECK constraints; 阶段 4"},
	{"media-public-base-url", regexp.MustCompile(`MEDIA_PUBLIC_BASE_URL`),
		"config key removed by ADR 0012; 阶段 0/2 clears the remaining references"},
	{"dashboard-auth-mode", regexp.MustCompile(`DASHBOARD_AUTH_MODE`),
		"three-mode dashboard auth collapses to external-only in 阶段 3"},
	// The pattern deliberately does NOT spell the deployment hostname, so this
	// gate cannot itself become the leak it is meant to catch.
	// 该模式**刻意不写出**部署主机名,故本闸不会变成它要抓的那个泄漏本身。
	{"deployment-domain", regexp.MustCompile(`(?i)\banselm\.[a-z]{2,10}\b`),
		"real deployment hostname belongs in GitHub secrets, not the repo; 阶段 1"},
}

// skipDirs are never walked: VCS internals, dependency trees, and build output
// that is not source.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "bin": true}

// exempt lists paths that may name a forbidden token forever, because naming it
// is their job.
//
//   - docs/decisions/ — ADRs are immutable historical records. An ADR about
//     retiring DeepSeek must be free to say "DeepSeek".
//   - docs/archive/ — the same graveyard the docs linter already exempts.
//   - the governance ticket — it is the document that lists what to remove.
//   - deploy/site/ — the public trust page. Its whole stated purpose is to
//     declare the official API domain, so it has to be able to name it. That
//     hostname is not a secret in the first place: every client resolves it to
//     reach the service. What the domain rule guards against is the hostname
//     leaking into places that have no business knowing which deployment this
//     is — tests, templates, how-to docs — not the one page whose subject it is.
//   - the gate's own two files — they hold the token list by definition.
//
// 这些路径可以永远提到禁词,因为提到它正是它们的职责。ADR 是不可变的历史记录——一篇讲「撤掉
// DeepSeek」的 ADR 必须能说出「DeepSeek」。archive/ 是 docs linter 本就豁免的同一片墓地,治理
// 工单则是那份列出「要删什么」的文档。deploy/site/ 是公开的信任页:它自称存在的目的就是**声明
// 官方 API 域名**,故它必须能说出那个域名——而那个主机名本来就不是机密,每个客户端都要解析它才
// 连得上。域名规则要防的是主机名渗进「本不该知道这是哪个部署」的地方(测试、模板、how-to 文档),
// 不是防那唯一一张以它为主题的页面。本闸自己的两个文件按定义持有禁词表。
func exempt(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "docs/decisions/"),
		strings.HasPrefix(rel, "docs/archive/"),
		strings.HasPrefix(rel, "deploy/site/"),
		rel == "docs/working/repo-governance.md",
		rel == "cmd/docs/forbidden.go",
		rel == baselineRel,
		rel == ".env": // local secrets file, gitignored; never inspected or reported
		return true
	}
	return false
}

const baselineRel = "cmd/docs/forbidden.baseline"

// hit is one (token, file) pair with its occurrence count.
type hit struct {
	Token string
	Path  string
	Count int
}

func (h hit) key() string { return h.Token + "\t" + h.Path }

// scanForbidden walks root and counts every forbidden-token occurrence per file.
func scanForbidden(root string) ([]hit, error) {
	var hits []hit
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if exempt(rel) {
			return nil
		}
		// path is produced by WalkDir over the repo (build-time dev tool, never
		// user input), so reading by variable path is safe here.
		raw, readErr := os.ReadFile(path) // #nosec G304
		if readErr != nil {
			return nil
		}
		// Binary files carry no reviewable prose; a NUL in the head is the
		// cheapest reliable signal. The embedded dashboard bundle is text and is
		// deliberately NOT skipped — a hostname compiled into it is still a leak.
		// 二进制文件没有可审阅的正文;头部有 NUL 是最便宜可靠的判据。嵌入的 dashboard
		// 产物是文本,**刻意不跳过**——编译进去的主机名同样是泄漏。
		head := raw
		if len(head) > 8192 {
			head = head[:8192]
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return nil
		}
		for _, f := range forbidden {
			if n := len(f.Re.FindAllIndex(raw, -1)); n > 0 {
				hits = append(hits, hit{Token: f.Name, Path: rel, Count: n})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].key() < hits[j].key() })
	return hits, nil
}

// readBaseline parses the committed allowance. A missing file means "nothing is
// allowed" — the strictest reading, so a deleted baseline fails loudly instead
// of silently disabling the gate.
//
// readBaseline 解析已提交的容许量。文件缺失 = 「什么都不允许」——最严的读法,故删掉 baseline 会
// 大声失败,而不是静默地把闸关掉。
func readBaseline(root string) (map[string]int, error) {
	path := filepath.Join(root, baselineRel)
	f, err := os.Open(path) // #nosec G304 — fixed repo-relative path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s: malformed line %q (want token\\tpath\\tcount)", baselineRel, line)
		}
		n, convErr := strconv.Atoi(parts[2])
		if convErr != nil {
			return nil, fmt.Errorf("%s: bad count in %q", baselineRel, line)
		}
		out[parts[0]+"\t"+parts[1]] = n
	}
	return out, sc.Err()
}

// renderBaseline produces the committed file body from a scan.
func renderBaseline(hits []hit) string {
	var b strings.Builder
	b.WriteString("# Forbidden-token baseline — the shrinking list of what is LEFT.\n")
	b.WriteString("# Generated by: go run ./cmd/docs -write-baseline\n")
	b.WriteString("#\n")
	b.WriteString("# The gate requires this file to match reality EXACTLY. Removing occurrences\n")
	b.WriteString("# without shrinking this file fails, and so does the reverse. Endgame: empty.\n")
	b.WriteString("# 本闸要求本文件与现实**逐条精确**吻合:删了命中却不收缩本文件会失败,反之亦然。终局:空。\n")
	b.WriteString("#\n")
	b.WriteString("# token\tpath\tcount\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "%s\t%s\t%d\n", h.Token, h.Path, h.Count)
	}
	return b.String()
}

// checkForbidden compares the live scan against the baseline and returns one
// error line per discrepancy, most actionable first.
func checkForbidden(root string) []string {
	hits, err := scanForbidden(root)
	if err != nil {
		return []string{fmt.Sprintf("forbidden: scan failed: %v", err)}
	}
	base, err := readBaseline(root)
	if err != nil {
		return []string{fmt.Sprintf("forbidden: %v", err)}
	}

	why := map[string]string{}
	for _, f := range forbidden {
		why[f.Name] = f.Why
	}

	var errs []string
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.key()] = true
		allowed, ok := base[h.key()]
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf(
				"forbidden: NEW occurrence of %q in %s (%d×) — %s", h.Token, h.Path, h.Count, why[h.Token]))
		case h.Count > allowed:
			errs = append(errs, fmt.Sprintf(
				"forbidden: %q grew in %s: %d > baseline %d", h.Token, h.Path, h.Count, allowed))
		case h.Count < allowed:
			errs = append(errs, fmt.Sprintf(
				"forbidden: %q shrank in %s: %d < baseline %d — good, now update the baseline "+
					"(go run ./cmd/docs -write-baseline)", h.Token, h.Path, h.Count, allowed))
		}
	}
	for key := range base {
		if !seen[key] {
			parts := strings.SplitN(key, "\t", 2)
			errs = append(errs, fmt.Sprintf(
				"forbidden: %q is gone from %s — good, now update the baseline "+
					"(go run ./cmd/docs -write-baseline)", parts[0], parts[1]))
		}
	}
	sort.Strings(errs)
	return errs
}

// writeBaseline regenerates the committed baseline from the current tree.
func writeBaseline(root string) error {
	hits, err := scanForbidden(root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, baselineRel)
	if err := os.WriteFile(path, []byte(renderBaseline(hits)), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d entries)\n", baselineRel, len(hits))
	return nil
}
