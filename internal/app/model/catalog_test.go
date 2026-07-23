package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dmodel "github.com/sunweilin/anselm/gateway/internal/domain/model"
)

// fakeCfg is a mutable ConfigLoader: swapping its pointer mid-test simulates a
// hot-reload of the single provider-neutral public model id.
type fakeCfg struct{ c *config.Config }

func (f *fakeCfg) Load() *config.Config { return f.c }

func TestListExposesOneLogicalModel(t *testing.T) {
	cat := New(&fakeCfg{c: &config.Config{
		PublicModelID: "anselm-auto", MaxTokensCap: 16_384,
		DeepSeekAPIKeys: []string{"text"}, KimiAPIKeys: []string{"media"},
	}})
	env := cat.List()
	if env.Object != dmodel.ObjectList {
		t.Fatalf("object = %q want list", env.Object)
	}
	if len(env.Data) != 1 {
		t.Fatalf("data len = %d want 1", len(env.Data))
	}
	m := env.Data[0]
	if m.ID != "anselm-auto" || m.Object != dmodel.ObjectModel || m.OwnedBy != dmodel.OwnedBy {
		t.Fatalf("data[0] = %+v", m)
	}
	if m.AnselmCapabilities == nil ||
		m.AnselmCapabilities.Text.InputLimit != billing.DeepSeekInputLimit ||
		m.AnselmCapabilities.Multimodal.InputLimit != billing.KimiInputLimit ||
		m.AnselmCapabilities.Text.OutputLimit != 16_384 {
		t.Fatalf("route capabilities = %+v", m.AnselmCapabilities)
	}
}

// A live alias edit is reflected on the very next List() — no restart, no
// cached snapshot held across calls.
func TestListHotReload(t *testing.T) {
	fc := &fakeCfg{c: &config.Config{PublicModelID: "anselm-auto"}}
	cat := New(fc)

	if env := cat.List(); len(env.Data) != 1 || env.Data[0].ID != "anselm-auto" {
		t.Fatalf("pre-reload = %+v", env.Data)
	}

	fc.c = &config.Config{PublicModelID: "anselm-v2"}

	env := cat.List()
	if len(env.Data) != 1 || env.Data[0].ID != "anselm-v2" {
		t.Fatalf("post-reload = %+v", env.Data)
	}
}

// Empty logical id → Data is a non-nil empty slice that marshals to "data":[],
// never "data":null.
func TestListEmptyIDIsEmptyNotNull(t *testing.T) {
	cat := New(&fakeCfg{c: &config.Config{}})
	env := cat.List()
	if env.Data == nil {
		t.Fatal("Data must be non-nil for an empty logical model id")
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
