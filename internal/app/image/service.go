// Package image owns the image-generation use case (WRK-082 批B): authorize an
// install, reserve the per-image plan (monthly + wallet + category-daily gates),
// call the sync DashScope upstream through a port, then settle the deterministic
// per-image cost — or roll back only when the upstream provably never billed.
// It never imports infra/net.http/sql; the upstream client satisfies the port.
package image

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

// Available reports whether the whole image path exists on this deployment:
// capability on AND a Qwen credential present (the double-half rule).
//
// Available 报告整条图像路是否存在:能力开 **且** Qwen 凭证在(双半才真)。
func (s *Service) Available() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	c := s.cfg.Load()
	return c != nil && c.ImageEnabled && len(c.QwenAPIKeys) > 0
}

// Generate runs the full use case and returns the upstream artifact URL
// (URL 直通, P13). The deterministic per-image plan makes settle == reserve on
// success; a provably-unbilled upstream rejection rolls the reservation back.
//
// Generate 跑完整用例,返回上游产物 URL(URL 直通,P13)。按张确定性 plan 使成功时
// settle == reserve;可证明未计费的上游拒绝回滚预留。
func (s *Service) Generate(ctx context.Context, installID, prompt, size string) (string, *apierr.APIError) {
	return s.run(ctx, installID, prompt, size, "")
}

// Edit is Generate with a source image (WRK-082 H9). It shares every gate — auth, ban, rate limit,
// reserve, settle, rollback — because an edit costs an image and IS an image: splitting it into a
// second service would have duplicated the whole GW-INV-50 settlement dance to express one extra
// upstream field.
//
// **The shape guard is the security decision, and it lives here.** ADR 0011 forbids a managed media
// input that carries a scheme or a host: an address the gateway would fetch is an SSRF primitive
// pointed at our own network. A data URL cannot be fetched — it IS the bytes — so requiring that
// shape is not a formality, it is the entire mitigation. Enforced at the trust boundary rather than
// trusted from the desktop, because "our client always sends data URLs" is a statement about our
// client, not about whoever is actually calling.
//
// Edit 是「带源图的 Generate」(H9)。它共用**每一道**闸——鉴权、封禁、限流、预留、结算、回滚——因为
// 一次改图**花的是一张图的钱、产出的也是一张图**:另起一个服务,等于把整套 GW-INV-50 结算舞步复制
// 一遍,只为表达上游多一个字段。
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
	if s == nil || s.auth == nil || s.cfg == nil || s.quota == nil || s.upstream == nil || s.clock == nil {
		return "", apierr.Internal()
	}
	if !s.Available() {
		return "", apierr.ErrImageUnavailable
	}
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

	c := s.cfg.Load()
	model := strings.TrimSpace(c.ImageUpstreamModel)
	plan, err := billing.NewUnitPlan(billing.ProviderQwen, model, billing.InputImages, 1)
	if err != nil {
		// Startup validation pins the card; reaching this means config drifted mid-flight.
		// 启动校验已钉卡;走到这里说明配置在途漂移。
		return "", apierr.Internal()
	}
	reservation, err := s.quota.Reserve(ctx, got, plan, s.quota.SnapshotPeriod(s.clock.Now()))
	if err != nil {
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return "", ae
		}
		return "", apierr.Internal()
	}

	var (
		imageURL string
		unbilled bool
	)
	if source == "" {
		imageURL, unbilled, err = s.upstream.GenerateImage(ctx, model, prompt, size)
	} else {
		imageURL, unbilled, err = s.upstream.EditImage(ctx, model, prompt, size, source)
	}
	if err != nil {
		if unbilled {
			if rbErr := s.quota.Rollback(ctx, reservation); rbErr != nil {
				s.mx.RollbackFailure()
			}
		} else if stErr := s.quota.Settle(ctx, reservation, reservation.ReservedPUSD); stErr != nil {
			// Ambiguous upstream outcome: the provider may have billed, keep the full
			// quote (GW-INV-50). A failed settle is observable, the orphan scanner is
			// only the backstop.
			// 歧义上游结果:上游可能已计费,保留全额(GW-INV-50)。settle 失败必须可观测,
			// 孤儿扫描只是兜底。
			s.mx.SettleFailure()
		}
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return "", ae
		}
		return "", apierr.ErrUpstreamError
	}

	cost, err := reservation.Plan.UnitCost(billing.InputImages, 1)
	if err != nil {
		cost = reservation.ReservedPUSD // frozen-card failure cannot under-charge / 冻卡异常绝不少收
	}
	if err := s.quota.Settle(ctx, reservation, cost); err != nil {
		s.mx.SettleFailure()
	}
	return imageURL, nil
}

type noopMetrics struct{}

func (noopMetrics) SettleFailure()   {}
func (noopMetrics) RollbackFailure() {}
