package main

// Mixed-script scar detector.
//
// This repository comments in two languages on purpose: an English paragraph and
// a Chinese one that says the same thing. That is a deliberate convention. What
// is NOT deliberate is a Chinese word spliced into the middle of an English
// sentence — `the upstream bills its真实 list price`, `model-list事实源`,
// `chunking长 text HERE`. Those are edit scars: someone was rewriting one half,
// stopped mid-sentence, and both halves have been wrong ever since.
//
// They are invisible to every other gate. The compiler does not read comments,
// gofmt does not read prose, and a human skimming a bilingual file reads whichever
// language they think in and never notices the other one is broken.
//
// The signal is narrow and mechanical: a Latin letter IMMEDIATELY adjacent to a
// CJK ideograph with no space between them. Both languages in this repo are
// written space-separated (`按 rune 计`, `走 Qwen3.7`), so adjacency is not a
// style question — it means a word boundary got eaten.
//
// 中英混排疤痕检测。
//
// 本仓刻意用两种语言写注释:一段英文,一段说同一件事的中文。那是**约定**。**不是**约定的是:一个
// 中文词被拼进一句英文的中间——`the upstream bills its真实 list price`、`model-list事实源`、
// `chunking长 text HERE`。那些是**修改留下的疤**:某人在重写其中一半时停在了句子中间,而从那以后
// 两半都是错的。
//
// 它们对其余每一道门禁都是隐形的:编译器不读注释,gofmt 不读散文,而一个人扫过一份双语文件时只读
// 自己思考所用的那种语言,永远不会注意到另一半断了。
//
// 判据窄而机械:一个拉丁字母**紧贴**一个汉字、中间没有空格。本仓两种语言都是空格分写的
// (`按 rune 计`、`走 Qwen3.7`),故「贴在一起」不是风格问题——它意味着一个词边界被吃掉了。

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// scarExempt lists paths allowed to contain adjacency forever.
//
//   - docs/decisions/, docs/archive/ — immutable records; they are not rewritten,
//     so a scar in them is history rather than a live defect.
//   - the governance ticket — it QUOTES the scars as the worklist that produced
//     this gate. A gate that fails on the document describing it is a gate nobody
//     can land.
//   - this file — it spells the examples out above.
//
// 这些路径可以永远含相邻:ADR 与归档是不可变记录,它们不会被重写,故其中的疤是历史而非活的缺陷;
// 治理工单**引用**这些疤作为催生本闸的待办清单——一道对着描述自己的文档报错的闸没人落得了地;
// 本文件把例子写在上面。
func scarExempt(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "docs/decisions/"),
		strings.HasPrefix(rel, "docs/archive/"),
		rel == "docs/working/repo-governance.md",
		rel == "cmd/docs/scars.go":
		return true
	}
	return false
}

// isCJK reports whether r is a Han ideograph. Punctuation is deliberately NOT
// included: `。` after a Latin word is ordinary Chinese sentence-ending, not a
// scar.
//
// isCJK 只认汉字。标点**刻意不算**:一个拉丁词后面跟 `。` 是正常的中文句末,不是疤。
func isCJK(r rune) bool { return unicode.Is(unicode.Han, r) }

func isLatin(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// checkScars walks the tree and reports every Latin/CJK adjacency, with the
// offending fragment quoted so the fix needs no second lookup.
func checkScars(root string) []string {
	var errs []string
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
		if ext := filepath.Ext(path); ext != ".go" && ext != ".md" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if scarExempt(rel) {
			return nil
		}
		f, openErr := os.Open(path) // #nosec G304 — WalkDir over the repo, never user input
		if openErr != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := []rune(sc.Text())
			for i := 1; i < len(line); i++ {
				a, b := line[i-1], line[i]
				if (isLatin(a) && isCJK(b)) || (isCJK(a) && isLatin(b)) {
					errs = append(errs, fmt.Sprintf(
						"scar: %s:%d: Latin and CJK with no space between them — %q "+
							"(a rewrite stopped mid-sentence; fix both halves)",
						rel, n, fragment(line, i)))
					break // one report per line is enough to send someone to it
				}
			}
		}
		return nil
	})
	if err != nil {
		return []string{fmt.Sprintf("scar: scan failed: %v", err)}
	}
	sort.Strings(errs)
	return errs
}

// fragment quotes a short window around the adjacency.
func fragment(line []rune, i int) string {
	lo, hi := i-16, i+16
	if lo < 0 {
		lo = 0
	}
	if hi > len(line) {
		hi = len(line)
	}
	return strings.TrimSpace(string(line[lo:hi]))
}
