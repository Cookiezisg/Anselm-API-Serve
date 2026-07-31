// Package genrun holds the skeleton every paid capability walks: authorize the
// install, reserve against the wallet, call the provider, then settle or roll
// back by the GW-INV-50 rule.
//
// It exists because that skeleton had been written out five times. image, tts,
// video, voice and speech each declared the same six ports — byte for byte, the
// hashes matched — and image, tts and video repeated the same settlement dance
// line for line. Five copies of a money path is not five times the code; it is
// five places where a rule can be fixed in four of them and stay broken in the
// fifth, silently, because the tests of the fixed four go green.
//
// What stays in each capability is what genuinely differs: its switch, its rate
// card and billed unit, its unavailable error, and its own upstream call. What
// moved here is everything that was identical.
//
// The steps are exported individually and Do is composed FROM them, not the other
// way round: voice settles before its record write and rolls back at two extra
// points, so it needs the pieces without the whole. A skeleton nobody can take
// apart forces the odd capability to keep its own copy — which is how the copies
// got here.
//
// genrun 持有每个付费能力都要走的骨架:鉴权 install、向钱包预留、调用 provider,然后按 GW-INV-50
// 规则结算或回滚。
//
// 它之所以存在,是因为这套骨架被写了**五遍**。image/tts/video/voice/speech 各自声明了同样的六个
// 端口——逐字节相同,哈希对得上——而 image/tts/video 更是逐行重复了同一套结算舞步。一条钱路径的
// 五份拷贝不是五倍代码,而是**五个地方**:一条规则可以在其中四处被修好、在第五处静默地留着错,因为
// 修好那四处的测试都绿了。
//
// 留在各能力里的是真正不同的东西:自己的开关、费率卡与计费单位、unavailable 错误、自己的上游调用。
// 搬到这里的是当初完全相同的一切。
//
// 各步骤**单独导出**,而 Do 是**由它们组合出来的**、不是反过来:voice 在记录写之前结算、且多两处
// 回滚点,故它需要零件而不是整体。一个拆不开的骨架会逼着那个特别的能力自留一份拷贝——而那正是这些
// 拷贝的由来。
package genrun

import (
	"context"
	"errors"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// Authenticator resolves an install id and reports whether it may spend.
//
// Authenticator 解析 install id,并报告它能不能花钱。
type Authenticator interface {
	LookupInstall(ctx context.Context, installID string) (id string, status dominstall.Status, found bool, err error)
}

// RateLimiter paces one install.
//
// RateLimiter 给单个 install 限速。
type RateLimiter interface {
	Allow(key string) bool
}

// Config is the live-config port: an atomically swapped snapshot, so a hot
// reload takes effect on the very next request with no restart.
//
// Config 是 live 配置端口:一份原子换掉的快照,故热重载在**下一个**请求上即生效、无需重启。
type Config interface {
	Load() *config.Config
}

// Quota is the pUSD wallet. Every capability reserves against the SAME one,
// because the money leaves the same provider credential.
//
// Quota 是 pUSD 钱包。每个能力预留的都是**同一个**,因为钱从同一把 provider 凭证出去。
type Quota interface {
	SnapshotPeriod(now time.Time) domquota.Period
	Reserve(ctx context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error)
	Settle(ctx context.Context, r *domquota.Reservation, actualPUSD int64) error
	Rollback(ctx context.Context, r *domquota.Reservation) error
}

// Clock is the time port.
//
// Clock 是时间端口。
type Clock interface {
	Now() time.Time
}

// Metrics makes a failed close-out observable. Money that fails to settle is
// money the operator's wallet under-reports until the orphan scanner finalizes
// it, so it must be visible rather than silently deferred.
//
// Metrics 让「没收上口」可观测。结算失败的钱,在孤儿扫描收口之前会被 operator 钱包**少报**,故它
// 必须看得见、而不是被静默推迟。
type Metrics interface {
	SettleFailure()
	RollbackFailure()
}

// Ports is the wiring struct. Each capability's own Deps forwards into it, which
// is why bootstrap did not have to change when this package appeared.
//
// Ports 是接线结构。各能力自己的 Deps 转发进来——这也是本包出现时 bootstrap 一行都不用改的原因。
type Ports struct {
	Auth    Authenticator
	RL      RateLimiter
	Config  Config
	Quota   Quota
	Clock   Clock
	Metrics Metrics
}

// Runner is the wired skeleton. It is a value: six interface fields, fixed at
// construction, copied freely.
//
// Runner 是接好线的骨架。它是**值**:六个接口字段,构造时定死,随便拷。
type Runner struct {
	auth  Authenticator
	rl    RateLimiter
	cfg   Config
	quota Quota
	clock Clock
	mx    Metrics
}

// New wires a Runner, defaulting the metrics port so no step has to nil-check it.
//
// New 接好一个 Runner,并给指标端口补默认值,使任何一步都不必再 nil 判断。
func New(p Ports) Runner {
	r := Runner{auth: p.Auth, rl: p.RL, cfg: p.Config, quota: p.Quota, clock: p.Clock, mx: p.Metrics}
	if r.mx == nil {
		r.mx = noopMetrics{}
	}
	return r
}

// Ready reports whether every port a paid call needs was wired. A half-built
// service answers Internal() rather than dereferencing nil. The rate limiter and
// the metrics port are absent on purpose: an unwired limiter means "do not pace",
// and metrics always has its no-op.
//
// Ready 报告付费调用所需的端口是否都接上了。半成品服务答 Internal(),而不是解引用 nil。限流器与
// 指标端口**刻意不在其中**:没接的限流器意思是「不限速」,而指标恒有它的 no-op。
func (r Runner) Ready() bool {
	return r.auth != nil && r.cfg != nil && r.quota != nil && r.clock != nil
}

// Settings snapshots the live config once, or nil if the port is unwired. The
// capability predicates on *config.Config are nil-safe, so callers may chain.
//
// Settings 取一次 live 配置快照;端口没接则为 nil。*config.Config 上的能力谓词对 nil 安全,故调用方
// 可以直接链下去。
func (r Runner) Settings() *config.Config {
	if r.cfg == nil {
		return nil
	}
	return r.cfg.Load()
}

// Now reads the clock port, so a capability that timestamps its own records does
// not have to hold a second reference to it.
//
// Now 读时钟端口,使一个要给自己记录打时间戳的能力不必再持一份它的引用。
func (r Runner) Now() time.Time {
	if r.clock == nil {
		return time.Time{}
	}
	return r.clock.Now()
}

// Authorize is the install gate: existence, ban status, pacing. It returns the
// STORED install id, which is what everything downstream must key on.
//
// The empty-id check comes before the lookup, not after: an empty id is not a
// question worth asking the store, and asking it would let a store that errors on
// the empty string turn a plain bad request into a 500.
//
// Authorize 是 install 闸:存在性、封禁、节流。它返回**库里那个** install id,下游一切都必须按它索引。
//
// 空 id 的检查在查库**之前**、不在之后:空 id 不是一个值得问库的问题,而问了它会让一个「对空串报错」
// 的库把一个普通的坏请求变成 500。
func (r Runner) Authorize(ctx context.Context, installID string) (string, *apierr.APIError) {
	if r.auth == nil {
		return "", apierr.Internal()
	}
	if installID == "" {
		return "", apierr.ErrInvalidInstall
	}
	got, status, found, err := r.auth.LookupInstall(ctx, installID)
	if err != nil {
		return "", apierr.Internal()
	}
	if !found || got == "" {
		return "", apierr.ErrInvalidInstall
	}
	if status == dominstall.StatusBanned {
		return "", apierr.ErrAccountBanned
	}
	if r.rl != nil && !r.rl.Allow(got) {
		return "", apierr.ErrRateLimited
	}
	return got, nil
}

// Reserve takes the double gate — per-install monthly requests and the operator's
// pUSD wallet — in one BEGIN IMMEDIATE, with the period snapshotted once. A quota
// refusal carries its own wire error; anything else is ours to own as Internal().
//
// Reserve 在一个 BEGIN IMMEDIATE 里过双闸——per-install 月请求额度与 operator 的 pUSD 钱包——period
// 只快照一次。配额拒绝自带线缆错误;其余一律算我们自己的,归 Internal()。
// The guard names exactly what this step touches — the wallet and the clock —
// rather than the whole port set: the realtime path reserves from a service that
// was never given an authenticator, because the WebSocket already authorized.
//
// 守卫只点名这一步真正碰的东西——钱包与时钟——而不是整套端口:实时那条路是从一个**从未拿到**
// 鉴权器的服务上预留的,因为 WebSocket 早已鉴过权。
func (r Runner) Reserve(ctx context.Context, installID string, plan billing.Plan) (*domquota.Reservation, *apierr.APIError) {
	if r.quota == nil || r.clock == nil {
		return nil, apierr.Internal()
	}
	res, err := r.quota.Reserve(ctx, installID, plan, r.quota.SnapshotPeriod(r.clock.Now()))
	if err != nil {
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, apierr.Internal()
	}
	return res, nil
}

// Settle closes a reservation at the actual cost and reports a failure to close
// it. The error is returned as well as counted, because the WebSocket path needs
// to know and the request paths deliberately do not.
//
// Settle 按实际成本收口一次预留,并报告收不上口。错误既计数**也**返回,因为 WebSocket 那条路需要
// 知道,而请求路径刻意不需要。
func (r Runner) Settle(ctx context.Context, res *domquota.Reservation, actualPUSD int64) error {
	if res == nil {
		return nil
	}
	if err := r.quota.Settle(ctx, res, actualPUSD); err != nil {
		r.mx.SettleFailure()
		return err
	}
	return nil
}

// Rollback reverses a charge for work that provably never reached the provider.
// Only that case — GW-INV-50 keeps every ambiguous outcome charged.
//
// Rollback 为一次**可证明从未抵达上游**的工作反转计费。**仅此一种**——GW-INV-50 要求一切歧义结果
// 照收。
func (r Runner) Rollback(ctx context.Context, res *domquota.Reservation) error {
	if res == nil {
		return nil
	}
	if err := r.quota.Rollback(ctx, res); err != nil {
		r.mx.RollbackFailure()
		return err
	}
	return nil
}

// Charge is what one paid call costs: the plan to reserve, and the unit to price
// the settlement by. For every capability here the quantity is known BEFORE the
// call — one image, N characters, N seconds — so settle equals reserve on
// success and no usage feedback from the provider is needed.
//
// Charge 是一次付费调用的成本:用来预留的 plan,以及给结算定价的单位。这里每个能力的量都在调用
// **之前**已知——一张图、N 个字符、N 秒——故成功时 settle == reserve,不需要上游回报用量。
type Charge struct {
	Plan  billing.Plan
	Class billing.InputClass
	Units int64
}

// Call is the provider half. unbilled is true ONLY for a provably-unbilled
// explicit rejection; every ambiguous outcome (timeout, connect, 5xx, cancel)
// keeps it false so the caller settles the full quote (GW-INV-50).
//
// Call 是 provider 那一半。unbilled **仅限**可证明未计费的显式拒绝为真;一切歧义结果(timeout、
// connect、5xx、取消)保持 false,使调用方按全额报价 settle(GW-INV-50)。
type Call[T any] func(ctx context.Context) (T, bool, error)

// Do reserves, calls, and closes the books — the whole money half of a
// synchronous paid capability, for a caller that has already authorized the
// install and built its plan.
//
// Do 预留、调用、平账——一个同步付费能力钱的那**整**半,给一个已经鉴过权、已经建好 plan 的调用方。
func Do[T any](ctx context.Context, r Runner, installID string, c Charge, call Call[T]) (T, *apierr.APIError) {
	var zero T
	res, ae := r.Reserve(ctx, installID, c.Plan)
	if ae != nil {
		return zero, ae
	}

	out, unbilled, err := call(ctx)
	if err != nil {
		if unbilled {
			_ = r.Rollback(ctx, res)
		} else {
			// Ambiguous upstream outcome: the provider may have billed, so keep the
			// full quote. The orphan scanner is only the backstop.
			// 歧义上游结果:上游可能已计费,故保留全额。孤儿扫描只是兜底。
			_ = r.Settle(ctx, res, res.ReservedPUSD)
		}
		var uae *apierr.APIError
		if errors.As(err, &uae) {
			return zero, uae
		}
		return zero, apierr.ErrUpstreamError
	}

	cost, cerr := res.Plan.UnitCost(c.Class, c.Units)
	if cerr != nil {
		cost = res.ReservedPUSD // frozen-card failure cannot under-charge / 冻卡异常绝不少收
	}
	_ = r.Settle(ctx, res, cost)
	return out, nil
}

type noopMetrics struct{}

func (noopMetrics) SettleFailure()   {}
func (noopMetrics) RollbackFailure() {}
