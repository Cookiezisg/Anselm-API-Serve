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
				// Text and Multimodal are two PROFILES of one route, not two routes:
				// both reach the same model, so both report that model's limits and
				// both depend on that model's credential. They stay separate entries
				// because they differ in what the caller must have working — media
				// additionally needs the upload/lease path (see below).
				//
				// Text therefore tracks the CREDENTIAL alone. Deriving it from
				// anything else is how a published capability starts describing a
				// precondition the route does not actually have.
				//
				// Text 与 Multimodal 是**同一条路由的两份画像**、不是两条路由:两者到达同一个
				// 模型,故都报那个模型的上限、都依赖那把凭证。它们仍然分开,是因为调用方需要
				// **备齐的东西**不同——媒体额外需要上传/lease 通道(见下)。
				//
				// 故 Text 只跟着**凭证**走。从别的东西推导它,正是「已发布的能力开始描述一个该
				// 路由其实没有的前提」的开端。
				Text: model.RouteProfile{
					InputLimit:  billing.Qwen37InputLimit,
					OutputLimit: min64(cfg.MaxTokensCap, billing.Qwen37OutputLimit),
					Available:   cfg.Credentialed(),
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
					Available:   cfg.MultimodalAvailable(),
				},
				// Image generation follows the same whole-path rule: the flag is true only
				// when the capability is on AND a credential exists.
				// 图像生成同守整条路法则:能力开 **且** 凭证在才 true。
				ImageGeneration: &model.GenProfile{
					Available:  cfg.ImageAvailable(),
					DailyLimit: cfg.ImageDailyLimit,
				},
				// Speech synthesis, same whole-path rule and its OWN switch: an operator may
				// run one capability without the other, so a client must not infer either
				// from the other. DailyLimit here is CHARACTERS.
				// 语音合成同守整条路法则、且是**自己的**开关:运营者可能只开其中一个,客户端不得
				// 从一个推另一个。此处 DailyLimit 的单位是**字符**。
				SpeechGeneration: &model.GenProfile{
					Available:  cfg.SpeechAvailable(),
					DailyLimit: cfg.SpeechDailyLimit,
				},
				// Video is the third generation capability, same whole-path rule with one
				// extra half: the handle-signing key. A gateway that could submit but never
				// let the caller poll would advertise a feature that eats a daily clip and
				// returns nothing. DailyLimit here is CLIPS.
				// 视频是第三个生成能力,同守整条路法则、外加一半:句柄签名密钥。一个「提交得了、却
				// 永远不让调用方轮询」的网关,宣告的是一个吃掉一条日额度、什么也不给的功能。
				// 此处 DailyLimit 的单位是**条**。
				VideoGeneration: &model.VideoGenProfile{
					Available:    cfg.VideoAvailable(),
					DailyLimit:   cfg.VideoDailyLimit,
					ImageToVideo: cfg.VideoI2VAvailable(),
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
