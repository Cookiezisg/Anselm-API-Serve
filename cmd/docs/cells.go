package main

// Table-cell width gate.
//
// api.md once carried a single table cell 1,901 characters long. No renderer
// shows that — it becomes one unreadable smear across a column — so nobody read
// it, and a contract nobody reads is a contract that drifts. It had drifted:
// the cell still described a model id that was being retired.
//
// The fix was not "write shorter contracts", it was "put prose in prose". A row
// states the shape and points at a subsection; the subsection carries the
// reasoning. This gate keeps that shape from decaying back.
//
// The threshold is set where it needs no exemptions: the widest legitimate cell
// in the repo is the invariants register's ~480 characters, where one row IS one
// invariant plus its consequence. A cell past 600 is not a dense row, it is a
// paragraph that lost its way into a table.
//
// 表格单元格宽度闸。
//
// api.md 曾经有一个 **1901 字符**的单元格。没有任何渲染器显示得了那个东西——它会糊成一列里
// 一片读不了的字——所以没人读它,而一份没人读的契约就是一份会漂移的契约。它确实漂了:那个格子
// 里当时还写着一个正在退役的 model id。
//
// 解法不是「契约写短点」,是「散文该住散文里」。表格行说清形状并指向一个小节,小节承载论证。
// 本闸让这个形状不再退回去。
//
// 阈值取在**不需要任何豁免**的位置:全仓最宽的正当单元格是不变量登记册的约 480 字符,那里一行
// **就是**一条不变量加它的失守后果。超过 600 的格子不是一行密集信息,是一段走错了地方的散文。

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxCellRunes is the ceiling for one markdown table cell.
const maxCellRunes = 600

func checkCellWidth(root string) []string {
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
		if filepath.Ext(path) != ".md" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// ADRs and the archive are immutable records — they are never rewritten, so
		// a wide cell in them is history, not a live readability defect.
		// ADR 与归档是不可变记录,不会被重写,故其中的宽格子是历史、不是活的可读性缺陷。
		if strings.HasPrefix(rel, "docs/decisions/") || strings.HasPrefix(rel, "docs/archive/") {
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
			line := sc.Text()
			if !strings.HasPrefix(line, "| ") {
				continue
			}
			for _, cell := range strings.Split(strings.Trim(line, "|"), " | ") {
				if w := len([]rune(cell)); w > maxCellRunes {
					errs = append(errs, fmt.Sprintf(
						"cell: %s:%d: table cell is %d runes (max %d) — move the reasoning into a "+
							"subsection and leave the row pointing at it", rel, n, w, maxCellRunes))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return []string{fmt.Sprintf("cell: scan failed: %v", err)}
	}
	sort.Strings(errs)
	return errs
}
