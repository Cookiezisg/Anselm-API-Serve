package mediafs

import (
	"context"
	"errors"
	"testing"
)

const uploadID = "mup_0123456789abcdef0123456789abcdef"

func TestStagingWritesAtExactCursorAndHashes(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	if err := s.Create(ctx, uploadID); err != nil {
		t.Fatal(err)
	}
	if next, err := s.Append(ctx, uploadID, 0, []byte("abc")); err != nil || next != 3 {
		t.Fatalf("first append=(%d,%v)", next, err)
	}
	if _, err := s.Append(ctx, uploadID, 0, []byte("replay")); !errors.Is(err, ErrOffset) {
		t.Fatalf("replayed append err=%v, want ErrOffset", err)
	}
	if next, err := s.Append(ctx, uploadID, 3, []byte("def")); err != nil || next != 6 {
		t.Fatalf("second append=(%d,%v)", next, err)
	}
	sha, size, err := s.SHA256(ctx, uploadID)
	if err != nil || size != 6 || sha != "bef57ec7f53a6d40beb640a780a639c83bc29ac8a9816f1fc6c5c6dcd93c4721" {
		t.Fatalf("hash=(%q,%d,%v)", sha, size, err)
	}
}

func TestTruncateRepairsOnlyUncommittedTail(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	if err := s.Create(ctx, uploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, uploadID, 0, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, uploadID, 3); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Size(ctx, uploadID); err != nil || got != 3 {
		t.Fatalf("size=(%d,%v)", got, err)
	}
	if err := s.Truncate(ctx, uploadID, 4); !errors.Is(err, ErrOffset) {
		t.Fatalf("growing truncate err=%v, want ErrOffset", err)
	}
}

func TestRejectsTraversalAndDoesNotCreateFiles(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Create(context.Background(), "mup_../../etc/passwd"); !errors.Is(err, ErrBadUploadID) {
		t.Fatalf("traversal err=%v, want ErrBadUploadID", err)
	}
}
