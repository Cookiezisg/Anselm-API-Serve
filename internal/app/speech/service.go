// Package speech owns the realtime microphone transcription gate. It is not a
// chat route: it authenticates an install and exposes the configured Qwen ASR
// endpoint/key to the transport WebSocket proxy without leaking prompt or key
// material to logs or clients.
package speech

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
)

const DefaultModel = "qwen-asr-realtime"

type Authenticator interface {
	LookupInstall(ctx context.Context, installID string) (id string, status dominstall.Status, found bool, err error)
}

type RateLimiter interface {
	Allow(key string) bool
}

type Config interface {
	Load() *config.Config
}

type Service struct {
	auth Authenticator
	rl   RateLimiter
	cfg  Config
}

type Deps struct {
	Auth   Authenticator
	RL     RateLimiter
	Config Config
}

func New(d Deps) *Service {
	return &Service{auth: d.Auth, rl: d.RL, cfg: d.Config}
}

type Upstream struct {
	URL    string
	APIKey string
	Model  string
}

func (s *Service) Authorize(ctx context.Context, installID string) *apierr.APIError {
	if s == nil || s.auth == nil || s.cfg == nil {
		return apierr.Internal()
	}
	if installID == "" {
		return apierr.ErrInvalidInstall
	}
	got, status, found, err := s.auth.LookupInstall(ctx, installID)
	if err != nil {
		return apierr.Internal()
	}
	if !found || got == "" {
		return apierr.ErrInvalidInstall
	}
	if status == dominstall.StatusBanned {
		return apierr.ErrAccountBanned
	}
	if s.rl != nil && !s.rl.Allow(got) {
		return apierr.ErrRateLimited
	}
	return nil
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
