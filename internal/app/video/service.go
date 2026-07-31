// Package video owns the video-generation use case (WRK-082 H1): authorize an
// install, reserve the per-second plan (monthly + wallet + category-daily gates),
// SUBMIT to the async DashScope upstream through a port, settle at submit, and
// hand back a signed handle the same install can later poll. It never imports
// infra/net.http/sql; the upstream client satisfies the port.
//
// The money shape is the one thing here that differs from every other capability,
// so it is stated once, plainly: **the charge lands at submit, and polling never
// moves money.** A generation that fails upstream is still paid for. That is
// deliberate — the alternative is a refund path that only runs when the client
// bothers to poll, which means a client that walks away gets its video for free
// while a client that waits pays. Free-tier money must never depend on whether
// somebody came back.
//
// video 包持有视频生成用例(H1):鉴权 install、按秒预留(月度+钱包+品类日三闸)、经端口向异步
// DashScope 上游**提交**、在提交处结算,并交回一个只有同一个 install 能轮询的签名句柄。它不 import
// infra/net.http/sql;上游 client 满足端口。
//
// 这里唯一与其余能力不同的是钱的形状,所以只说一次、说明白:**钱落在提交,轮询绝不动钱。**
// 一次在上游失败的生成**照样付费**。这是刻意的——另一条路是「只有客户端肯回来轮询时才跑」的退款路径,
// 那意味着**走开的人白拿视频、等着的人付钱**。免费档的钱绝不能取决于有没有人回来看。
package video

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
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

// Upstream is the async video port: two verbs, because the family has no
// synchronous form. unbilled=true ONLY for a provably-unbilled explicit
// rejection (GW-INV-50).
//
// Upstream 是异步视频端口:**两个**动词,因为本族没有同步形态。unbilled=true **仅限**可证明未计费
// 的显式拒绝(GW-INV-50)。
type Upstream interface {
	// SubmitVideo takes an optional firstFrame data URL: empty = text-to-video, present =
	// image-to-video (WRK-082 H9). One method rather than two because the upstream is the same
	// endpoint with one more input field, and the entire settlement dance below is identical.
	// SubmitVideo 收一个可选的 firstFrame data URL:空=文生视频,有=图生视频(H9)。**一个方法**而非
	// 两个,因为上游是同一条端点多一个输入字段,而下面整套结算舞步一模一样。
	SubmitVideo(ctx context.Context, model, prompt string, seconds int, ratio, resolution, firstFrame string) (taskID string, unbilled bool, err error)
	PollVideo(ctx context.Context, taskID string) (status VideoStatus, err error)
}

// VideoStatus mirrors the infra client's answer without importing it (the app
// layer names its own port types).
//
// VideoStatus 镜像 infra client 的答案而不 import 它(app 层给自己的端口起名)。
type VideoStatus struct {
	Phase domvideo.Phase
	URL   string
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
	clock    Clock
	mx       Metrics
}

type Deps struct {
	Auth     Authenticator
	Quota    Quota
	RL       RateLimiter
	Config   Config
	Upstream Upstream
	Clock    Clock
	Metrics  Metrics
}

func New(d Deps) *Service {
	s := &Service{auth: d.Auth, quota: d.Quota, rl: d.RL, cfg: d.Config, upstream: d.Upstream, clock: d.Clock, mx: d.Metrics}
	if s.mx == nil {
		s.mx = noopMetrics{}
	}
	return s
}

// Available reports whether the whole video path exists: capability on, a Qwen
// credential present, AND handle-signing material configured. All three, because
// a submission the caller can never poll is worse than no submission at all —
// it would spend the user's daily allowance on a video they cannot reach.
//
// Available 报告整条视频路是否存在:能力开、Qwen 凭证在、**且**句柄签名材料已配。三者缺一不可,
// 因为一次「提交了却永远轮询不到」比根本不提交更糟——它会花掉用户当天的额度去换一条他拿不到的片子。
func (s *Service) Available() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	c := s.cfg.Load()
	return c != nil && c.VideoEnabled && len(c.QwenAPIKeys) > 0 && len(c.VideoHandleKey) > 0
}

// Submit runs the money half of the use case and returns the signed handle. The
// per-second plan is deterministic before the call (the requested duration IS the
// billed quantity), so settle == reserve on an accepted submission.
//
// Submit 跑完用例的钱那一半,返回签名句柄。按秒 plan 在调用前即确定(请求的时长**就是**计费量),
// 故受理成功时 settle == reserve。
func (s *Service) Submit(ctx context.Context, installID, prompt string, seconds int, ratio, resolution string) (string, *apierr.APIError) {
	return s.submit(ctx, installID, prompt, seconds, ratio, resolution, "")
}

// Animate is Submit with a first frame (WRK-082 H9): image-to-video. It shares every gate and the
// whole settlement path — an animated clip costs seconds of video and IS a video, so splitting it
// would duplicate the GW-INV-50 dance to express one extra upstream field.
//
// **The shape guard is the same security decision as the image editor's**, for the same reason and
// at the same boundary: ADR 0011 forbids a managed media input carrying a scheme or a host, because
// an address this gateway would fetch is an SSRF primitive aimed at our own network. A data URL
// cannot be fetched — it IS the bytes.
//
// **Geometry is dropped, not defaulted.** The clip inherits the frame, so passing our ratio and
// resolution through would ask the upstream to letterbox or crop the very picture the user handed
// over — silently.
//
// Animate 是「带首帧的 Submit」(H9):图生视频。它共用每一道闸与整条结算路径——一段动起来的片子花的是
// 视频的秒数、产出的也是视频,拆开只会把 GW-INV-50 舞步复制一遍,只为表达上游多一个字段。
//
// **形状闸与改图那条是同一个安全决定**,同样的理由、同样的边界:ADR 0011 禁止带 scheme 或 host 的
// 受管媒体输入,因为一个本网关会去取的地址是指向我们自己网络的 SSRF 原语。data URL 取不了——它就是字节。
//
// **几何被丢弃、不是被默认。** 片子继承首帧,把我们的 ratio 与 resolution 递过去,等于要求上游对用户
// 刚递来的那张图做信箱边或裁切——而且是静默地做。
func (s *Service) Animate(ctx context.Context, installID, prompt string, seconds int, firstFrame string) (string, *apierr.APIError) {
	if !strings.HasPrefix(firstFrame, "data:") || strings.Contains(firstFrame, "://") {
		return "", apierr.ErrVideoFrameInvalid
	}
	return s.submit(ctx, installID, prompt, seconds, "", "", firstFrame)
}

func (s *Service) submit(ctx context.Context, installID, prompt string, seconds int, ratio, resolution, firstFrame string) (string, *apierr.APIError) {
	if s == nil || s.auth == nil || s.cfg == nil || s.quota == nil || s.upstream == nil || s.clock == nil {
		return "", apierr.Internal()
	}
	if !s.Available() {
		return "", apierr.ErrVideoUnavailable
	}
	got, ae := s.authorize(ctx, installID)
	if ae != nil {
		return "", ae
	}

	c := s.cfg.Load()
	model := strings.TrimSpace(c.VideoUpstreamModel)
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, model, billing.InputVideoSeconds, int64(seconds))
	if err != nil {
		// Startup validation pins the card; reaching this means config drifted mid-flight.
		// 启动校验已钉卡;走到这里说明配置在途漂移。
		return "", apierr.Internal()
	}
	reservation, err := s.quota.Reserve(ctx, got, plan, s.quota.SnapshotPeriod(s.clock.Now()))
	if err != nil {
		var qae *apierr.APIError
		if errors.As(err, &qae) {
			return "", qae
		}
		return "", apierr.Internal()
	}

	taskID, unbilled, err := s.upstream.SubmitVideo(ctx, model, prompt, seconds, ratio, resolution, firstFrame)
	if err != nil {
		if unbilled {
			if rbErr := s.quota.Rollback(ctx, reservation); rbErr != nil {
				s.mx.RollbackFailure()
			}
		} else if stErr := s.quota.Settle(ctx, reservation, reservation.ReservedPUSD); stErr != nil {
			s.mx.SettleFailure()
		}
		var uae *apierr.APIError
		if errors.As(err, &uae) {
			return "", uae
		}
		return "", apierr.ErrUpstreamError
	}

	cost, err := reservation.Plan.UnitCost(billing.InputVideoSeconds, int64(seconds))
	if err != nil {
		cost = reservation.ReservedPUSD // frozen-card failure cannot under-charge / 冻卡异常绝不少收
	}
	if err := s.quota.Settle(ctx, reservation, cost); err != nil {
		s.mx.SettleFailure()
	}
	return domvideo.SignHandle(c.VideoHandleKey, got, taskID), nil
}

// Status answers one poll. It touches no money at all — not the wallet, not the
// category ledger — and its only gate besides the signature is the rate limiter,
// because a client waiting on a 3-minute generation will legitimately ask a dozen
// times and must not burn its request entitlement doing so.
//
// Status 答一次轮询。它**完全不碰钱**——不碰钱包、不碰品类账本——除签名外唯一的闸是限流器,因为
// 一个在等 3 分钟生成的客户端会正当地问上十几次,不该为此烧掉自己的请求额度。
func (s *Service) Status(ctx context.Context, installID, handle string) (VideoStatus, *apierr.APIError) {
	if s == nil || s.auth == nil || s.cfg == nil || s.upstream == nil {
		return VideoStatus{}, apierr.Internal()
	}
	if !s.Available() {
		return VideoStatus{}, apierr.ErrVideoUnavailable
	}
	got, ae := s.authorize(ctx, installID)
	if ae != nil {
		return VideoStatus{}, ae
	}
	taskID, err := domvideo.ParseHandle(s.cfg.Load().VideoHandleKey, got, handle)
	if err != nil {
		return VideoStatus{}, apierr.ErrVideoTaskNotFound
	}
	st, err := s.upstream.PollVideo(ctx, taskID)
	if err != nil {
		var uae *apierr.APIError
		if errors.As(err, &uae) {
			return VideoStatus{}, uae
		}
		return VideoStatus{}, apierr.ErrUpstreamError
	}
	return st, nil
}

// authorize is the install gate both verbs share: existence, ban status, pacing.
//
// authorize 是两个动词共用的 install 闸:存在性、封禁、节流。
func (s *Service) authorize(ctx context.Context, installID string) (string, *apierr.APIError) {
	got, status, found, err := s.auth.LookupInstall(ctx, installID)
	if err != nil {
		return "", apierr.Internal()
	}
	if installID == "" || !found || got == "" {
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

type noopMetrics struct{}

func (noopMetrics) SettleFailure()   {}
func (noopMetrics) RollbackFailure() {}
