package voicestore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"

	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"
)

// These tests run against real SQLite because the two things worth asserting here are things only
// a real database does: the UNIQUE index that catches a name race, and the serialized writer that
// makes the in-transaction ceiling re-check meaningful. A fake would assert my own arithmetic.
//
// 这些测试跑在**真的** SQLite 上,因为这里唯二值得断言的东西都是只有真数据库才做的事:接住重名竞态的
// UNIQUE 索引,以及使事务内上限重查有意义的串行写者。用假件,只会断言我自己写的算术。

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	w.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = w.Close() })
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	for _, stmt := range []string{
		`CREATE TABLE install_voices (
			id TEXT PRIMARY KEY, install_id TEXT NOT NULL, name TEXT NOT NULL,
			upstream_id TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE UNIQUE INDEX idx_install_voice_name ON install_voices (install_id, name)`,
		`CREATE INDEX idx_install_voice_owner ON install_voices (install_id)`,
	} {
		if _, err := w.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return New(orm.Open(w), orm.Open(r))
}

func voice(id, name string, at int64) domvoice.Voice {
	return domvoice.Voice{ID: id, Name: name, UpstreamID: "u-" + id, CreatedAt: time.Unix(at, 0).UTC()}
}

// TestCreateAndList_IsolatedPerInstall: one install must never see or count another's voices — the
// per-install ceiling would otherwise be enforced against a total nobody asked for.
//
// TestCreateAndList_IsolatedPerInstall:一个 install 绝不能看见或数进另一个的音色——否则逐 install
// 上限就会拿一个没人要求过的总数来强制执行。
func TestCreateAndList_IsolatedPerInstall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateVoice(ctx, "ins_a", voice("v1", "narrator", 100)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateVoice(ctx, "ins_b", voice("v2", "narrator", 200)); err != nil {
		t.Fatalf("create for the other install (same name) must be allowed: %v", err)
	}
	a, err := s.ListVoices(ctx, "ins_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(a) != 1 || a[0].ID != "v1" || a[0].UpstreamID != "u-v1" {
		t.Fatalf("ins_a sees %+v", a)
	}
	if n, err := s.CountAllVoices(ctx); err != nil || n != 2 {
		t.Fatalf("account total = %d, %v; want 2 — the number only this gateway can see", n, err)
	}
}

// TestList_EmptyIsNonNil: an install with no voices must produce [] rather than null, so no client
// has to special-case null before it can count.
//
// TestList_EmptyIsNonNil:没有音色的 install 必须给出 [] 而非 null,使没有客户端需要先判 null 才数得了数。
func TestList_EmptyIsNonNil(t *testing.T) {
	got, err := newTestStore(t).ListVoices(context.Background(), "ins_nobody")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("empty inventory must be a non-nil slice")
	}
}

// TestList_StableOrder: the rows a user may keep must not swap places between two reads of an
// unchanged inventory — a list that reorders itself makes "delete the second one" ambiguous.
//
// TestList_StableOrder:用户能留的那几行,绝不能在同一份没变过的库存的两次读之间互换位置——一个会自己
// 重排的列表,会让「删第二个」变成歧义。
func TestList_StableOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Same created_at on purpose: the tiebreaker, not the timestamp, is what makes this stable.
	// 刻意给同一个 created_at:让它稳定的是**次键**、不是时间戳。
	if err := s.CreateVoice(ctx, "ins_a", voice("v_zzz", "b", 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateVoice(ctx, "ins_a", voice("v_aaa", "a", 100)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := s.ListVoices(ctx, "ins_a")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != "v_aaa" || got[1].ID != "v_zzz" {
			t.Fatalf("read %d: order = %v", i, []string{got[0].ID, got[1].ID})
		}
	}
}

// TestCreate_CeilingIsRecheckedInTransaction: the service checked the ceiling before it spent money,
// but another request can land between that read and this write. The transaction is what makes the
// third voice impossible rather than merely unlikely.
//
// TestCreate_CeilingIsRecheckedInTransaction:service 在花钱之前查过上限,但那次读与这次写之间会落进
// 另一个请求。**事务**才是让第三个音色「不可能」而不只是「不太可能」的那个东西。
func TestCreate_CeilingIsRecheckedInTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < domvoice.PerInstallInventory; i++ {
		if err := s.CreateVoice(ctx, "ins_a", voice(string(rune('a'+i)), string(rune('a'+i)), int64(100+i))); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	err := s.CreateVoice(ctx, "ins_a", voice("over", "over", 999))
	if !errors.Is(err, domvoice.ErrInventoryFull) {
		t.Fatalf("err = %v, want ErrInventoryFull", err)
	}
	if n, _ := s.CountAllVoices(ctx); n != domvoice.PerInstallInventory {
		t.Fatalf("a refused create still wrote a row: total = %d", n)
	}
}

// TestCreate_ConcurrentSameNameYieldsExactlyOne: the UNIQUE index is the name race's only catcher.
// Two concurrent enrollments of one name both pass the service's pre-check; if both landed, the
// first registration would be stranded upstream where nothing can address it again.
//
// TestCreate_ConcurrentSameNameYieldsExactlyOne:UNIQUE 索引是重名竞态**唯一**的接手者。同一个名字的
// 两次并发登记在 service 前置检查里都会通过;若两个都落盘,第一个登记会搁浅在上游、再没有东西寻址得到它。
func TestCreate_ConcurrentSameNameYieldsExactlyOne(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.CreateVoice(ctx, "ins_a", voice(string(rune('x'+i)), "narrator", int64(100+i)))
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	var ok, taken int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domvoice.ErrNameTaken):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || taken != 1 {
		t.Fatalf("ok=%d taken=%d; exactly one enrollment of a name may win", ok, taken)
	}
}

// TestDelete_OwnershipEnforced: another install's voice id must read as absent, so ids never become
// an existence oracle — and the row must survive.
//
// TestDelete_OwnershipEnforced:别的 install 的音色 id 必须读作不存在,故 id 永远不会变成存在性预言机
// ——且那一行必须活下来。
func TestDelete_OwnershipEnforced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateVoice(ctx, "ins_a", voice("v1", "narrator", 100)); err != nil {
		t.Fatal(err)
	}
	up, found, err := s.DeleteVoice(ctx, "ins_b", "v1")
	if err != nil || found || up != "" {
		t.Fatalf("another install deleted it: up=%q found=%v err=%v", up, found, err)
	}
	if n, _ := s.CountAllVoices(ctx); n != 1 {
		t.Fatalf("row gone: total = %d", n)
	}
	up, found, err = s.DeleteVoice(ctx, "ins_a", "v1")
	if err != nil || !found || up != "u-v1" {
		t.Fatalf("owner delete: up=%q found=%v err=%v", up, found, err)
	}
	if n, _ := s.CountAllVoices(ctx); n != 0 {
		t.Fatalf("row survived its owner's delete: total = %d", n)
	}
}

// TestDelete_ReturnsUpstreamIDBeforeDropping: the row is the ONLY thing holding the upstream id, so
// the caller must get it back or the registration becomes unreclaimable.
//
// TestDelete_ReturnsUpstreamIDBeforeDropping:行是**唯一**持有上游 id 的东西,故调用方必须拿得回它,
// 否则那份登记就再也收不回来了。
func TestDelete_ReturnsUpstreamIDBeforeDropping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateVoice(ctx, "ins_a", voice("v1", "n", 100)); err != nil {
		t.Fatal(err)
	}
	up, found, err := s.DeleteVoice(ctx, "ins_a", "v1")
	if err != nil || !found || up == "" {
		t.Fatalf("the upstream handle was not returned: up=%q found=%v err=%v", up, found, err)
	}
	if _, found, _ := s.DeleteVoice(ctx, "ins_a", "v1"); found {
		t.Fatal("a second delete reported found")
	}
}
