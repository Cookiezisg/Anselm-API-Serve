// Package chatprovider binds a canonical Chat Completions request to the
// provider account that will serve it. It owns provider-specific JSON encoding;
// resilience and auth stay in the provider-local upstream clients.
//
// The registry is keyed by provider even though exactly one is wired today. The
// key is what makes "which account serves this request" an explicit lookup that
// fails closed on an unknown value, rather than an assumption compiled into the
// call site — and it is why a second account would be a map entry instead of a
// new branch in every caller.
//
// registry 按 provider 建键,尽管今天只接了一家。这个键让「哪个账号服务这条请求」成为一次
// **显式查表**、未知值直接失败,而不是编译进调用点的一个假设——也正因如此,将来接第二个账号
// 是加一个 map 条目,而不是在每个调用方里加一条分支。
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

// Registry has no fallback path by design. Each account owns an independent
// endpoint, key pool and breaker; a failure can never silently change the
// model or the account after billing has already been reserved against them.
//
// Registry 刻意没有 fallback。每个账号自带独立的端点、key 池与熔断器;一次失败绝不会在账已
// 经按某个模型/账号预留之后,悄悄换成另一个。
type Registry struct {
	clients map[billing.Provider]upstream.BackendClient
}

func New(qwen upstream.BackendClient) *Registry {
	return &Registry{clients: map[billing.Provider]upstream.BackendClient{
		billing.ProviderQwen: qwen,
	}}
}

func (r *Registry) client(provider billing.Provider) upstream.BackendClient {
	if r == nil {
		return nil
	}
	return r.clients[provider]
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

// encode fails closed on any provider it does not know how to serialize for.
// The check is not redundant with Open's client lookup: a wired client and a
// known encoding are two different facts, and silently encoding an unknown
// provider in some other provider's dialect is exactly the kind of mistake that
// only surfaces as a paid-for upstream rejection.
//
// encode 对任何它不会序列化的 provider **失败关闭**。这与 Open 的 client 查表**不重复**:
// 「接了这个客户端」与「知道怎么给它编码」是两件不同的事,而把一个未知 provider 悄悄按另一家
// 的方言编码,正是那种只会以「花了钱的上游拒绝」形式浮现的错误。
func encode(provider billing.Provider, model string, req domchat.CompletionRequest) ([]byte, error) {
	if provider != billing.ProviderQwen {
		return nil, apierr.Internal()
	}
	// Historical thinking is deliberately NOT sent back up. It is provider-private,
	// it is billed again when preserved, and the durable conversation summary plus
	// the tool turns are what actually carry product context forward.
	//
	// 历史 thinking **刻意不回传**。它是 provider 私有的、回传时会**再计一次费**,而真正把产品
	// 上下文带向下一轮的是持久化的对话摘要与 tool 轮次。
	req.Messages = append([]domchat.Message(nil), req.Messages...)
	for i := range req.Messages {
		req.Messages[i].ReasoningContent = nil
	}
	return json.Marshal(struct {
		domchat.UpstreamRequest
		EnableThinking bool `json:"enable_thinking"`
	}{
		UpstreamRequest: req.WithModel(model),
		EnableThinking:  true,
	})
}
