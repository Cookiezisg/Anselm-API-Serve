// Command docs is the documentation gate (GOVERNANCE.md §11): it lints docs/ for
// frontmatter validity, lifecycle rules, freshness, the INDEX line cap, the
// type↔directory mapping, working-doc age, and orphan relative .md links. Errors
// exit 1 (fail the gate); warnings print but do not fail. Run from the repo root:
// `go run ./cmd/docs` (Makefile `make docs`).
//
// Command docs 是文档门禁（GOVERNANCE §11）：机械校验 frontmatter 合法性、生命周期、
// 新鲜度、INDEX 行数上限、type↔目录映射、working 文档年龄、孤儿相对 .md 链接。
// 错误 → exit 1（门禁失败）；警告只打印不失败。默认 root=".".
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// validTypes / validStatuses / requiredFields mirror GOVERNANCE §2/§3/§6 — the
// single source is that doc; a change here is a doc change there and vice versa.
// typeDir maps each type to the canonical directory its docs MUST live under
// (GOVERNANCE §2 表末列 + §5 目录地图）；INDEX.md / GOVERNANCE.md / archive 豁免。
//
// validTypes / validStatuses / requiredFields 对应 GOVERNANCE §2/§3/§6；typeDir 对应
// §2/§5 目录归属，二者与那篇文档保持一致。
var (
	validTypes     = map[string]bool{"concept": true, "reference": true, "how-to": true, "decision": true, "log": true, "working": true}
	validStatuses  = map[string]bool{"draft": true, "active": true, "superseded": true, "deprecated": true, "archived": true}
	requiredFields = []string{"id", "type", "status", "owner", "created", "reviewed", "review-due", "audience"}
	// typeDir is the canonical top-level dir per type. A doc whose type is X but
	// that does not live under typeDir[X] (or its subtree) is misplaced (§5).
	typeDir = map[string]string{
		"concept":   "concepts",
		"reference": "references",
		"how-to":    "how-to",
		"decision":  "decisions",
		"working":   "working",
		// log lives inline at references/changelog.md (§2/§5); checked by name, not dir.
	}
	idRe            = regexp.MustCompile(`^DOC-\d{3}$`)
	indexMaxLines   = 50
	workingMaxDays  = 90
	linkRe          = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	fencedRe        = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRe    = regexp.MustCompile("`[^`]*`")
	frontmatterDate = "2006-01-02"
)

func main() {
	root := flag.String("root", ".", "repo root (docs/ lives under it)")
	writeBase := flag.Bool("write-baseline", false, "regenerate the forbidden-token baseline from the current tree")
	flag.Parse()

	if *writeBase {
		if err := writeBaseline(*root); err != nil {
			fmt.Fprintf(os.Stderr, "docs: write baseline: %v\n", err)
			os.Exit(2)
		}
		return
	}

	l := &linter{docsDir: filepath.Join(*root, "docs"), now: time.Now()}
	if _, err := os.Stat(l.docsDir); err != nil {
		fmt.Fprintf(os.Stderr, "docs: no docs/ under %q: %v\n", *root, err)
		os.Exit(2)
	}
	l.run()

	// The forbidden-token ratchet scans the WHOLE repo, not just docs/: the
	// deletions it guards live in Go, SQL, shell, and the env template too.
	// 禁词棘轮扫**整个仓库**、不只 docs/:它守的那些删除同样住在 Go、SQL、shell 与 env 模板里。
	l.errs = append(l.errs, checkForbidden(*root)...)
	l.errs = append(l.errs, checkScars(*root)...)

	for _, w := range l.warns {
		fmt.Printf("WARN  %s\n", w)
	}
	for _, e := range l.errs {
		fmt.Printf("FAIL  %s\n", e)
	}
	if len(l.errs) > 0 {
		fmt.Printf("\nFAIL docs lint: %d error(s), %d warning(s)\n", len(l.errs), len(l.warns))
		os.Exit(1)
	}
	fmt.Printf("OK docs lint clean (%d file(s) ok, %d warning(s))\n", l.checked, len(l.warns))
}

type linter struct {
	docsDir string
	now     time.Time
	checked int
	errs    []string
	warns   []string
}

func (l *linter) errf(format string, a ...any)  { l.errs = append(l.errs, fmt.Sprintf(format, a...)) }
func (l *linter) warnf(format string, a ...any) { l.warns = append(l.warns, fmt.Sprintf(format, a...)) }

// run walks every .md under docs/ and applies the per-file checks.
//
// run 遍历 docs/ 下每个 .md 应用逐文件检查。
func (l *linter) run() {
	var files []string
	_ = filepath.WalkDir(l.docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	for _, path := range files {
		l.checkFile(path)
	}
}

func (l *linter) rel(path string) string {
	r, _ := filepath.Rel(l.docsDir, path)
	return filepath.ToSlash(r)
}

// checkFile applies frontmatter + lifecycle + directory + special-file checks.
//
// checkFile 对一篇文档应用 frontmatter + 生命周期 + 目录归属 + 特例检查。
func (l *linter) checkFile(path string) {
	rel := l.rel(path)
	// path comes from filepath.WalkDir over the repo docs/ tree (build-time dev
	// tool, never user input), so reading it by variable path is safe.
	raw, err := os.ReadFile(path) // #nosec G304

	if err != nil {
		l.errf("%s: read: %v", rel, err)
		return
	}
	content := string(raw)
	l.checked++

	// INDEX.md — line cap; frontmatter-exempt (entry point, §11.6).
	if rel == "INDEX.md" {
		if n := strings.Count(content, "\n") + 1; n > indexMaxLines {
			l.errf("INDEX.md: %d lines > %d (GOVERNANCE §11.6)", n, indexMaxLines)
		}
		l.checkLinks(path, content)
		return
	}
	// archive/ — read-only graveyard, frontmatter-exempt (§3).
	if strings.HasPrefix(rel, "archive/") {
		return
	}

	l.checkLinks(path, content)

	fm, ok := parseFrontmatter(content)
	if !ok {
		l.errf("%s: missing frontmatter (GOVERNANCE §3)", rel)
		return
	}
	for _, f := range requiredFields {
		if strings.TrimSpace(fm[f]) == "" {
			l.errf("%s: frontmatter missing required field %q", rel, f)
		}
	}
	if id := fm["id"]; id != "" && !idRe.MatchString(id) {
		l.errf("%s: invalid id %q (want DOC-NNN, GOVERNANCE §4)", rel, id)
	}
	t := fm["type"]
	if t != "" && !validTypes[t] {
		l.errf("%s: invalid type %q (GOVERNANCE §2)", rel, t)
	}
	if s := fm["status"]; s != "" && !validStatuses[s] {
		l.errf("%s: invalid status %q (GOVERNANCE §6)", rel, s)
	}
	l.checkDir(rel, t)

	// review-due past → warn (not fail, §11.3).
	if due, err := time.Parse(frontmatterDate, fm["review-due"]); err == nil && due.Before(l.now) {
		l.warnf("%s: review-due %s is past (re-review)", rel, fm["review-due"])
	}
	// working doc > 90 days with empty landed-into → fail (§11.4 / §9).
	if t == "working" {
		if created, err := time.Parse(frontmatterDate, fm["created"]); err == nil {
			if l.now.Sub(created) > time.Duration(workingMaxDays)*24*time.Hour && strings.TrimSpace(fm["landed-into"]) == "" {
				l.errf("%s: working doc older than %dd with empty landed-into (GOVERNANCE §9)", rel, workingMaxDays)
			}
		}
	}
}

// checkDir enforces type↔directory placement (GOVERNANCE §2/§5). The governance
// doc itself (DOC-000) is a concept living at the docs root by design, so the
// root GOVERNANCE.md is exempt; every other typed doc must sit under its dir.
//
// checkDir 强制 type↔目录映射；GOVERNANCE.md（根 concept 样板）豁免，log 按文件名校验。
func (l *linter) checkDir(rel, typ string) {
	if rel == "GOVERNANCE.md" {
		return
	}
	if typ == "log" {
		if rel != "references/changelog.md" {
			l.errf("%s: type log must be references/changelog.md (GOVERNANCE §5)", rel)
		}
		return
	}
	want, ok := typeDir[typ]
	if !ok {
		return // invalid type already reported
	}
	if !strings.HasPrefix(rel, want+"/") {
		l.errf("%s: type %q must live under %s/ (GOVERNANCE §2/§5)", rel, typ, want)
	}
}

// checkLinks flags orphan relative links: a markdown link to a local path (not
// http/anchor) whose target does not exist. Code spans (fenced ``` blocks +
// inline `code`) are stripped first so an illustrative link inside backticks
// (e.g. GOVERNANCE's "use `[api.md](…)`" example) is not checked.
//
// checkLinks 报告孤儿相对链接；先剥代码片段，故反引号里的示例链接不被检查。
func (l *linter) checkLinks(path, content string) {
	dir := filepath.Dir(path)
	content = inlineCodeRe.ReplaceAllString(fencedRe.ReplaceAllString(content, ""), "")
	for _, m := range linkRe.FindAllStringSubmatch(content, -1) {
		target := strings.TrimSpace(m[1])
		if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") {
			continue
		}
		target = strings.SplitN(target, "#", 2)[0] // drop anchor
		if target == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, target)); err != nil {
			l.errf("%s: orphan link → %q (target missing)", l.rel(path), m[1])
		}
	}
}

// parseFrontmatter reads the leading `---` … `---` block into a key→value map
// (values verbatim; array values like `[human, ai]` kept as the raw string,
// presence is what matters). ok=false when there is no frontmatter block.
//
// parseFrontmatter 把开头的 `---`…`---` 块读成 key→value（值原样，看存在性）。
func parseFrontmatter(content string) (map[string]string, bool) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, false
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return nil, false
	}
	block := content[4 : 4+end]
	fm := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			fm[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return fm, true
}
