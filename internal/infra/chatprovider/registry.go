// Package chatprovider binds canonical Chat Completions requests to one of the
// gateway's two OpenAI-compatible provider accounts. It owns provider-specific
// JSON encoding; resilience/auth remain in the provider-local upstream clients.
package chatprovider

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/infra/upstream"
)

// Registry has no fallback path by design. Each provider owns an independent
// endpoint/key pool/breaker; a Kimi failure can never consume a DeepSeek key or
// silently change the model/account after billing has been reserved.
type Registry struct {
	deepSeek upstream.BackendClient
	kimi     upstream.BackendClient
}

func New(deepSeek, kimi upstream.BackendClient) *Registry {
	return &Registry{deepSeek: deepSeek, kimi: kimi}
}

func (r *Registry) client(provider billing.Provider) upstream.BackendClient {
	if r == nil {
		return nil
	}
	switch provider {
	case billing.ProviderDeepSeek:
		return r.deepSeek
	case billing.ProviderKimi:
		return r.kimi
	default:
		return nil
	}
}

// Available reports construction-time capability only. Runtime health is the
// provider-local breaker state and is queried separately.
func (r *Registry) Available(provider billing.Provider) bool {
	return r.client(provider) != nil
}

func (r *Registry) BreakerOpen(provider billing.Provider) bool {
	c := r.client(provider)
	return c != nil && c.BreakerOpen()
}

// Open encodes the canonical request for exactly provider/model and opens it.
func (r *Registry) Open(ctx context.Context, provider billing.Provider, model string, req domchat.CompletionRequest, firstByteTimeout time.Duration) (*upstream.Stream, *upstream.CallFailure) {
	c := r.client(provider)
	if c == nil {
		return nil, &upstream.CallFailure{APIError: apierr.ErrUpstreamError, Exposure: billing.DefinitelyUnbilled}
	}
	payload, err := encode(provider, model, req)
	if err != nil {
		return nil, &upstream.CallFailure{APIError: apierr.Internal(), Exposure: billing.DefinitelyUnbilled}
	}
	return c.DoCall(ctx, upstream.Call{
		Payload: payload, Stream: req.Stream, FirstByteTimeout: firstByteTimeout,
	})
}

func encode(provider billing.Provider, model string, req domchat.CompletionRequest) ([]byte, error) {
	switch provider {
	case billing.ProviderDeepSeek:
		return json.Marshal(struct {
			domchat.UpstreamRequest
			Thinking        thinkingParam `json:"thinking"`
			ReasoningEffort string        `json:"reasoning_effort"`
		}{
			UpstreamRequest: req.WithModel(model),
			Thinking:        thinkingParam{Type: "enabled"},
			ReasoningEffort: "high",
		})
	case billing.ProviderKimi:
		// DeepSeek reasoning_content is an account-specific continuation token. If
		// an earlier image keeps the whole history on Kimi, that extension must
		// not leak into Kimi's request. Tool-call JSON remains opaque/preserved.
		req.Messages = append([]domchat.Message(nil), req.Messages...)
		for i := range req.Messages {
			req.Messages[i].ReasoningContent = nil
		}
		return json.Marshal(struct {
			domchat.UpstreamRequest
			Thinking thinkingParam `json:"thinking"`
		}{
			UpstreamRequest: req.WithModel(model),
			Thinking:        thinkingParam{Type: "enabled"},
		})
	default:
		return nil, apierr.Internal()
	}
}

type thinkingParam struct {
	Type string `json:"type"`
}
