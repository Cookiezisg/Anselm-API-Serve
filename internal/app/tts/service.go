// Package tts owns the speech-synthesis use case (WRK-082 批C): authorize an
// install, reserve the per-character plan (monthly + wallet + category-daily
// gates), call the sync DashScope upstream through a port, then settle the
// deterministic per-character cost — or roll back only when the upstream
// provably never billed. It is the app/image twin, one billing unit over.
//
// It is deliberately NOT named `speech`: `app/speech` is already the realtime
// ASR (speech→text) use case, and one package name answering for both
// directions would put transcription and synthesis in the same drawer.
//
// Package tts 拥有语音合成用例(批C),是 app/image 的孪生件、只差一个计费单位。
// 刻意**不**叫 `speech`:`app/speech` 已是实时 ASR(语音→文本)用例,一个包名答两个方向
// 会把转写与合成塞进同一个抽屉。
package tts

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

type Authenticator interface {
	LookupInstall(ctx context.Context, installID string) (id string, status dominstall.Status, found bool, err error)
}

type RateLimiter interface {
	Allow(key string) bool
}

type Config interface {
	Load() *config.Config
}

type Quota interface {
	SnapshotPeriod(now time.Time) domquota.Period
	Reserve(ctx context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error)
	Settle(ctx context.Context, r *domquota.Reservation, actualPUSD int64) error
	Rollback(ctx context.Context, r *domquota.Reservation) error
}

// Upstream is the sync speech-synthesis port. Same unbilled discipline as the
// image port: unbilled=true ONLY for a provably-unbilled explicit rejection.
//
// Upstream 是同步语音合成端口。与图像端口同一条 unbilled 纪律:仅可证明未计费的显式拒绝才为真。
type Upstream interface {
	GenerateSpeech(ctx context.Context, model, text, voice string) (audio []byte, unbilled bool, err error)
}

type Clock interface {
	Now() time.Time
}

type Metrics interface {
	SettleFailure()
	RollbackFailure()
}

type Service struct {
	auth     Authenticator
	quota    Quota
	rl       RateLimiter
	cfg      Config
	upstream Upstream
	voices   Voices
	clock    Clock
	mx       Metrics
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

type Deps struct {
	Auth     Authenticator
	Quota    Quota
	RL       RateLimiter
	Config   Config
	Upstream Upstream
	Voices   Voices
	Clock    Clock
	Metrics  Metrics
}

func New(d Deps) *Service {
	s := &Service{auth: d.Auth, quota: d.Quota, rl: d.RL, cfg: d.Config, upstream: d.Upstream,
		voices: d.Voices, clock: d.Clock, mx: d.Metrics}
	if s.mx == nil {
		s.mx = noopMetrics{}
	}
	return s
}

// Available reports whether the whole speech path exists on this deployment:
// capability on AND a Qwen credential present (the double-half rule).
//
// Available 报告整条语音路是否存在:能力开 **且** Qwen 凭证在(双半才真)。
func (s *Service) Available() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	c := s.cfg.Load()
	return c != nil && c.SpeechEnabled && len(c.QwenAPIKeys) > 0
}

// Synthesize runs the full use case and returns the upstream artifact URL
// (URL 直通, P13).
//
// The billed unit is the INPUT text's rune count, known exactly before the call
// — so reserve == settle on success without any usage feedback from the
// upstream. Runes, not bytes: the whole point of a character price is that one
// Chinese character costs one character, and a byte count would charge it three.
//
// Synthesize 跑完整用例并返回上游产物 URL(URL 直通,P13)。计费单位是**输入**文本的 rune 数,
// 调用前即精确已知——故成功时 reserve == settle,无需上游回报 usage。按 rune 而非 byte:字符
// 计价的全部意义就是一个汉字算一个字符,按字节数会把它算成三个。
func (s *Service) Synthesize(ctx context.Context, installID, text, voice string) ([]byte, *apierr.APIError) {
	if s == nil || s.auth == nil || s.cfg == nil || s.quota == nil || s.upstream == nil || s.clock == nil {
		return nil, apierr.Internal()
	}
	if !s.Available() {
		return nil, apierr.ErrTTSUnavailable
	}
	got, status, found, err := s.auth.LookupInstall(ctx, installID)
	if err != nil {
		return nil, apierr.Internal()
	}
	if installID == "" || !found || got == "" {
		return nil, apierr.ErrInvalidInstall
	}
	if status == dominstall.StatusBanned {
		return nil, apierr.ErrAccountBanned
	}
	if s.rl != nil && !s.rl.Allow(got) {
		return nil, apierr.ErrRateLimited
	}

	c := s.cfg.Load()
	model := strings.TrimSpace(c.TTSUpstreamModel)
	characters := int64(utf8.RuneCountInString(text))
	plan, err := billing.NewCharactersPlan(billing.ProviderQwen, model, characters)
	if err != nil {
		// Startup validation pins the card and the handler bounds the length; reaching
		// this means config drifted mid-flight.
		// 启动校验已钉卡、handler 已界长度;走到这里说明配置在途漂移。
		return nil, apierr.Internal()
	}
	reservation, err := s.quota.Reserve(ctx, got, plan, s.quota.SnapshotPeriod(s.clock.Now()))
	if err != nil {
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, apierr.Internal()
	}

	if strings.TrimSpace(voice) == "" {
		voice = strings.TrimSpace(c.TTSDefaultVoice)
	}
	voice = s.resolveVoice(ctx, installID, voice)
	audio, unbilled, err := s.upstream.GenerateSpeech(ctx, model, text, voice)
	if err != nil {
		if unbilled {
			if rbErr := s.quota.Rollback(ctx, reservation); rbErr != nil {
				s.mx.RollbackFailure()
			}
		} else if stErr := s.quota.Settle(ctx, reservation, reservation.ReservedPUSD); stErr != nil {
			// Ambiguous upstream outcome: the provider may have billed, keep the full
			// quote (GW-INV-50). 歧义上游结果:上游可能已计费,保留全额(GW-INV-50)。
			s.mx.SettleFailure()
		}
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, apierr.ErrUpstreamError
	}

	cost, err := reservation.Plan.CharactersCost(characters)
	if err != nil {
		cost = reservation.ReservedPUSD // frozen-card failure cannot under-charge / 冻卡异常绝不少收
	}
	if err := s.quota.Settle(ctx, reservation, cost); err != nil {
		s.mx.SettleFailure()
	}
	return audio, nil
}

type noopMetrics struct{}

func (noopMetrics) SettleFailure()   {}
func (noopMetrics) RollbackFailure() {}
