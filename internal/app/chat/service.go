package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// nonStreamBodyLimit bounds a non-streaming upstream body read so a hostile/
// runaway upstream cannot make us buffer unbounded after the 2xx commit.
const nonStreamBodyLimit = 8 * 1024 * 1024

// frameWriteDeadline rolls per-frame on the stream relay. It is NOT a global
// write timeout (which would truncate a long, legitimately-slow stream); each
// frame just gets a fresh ceiling so a wedged client can't pin a slot forever.
const frameWriteDeadline = 30 * time.Second

// Service is the chat-spend use case over its ports. The N_global semaphore is
// owned HERE (an account-level hard cap fixed at construction, GW-INV-22): a
// buffered channel whose capacity never grows so queue-wait can only delay, never
// amplify, concurrency.
type Service struct {
	auth     Authenticator
	quota    Quota
	upstream Upstream
	rl       RateLimiter
	throttle Throttle
	disk     DiskGuard
	cfg      Config
	clock    Clock
	log      Logger
	mx       Metrics
	bgWG     WaitGroupLike
	leases   MediaLeases

	sem chan struct{} // N_global slots (cap fixed at construction).
}

// Deps bundles the Service's ports. Optional ports (throttle/disk/log/mx) get a
// no-op default in New so the hot path never nil-checks.
type Deps struct {
	Auth     Authenticator
	Quota    Quota
	Upstream Upstream
	RL       RateLimiter
	Throttle Throttle
	Disk     DiskGuard
	Config   Config
	Clock    Clock
	Logger   Logger
	Metrics  Metrics
	BgWG     WaitGroupLike
	// Leases resolves media lease references a request may carry (ADR 0011). nil when
	// MEDIA_ENABLED is false — in which case a request that names a lease is refused rather than
	// silently forwarded, because nothing could have issued that lease.
	// Leases 解析请求可能携带的媒体 lease 引用(ADR 0011)。MEDIA_ENABLED=false 时为 nil——此时携带
	// lease 的请求会被**拒绝**而非静默转发,因为根本没有东西能签发过那个 lease。
	Leases MediaLeases
}

// MediaLeases verifies AND opens a lease reference under the strict predicate — it must belong to
// the CALLING install and still be usable. Implemented by the media service; see its
// OpenLeaseForInstall for why chat needs a predicate stricter than the unauthenticated
// provider-fetch route's, and ADR 0012 for why chat reads the BYTES (the provider's fetcher refuses
// to download from this gateway's public host, so the verified content is inlined upstream).
//
// MediaLeases 在严格谓词下校验**并打开** lease 引用——须属于发起请求的 install 且仍可用。由 media service
// 实现;chat 为何需要比未鉴权 provider 拉取路由更严的谓词见其 OpenLeaseForInstall,chat 为何要读**字节**
// 见 ADR 0012(provider 拉取器拒绝从本网关公开主机下载,故已校验内容改为内联上游)。
type MediaLeases interface {
	OpenLeaseForInstall(ctx context.Context, installID, leaseID, token string) (*LeaseContent, error)
}

// LeaseContent is one opened, verified lease: its MIME, its exact recorded size, and its bytes.
// Declared HERE (not imported from the media package) so the dependency stays a port — bootstrap
// adapts the media service's own source type into this one.
// LeaseContent 是一个已打开、已校验的 lease:MIME、记录在案的精确大小、字节流。声明在**本包**(不从 media
// 包导入),依赖保持端口形——由 bootstrap 把 media service 自己的源类型适配成它。
type LeaseContent struct {
	MIMEType  string
	SizeBytes int64
	Body      io.ReadCloser
}

// New wires the Service and fixes the N_global semaphore capacity from the
// startup config (account-level hard cap, restart-effective). Optional ports
// default to no-ops.
func New(d Deps) *Service {
	n := d.Config.Load().NGlobalConcurrency
	if n < 1 {
		n = 1
	}
	s := &Service{
		auth: d.Auth, quota: d.Quota, upstream: d.Upstream, rl: d.RL,
		throttle: d.Throttle, disk: d.Disk, cfg: d.Config, clock: d.Clock,
		log: d.Logger, mx: d.Metrics, bgWG: d.BgWG, leases: d.Leases,
		sem: make(chan struct{}, n),
	}
	if s.throttle == nil {
		s.throttle = noopThrottle{}
	}
	if s.disk == nil {
		s.disk = noopDisk{}
	}
	if s.log == nil {
		s.log = noopLogger{}
	}
	if s.mx == nil {
		s.mx = noopMetrics{}
	}
	if s.bgWG == nil {
		s.bgWG = noopWG{}
	}
	return s
}

// HandleInput is the transport-extracted request: the proof-verified install id, the raw body
// (already MaxBytesReader-capped at transport), the rate-limit IP key, and the
// request id. The use case owns no net/http types.
type HandleInput struct {
	InstallID string
	Body      []byte
	BodyError error // non-nil ⇒ body read failed/over-cap (transport surfaces it).
	// BodyTooLarge distinguishes the exact transport memory envelope from a
	// malformed body. It must never be reported as a model-context failure.
	BodyTooLarge bool
	IPKey        string
	RequestID    string
}

// Handle runs the EXACT cheapest-first gate order then the reserve→forward→settle
// saga (spec-behaviors §2). Each gate rejects before the next so the expensive
// work (DB reserve, upstream call) is reached only after the cheap gates pass.
// Method (POST) is already enforced by the handler. Returns nothing: every
// outcome is written to the sink. The single config snapshot is taken once and
// reused for all guardrails + provider selection + response headers (GW-INV-08).
func (s *Service) Handle(ctx context.Context, in HandleInput, sink Sink) {
	// 1) Identity status — transport has already verified the device signature.
	if in.InstallID == "" {
		writeErr(sink, apierr.ErrInvalidInstall)
		return
	}
	installID, status, found, err := s.auth.LookupInstall(ctx, in.InstallID)
	if err != nil {
		writeErr(sink, apierr.Internal())
		return
	}
	if !found {
		writeErr(sink, apierr.ErrInvalidInstall)
		return
	}
	if status == dominstall.StatusBanned {
		writeErr(sink, apierr.ErrAccountBanned)
		return
	}

	// 2) Anomaly observe (sets audit flag, NEVER rejects — the tightened bucket
	// bites on the Allow below / next request), then the per-install minute gate.
	throttled := s.throttle.Observe(installID)
	if !s.rl.Allow(installID) {
		if throttled {
			s.log.Warn(ctx, "security_event", "event", "token_throttle",
				"install_id", installID, "outcome", "rate_limited")
		}
		writeErr(sink, apierr.ErrRateLimited)
		return
	}

	// 2b) REL-6 disk-degrade: shed BEFORE any reservation (a DB write).
	if s.disk.Degraded() {
		writeErr(sink, apierr.ErrDiskLow)
		return
	}

	// 3) Body read result + strict decode + non-empty messages.
	if in.BodyTooLarge {
		writeErr(sink, apierr.ErrRequestBodyTooLarge)
		return
	}
	if in.BodyError != nil {
		writeErr(sink, apierr.ErrBadRequest)
		return
	}
	req, derr := domchat.DecodeInbound(in.Body)
	if derr != nil {
		writeErr(sink, derr)
		return
	}
	if req.MessagesEmpty() {
		writeErr(sink, apierr.NewError(apierr.ErrBadRequest.Status, "BAD_REQUEST", "messages is required"))
		return
	}

	// Snapshot config ONCE for the whole request's guardrails + model resolution.
	cfg := s.cfg.Load()

	// 3b) SEC-1 shape gate (array len + per-message rune cap), cheap, before the
	// full token estimate.
	if msg := req.ShapeError(cfg.MaxMessages, cfg.MaxMessageChars); msg != "" {
		writeErr(sink, apierr.NewError(apierr.ErrBadRequest.Status, "BAD_REQUEST", msg))
		return
	}

	// 4) Validate the strict content union and deterministically route from the
	// COMPLETE history. A client model string is never consulted: any accepted
	// image/video selects Qwen; string/text-only content selects DeepSeek.
	modality, contentErr := req.ValidateAndClassify(domchat.MediaLimits{
		MaxParts:        cfg.MaxMediaParts,
		MaxDecodedBytes: cfg.MaxMediaDecodedBytes,
	})
	if contentErr != nil {
		writeErr(sink, contentErr)
		return
	}
	// Audio belongs to the common public content contract already, but the current two fixed
	// upstreams are text DeepSeek and image/video Qwen. Reject it as an unavailable capability before
	// any plan/reservation rather than lying by routing it to Qwen or treating raw bytes as text.
	if modality == domchat.ModalityAudio {
		writeErr(sink, apierr.ErrAudioUnavailable)
		return
	}
	// 4b) Resolve any media lease references BEFORE routing, reserving or forwarding (ADR 0011/0012).
	// Each one is a CLAIM until verified: it must name a lease this gateway issued to THIS install
	// and still active. Verified content is then INLINED into the upstream request as data URIs —
	// the provider's fetcher refuses to download from this gateway's public host (measured live,
	// ADR 0012), and the gateway holds the verified bytes locally anyway, so a fetch-back URL was a
	// dependency on third-party fetcher policy this path never needed. Handing anything unverified
	// upstream — in URL OR inline form — stays forbidden for the same reason as ever.
	//
	// 4b) 在路由/预留/转发**之前**解析媒体 lease 引用(ADR 0011/0012)。每个引用在校验前都只是**主张**:
	// 它必须指称本网关签发给**当前 install** 且仍 active 的 lease。校验通过的内容随后以 data URI **内联**
	// 进上游请求——provider 的拉取器拒绝从本网关公开主机下载(线上实测,ADR 0012),而网关本地本就持有已
	// 校验的字节,「让对方回头拉一趟」是这条路径从不需要的第三方策略依赖。未经校验的东西——无论 URL 形还是
	// 内联形——照旧不得抵达上游。
	if refs := req.MediaLeaseRefs(); len(refs) > 0 {
		if s.leases == nil {
			// Nothing could have issued this lease on this deployment. 本部署根本签发不出这个 lease。
			writeErr(sink, apierr.ErrMediaUnavailable)
			return
		}
		inline := make(map[string]domchat.InlinedLease, len(refs))
		var inlinedBytes int64
		for _, ref := range refs {
			src, err := s.leases.OpenLeaseForInstall(ctx, installID, ref.LeaseID, ref.Token)
			if err != nil {
				// One collapsed answer for absent / foreign / expired / tampered — never an
				// existence oracle over another install's lease ids.
				// 不存在/非本人/已过期/被篡改共用一个答复——绝不做他人 lease id 的存在性预言机。
				writeErr(sink, apierr.ErrMediaLeaseNotFound)
				return
			}
			// The same cumulative decoded-bytes budget ValidateAndClassify charges data URIs.
			// Lease media bypassed that meter (it contributed zero decoded bytes on the way in), so
			// the budget is enforced HERE, where the bytes actually join the upstream body — without
			// it a 100MiB lease upload would happily inflate into an upstream request no provider
			// accepts. 与 ValidateAndClassify 对 data URI 记账的同一套累计解码字节预算。lease 媒体在入口
			// 处计零解码字节、绕过了那只表,故预算在**字节真正并入上游请求体**的这里执行——没有它,一个
			// 100MiB 的 lease 上传会膨胀成没有任何 provider 会接受的上游请求。
			inlinedBytes += src.SizeBytes
			if cfg.MaxMediaDecodedBytes > 0 && inlinedBytes > cfg.MaxMediaDecodedBytes {
				_ = src.Body.Close()
				writeErr(sink, apierr.NewError(apierr.ErrBadRequest.Status, apierr.ErrBadRequest.Code,
					"media exceeds the per-request decoded size limit"))
				return
			}
			data, err := io.ReadAll(io.LimitReader(src.Body, src.SizeBytes+1))
			_ = src.Body.Close()
			if err != nil || int64(len(data)) != src.SizeBytes {
				// The lease row and the staged file disagree — a storage fault, not a caller error.
				// lease 行与落盘文件不一致——存储侧故障,非调用方错误。
				writeErr(sink, apierr.ErrMediaUnavailable)
				return
			}
			inline[ref.Raw] = domchat.InlinedLease{
				MIMEType: src.MIMEType,
				Base64:   base64.StdEncoding.EncodeToString(data),
			}
		}
		req = req.WithInlinedMediaLeases(inline)
	}

	provider, model, modelOutputLimit := routeFor(modality, cfg)
	if !s.upstream.Available(provider) {
		if provider == billing.ProviderQwen {
			writeErr(sink, apierr.ErrMultimodalUnavailable)
		} else {
			writeErr(sink, apierr.ErrUpstreamBusy)
		}
		return
	}

	// 5) Build a conservative prompt quote for accounting only. This is a
	// byte-level upper bound, not the selected provider's tokenizer, so it MUST
	// NOT decide whether a request fits the model. Exact shape/body/media guards
	// above protect gateway resources; the upstream model is the sole context
	// authority and normalizes a real rejection to UPSTREAM_REJECTED.
	//
	// Multimodal quotes exclude base64 transport bytes: Qwen tokenizes decoded
	// media rather than its wire encoding, and Qwen's plan reserves the complete
	// model input hard limit below.
	promptEst := req.PromptEstimate()
	if modality == domchat.ModalityMultimodal {
		promptEst = req.TextPromptEstimate()
	}

	// 6) Bound caller-owned max_tokens for this exact model, then freeze a
	// provider-aware pUSD plan. Qwen's compatibility usage cannot prove a
	// thinking-token sub-cap, so its wallet quote reserves the model's COMPLETE
	// input/output hard limits. DeepSeek can use the request prompt estimate and
	// conservative output quote.
	wireMaxTok, quoteMaxTok := domchat.BoundMaxTokens(req.MaxTokens, min64(cfg.MaxTokensCap, modelOutputLimit))
	plan, planAPIError := billingPlan(provider, model, req, promptEst, quoteMaxTok)
	if planAPIError != nil {
		writeErr(sink, planAPIError)
		return
	}
	out := domchat.Sanitize(req, wireMaxTok)

	// 7) Reserve atomically (per-install monthly count + operator monthly pUSD
	// wallet, while daily/provider tables remain accounting statistics). The
	// period is snapshotted ONCE and threaded unchanged through settle/rollback.
	period := s.quota.SnapshotPeriod(s.clock.Now())
	resv, rerr := s.quota.Reserve(ctx, installID, plan, period)
	if rerr != nil {
		// app/quota maps denials to apierr sentinels (returned as error); any other
		// error normalizes to INTERNAL — never leak an internal detail.
		writeAnyErr(sink, rerr) // QUOTA_EXHAUSTED / RATE_LIMITED / BUDGET_EXHAUSTED / 500
		return
	}

	// 7b) Provider-local breaker fast-path (REL-2): one provider's outage must not
	// shed the other provider or occupy an N_global slot. Roll the reservation
	// back first because no provider request was attempted.
	if s.upstream.BreakerOpen(provider) {
		s.rollback(ctx, resv)
		s.mx.Upstream(provider, "busy")
		writeErr(sink, apierr.ErrUpstreamBusy)
		return
	}

	// 8) Acquire an N_global slot with REL-7 bounded queue-wait. ctx-cancel ⇒ 499
	// (no busy charge), timeout/full ⇒ UPSTREAM_BUSY; both roll back. The slot is
	// released on the defer once acquired so forward owns it for the whole relay.
	if !s.acquireSlot(ctx, cfg.QueueWait) {
		s.rollback(ctx, resv)
		if ctx.Err() != nil {
			// Client gave up while queued: nothing to write, just release quota.
			return
		}
		s.mx.Upstream(provider, "busy")
		writeErr(sink, apierr.ErrUpstreamBusy)
		return
	}
	s.mx.Inflight(len(s.sem))
	defer func() { <-s.sem; s.mx.Inflight(len(s.sem)) }()

	s.forward(ctx, sink, provider, model, out, cfg, resv)
}

// routeFor is the entire model-routing policy. It is intentionally a closed,
// deterministic two-way mapping and never accepts the client's model field.
func routeFor(modality domchat.Modality, cfg *config.Config) (billing.Provider, string, int64) {
	if modality == domchat.ModalityMultimodal {
		return billing.ProviderQwen, cfg.MultimodalUpstreamModel, billing.Qwen37OutputLimit
	}
	return billing.ProviderDeepSeek, cfg.TextUpstreamModel, billing.DeepSeekOutputLimit
}

func billingPlan(provider billing.Provider, model string, req domchat.InboundRequest, promptEst, maxTok int64) (billing.Plan, *apierr.APIError) {
	card, err := billing.Lookup(provider, model)
	if err != nil {
		// Models are operator-owned exact ids. An unknown card is a deployment
		// fault, never something a client's model string can repair.
		return billing.Plan{}, apierr.Internal()
	}
	if provider == billing.ProviderQwen {
		plan, err := billing.NewPlan(provider, model, billing.InputStandard, card.InputLimit, card.OutputLimit)
		if err != nil {
			return billing.Plan{}, apierr.Internal()
		}
		return plan, nil
	}
	// The byte upper bound can exceed the model token limit while the real
	// tokenizer input still fits. Reserve the largest amount the provider could
	// possibly accept, forward, and settle from authoritative usage. This keeps
	// the wallet proof conservative without turning an accounting estimate into
	// a false context gate.
	promptQuote := min64(promptEst, card.InputLimit)
	plan, err := billing.NewPlan(provider, model, billing.InputStandard, promptQuote, maxTok)
	if err != nil {
		return billing.Plan{}, apierr.Internal()
	}
	return plan, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// acquireSlot takes an N_global slot with REL-7 bounded backpressure: grab a free
// slot immediately, else wait up to queueWait (absorbing spikes), bailing the
// instant the client cancels (a queued-then-abandoned request never occupies a
// slot). queueWait<=0 degrades to a binary immediate reject. Returns true iff a
// slot was taken (caller MUST release it); on false the caller inspects ctx.Err()
// to tell client-cancel from queue-timeout. The cap is fixed — waiting only
// queues, never amplifies concurrency (GW-INV-22).
func (s *Service) acquireSlot(ctx context.Context, queueWait time.Duration) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
	}
	if queueWait <= 0 {
		return false
	}
	timer := time.NewTimer(queueWait)
	defer timer.Stop()
	select {
	case s.sem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// settle runs Settle on a detached context (REL-4), tracked by bgWG so shutdown
// awaits accounting before DB close. A non-nil error is COUNTED + WARNed (B2):
// it is never swallowed, so a failed settle is observable rather than silently
// left for the orphan scanner to finalize at the full reservation.
func (s *Service) settle(parent context.Context, r *domquota.Reservation, actualPUSD int64) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		if err := s.quota.Settle(detach(parent), r, actualPUSD); err != nil {
			s.mx.SettleFailure()
			s.log.Warn(parent, "settle_failed", "event", "accounting_settle_failed",
				"request_id", r.RequestID, "install_id", r.InstallID,
				"period_day", r.Period.Day, "err", err.Error())
		}
	}()
}

// rollback mirrors settle for a definitely-unbilled failure (REL-5), with B2
// observability. Ambiguous outcomes after Open use settle(full quote) instead.
func (s *Service) rollback(parent context.Context, r *domquota.Reservation) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		if err := s.quota.Rollback(detach(parent), r); err != nil {
			s.mx.RollbackFailure()
			s.log.Warn(parent, "rollback_failed", "event", "accounting_rollback_failed",
				"request_id", r.RequestID, "install_id", r.InstallID,
				"period_day", r.Period.Day, "err", err.Error())
		}
	}()
}

// detach preserves request-scoped values (the logger) but is immune to request
// cancel, so settle/rollback complete after the client disconnects (REL-4).
func detach(parent context.Context) context.Context { return context.WithoutCancel(parent) }

// writeErr renders an *apierr to the sink as the §5.2 envelope. The app writes
// the envelope directly (rather than calling transport/response) so it stays
// net/http-free; the shape MUST match transport/response.WriteError byte-for-byte.
func writeErr(sink Sink, ae *apierr.APIError) {
	if ae == nil {
		ae = apierr.Internal()
	}
	sink.SetHeader("Content-Type", "application/json")
	if _, hdr, ok := ae.RetryAfterSeconds(); ok {
		sink.SetHeader("Retry-After", hdr)
	}
	sink.WriteHeader(ae.Status)
	body, _ := json.Marshal(errEnvelope{Error: errBody{Code: ae.Code, Message: ae.Message, Details: ae.Details}})
	_, _ = sink.Write(body)
}

// writeAnyErr renders an arbitrary error: an *apierr passes through, anything
// else normalizes to INTERNAL (never leak an internal detail). Mirrors
// transport/response.WriteError's normalization so the reserve path's typed
// sentinels and a stray internal error both render correctly.
func writeAnyErr(sink Sink, err error) {
	if ae, ok := err.(*apierr.APIError); ok {
		writeErr(sink, ae)
		return
	}
	writeErr(sink, apierr.Internal())
}

// errEnvelope/errBody are the §5.2 (ADR-003) error wire shape. Kept here so the
// app's pre-output error writes match transport/response exactly.
type errEnvelope struct {
	Error errBody `json:"error"`
}
type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
