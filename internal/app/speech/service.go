// Package speech owns the realtime microphone transcription gate. It is not a
// chat route: it authenticates an install and exposes the configured Qwen ASR
// endpoint/key to the transport WebSocket proxy without leaking prompt or key
// material to logs or clients.
package speech

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

const (
	DefaultModel     = billing.Qwen3ASRFlashRealtime
	pcmSampleRateHz  = int64(16_000)
	pcmBytesPerFrame = int64(2) // PCM16 mono.
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

type Clock interface {
	Now() time.Time
}

type Metrics interface {
	SettleFailure()
	RollbackFailure()
}

type Service struct {
	auth  Authenticator
	quota Quota
	rl    RateLimiter
	cfg   Config
	clock Clock
	mx    Metrics
}

type Deps struct {
	Auth    Authenticator
	Quota   Quota
	RL      RateLimiter
	Config  Config
	Clock   Clock
	Metrics Metrics
}

func New(d Deps) *Service {
	s := &Service{auth: d.Auth, quota: d.Quota, rl: d.RL, cfg: d.Config, clock: d.Clock, mx: d.Metrics}
	if s.mx == nil {
		s.mx = noopMetrics{}
	}
	return s
}

type Upstream struct {
	URL    string
	APIKey string
	Model  string
}

func (s *Service) Authorize(ctx context.Context, installID string) (string, *apierr.APIError) {
	if s == nil || s.auth == nil || s.cfg == nil {
		return "", apierr.Internal()
	}
	if installID == "" {
		return "", apierr.ErrInvalidInstall
	}
	got, status, found, err := s.auth.LookupInstall(ctx, installID)
	if err != nil {
		return "", apierr.Internal()
	}
	if !found || got == "" {
		return "", apierr.ErrInvalidInstall
	}
	if status == dominstall.StatusBanned {
		return "", apierr.ErrAccountBanned
	}
	if s.rl != nil && !s.rl.Allow(got) {
		return "", apierr.ErrRateLimited
	}
	return got, nil
}

func (s *Service) Upstream() (Upstream, *apierr.APIError) {
	if s == nil || s.cfg == nil {
		return Upstream{}, apierr.Internal()
	}
	c := s.cfg.Load()
	if c == nil || len(c.QwenAPIKeys) == 0 || strings.TrimSpace(c.QwenBaseURL) == "" {
		return Upstream{}, apierr.ErrSpeechUnavailable
	}
	u, err := RealtimeURL(c.QwenBaseURL, DefaultModel)
	if err != nil {
		return Upstream{}, apierr.ErrSpeechUnavailable
	}
	return Upstream{URL: u, APIKey: c.QwenAPIKeys[0], Model: DefaultModel}, nil
}

func (s *Service) Reserve(ctx context.Context, installID string, maxAudioSeconds int64) (*domquota.Reservation, *apierr.APIError) {
	if s == nil || s.quota == nil || s.clock == nil {
		return nil, apierr.Internal()
	}
	plan, err := billing.NewAudioSecondsPlan(billing.ProviderQwen, DefaultModel, maxAudioSeconds)
	if err != nil {
		return nil, apierr.Internal()
	}
	r, err := s.quota.Reserve(ctx, installID, plan, s.quota.SnapshotPeriod(s.clock.Now()))
	if err != nil {
		if ae, ok := err.(*apierr.APIError); ok {
			return nil, ae
		}
		return nil, apierr.Internal()
	}
	return r, nil
}

func (s *Service) Settle(ctx context.Context, r *domquota.Reservation, audioBytes int64) error {
	if r == nil {
		return nil
	}
	seconds := AudioBytesToBillableSeconds(audioBytes)
	cost, err := r.Plan.AudioSecondsCost(seconds)
	if err != nil {
		return err
	}
	if err := s.quota.Settle(ctx, r, cost); err != nil {
		s.mx.SettleFailure()
		return err
	}
	return nil
}

func (s *Service) Rollback(ctx context.Context, r *domquota.Reservation) error {
	if r == nil {
		return nil
	}
	if err := s.quota.Rollback(ctx, r); err != nil {
		s.mx.RollbackFailure()
		return err
	}
	return nil
}

func AudioBytesToBillableSeconds(n int64) int64 {
	if n <= 0 {
		return 0
	}
	bytesPerSecond := pcmSampleRateHz * pcmBytesPerFrame
	return int64(math.Ceil(float64(n) / float64(bytesPerSecond)))
}

type noopMetrics struct{}

func (noopMetrics) SettleFailure()   {}
func (noopMetrics) RollbackFailure() {}

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
