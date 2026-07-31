// Package speech owns the realtime microphone transcription gate. It is not a
// chat route: it authenticates an install and exposes the configured Qwen ASR
// endpoint/key to the transport WebSocket proxy without leaking prompt or key
// material to logs or clients.
//
// It uses genrun's ports and steps but not genrun.Do: a WebSocket has no single
// upstream call to wrap. The transport drives the session, reserving for the
// maximum duration up front and settling the audio actually spoken when it ends.
//
// speech 包持有实时麦克风转写闸。它不是一条 chat 路由:它鉴权 install,并把配置好的 Qwen ASR
// 端点/key 交给 transport 的 WebSocket 代理,不向日志或客户端泄漏 prompt 与 key 材料。
//
// 它用 genrun 的端口与步骤,但**不用** genrun.Do:一个 WebSocket 没有单次上游调用可包。会话由
// transport 驱动——先按最长时长预留,结束时按**实际说了多少**结算。
package speech

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/app/genrun"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

const (
	DefaultModel     = billing.Qwen3ASRFlashRealtime
	pcmSampleRateHz  = int64(16_000)
	pcmBytesPerFrame = int64(2) // PCM16 mono.
)

type Service struct {
	gen genrun.Runner
}

type Deps struct {
	Auth    genrun.Authenticator
	Quota   genrun.Quota
	RL      genrun.RateLimiter
	Config  genrun.Config
	Clock   genrun.Clock
	Metrics genrun.Metrics
}

func New(d Deps) *Service {
	return &Service{gen: genrun.New(genrun.Ports{Auth: d.Auth, RL: d.RL, Config: d.Config,
		Quota: d.Quota, Clock: d.Clock, Metrics: d.Metrics})}
}

// Upstream is the realtime endpoint and the credential to reach it with. It never
// leaves this process boundary except into the proxy dialer.
//
// Upstream 是实时端点与到达它的凭证。除了进入代理拨号器,它不越过本进程边界。
type Upstream struct {
	URL    string
	APIKey string
	Model  string
}

func (s *Service) Authorize(ctx context.Context, installID string) (string, *apierr.APIError) {
	if s == nil || s.gen.Settings() == nil {
		return "", apierr.Internal()
	}
	return s.gen.Authorize(ctx, installID)
}

func (s *Service) Upstream() (Upstream, *apierr.APIError) {
	if s == nil {
		return Upstream{}, apierr.Internal()
	}
	c := s.gen.Settings()
	if c == nil || len(c.QwenAPIKeys) == 0 || strings.TrimSpace(c.QwenBaseURL) == "" {
		return Upstream{}, apierr.ErrSpeechUnavailable
	}
	u, err := RealtimeURL(c.QwenBaseURL, DefaultModel)
	if err != nil {
		return Upstream{}, apierr.ErrSpeechUnavailable
	}
	return Upstream{URL: u, APIKey: c.QwenAPIKeys[0], Model: DefaultModel}, nil
}

// Reserve holds the session's MAXIMUM billable duration before a single frame is
// spoken, because a stream cannot be refused halfway through politely.
//
// Reserve 在说出第一帧之前就按会话的**最长**可计费时长预留,因为一条流没法在中途被礼貌地拒掉。
func (s *Service) Reserve(ctx context.Context, installID string, maxAudioSeconds int64) (*domquota.Reservation, *apierr.APIError) {
	if s == nil {
		return nil, apierr.Internal()
	}
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, DefaultModel, billing.InputAudioSeconds, maxAudioSeconds)
	if err != nil {
		return nil, apierr.Internal()
	}
	return s.gen.Reserve(ctx, installID, plan)
}

// Settle closes the session at the seconds actually spoken.
//
// Settle 按**实际说了多少秒**给会话收口。
func (s *Service) Settle(ctx context.Context, r *domquota.Reservation, audioBytes int64) error {
	if r == nil {
		return nil
	}
	cost, err := r.Plan.UnitCost(billing.InputAudioSeconds, AudioBytesToBillableSeconds(audioBytes))
	if err != nil {
		return err
	}
	return s.gen.Settle(ctx, r, cost)
}

func (s *Service) Rollback(ctx context.Context, r *domquota.Reservation) error {
	return s.gen.Rollback(ctx, r)
}

func AudioBytesToBillableSeconds(n int64) int64 {
	if n <= 0 {
		return 0
	}
	bytesPerSecond := pcmSampleRateHz * pcmBytesPerFrame
	return int64(math.Ceil(float64(n) / float64(bytesPerSecond)))
}

func RealtimeURL(baseURL, model string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Host == "" {
		if err == nil {
			err = errors.New("missing host")
		}
		return "", err
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	default:
		base.Scheme = "wss"
	}
	base.Path = "/api-ws/v1/realtime"
	base.RawQuery = "model=" + url.QueryEscape(model)
	base.Fragment = ""
	base.User = nil
	return base.String(), nil
}
