package sqlite

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden schema gate.
//
// The migration file is what we WRITE; this golden file is what SQLite actually
// BUILT from it. They are not the same artifact, and the gap between them is
// where a schema quietly drifts: an ALTER lands a column at the end of a line, a
// rebuild-and-rename leaves a quoted table name, a CHECK keeps listing an
// identity nothing can write. All three of those were genuinely present in this repo
// before the squash, and none of them was visible by reading the migrations.
//
// So the assertion is made against the built schema, and any change to it must
// be re-approved by regenerating this file on purpose:
//
//	go test ./internal/infra/sqlite -run TestSchemaMatchesGolden -update-golden
//
// migration 文件是我们**写**的东西;这份 golden 是 SQLite 照它**建**出来的东西。两者不是同一个
// 产物,而 schema 正是在两者的缝隙里悄悄漂移的:ALTER 把一列落在某行末尾、重建改名留下一个带
// 引号的表名、CHECK 里继续列着没有东西写得进去的身份——压平之前这三样在本仓**都真实存在**,而且
// 光读迁移文件一样也看不出来。
//
// 故断言对着**建出来的** schema 做,任何改动都必须靠刻意重新生成本文件来重新过审。
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden schema file from the live schema")

const goldenPath = "testdata/schema.golden.sql"

// dumpSchema renders every table and index in a stable order, normalized so the
// comparison is about STRUCTURE and never about whitespace SQLite happened to
// echo back.
//
// dumpSchema 以稳定顺序渲染每张表与每个索引,并做归一化——比较的是**结构**,不是 SQLite 恰好
// 回显的空白。
func dumpSchema(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.Reader.Query(context.Background(),
		`SELECT sql FROM sqlite_master
		  WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'
		  ORDER BY type DESC, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			t.Fatalf("scan ddl: %v", err)
		}
		var norm []string
		for _, line := range strings.Split(ddl, "\n") {
			if line = strings.TrimRight(line, " \t"); strings.TrimSpace(line) != "" {
				norm = append(norm, line)
			}
		}
		out = append(out, strings.Join(norm, "\n")+";")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return strings.Join(out, "\n\n") + "\n"
}

func TestSchemaMatchesGolden(t *testing.T) {
	got := dumpSchema(t, openT(t))

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden schema rewritten (%d bytes)", len(got))
		return
	}

	want, err := os.ReadFile(goldenPath) // #nosec G304 — fixed repo-relative path
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	if got != string(want) {
		t.Errorf("live schema differs from %s.\n"+
			"If the change is intended, re-approve it explicitly:\n"+
			"  go test ./internal/infra/sqlite -run TestSchemaMatchesGolden -update-golden\n\n"+
			"--- golden ---\n%s\n--- live ---\n%s", goldenPath, want, got)
	}
}

// TestGoldenSchemaHasNoRetiredProviderIdentity is a second, narrower assertion on
// the same artifact. The golden file would happily record a resurrected
// `deepseek` or `gemini` in a CHECK — it only pins whatever is there. This one
// says which values are not allowed to be there at all.
//
// 这是对同一份产物的第二条、更窄的断言。golden 文件会**照实**记录 CHECK 里复活的 `deepseek`
// 或 `gemini`——它只钉住「现状」。这一条说的是:哪些值根本不允许出现。
func TestGoldenSchemaHasNoRetiredProviderIdentity(t *testing.T) {
	schema := strings.ToLower(dumpSchema(t, openT(t)))
	for _, retired := range []string{"deepseek", "gemini"} {
		if strings.Contains(schema, retired) {
			t.Errorf("retired provider identity %q is back in the schema", retired)
		}
	}
}
