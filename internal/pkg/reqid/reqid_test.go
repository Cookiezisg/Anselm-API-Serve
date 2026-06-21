package reqid

import (
	"strings"
	"testing"
)

func TestMintNonEmptyAndUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := Mint()
		if id == "" {
			t.Fatal("Mint returned empty")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("Mint collision: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestSanitizeReusesSafeValue(t *testing.T) {
	for _, in := range []string{"abc123", "A-B_c-9", "x", strings.Repeat("a", MaxLen)} {
		if got := Sanitize(in); got != in {
			t.Errorf("Sanitize(%q) = %q, want passthrough", in, got)
		}
	}
}

func TestSanitizeMintsOnUnsafe(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"too long":  strings.Repeat("a", MaxLen+1),
		"newline":   "abc\ndef",
		"space":     "abc def",
		"slash":     "a/b",
		"control":   "abc\x00",
		"non-ascii": "abcé",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := Sanitize(in)
			if got == in {
				t.Fatalf("Sanitize(%q) reused unsafe value", in)
			}
			if got == "" {
				t.Fatal("Sanitize returned empty")
			}
			if clean(got) != got {
				t.Fatalf("Sanitize minted unsafe id %q", got)
			}
		})
	}
}

func TestSanitizeBoundsLength(t *testing.T) {
	if got := Sanitize(strings.Repeat("a", MaxLen+50)); len(got) > MaxLen {
		t.Fatalf("len=%d exceeds MaxLen=%d", len(got), MaxLen)
	}
}
