package chat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
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
		log: d.Logger, mx: d.Metrics, bgWG: d.BgWG,
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

// HandleInput is the transport-extracted request: the bearer token, the raw body
// (already MaxBytesReader-capped at transport), the rate-limit IP key, and the
// request id. The use case owns no net/http types.
type HandleInput struct {
	Token     string
	Body      []byte
	BodyError error // non-nil ⇒ body read failed/over-cap (transport surfaces it).
	IPKey     string
	RequestID string
}

// Handle runs the EXACT cheapest-first gate order then the reserve→forward→settle
// saga (spec-behaviors §2). Each gate rejects before the next so the expensive
// work (DB reserve, upstream call) is reached only after the cheap gates pass.
// Method (POST) is already enforced by the handler. Returns nothing: every
// outcome is written to the sink. The single config snapshot is taken once and
// reused for all guardrails + model resolution (GW-INV-08).
func (s *Service) Handle(ctx context.Context, in HandleInput, sink Sink) {
	// 1) Auth — bearer present, then token→install lookup.
	if in.Token == "" {
		writeErr(sink, apierr.ErrInvalidToken)
		return
	}
	installID, status, found, err := s.auth.LookupInstall(ctx, in.Token)
	if err != nil {
		writeErr(sink, apierr.Internal())
		return
	}
	if !found {
		writeErr(sink, apierr.ErrInvalidToken)
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

	// 4) Input-token cap: conservative prompt estimate (incl tools) ≤ cap.
	// INPUT_TOKEN_CAP=0 disables this gate — the upstream model's own context
	// limit judges instead (its 4xx comes back as 400 UPSTREAM_REJECTED); the
	// estimate below still feeds the reservation either way.
	promptEst := req.PromptEstimate()
	if cfg.InputTokenCap > 0 && promptEst > cfg.InputTokenCap {
		writeErr(sink, apierr.NewError(apierr.ErrBadRequest.Status, "BAD_REQUEST", "input too large"))
		return
	}

	// 5) Build sanitized upstream body: model rewrite, max_tokens clamp, tools
	// passthrough, forced include_usage on stream. est = promptEst + clamped max.
	model := domchat.ResolveModel(cfg.ModelAllowlist, cfg.DefaultModel, req.Model)
	maxTok := domchat.ClampMaxTokens(maxTokensOf(req), cfg.MaxTokensCap)
	out := domchat.SanitizeUpstream(req, model, maxTok)
	est := promptEst + maxTok

	// 4b) A request whose worst-case reservation exceeds the install-day sub-cap
	// can NEVER succeed — today or any other day — so it is a 400, not a
	// misleading RATE_LIMITED from the reserve gate. This is the runtime
	// replacement for the SEC-2 static INPUT+MAX_TOKENS bound once
	// INPUT_TOKEN_CAP=0 leaves promptEst statically unbounded. The >0 guard only
	// shields zero-value configs (production validates the cap >0 at load).
	if cfg.InstallDailyTokenCap > 0 && est > cfg.InstallDailyTokenCap {
		writeErr(sink, apierr.NewError(apierr.ErrBadRequest.Status, "BAD_REQUEST",
			"request exceeds the per-install daily token capacity"))
		return
	}

	payload, err := json.Marshal(out)
	if err != nil {
		writeErr(sink, apierr.Internal())
		return
	}

	// 6) Reserve atomically (count / install-day-tokens / global-day-budget). The
	// period is snapshotted ONCE and threaded unchanged through settle/rollback.
	period := s.quota.SnapshotPeriod(s.clock.Now())
	resv, rerr := s.quota.Reserve(ctx, installID, est, period)
	if rerr != nil {
		// app/quota maps denials to apierr sentinels (returned as error); any other
		// error normalizes to INTERNAL — never leak an internal detail.
		writeAnyErr(sink, rerr) // QUOTA_EXHAUSTED / RATE_LIMITED / BUDGET_EXHAUSTED / 500
		return
	}

	// 6b) Breaker fast-path (REL-2): an already-open breaker sheds to UPSTREAM_BUSY
	// WITHOUT taking an N_global slot (red-line: breaker-shed never occupies the
	// account-level concurrency cap). Roll the reservation back first.
	if s.upstream.BreakerOpen() {
		s.rollback(ctx, resv)
		s.mx.Upstream("busy")
		writeErr(sink, apierr.ErrUpstreamBusy)
		return
	}

	// 7) Acquire an N_global slot with REL-7 bounded queue-wait. ctx-cancel ⇒ 499
	// (no busy charge), timeout/full ⇒ UPSTREAM_BUSY; both roll back. The slot is
	// released on the defer once acquired so forward owns it for the whole relay.
	if !s.acquireSlot(ctx, cfg.QueueWait) {
		s.rollback(ctx, resv)
		if ctx.Err() != nil {
			// Client gave up while queued: nothing to write, just release quota.
			return
		}
		s.mx.Upstream("busy")
		writeErr(sink, apierr.ErrUpstreamBusy)
		return
	}
	s.mx.Inflight(len(s.sem))
	defer func() { <-s.sem; s.mx.Inflight(len(s.sem)) }()

	s.forward(ctx, sink, payload, req.Stream, cfg, resv)
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

// maxTokensOf returns the inbound max_tokens pointer for clamping. Kept tiny so
// the domain type's field stays unexported while the app can clamp it.
func maxTokensOf(in domchat.InboundRequest) *int64 { return in.MaxTokens }

// settle runs Settle on a detached context (REL-4), tracked by bgWG so shutdown
// awaits accounting before DB close. A non-nil error is COUNTED + WARNed (B2):
// it is never swallowed, so a failed settle is observable rather than silently
// left for the orphan scanner to refund full.
func (s *Service) settle(parent context.Context, r *domquota.Reservation, actual int64) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		if err := s.quota.Settle(detach(parent), r, actual); err != nil {
			s.mx.SettleFailure()
			s.log.Warn(parent, "settle_failed", "event", "accounting_settle_failed",
				"request_id", r.RequestID, "install_id", r.InstallID,
				"period_day", r.Period.Day, "err", err.Error())
		}
	}()
}

// rollback mirrors settle for a pre-output failure (REL-5), with B2 observability.
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
