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
		DeepSeekAPIKeys: []string{"text"}, QwenAPIKeys: []string{"media"},
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
		m.AnselmCapabilities.Multimodal.InputLimit != billing.Qwen37InputLimit ||
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

// Availability must describe the WHOLE path a caller will walk, not just whether a key exists. A
// Qwen key with MEDIA_ENABLED=false has no upload/lease channel at all, so a desktop that believed
// `multimodal.available` would offer image/video and then fail on the user's FIRST media message —
// late, mid-conversation — instead of simply not offering it.
//
// This is exactly the shape of bug that ships when a published capability is derived from one half
// of its precondition.
//
// 可用性必须描述调用方将要走完的**整条**路,而不只是「有没有 key」。有 Qwen key 但 MEDIA_ENABLED=false 时
// 根本没有上传/lease 通道:信了 `multimodal.available` 的桌面端会提供图片/视频,然后在用户**第一条**媒体
// 消息上失败——晚失败、失败在对话中途,而不是干脆不提供。
//
// 「已发布的能力只由其前提的一半推导」正是这类 bug 的形状。
func TestMultimodalAvailabilityRequiresBothTheKeyAndTheMediaPath(t *testing.T) {
	cases := []struct {
		name     string
		qwenKeys []string
		media    bool
		want     bool
	}{
		{"key and media path", []string{"media"}, true, true},
		{"key but no media path", []string{"media"}, false, false},
		{"media path but no key", nil, true, false},
		{"neither", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := New(&fakeCfg{c: &config.Config{
				PublicModelID: "anselm-auto", MaxTokensCap: 16_384,
				DeepSeekAPIKeys: []string{"text"},
				QwenAPIKeys:     tc.qwenKeys,
				MediaEnabled:    tc.media,
			}})
			caps := cat.List().Data[0].AnselmCapabilities
			if caps.Multimodal.Available != tc.want {
				t.Fatalf("multimodal.available = %v, want %v (qwenKeys=%v mediaEnabled=%v)",
					caps.Multimodal.Available, tc.want, tc.qwenKeys, tc.media)
			}
			// The text route must stay independent of the media capability. 文本路由不受媒体能力牵连。
			if !caps.Text.Available {
				t.Fatal("the text route must not be gated on the media path")
			}
		})
	}
}
