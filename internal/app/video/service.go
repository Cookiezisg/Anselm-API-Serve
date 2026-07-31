// Package video owns the video-generation use case: authorize an install, reserve
// the per-second plan (monthly + wallet + category-daily gates), SUBMIT to the
// async DashScope upstream through a port, settle at submit, and hand back a
// signed handle the same install can later poll.
//
// The money shape is the one thing here that differs from every other capability,
// so it is stated once, plainly: **the charge lands at submit, and polling never
// moves money.** A generation that fails upstream is still paid for. That is
// deliberate — the alternative is a refund path that only runs when the client
// bothers to poll, which means a client that walks away gets its video for free
// while a client that waits pays. Free-tier money must never depend on whether
// somebody came back.
//
// video 包持有视频生成用例:鉴权 install、按秒预留(月度+钱包+品类日三闸)、经端口向异步 DashScope
// 上游**提交**、在提交处结算,并交回一个只有同一个 install 能轮询的签名句柄。
//
// 这里唯一与其余能力不同的是钱的形状,所以只说一次、说明白:**钱落在提交,轮询绝不动钱。**
// 一次在上游失败的生成**照样付费**。这是刻意的——另一条路是「只有客户端肯回来轮询时才跑」的退款路径,
// 那意味着**走开的人白拿视频、等着的人付钱**。免费档的钱绝不能取决于有没有人回来看。
package video

import (
	"context"
	"errors"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/app/genrun"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
)

// Upstream is the async video port: two verbs, because the family has no
// synchronous form. unbilled=true ONLY for a provably-unbilled explicit
// rejection (GW-INV-50).
//
// Upstream 是异步视频端口:**两个**动词,因为本族没有同步形态。unbilled=true **仅限**可证明未计费
// 的显式拒绝(GW-INV-50)。
type Upstream interface {
	// SubmitVideo takes an optional firstFrame data URL: empty = text-to-video, present =
	// image-to-video. One method rather than two because the upstream is the same endpoint with
	// one more input field, and the entire settlement dance is identical.
	// SubmitVideo 收一个可选的 firstFrame data URL:空=文生视频,有=图生视频。**一个方法**而非两个,
	// 因为上游是同一条端点多一个输入字段,而整套结算舞步一模一样。
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

type Service struct {
	gen      genrun.Runner
	upstream Upstream
}

type Deps struct {
	Auth     genrun.Authenticator
	Quota    genrun.Quota
	RL       genrun.RateLimiter
	Config   genrun.Config
	Upstream Upstream
	Clock    genrun.Clock
	Metrics  genrun.Metrics
}

func New(d Deps) *Service {
	return &Service{
		gen: genrun.New(genrun.Ports{Auth: d.Auth, RL: d.RL, Config: d.Config,
			Quota: d.Quota, Clock: d.Clock, Metrics: d.Metrics}),
		upstream: d.Upstream,
	}
}

// Available reports whether the whole video path exists, handle-signing material
// included: a submission the caller can never poll is worse than no submission at
// all — it would spend the user's daily allowance on a video they cannot reach.
//
// Available 报告整条视频路是否存在,**含**句柄签名材料:一次「提交了却永远轮询不到」比根本不提交
// 更糟——它会花掉用户当天的额度去换一条他拿不到的片子。
func (s *Service) Available() bool {
	return s != nil && s.gen.Settings().VideoAvailable()
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

// Animate is Submit with a first frame: image-to-video. It shares every gate and the whole
// settlement path — an animated clip costs seconds of video and IS a video.
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
// Animate 是「带首帧的 Submit」:图生视频。它共用每一道闸与整条结算路径——一段动起来的片子花的是视频
// 的秒数、产出的也是视频。
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
	if s == nil || !s.gen.Ready() || s.upstream == nil {
		return "", apierr.Internal()
	}
	if !s.Available() {
		return "", apierr.ErrVideoUnavailable
	}
	got, ae := s.gen.Authorize(ctx, installID)
	if ae != nil {
		return "", ae
	}

	c := s.gen.Settings()
	model := strings.TrimSpace(c.VideoUpstreamModel)
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, model, billing.InputVideoSeconds, int64(seconds))
	if err != nil {
		// Startup validation pins the card; reaching this means config drifted mid-flight.
		// 启动校验已钉卡;走到这里说明配置在途漂移。
		return "", apierr.Internal()
	}
	taskID, ae := genrun.Do(ctx, s.gen, got,
		genrun.Charge{Plan: plan, Class: billing.InputVideoSeconds, Units: int64(seconds)},
		func(ctx context.Context) (string, bool, error) {
			return s.upstream.SubmitVideo(ctx, model, prompt, seconds, ratio, resolution, firstFrame)
		})
	if ae != nil {
		return "", ae
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
	if s == nil || !s.gen.Ready() || s.upstream == nil {
		return VideoStatus{}, apierr.Internal()
	}
	if !s.Available() {
		return VideoStatus{}, apierr.ErrVideoUnavailable
	}
	got, ae := s.gen.Authorize(ctx, installID)
	if ae != nil {
		return VideoStatus{}, ae
	}
	taskID, err := domvideo.ParseHandle(s.gen.Settings().VideoHandleKey, got, handle)
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
