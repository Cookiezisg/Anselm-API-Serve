package idgen

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
)

func TestInstallIDPrefixAndWidth(t *testing.T) {
	id := InstallID()
	body, ok := strings.CutPrefix(id, "ins_")
	if !ok {
		t.Fatalf("InstallID() = %q, want ins_ prefix", id)
	}
	// B13: 16 bytes of entropy ⇒ 32 hex chars. Guards against a silent
	// regression back to the collision-prone 8-byte id.
	raw, err := hex.DecodeString(body)
	if err != nil {
		t.Fatalf("InstallID() body not hex: %v", err)
	}
	if len(raw) != installEntropyBytes {
		t.Fatalf("InstallID() entropy = %d bytes, want %d", len(raw), installEntropyBytes)
	}
	if len(body) != 32 {
		t.Fatalf("InstallID() hex len = %d, want 32", len(body))
	}
}

func TestRequestIDPrefixAndWidth(t *testing.T) {
	id := RequestID()
	body, ok := strings.CutPrefix(id, "req_")
	if !ok {
		t.Fatalf("RequestID() = %q, want req_ prefix", id)
	}
	raw, err := hex.DecodeString(body)
	if err != nil {
		t.Fatalf("RequestID() body not hex: %v", err)
	}
	if len(raw) != requestEntropyBytes {
		t.Fatalf("RequestID() entropy = %d bytes, want %d", len(raw), requestEntropyBytes)
	}
}

func TestUniquenessAcrossManyCalls(t *testing.T) {
	const n = 50_000
	cases := map[string]func() string{
		"InstallID": InstallID,
		"RequestID": RequestID,
	}
	for name, gen := range cases {
		seen := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			v := gen()
			if _, dup := seen[v]; dup {
				t.Fatalf("%s produced duplicate after %d calls: %q", name, i, v)
			}
			seen[v] = struct{}{}
		}
	}
}

// Concurrency smoke test: crypto/rand is safe for concurrent use, and the
// minters hold no shared state. Run under -race to assert that.
func TestConcurrentMintingNoDup(t *testing.T) {
	const goroutines, per = 16, 2000
	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*per)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, per)
			for i := 0; i < per; i++ {
				local = append(local, InstallID(), RequestID())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, v := range local {
				if _, dup := seen[v]; dup {
					t.Errorf("concurrent duplicate: %q", v)
					return
				}
				seen[v] = struct{}{}
			}
		}()
	}
	wg.Wait()
}
