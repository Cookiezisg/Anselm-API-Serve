package config

import "github.com/sunweilin/anselm/gateway/internal/domain/billing"

// The double-half rule: a capability exists on this deployment only when BOTH
// halves are present — its own switch is on, AND the Qwen credential that pays
// for it is configured.
//
// The rule was spelled out by hand in five places: the four generation services
// and the published capability list. That is exactly the arrangement in which one
// copy drifts and starts advertising a path the caller cannot walk — and the one
// that drifted really was the published list, which is the copy a client believes.
//
// It lives in domain because it is a property of the configuration itself, not of
// any use case: given a Config, whether image generation can happen is already
// decided, and every layer should be reading the same answer.
//
// 双半规则:一个能力在本部署上存在,当且仅当**两半都在**——自己的开关开着,**且**为它付钱的 Qwen
// 凭证已配置。
//
// 这条规则被手写了五遍:四个生成服务加已发布的能力列表。那正是「其中一份漂移、开始宣告一条调用方
// 走不通的路」的排布——而真的漂移过的那一份正是**已发布的列表**,也就是客户端会相信的那一份。
//
// 它住在 domain,因为它是**配置自身**的属性、不是任何用例的属性:给定一份 Config,图像生成能不能
// 发生就已经定了,而每一层都该读到同一个答案。

// Credentialed reports the half every capability shares. A nil Config is not
// credentialed rather than a panic — callers hold a snapshot they did not build.
//
// Credentialed 报告每个能力共有的那一半。nil Config 视为「无凭证」而不是 panic——调用方持有的是
// 一份不是它自己造的快照。
func (c *Config) Credentialed() bool {
	return c != nil && len(c.QwenAPIKeys) > 0
}

// MultimodalAvailable: media additionally needs the upload/lease path. Without
// it a client that believed the flag fails on its FIRST media request, mid
// conversation, instead of simply not offering an image picker.
//
// MultimodalAvailable:媒体额外需要上传/lease 通道。没有它,信了这个标志的客户端会在**第一次**发
// 媒体时、在对话中途失败,而不是干脆不提供图片选择器。
func (c *Config) MultimodalAvailable() bool {
	return c.Credentialed() && c.MediaEnabled
}

// ImageAvailable 图像生成。
func (c *Config) ImageAvailable() bool {
	return c.Credentialed() && c.ImageEnabled
}

// SpeechAvailable is speech SYNTHESIS. An operator may run one capability without
// the other, so no client may infer either from the other.
//
// SpeechAvailable 指语音**合成**。运营者可能只开其中一个,故客户端不得从一个推另一个。
func (c *Config) SpeechAvailable() bool {
	return c.Credentialed() && c.SpeechEnabled
}

// VideoAvailable needs a THIRD half: the handle-signing key. A gateway that could
// submit but never let the caller poll would advertise a feature that eats a
// daily clip and returns nothing.
//
// VideoAvailable 需要**第三半**:句柄签名密钥。一个「提交得了、却永远不让调用方轮询」的网关,宣告
// 的是一个吃掉一条日额度、什么也不给的功能。
func (c *Config) VideoAvailable() bool {
	return c.Credentialed() && c.VideoEnabled && len(c.VideoHandleKey) > 0
}

// VideoI2VAvailable is narrower than VideoAvailable: publishing animation also
// requires the separately priced first-frame model. It is intentionally derived
// from the whole video path so a caller never sees a capability whose task it
// cannot later poll.
func (c *Config) VideoI2VAvailable() bool {
	if !c.VideoAvailable() {
		return false
	}
	_, err := billing.NewUnitPlan(billing.ProviderQwen, c.VideoI2VUpstreamModel, billing.InputVideoSeconds, 1)
	return err == nil
}

// VoiceAvailable rides the speech switch — a deployment that cannot speak has no
// use for a cloned voice. Named separately so that decision has one home to
// change rather than being re-derived at each call site.
//
// VoiceAvailable 搭在语音开关上——说不了话的部署要克隆音色没有用。单独命名,使这个决定有**一个**
// 可改的家,而不是在每个调用点被重新推导一遍。
func (c *Config) VoiceAvailable() bool {
	return c.SpeechAvailable()
}
