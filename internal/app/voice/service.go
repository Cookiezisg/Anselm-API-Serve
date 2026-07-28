// Package voice owns the cloned-voice use case (WRK-082 H9): enroll a reference clip as a named
// voice, list what an install has, and delete one.
//
// **The resource lives in OUR provider account, and that single fact shapes the whole package.**
// Every install's clone is registered under one DashScope credential, so this service guards two
// ceilings rather than one: the per-install inventory (what a user may keep) and the per-account
// total (what the provider will let us hold at all). Nothing else in the system can see the second
// one — the desktop has no idea other installs exist.
//
// It is INVENTORY, not quota, in both directions: nothing frees a slot with the passage of time,
// creation costs money once, and a voice occupies its slot until deleted. So the refusals here say
// "delete one", never "try again later" — and when OUR ceiling is the one that is full, the answer
// is to refuse and log loudly, never to evict somebody else's voice to make room.
//
// Package voice 拥有克隆音色用例(H9):把一段参考音频登记成具名音色、列出某 install 有什么、删掉一个。
//
// **资源住在我们的 provider 账号里,而这一个事实塑造了整个包。** 每个 install 的克隆都登记在**同一把**
// DashScope 凭证之下,故本服务守的是**两条**上限而非一条:逐 install 的库存(用户能留多少)与**账号级**
// 总数(供应商根本允许我们持有多少)。系统里没有别的东西看得见第二条——桌面端根本不知道还有别的 install。
//
// 两个方向上它都是**库存、不是配额**:时间流逝不腾位、创建花一次钱、一个音色占着位直到被删。故这里的
// 拒绝话术一律是「删一个」、绝不是「过会儿再试」——而当满的是**我们**那条上限时,答案是**拒绝并大声记
// 日志**,绝不是驱逐别人的音色来腾地方。
package voice

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
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
	"github.com/sunweilin/anselm/gateway/internal/pkg/secureurl"
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

// Store is the voice-inventory port.
//
// Store 是音色库存端口。
type Store interface {
	ListVoices(ctx context.Context, installID string) ([]domvoice.Voice, error)
	CountAllVoices(ctx context.Context) (int, error)
	CreateVoice(ctx context.Context, installID string, v domvoice.Voice) error
	DeleteVoice(ctx context.Context, installID, id string) (upstreamID string, found bool, err error)
}

// Upstream is the provider port for the voice lifecycle. `unbilled` is true ONLY for a provably
// pre-creation rejection; everything ambiguous keeps the charge (GW-INV-50).
//
// Upstream 是音色生命周期的 provider 端口。`unbilled` **只有**在可证明的创建前拒绝时才为 true;
// 一切歧义都保留计费(GW-INV-50)。
type Upstream interface {
	EnrollVoice(ctx context.Context, sampleURL string) (upstreamID string, unbilled bool, err error)
	AwaitVoiceReady(ctx context.Context, upstreamID string) error
	DeleteVoice(ctx context.Context, upstreamID string) error
}

// Samples turns an install-owned media lease into an address the upstream can fetch, and spends it
// afterwards. It is the seam that keeps this package from importing app/media.
//
// **Why a URL at all** — `voice-enrollment` accepts no base64 (真机实测). This is the single narrow
// exception to ADR 0012's "never hand the provider an address"; chat and image inputs still inline
// their bytes.
//
// Samples 把一个归本 install 所有的媒体 lease 变成上游能取的地址,并在用完后**花掉**它。它是让本包
// 不必 import app/media 的那道缝。
//
// **为什么非要 URL**——`voice-enrollment` 不收 base64(真机实测)。这是 ADR 0012「绝不给 provider
// 一个地址」的**唯一一条窄缝例外**;chat 与图像输入仍然内联字节。
type Samples interface {
	SampleFetchToken(ctx context.Context, installID, leaseID string) (string, error)
	RevokeSample(ctx context.Context, installID, leaseID string) error
}

// Quota is the pUSD wallet port — the same one image, tts and video reserve against, because the
// money leaves the SAME provider credential. Enrollment has no usage feedback to settle against:
// the price is fixed and known before the call, so reserve == settle for a success.
//
// Quota 是 pUSD 钱包端口——与 image/tts/video 预留的是同一个,因为钱从**同一把** provider 凭证出去。
// 登记没有可结算的用量回报:价格固定、调用前已知,故成功时 reserve == settle。
type Quota interface {
	SnapshotPeriod(now time.Time) domquota.Period
	Reserve(ctx context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error)
	Settle(ctx context.Context, r *domquota.Reservation, actualPUSD int64) error
	Rollback(ctx context.Context, r *domquota.Reservation) error
}

type Clock interface {
	Now() time.Time
}

type IDGen interface {
	New() string
}

// Logger reports the two conditions an operator must act on, as two distinct signals — they are
// different problems with different remedies, and collapsing them into one would send whoever reads
// the log looking in the wrong place.
//
// Logger 报告运营者**必须**处理的两个状况,作为**两个不同的信号**——它们是不同的问题、不同的补救办法,
// 合并成一个会让读日志的人去错的地方找。
type Logger interface {
	// AccountVoiceCeilingReached: our shared account is full. Remedy: raise the ceiling or delete
	// unused voices upstream. 我们的共享账号满了。补救:抬上限,或去上游删掉没人用的音色。
	AccountVoiceCeilingReached(total, ceiling int)
	// VoiceOrphaned: an upstream registration exists that nothing can address — the record write
	// failed AND the rollback failed. Remedy: delete it upstream by hand. Nothing automatic can.
	// VoiceOrphaned:存在一个谁也寻址不到的上游登记——记录写失败**且**回滚也失败。补救:去上游手动
	// 删掉它。没有任何自动机制做得到。
	VoiceOrphaned(upstreamID string)
	// SampleNotRevoked: a spent fetch capability is still live until its TTL. Not fatal, but it is
	// a window nobody intends to be open. SampleNotRevoked:一个已经用掉的取回凭据在 TTL 之前仍然
	// 活着。不致命,但那是一扇没人打算开着的窗。
	SampleNotRevoked(leaseID string)
	// VoiceSlowToDeploy: paid for and recorded, but not yet usable when we stopped waiting.
	// VoiceSlowToDeploy:已付费、已记录,但在我们不再等的时候还不能用。
	VoiceSlowToDeploy(upstreamID string)
	// SettleFailure / RollbackFailure: the books did not close. Money that fails to settle is money
	// the operator's wallet under-reports until the orphan scanner finalizes it, so it must be
	// observable rather than silently deferred (B2, the same rule as the other three capabilities).
	// SettleFailure / RollbackFailure:账没平。结算失败的钱,在孤儿扫描收口之前会被 operator 钱包
	// **少报**,故它必须可观测、而不是被静默推迟(B2,与另外三个能力同一条规矩)。
	SettleFailure()
	RollbackFailure()
}

// Service enrolls, lists and deletes voices.
//
// Service 登记、列出、删除音色。
type Service struct {
	auth     Authenticator
	samples  Samples
	quota    Quota
	rl       RateLimiter
	cfg      Config
	store    Store
	upstream Upstream
	clock    Clock
	ids      IDGen
	log      Logger
}

// Deps wires the service.
type Deps struct {
	Auth     Authenticator
	Samples  Samples
	Quota    Quota
	RL       RateLimiter
	Config   Config
	Store    Store
	Upstream Upstream
	Clock    Clock
	IDs      IDGen
	Log      Logger
}

// New builds the service.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = noopLogger{}
	}
	return &Service{auth: d.Auth, samples: d.Samples, quota: d.Quota, rl: d.RL, cfg: d.Config, store: d.Store,
		upstream: d.Upstream, clock: d.Clock, ids: d.IDs, log: log}
}

// Available reports whether voice cloning is configured at all (it rides the speech capability —
// a deployment that cannot speak has no use for a voice).
//
// Available 报告音色克隆是否配置了(它搭在语音能力上——说不了话的部署要音色没有用)。
func (s *Service) Available() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	c := s.cfg.Load()
	return c.SpeechEnabled && len(c.QwenAPIKeys) > 0
}

// Enroll registers a sample as a named voice.
//
// The ORDER is the correctness argument, and it is the mirror of the desktop's: both ceilings and
// the name collision are checked BEFORE the paid upstream call, because enrolling and then failing
// to record it would leave an orphan in our account — consuming a slot of the shared ceiling that
// nothing can ever address again. If the record fails anyway, the upstream registration is rolled
// back rather than abandoned.
//
// Enroll 把一段样本登记成具名音色。
//
// **顺序就是那个正确性论证**,且与桌面侧互为镜像:两条上限与重名都在**付费上游调用之前**查,因为
// 先登记、后记不下来,会在我们账号里留下一个孤儿——占着共享上限的一个位,而再没有东西寻址得到它。
// 万一记录仍然失败,上游登记会被**回滚**而不是被丢下。
func (s *Service) Enroll(ctx context.Context, installID, name, leaseID string) (domvoice.Voice, *apierr.APIError) {
	if s == nil || s.auth == nil || s.cfg == nil || s.store == nil || s.upstream == nil ||
		s.clock == nil || s.ids == nil || s.quota == nil || s.samples == nil {
		return domvoice.Voice{}, apierr.Internal()
	}
	if !s.Available() {
		return domvoice.Voice{}, apierr.ErrVoiceUnavailable
	}
	// **The sample is named by a LEASE ID, never by an address.** ADR 0011's入站 half is untouched:
	// a caller still cannot hand this gateway a URL to fetch, because that is an SSRF primitive
	// aimed at our own network. What changed is only the OUTBOUND hop — the gateway resolves the
	// lease it already owns into its own public address (ADR 0012 clause 4's parked media subdomain),
	// because `voice-enrollment` accepts nothing else.
	// **样本由 lease id 指名,绝不由地址指名。** ADR 0011 的**入站**那半原样不动:调用方**仍然**不能
	// 递给本网关一个 URL 让它去取——那是一枚指向我们自己网络的 SSRF 原语。变的只是**出站**那一跳:
	// 网关把自己本来就拥有的 lease 解析成**它自己的**公开地址(ADR 0012 第 4 条为此留的媒体子域),
	// 因为 `voice-enrollment` 别的什么都不收。
	if strings.TrimSpace(leaseID) == "" {
		return domvoice.Voice{}, apierr.ErrVoiceSampleInvalid
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return domvoice.Voice{}, apierr.ErrBadRequest
	}
	got, ae := s.authorize(ctx, installID)
	if ae != nil {
		return domvoice.Voice{}, ae
	}

	mine, err := s.store.ListVoices(ctx, got)
	if err != nil {
		return domvoice.Voice{}, apierr.Internal()
	}
	if len(mine) >= domvoice.PerInstallInventory {
		return domvoice.Voice{}, apierr.ErrVoiceInventoryFull
	}
	for _, v := range mine {
		if v.Name == name {
			return domvoice.Voice{}, apierr.ErrVoiceNameTaken
		}
	}

	// OUR ceiling, the one only this gateway can see. Refuse and log — never evict somebody else's
	// voice to make room, because the person losing it did nothing and would never learn why.
	// **我们**那条上限,只有本网关看得见的那条。**拒绝并记日志**——绝不驱逐别人的音色来腾地方:丢掉它
	// 的那个人什么也没做,而且永远不会知道为什么。
	if ceiling := s.cfg.Load().VoiceAccountCeiling; ceiling > 0 {
		total, err := s.store.CountAllVoices(ctx)
		if err != nil {
			return domvoice.Voice{}, apierr.Internal()
		}
		if int64(total) >= ceiling {
			s.log.AccountVoiceCeilingReached(total, int(ceiling))
			return domvoice.Voice{}, apierr.ErrVoiceCapacityReached
		}
	}

	// The wallet is the LAST gate before the money moves, and the daily category gate rides inside
	// it (VOICE_DAILY_LIMIT, consumed in the same BEGIN IMMEDIATE as the reservation). It is the
	// gate that actually bounds this capability: the inventory ceilings above bound how many voices
	// exist AT ONCE, and since Delete frees a slot, an enroll→delete cycle would otherwise spend
	// without any limit at all.
	// 钱包是钱动之前的**最后**一道闸,而品类日闸搭在它里面(VOICE_DAILY_LIMIT,与预留在同一个
	// BEGIN IMMEDIATE 里消耗)。**它才是真正界住这个能力的那道闸**:上面那两条库存上限界的是**同时**
	// 存在几个音色,而既然 Delete 会腾出位置,enroll→delete 循环在没有它的时候可以毫无上界地花钱。
	plan, err := billing.NewVoicesPlan(billing.ProviderQwen, billing.QwenTTSClone, 1)
	if err != nil {
		return domvoice.Voice{}, apierr.Internal()
	}
	reservation, err := s.quota.Reserve(ctx, got, plan, s.quota.SnapshotPeriod(s.clock.Now()))
	if err != nil {
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return domvoice.Voice{}, ae
		}
		return domvoice.Voice{}, apierr.Internal()
	}

	// Resolve the lease AFTER the wallet, not before: a caller who is out of daily allowance must
	// not have caused us to mint a public address for their audio at all.
	// 在钱包**之后**才解析 lease,不在之前:一个当天额度已用尽的调用方,不该导致我们**根本**为他的
	// 音频铸出过一个公开地址。
	token, terr := s.samples.SampleFetchToken(ctx, got, leaseID)
	if terr != nil {
		s.rollbackReservation(ctx, reservation)
		return domvoice.Voice{}, apierr.ErrVoiceSampleInvalid
	}
	sampleURL, uerr := secureurl.PublicFetchURL(s.cfg.Load().MediaFetchDomain, leaseID, token)
	if uerr != nil {
		// An unconfigured or `api.`-prefixed media host cannot serve the upstream, and the failure
		// would be invisible on the wire — refuse locally with nothing spent.
		// 未配置或 `api.` 前缀的媒体主机服务不了上游,而那种失败在线缆上**看不见**——本地拒绝,一分
		// 钱不花。
		s.rollbackReservation(ctx, reservation)
		return domvoice.Voice{}, apierr.ErrVoiceUnavailable
	}

	upstreamID, unbilled, err := s.upstream.EnrollVoice(ctx, sampleURL)
	if err != nil {
		if unbilled {
			if rbErr := s.quota.Rollback(ctx, reservation); rbErr != nil {
				s.log.RollbackFailure()
			}
		} else if stErr := s.quota.Settle(ctx, reservation, reservation.ReservedPUSD); stErr != nil {
			s.log.SettleFailure()
		}
		var ae *apierr.APIError
		if errors.As(err, &ae) {
			return domvoice.Voice{}, ae
		}
		return domvoice.Voice{}, apierr.ErrUpstreamError
	}
	// Settle BEFORE the record write, and at the full quote. The price is fixed, so there is no
	// usage to wait for — and everything after this point is OUR bookkeeping, which cannot un-spend
	// money the provider already took. Deleting the registration on a failed write reclaims the
	// SLOT, never the fee.
	// **在记录写之前**结算,且按全额报价。价格固定,没有用量可等——而这一点之后的一切都是**我们自己的**
	// 记账,它退不掉上游已经收走的钱。写失败时删掉那份登记,收回的是**位置**、从来不是那笔费用。
	if err := s.quota.Settle(ctx, reservation, reservation.ReservedPUSD); err != nil {
		s.log.SettleFailure()
	}

	// **Spend the capability the moment the upstream is done with it.** The fetch URL is a bearer
	// token: anyone holding it can download the user's voice until the lease TTL runs out. The
	// upstream fetches exactly once, so every second of validity after that is a window with no
	// remaining purpose. Revoking beats shortening a TTL — it does not depend on a clock.
	// **上游一用完就把那个 capability 花掉。** 取回 URL 是持有型凭据:拿到它的人在 lease TTL 走完
	// 之前都能下载用户的声音。上游**恰好取一次**,故那之后每一秒的有效期都是一扇没有剩余用途的窗。
	// **撤销胜过缩短 TTL**——它不依赖时钟。
	//
	// A failed revoke does not fail the enrollment: the voice is real and paid for, and the lease
	// still expires on its own. 撤销失败**不**让登记失败:音色是真的、已付费,而 lease 自己也会过期。
	awaitErr := s.upstream.AwaitVoiceReady(ctx, upstreamID)
	if rerr := s.samples.RevokeSample(ctx, got, leaseID); rerr != nil {
		s.log.SampleNotRevoked(leaseID)
	}
	if awaitErr != nil {
		// Out of patience, not out of voice — the registration exists and is paid for. Record it
		// anyway so the user owns what they bought; it becomes usable when the provider finishes.
		// 是我们没耐心了,不是音色没了——那份登记存在且已付费。**照样记下来**,使用户拥有他买到的
		// 东西;上游做完之后它就能用了。
		s.log.VoiceSlowToDeploy(upstreamID)
	}
	v := domvoice.Voice{ID: s.ids.New(), Name: name, UpstreamID: upstreamID, CreatedAt: s.clock.Now().UTC()}
	if err := s.store.CreateVoice(ctx, got, v); err != nil {
		// Roll the paid registration back whichever way the write failed — a lost race is still a
		// registration nobody will ever address. Then answer with the SAME code the pre-check would
		// have used, because "you lost a race" and "you arrived late" are one fact to the caller.
		// 无论写失败于哪一种,都**回滚**那次已付费的登记——输掉的竞态同样是一个再没人寻址得到的登记。
		// 然后用与前置检查**相同**的码作答:对调用方而言,「你输了竞态」与「你来晚了」是同一个事实。
		if delErr := s.upstream.DeleteVoice(ctx, upstreamID); delErr != nil {
			// Both halves failed: the voice is now an orphan in our account. This is the one
			// outcome an operator must know about, because nothing automatic can clean it up.
			// 两半都失败了:该音色现在是我们账号里的一个孤儿。这是运营者**必须**知道的那一种结局,
			// 因为没有任何自动机制清得掉它。
			s.log.VoiceOrphaned(upstreamID)
		}
		switch {
		case errors.Is(err, domvoice.ErrInventoryFull):
			return domvoice.Voice{}, apierr.ErrVoiceInventoryFull
		case errors.Is(err, domvoice.ErrNameTaken):
			return domvoice.Voice{}, apierr.ErrVoiceNameTaken
		}
		return domvoice.Voice{}, apierr.Internal()
	}
	return v, nil
}

// List returns this install's voices plus the inventory arithmetic.
//
// List 返回本 install 的音色 + 库存算术。
func (s *Service) List(ctx context.Context, installID string) ([]domvoice.Voice, *apierr.APIError) {
	if s == nil || s.store == nil {
		return nil, apierr.Internal()
	}
	got, ae := s.authorize(ctx, installID)
	if ae != nil {
		return nil, ae
	}
	out, err := s.store.ListVoices(ctx, got)
	if err != nil {
		return nil, apierr.Internal()
	}
	return out, nil
}

// Delete removes the upstream registration and then the record. Upstream first: the record is the
// only thing holding the upstream id, so dropping it first strands a voice that keeps consuming
// the shared ceiling forever.
//
// Delete 先删上游登记、再删记录。**上游在先**:记录是唯一持有上游 id 的东西,先丢掉它会让一个音色搁浅,
// 而它会永远占着那份共享上限。
func (s *Service) Delete(ctx context.Context, installID, id string) *apierr.APIError {
	if s == nil || s.store == nil || s.upstream == nil {
		return apierr.Internal()
	}
	got, ae := s.authorize(ctx, installID)
	if ae != nil {
		return ae
	}
	mine, err := s.store.ListVoices(ctx, got)
	if err != nil {
		return apierr.Internal()
	}
	var target *domvoice.Voice
	for i := range mine {
		if mine[i].ID == id {
			target = &mine[i]
			break
		}
	}
	if target == nil {
		return apierr.ErrVoiceNotFound
	}
	if err := s.upstream.DeleteVoice(ctx, target.UpstreamID); err != nil {
		// Abort with the record intact: the caller can retry, and the inventory count keeps telling
		// the truth. "Succeeding" here would leave a paid registration alive and invisible.
		// **保留记录并中止**:调用方可重试、库存计数继续说真话。在这里「成功」会留下一个还活着、
		// 且不可见的已付费登记。
		return apierr.ErrUpstreamError
	}
	if _, _, err := s.store.DeleteVoice(ctx, got, id); err != nil {
		return apierr.Internal()
	}
	return nil
}

// rollbackReservation reverses a charge for work that provably never reached the provider.
//
// rollbackReservation 为一次**可证明从未抵达上游**的工作反转计费。
func (s *Service) rollbackReservation(ctx context.Context, r *domquota.Reservation) {
	if err := s.quota.Rollback(ctx, r); err != nil {
		s.log.RollbackFailure()
	}
}

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

type noopLogger struct{}

func (noopLogger) AccountVoiceCeilingReached(int, int) {}
func (noopLogger) VoiceOrphaned(string)                {}
func (noopLogger) SampleNotRevoked(string)             {}
func (noopLogger) VoiceSlowToDeploy(string)            {}
func (noopLogger) SettleFailure()                      {}
func (noopLogger) RollbackFailure()                    {}
