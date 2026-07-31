// Package image owns the image-generation use case: authorize an install, reserve
// the per-image plan (monthly + wallet + category-daily gates), call the sync
// DashScope upstream through a port, then settle the deterministic per-image cost
// — or roll back only when the upstream provably never billed.
//
// Everything in that sentence except the port and the rate card is genrun's; this
// package holds the image-shaped remainder. It never imports infra/net.http/sql.
//
// image 包持有图像生成用例:鉴权 install、按张预留(月度+钱包+品类日三闸)、经端口调用同步 DashScope
// 上游,再结算那笔确定的按张成本——仅当上游可证明从未计费时才回滚。
//
// 上面这句话里除了端口与费率卡之外的一切都归 genrun;本包留下的是图像形状的那点余数。它不 import
// infra/net.http/sql。
package image

import (
	"context"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/app/genrun"
	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// Upstream is the sync image-generation port. The infra client returns the
// upstream artifact URL; unbilled=true ONLY for a provably-unbilled explicit
// upstream rejection — every ambiguous outcome (timeout/connect/5xx) keeps
// unbilled=false so the caller settles the full quote (GW-INV-50).
//
// Upstream 是同步图像生成端口。infra client 返回上游产物 URL;unbilled=true **仅限**可证明
// 未计费的显式上游拒绝——一切歧义结果(timeout/connect/5xx)保持 unbilled=false,调用方按
// full quote settle(GW-INV-50)。
type Upstream interface {
	GenerateImage(ctx context.Context, model, prompt, size string) (imageURL string, unbilled bool, err error)
	// EditImage is generation with a source. The source is a base64 data URL, never an address —
	// see Service.Edit for why that is enforced HERE rather than trusted from the caller.
	// EditImage 是「带源的生成」。源是 base64 data URL、绝不是地址——为什么这条在**这里**执行而不是
	// 信任调用方,见 Service.Edit。
	EditImage(ctx context.Context, model, prompt, size, sourceDataURL string) (imageURL string, unbilled bool, err error)
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

// Available reports whether the whole image path exists on this deployment.
//
// Available 报告整条图像路是否存在。
func (s *Service) Available() bool {
	return s != nil && s.gen.Settings().ImageAvailable()
}

// Generate runs the full use case and returns the upstream artifact URL. The
// deterministic per-image plan makes settle == reserve on success; a
// provably-unbilled upstream rejection rolls the reservation back.
//
// Generate 跑完整用例,返回上游产物 URL。按张确定性 plan 使成功时 settle == reserve;可证明未计费
// 的上游拒绝回滚预留。
func (s *Service) Generate(ctx context.Context, installID, prompt, size string) (string, *apierr.APIError) {
	return s.run(ctx, installID, prompt, size, "")
}

// Edit is Generate with a source image. It shares every gate — auth, ban, rate limit, reserve,
// settle, rollback — because an edit costs an image and IS an image.
//
// **The shape guard is the security decision, and it lives here.** ADR 0011 forbids a managed media
// input that carries a scheme or a host: an address the gateway would fetch is an SSRF primitive
// pointed at our own network. A data URL cannot be fetched — it IS the bytes — so requiring that
// shape is not a formality, it is the entire mitigation. Enforced at the trust boundary rather than
// trusted from the desktop, because "our client always sends data URLs" is a statement about our
// client, not about whoever is actually calling.
//
// Edit 是「带源图的 Generate」。它共用**每一道**闸——鉴权、封禁、限流、预留、结算、回滚——因为一次
// 改图**花的是一张图的钱、产出的也是一张图**。
//
// **形状闸就是那个安全决定,而它住在这里。** ADR 0011 禁止带 scheme 或 host 的受管媒体输入:一个网关
// 会去取的地址,是一个指向**我们自己网络**的 SSRF 原语。data URL **取不了**——它**就是**字节——故要求
// 这个形状不是形式主义,它**就是**全部的缓解措施。在**信任边界**上执行、而不是信任桌面端,因为「我们的
// 客户端总是发 data URL」说的是**我们的客户端**,不是实际在调用的那一位。
func (s *Service) Edit(ctx context.Context, installID, prompt, size, source string) (string, *apierr.APIError) {
	if !strings.HasPrefix(source, "data:") || strings.Contains(source, "://") {
		return "", apierr.ErrImageSourceInvalid
	}
	return s.run(ctx, installID, prompt, size, source)
}

func (s *Service) run(ctx context.Context, installID, prompt, size, source string) (string, *apierr.APIError) {
	if s == nil || !s.gen.Ready() || s.upstream == nil {
		return "", apierr.Internal()
	}
	if !s.Available() {
		return "", apierr.ErrImageUnavailable
	}
	got, ae := s.gen.Authorize(ctx, installID)
	if ae != nil {
		return "", ae
	}

	model := strings.TrimSpace(s.gen.Settings().ImageUpstreamModel)
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, model, billing.InputImages, 1)
	if err != nil {
		// Startup validation pins the card; reaching this means config drifted mid-flight.
		// 启动校验已钉卡;走到这里说明配置在途漂移。
		return "", apierr.Internal()
	}
	return genrun.Do(ctx, s.gen, got,
		genrun.Charge{Plan: plan, Class: billing.InputImages, Units: 1},
		func(ctx context.Context) (string, bool, error) {
			if source == "" {
				return s.upstream.GenerateImage(ctx, model, prompt, size)
			}
			return s.upstream.EditImage(ctx, model, prompt, size, source)
		})
}
