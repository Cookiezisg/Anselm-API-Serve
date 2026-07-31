// Package tts owns the speech-synthesis use case: authorize an install, reserve
// the per-character plan (monthly + wallet + category-daily gates), call the sync
// DashScope upstream through a port, then settle the deterministic per-character
// cost — or roll back only when the upstream provably never billed. That skeleton
// is genrun's; what is tts-shaped is the character unit and voice resolution.
//
// It is deliberately NOT named `speech`: `app/speech` is already the realtime
// ASR (speech→text) use case, and one package name answering for both
// directions would put transcription and synthesis in the same drawer.
//
// tts 包持有语音合成用例:鉴权 install、按字符预留(月度+钱包+品类日三闸)、经端口调用同步 DashScope
// 上游,再结算那笔确定的按字符成本——仅当上游可证明从未计费时才回滚。那套骨架归 genrun;属于 tts
// 形状的是字符这个单位与音色解析。
//
// 刻意**不**叫 `speech`:`app/speech` 已是实时 ASR(语音→文本)用例,一个包名答两个方向会把转写与
// 合成塞进同一个抽屉。
package tts

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/sunweilin/anselm/gateway/internal/app/genrun"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

// Upstream is the sync speech-synthesis port. Same unbilled discipline as the
// image port: unbilled=true ONLY for a provably-unbilled explicit rejection.
//
// Upstream 是同步语音合成端口。与图像端口同一条 unbilled 纪律:仅可证明未计费的显式拒绝才为真。
type Upstream interface {
	GenerateSpeech(ctx context.Context, model, text, voice string) (audio []byte, unbilled bool, err error)
}

// Voices resolves a voice HANDLE this install owns into the id the provider knows.
//
// **The handle is deliberately not the provider's id, and that is an isolation boundary, not a
// formality.** `GET /v1/voices` hands out the gateway's own row id; if the provider's id crossed
// that line instead, any install could synthesize with any other install's cloned voice by naming
// it — the same reasoning that made video hand back a signed handle rather than a bare task id
// (ADR 0015). Resolution therefore happens HERE, scoped to the caller's install, or not at all.
//
// Voices 把**本 install 拥有的**音色句柄解析成供应商认识的 id。
//
// **句柄刻意不是供应商的 id,而这是一条隔离边界、不是形式。** `GET /v1/voices` 给出的是网关自己的行 id;
// 若越过这条线交出供应商 id,任何 install 都能**指名**别人的克隆音色去合成——与视频交回**签名句柄**而非
// 裸 task id 同一条理由(ADR 0015)。故解析发生在**这里**、按调用方的 install 收窄,否则就不发生。
type Voices interface {
	ListVoices(ctx context.Context, installID string) ([]domvoice.Voice, error)
}

type Service struct {
	gen      genrun.Runner
	upstream Upstream
	voices   Voices
}

type Deps struct {
	Auth     genrun.Authenticator
	Quota    genrun.Quota
	RL       genrun.RateLimiter
	Config   genrun.Config
	Upstream Upstream
	Voices   Voices
	Clock    genrun.Clock
	Metrics  genrun.Metrics
}

func New(d Deps) *Service {
	return &Service{
		gen: genrun.New(genrun.Ports{Auth: d.Auth, RL: d.RL, Config: d.Config,
			Quota: d.Quota, Clock: d.Clock, Metrics: d.Metrics}),
		upstream: d.Upstream,
		voices:   d.Voices,
	}
}

// Available reports whether the whole speech path exists on this deployment.
//
// Available 报告整条语音路是否存在。
func (s *Service) Available() bool {
	return s != nil && s.gen.Settings().SpeechAvailable()
}

// Synthesize runs the full use case and returns the synthesized audio.
//
// The billed unit is the INPUT text's rune count, known exactly before the call
// — so reserve == settle on success without any usage feedback from the
// upstream. Runes, not bytes: the whole point of a character price is that one
// Chinese character costs one character, and a byte count would charge it three.
//
// Synthesize 跑完整用例并返回合成音频。计费单位是**输入**文本的 rune 数,调用前即精确已知——故成功时
// reserve == settle,无需上游回报 usage。按 rune 而非 byte:字符计价的全部意义就是一个汉字算一个
// 字符,按字节数会把它算成三个。
func (s *Service) Synthesize(ctx context.Context, installID, text, voice string) ([]byte, *apierr.APIError) {
	if s == nil || !s.gen.Ready() || s.upstream == nil {
		return nil, apierr.Internal()
	}
	if !s.Available() {
		return nil, apierr.ErrTTSUnavailable
	}
	got, ae := s.gen.Authorize(ctx, installID)
	if ae != nil {
		return nil, ae
	}

	c := s.gen.Settings()
	model := strings.TrimSpace(c.TTSUpstreamModel)
	characters := int64(utf8.RuneCountInString(text))
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, model, billing.InputCharacters, characters)
	if err != nil {
		// Startup validation pins the card and the handler bounds the length; reaching
		// this means config drifted mid-flight.
		// 启动校验已钉卡、handler 已界长度;走到这里说明配置在途漂移。
		return nil, apierr.Internal()
	}
	return genrun.Do(ctx, s.gen, got,
		genrun.Charge{Plan: plan, Class: billing.InputCharacters, Units: characters},
		func(ctx context.Context) ([]byte, bool, error) {
			// Resolved after the wallet, not before: a caller with no allowance left must
			// not have caused a lookup of what voices they own.
			// 在钱包**之后**才解析,不在之前:一个额度已尽的调用方,不该导致我们去查他有哪些音色。
			name := voice
			if strings.TrimSpace(name) == "" {
				name = strings.TrimSpace(c.TTSDefaultVoice)
			}
			return s.upstream.GenerateSpeech(ctx, model, text, s.resolveVoice(ctx, got, name))
		})
}

// resolveVoice swaps a handle this install owns for the provider's id, and passes anything else
// through UNCHANGED — preset voices are not rows here, and rewriting them would break every
// synthesis that never involved cloning.
//
// A handle that belongs to someone else simply does not match, so it travels on as a literal name
// the provider will reject. That is the desired outcome: refusing loudly here would tell the caller
// that some OTHER install owns that id.
//
// resolveVoice 把**本 install 拥有的**句柄换成供应商的 id,其余一律**原样透传**——预置音色不是这里的行,
// 改写它们会弄坏每一次根本不涉及克隆的合成。
//
// 属于别人的句柄**匹配不上**,于是作为一个字面名字继续走下去、被供应商拒掉。这正是想要的结果:在这里
// 大声拒绝,等于告诉调用方「那个 id 属于**另一个** install」。
func (s *Service) resolveVoice(ctx context.Context, installID, voice string) string {
	handle := strings.TrimSpace(voice)
	if s == nil || s.voices == nil || handle == "" {
		return voice
	}
	rows, err := s.voices.ListVoices(ctx, installID)
	if err != nil {
		return voice
	}
	for _, v := range rows {
		if v.ID == handle && strings.TrimSpace(v.UpstreamID) != "" {
			return v.UpstreamID
		}
	}
	return voice
}
