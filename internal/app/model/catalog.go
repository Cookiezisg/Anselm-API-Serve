// Package model is the /v1/models use-case layer: it assembles the OpenAI
// model-list envelope from the one live provider-neutral PUBLIC_MODEL_ID. It declares its only
// dependency as a config PORT and never imports infra/net.http/sql. Auth lives
// in transport (device proof → install lookup); this layer is pure declaration and
// bills nothing — listing models reserves no quota.
package model

import (
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/model"
)

// ConfigLoader is the runtime-config port: a snapshot of the live, atomically
// swapped Config. Satisfied structurally by *infra/configprovider.Provider.
// List() snapshots ONCE per call so a hot-reloaded PUBLIC_MODEL_ID is reflected
// on the very next request with no restart.
type ConfigLoader interface {
	Load() *config.Config
}

// Catalog assembles the model list from the live logical model id.
type Catalog struct {
	cfg ConfigLoader
}

// New wires the Catalog to its config port.
func New(cfg ConfigLoader) *Catalog {
	return &Catalog{cfg: cfg}
}

// List exposes exactly one provider-neutral logical model. Provider model ids
// are intentionally absent: request shape selects text vs multimodal and a
// client can never buy/select a hidden "tier" through the model string.
func (c *Catalog) List() model.ListEnvelope {
	cfg := c.cfg.Load()
	id := cfg.PublicModelID
	data := make([]model.Model, 0, 1)
	if id != "" {
		data = append(data, model.Model{
			ID: id, Object: model.ObjectModel, OwnedBy: model.OwnedBy,
			AnselmCapabilities: &model.AnselmCapabilities{
				Version: 1,
				Routing: model.RoutingByContent,
				Text: model.RouteProfile{
					InputLimit:  billing.DeepSeekInputLimit,
					OutputLimit: min64(cfg.MaxTokensCap, billing.DeepSeekOutputLimit),
					Available:   len(cfg.DeepSeekAPIKeys) > 0,
				},
				// Multimodal availability needs BOTH halves. A Qwen key alone is not enough:
				// with MEDIA_ENABLED=false there is no upload/lease path at all, so a client
				// that believed this flag would fail on the very first media request — and it
				// would fail LATE, mid-conversation, instead of the desktop simply not offering
				// image/video. Availability must describe the whole path a caller will walk.
				//
				// 多模态可用性需要**两半都在**。光有 Qwen key 不够:MEDIA_ENABLED=false 时根本没有
				// 上传/lease 通道,信了这个标志的客户端会在**第一次**发媒体时失败——而且是**晚**失败、
				// 失败在对话中途,而不是桌面端干脆不提供图片/视频。可用性必须描述调用方将要走完的**整条**路。
				Multimodal: model.RouteProfile{
					InputLimit:  billing.Qwen37InputLimit,
					OutputLimit: min64(cfg.MaxTokensCap, billing.Qwen37OutputLimit),
					Available:   len(cfg.QwenAPIKeys) > 0 && cfg.MediaEnabled,
				},
				// Image generation follows the same whole-path rule: the flag is true only
				// when the capability is on AND a credential exists (WRK-082 批B).
				// 图像生成同守整条路法则:能力开 **且** 凭证在才 true(批B)。
				ImageGeneration: &model.GenProfile{
					Available:  cfg.ImageEnabled && len(cfg.QwenAPIKeys) > 0,
					DailyLimit: cfg.ImageDailyLimit,
				},
			},
		})
	}
	return model.ListEnvelope{Object: model.ObjectList, Data: data}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
