package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dmodel "github.com/sunweilin/anselm/gateway/internal/domain/model"
)

// fakeCfg is a mutable ConfigLoader: swapping its pointer mid-test simulates a
// hot-reload of the allowlist (the real Provider swaps atomically; List() reads
// Load() per call, so the next List sees the new value).
type fakeCfg struct{ c *config.Config }

func (f *fakeCfg) Load() *config.Config { return f.c }

func TestListReflectsAllowlistOrder(t *testing.T) {
	want := []string{"deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}
	cat := New(&fakeCfg{c: &config.Config{ModelAllowlist: want}})
	env := cat.List()
	if env.Object != dmodel.ObjectList {
		t.Fatalf("object = %q want list", env.Object)
	}
	if len(env.Data) != len(want) {
		t.Fatalf("data len = %d want %d", len(env.Data), len(want))
	}
	for i, id := range want {
		m := env.Data[i]
		if m.ID != id || m.Object != dmodel.ObjectModel || m.OwnedBy != dmodel.OwnedBy {
			t.Fatalf("data[%d] = %+v want id=%q model anselm-gateway", i, m, id)
		}
	}
}

// A live allowlist edit is reflected on the very next List() — no restart, no
// cached snapshot held across calls.
func TestListHotReload(t *testing.T) {
	fc := &fakeCfg{c: &config.Config{ModelAllowlist: []string{"deepseek-v4-flash"}}}
	cat := New(fc)

	if env := cat.List(); len(env.Data) != 1 || env.Data[0].ID != "deepseek-v4-flash" {
		t.Fatalf("pre-reload = %+v want single deepseek-v4-flash", env.Data)
	}

	fc.c = &config.Config{ModelAllowlist: []string{"deepseek-v4-flash", "deepseek-reasoner"}}

	env := cat.List()
	if len(env.Data) != 2 || env.Data[1].ID != "deepseek-reasoner" {
		t.Fatalf("post-reload = %+v want two entries incl deepseek-reasoner", env.Data)
	}
}

// Empty allowlist → Data is a non-nil empty slice that marshals to "data":[],
// never "data":null.
func TestListEmptyAllowlistIsEmptyNotNull(t *testing.T) {
	cat := New(&fakeCfg{c: &config.Config{ModelAllowlist: nil}})
	env := cat.List()
	if env.Data == nil {
		t.Fatal("Data must be non-nil for an empty allowlist")
	}
	if len(env.Data) != 0 {
		t.Fatalf("Data len = %d want 0", len(env.Data))
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"data":[]`) {
		t.Fatalf("empty list = %s, want data:[]", got)
	}
}
