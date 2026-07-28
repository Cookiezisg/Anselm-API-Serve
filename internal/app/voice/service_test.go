package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

// The voice service's correctness is about ORDER and ROLLBACK, because the thing it creates lives
// on someone else's servers and costs money to make. Every test here asserts one of the two
// outcomes that cost something real: a paid call made after we already knew it would be refused,
// and an upstream registration nobody can ever address again.
//
// 本服务的正确性在于**顺序**与**回滚**,因为它创建的东西住在别人的服务器上、且造出来要花钱。这里每个
// 测试断言的都是**真有代价**的那两种结局之一:在**已经知道会被拒**之后才发出的那次付费调用,以及一份
// 再没有人寻址得到的上游登记。

type fakeAuth struct {
	status dominstall.Status
	absent bool
}

func (f fakeAuth) LookupInstall(_ context.Context, id string) (string, dominstall.Status, bool, error) {
	if f.status == "" {
		f.status = dominstall.StatusActive
	}
	if f.absent {
		return "", dominstall.StatusActive, false, nil
	}
	return id, f.status, true, nil
}

type fakeCfg struct{ c config.Config }

func (f *fakeCfg) Load() *config.Config { return &f.c }

type fakeStore struct {
	rows      map[string][]domvoice.Voice
	total     int
	createErr error
	created   int
}

func newStore() *fakeStore { return &fakeStore{rows: map[string][]domvoice.Voice{}} }

func (f *fakeStore) ListVoices(_ context.Context, id string) ([]domvoice.Voice, error) {
	return f.rows[id], nil
}
func (f *fakeStore) CountAllVoices(context.Context) (int, error) { return f.total, nil }
func (f *fakeStore) CreateVoice(_ context.Context, id string, v domvoice.Voice) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created++
	f.rows[id] = append(f.rows[id], v)
	return nil
}
func (f *fakeStore) DeleteVoice(_ context.Context, id, vid string) (string, bool, error) {
	for i, v := range f.rows[id] {
		if v.ID == vid {
			f.rows[id] = append(f.rows[id][:i], f.rows[id][i+1:]...)
			return v.UpstreamID, true, nil
		}
	}
	return "", false, nil
}

type fakeUpstream struct {
	enrolled     int
	deleted      []string
	enrollErr    error
	enrollUnbill bool
	deleteErr    error
}

func (f *fakeUpstream) EnrollVoice(context.Context, string, string) (string, bool, error) {
	if f.enrollErr != nil {
		return "", f.enrollUnbill, f.enrollErr
	}
	f.enrolled++
	return "upstream-voice-1", false, nil
}
func (f *fakeUpstream) DeleteVoice(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

type seqIDs struct{ n int }

func (s *seqIDs) New() string { s.n++; return "vce_test" }

type capturingLog struct {
	ceilingHits int
	orphans     []string
	settleFails int
	rbFails     int
}

func (l *capturingLog) AccountVoiceCeilingReached(int, int) { l.ceilingHits++ }
func (l *capturingLog) VoiceOrphaned(id string)             { l.orphans = append(l.orphans, id) }
func (l *capturingLog) SettleFailure()                      { l.settleFails++ }
func (l *capturingLog) RollbackFailure()                    { l.rbFails++ }

// fakeQuota records what the wallet was asked to do. The assertions care about the SEQUENCE, not
// the arithmetic: reserving after the money moved, or rolling back a charge the provider kept, are
// the two ways this capability can lie about the operator's balance.
//
// fakeQuota 记下钱包被要求做了什么。断言在意的是**次序**、不是算术:在钱动了之后才预留,或者回滚一笔
// 上游已经收走的钱,是这个能力能对 operator 余额撒谎的那两种方式。
type fakeQuota struct {
	reserved   int
	settled    []int64
	rolledBack int
	reserveErr error
	settleErr  error
	rbErr      error
}

func (f *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-07", Day: "2026-07-28"}
}

func (f *fakeQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	f.reserved++
	return &domquota.Reservation{
		RequestID: "req_1", InstallID: installID, Period: p, Plan: plan,
		ReservedPUSD: 200_000_000_000,
	}, nil
}

func (f *fakeQuota) Settle(_ context.Context, _ *domquota.Reservation, actual int64) error {
	if f.settleErr != nil {
		return f.settleErr
	}
	f.settled = append(f.settled, actual)
	return nil
}

func (f *fakeQuota) Rollback(context.Context, *domquota.Reservation) error {
	if f.rbErr != nil {
		return f.rbErr
	}
	f.rolledBack++
	return nil
}

const sample = "data:audio/wav;base64,UklGRg=="

func newSvc(t *testing.T, store *fakeStore, up *fakeUpstream, ceiling int64) (*Service, *capturingLog, *fakeQuota) {
	t.Helper()
	lg := &capturingLog{}
	q := &fakeQuota{}
	return New(Deps{
		Auth:   fakeAuth{},
		Quota:  q,
		Config: &fakeCfg{c: config.Config{SpeechEnabled: true, QwenAPIKeys: []string{"k"}, VoiceAccountCeiling: ceiling}},
		Store:  store, Upstream: up, Clock: fixedClock{}, IDs: &seqIDs{}, Log: lg,
	}), lg, q
}

// TestEnroll_InventoryFullBeforeSpending: the per-install cap is checked BEFORE the paid upstream
// call. Calling first and refusing after would charge for a voice the caller cannot keep.
//
// TestEnroll_InventoryFullBeforeSpending:逐 install 上限在**付费上游调用之前**查。先调再拒,等于
// 为一个调用方留不住的音色收了钱。
func TestEnroll_InventoryFullBeforeSpending(t *testing.T) {
	st := newStore()
	for i := 0; i < domvoice.PerInstallInventory; i++ {
		st.rows["ins_1"] = append(st.rows["ins_1"], domvoice.Voice{ID: string(rune('a' + i)), Name: string(rune('a' + i))})
	}
	up := &fakeUpstream{}
	svc, _, _ := newSvc(t, st, up, 0)
	_, ae := svc.Enroll(context.Background(), "ins_1", "another", sample)
	if ae != apierr.ErrVoiceInventoryFull {
		t.Fatalf("ae = %v, want ErrVoiceInventoryFull", ae)
	}
	if up.enrolled != 0 {
		t.Fatalf("the paid call ran %d times before a refusal we already knew about", up.enrolled)
	}
}

// TestEnroll_DuplicateNameBeforeSpending: same discipline, other precondition — enrolling over a
// name would strand the first registration upstream.
//
// TestEnroll_DuplicateNameBeforeSpending:同一条纪律、另一个前置条件——覆盖一个名字会让第一个登记
// 在上游搁浅。
func TestEnroll_DuplicateNameBeforeSpending(t *testing.T) {
	st := newStore()
	st.rows["ins_1"] = []domvoice.Voice{{ID: "v1", Name: "narrator"}}
	up := &fakeUpstream{}
	svc, _, _ := newSvc(t, st, up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae != apierr.ErrVoiceNameTaken {
		t.Fatalf("ae = %v, want ErrVoiceNameTaken", ae)
	}
	if up.enrolled != 0 {
		t.Fatalf("paid before checking the name: %d calls", up.enrolled)
	}
}

// TestEnroll_AccountCeilingRefusesAndLogs: when OUR shared ceiling is the full one, the answer is a
// refusal plus a loud log — never an eviction. The person whose voice would be evicted did nothing
// and would never learn why it vanished.
//
// TestEnroll_AccountCeilingRefusesAndLogs:当满的是**我们**那条共享上限时,答案是拒绝 + 大声记日志
// ——**绝不是驱逐**。被驱逐的那个人什么也没做,而且永远不会知道它为什么消失了。
func TestEnroll_AccountCeilingRefusesAndLogs(t *testing.T) {
	st := newStore()
	st.total = 5
	up := &fakeUpstream{}
	svc, lg, _ := newSvc(t, st, up, 5)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae != apierr.ErrVoiceCapacityReached {
		t.Fatalf("ae = %v, want ErrVoiceCapacityReached", ae)
	}
	if up.enrolled != 0 {
		t.Fatalf("paid past our own ceiling: %d calls", up.enrolled)
	}
	if lg.ceilingHits != 1 {
		t.Fatalf("the operator must be told the shared account is full; got %d signals", lg.ceilingHits)
	}
}

// TestEnroll_ZeroCeilingIsUnenforced: 0 means unenforced, not "no voices allowed" — the provider's
// real ceiling is undocumented, so the default must not block the feature outright.
//
// TestEnroll_ZeroCeilingIsUnenforced:0 是**不强制**、不是「一个也不许」——provider 的真实上限没有
// 文档,故默认值绝不能把整个功能堵死。
func TestEnroll_ZeroCeilingIsUnenforced(t *testing.T) {
	st := newStore()
	st.total = 9999
	svc, _, _ := newSvc(t, st, &fakeUpstream{}, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae != nil {
		t.Fatalf("ae = %v, want success with the ceiling unset", ae)
	}
}

// TestEnroll_AddressShapedSampleIsRefused: ADR 0011 — a managed media input that carries an address
// is an SSRF primitive aimed at our own network. The shape IS the control.
//
// TestEnroll_AddressShapedSampleIsRefused:ADR 0011——携带地址的受管媒体输入是一枚指向我们自己网络
// 的 SSRF 原语。**形状本身就是那道控制**。
func TestEnroll_AddressShapedSampleIsRefused(t *testing.T) {
	for _, bad := range []string{
		"https://internal.example/clip.wav",
		"http://169.254.169.254/latest/meta-data/",
		"data:audio/wav;base64,x://y",
	} {
		up := &fakeUpstream{}
		svc, _, _ := newSvc(t, newStore(), up, 0)
		if _, ae := svc.Enroll(context.Background(), "ins_1", "n", bad); ae != apierr.ErrVoiceSampleInvalid {
			t.Fatalf("sample %q: ae = %v, want ErrVoiceSampleInvalid", bad, ae)
		}
		if up.enrolled != 0 {
			t.Fatalf("sample %q reached the upstream", bad)
		}
	}
}

// TestEnroll_RecordFailureRollsBackUpstream: if the record cannot be written, the paid registration
// is deleted rather than abandoned — an abandoned one occupies the shared ceiling forever and no
// later call can name it.
//
// TestEnroll_RecordFailureRollsBackUpstream:记录写不下来时,那份已付费的登记被**删掉**而不是被丢下
// ——丢下的那个会永远占着共享上限,而此后没有任何调用叫得出它的名字。
func TestEnroll_RecordFailureRollsBackUpstream(t *testing.T) {
	st := newStore()
	st.createErr = errors.New("disk on fire")
	up := &fakeUpstream{}
	svc, lg, _ := newSvc(t, st, up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae == nil {
		t.Fatal("a failed record must not report success")
	}
	if len(up.deleted) != 1 || up.deleted[0] != "upstream-voice-1" {
		t.Fatalf("the paid registration was not rolled back: %v", up.deleted)
	}
	if len(lg.orphans) != 0 {
		t.Fatalf("a successful rollback is not an orphan: %v", lg.orphans)
	}
}

// TestEnroll_RollbackFailureIsReportedAsAnOrphan: both halves failed, so a registration now exists
// that nothing automatic can reach. That is the one outcome an operator must be told about.
//
// TestEnroll_RollbackFailureIsReportedAsAnOrphan:两半都失败了,故现在存在一份自动机制够不着的登记。
// 那是运营者**必须**被告知的那一种结局。
func TestEnroll_RollbackFailureIsReportedAsAnOrphan(t *testing.T) {
	st := newStore()
	st.createErr = errors.New("disk on fire")
	up := &fakeUpstream{deleteErr: errors.New("upstream down too")}
	svc, lg, _ := newSvc(t, st, up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae == nil {
		t.Fatal("a failed record must not report success")
	}
	if len(lg.orphans) != 1 || lg.orphans[0] != "upstream-voice-1" {
		t.Fatalf("the orphan was not reported: %v", lg.orphans)
	}
}

// TestEnroll_LostRaceAnswersLikeAPreCheck: the store's transaction catches what the pre-check
// cannot see. The caller gets the SAME code either way — "you lost a race" and "you arrived late"
// are one fact about the world, and a second code would only invite a retry loop.
//
// TestEnroll_LostRaceAnswersLikeAPreCheck:store 的事务接住前置检查看不见的东西。**两条路给同一个码**
// ——「你输了竞态」与「你来晚了」是关于世界的同一个事实,而第二个码只会招来一个重试循环。
func TestEnroll_LostRaceAnswersLikeAPreCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want *apierr.APIError
	}{
		{"inventory", domvoice.ErrInventoryFull, apierr.ErrVoiceInventoryFull},
		{"name", domvoice.ErrNameTaken, apierr.ErrVoiceNameTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.createErr = tc.err
			up := &fakeUpstream{}
			svc, _, _ := newSvc(t, st, up, 0)
			_, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample)
			if ae != tc.want {
				t.Fatalf("ae = %v, want %v", ae, tc.want)
			}
			// Losing the race still spent money; that registration must not be left behind.
			// 输掉竞态**照样花了钱**;那份登记绝不能被留下。
			if len(up.deleted) != 1 {
				t.Fatalf("a lost race left the paid registration upstream: %v", up.deleted)
			}
		})
	}
}

// TestDelete_UpstreamFirst: the record is the only thing holding the upstream id, so an upstream
// failure must ABORT with the record intact. "Succeeding" here leaves a paid registration alive and
// permanently invisible.
//
// TestDelete_UpstreamFirst:记录是唯一持有上游 id 的东西,故上游失败必须**保留记录并中止**。在这里
// 「成功」,会留下一份还活着、且永久不可见的已付费登记。
func TestDelete_UpstreamFirst(t *testing.T) {
	st := newStore()
	st.rows["ins_1"] = []domvoice.Voice{{ID: "vce_1", Name: "n", UpstreamID: "u1"}}
	up := &fakeUpstream{deleteErr: errors.New("upstream down")}
	svc, _, _ := newSvc(t, st, up, 0)
	if ae := svc.Delete(context.Background(), "ins_1", "vce_1"); ae != apierr.ErrUpstreamError {
		t.Fatalf("ae = %v, want ErrUpstreamError", ae)
	}
	if len(st.rows["ins_1"]) != 1 {
		t.Fatal("the record was dropped even though the upstream registration survives")
	}
}

// TestDelete_OtherInstallsVoiceIsAbsent: ownership enforcement — another install's id must read as
// absent so voice ids never become an existence oracle.
//
// TestDelete_OtherInstallsVoiceIsAbsent:强制归属——别的 install 的 id 必须读作**不存在**,故音色 id
// 永远不会变成一个存在性预言机。
func TestDelete_OtherInstallsVoiceIsAbsent(t *testing.T) {
	st := newStore()
	st.rows["ins_other"] = []domvoice.Voice{{ID: "vce_1", UpstreamID: "u1"}}
	up := &fakeUpstream{}
	svc, _, _ := newSvc(t, st, up, 0)
	if ae := svc.Delete(context.Background(), "ins_1", "vce_1"); ae != apierr.ErrVoiceNotFound {
		t.Fatalf("ae = %v, want ErrVoiceNotFound", ae)
	}
	if len(up.deleted) != 0 {
		t.Fatal("another install's registration was deleted upstream")
	}
}

// TestUnavailableWhenSpeechIsOff: cloning rides the speech capability — a deployment that cannot
// speak has no use for a voice, and must say so rather than fail at the upstream.
//
// TestUnavailableWhenSpeechIsOff:克隆搭在语音能力上——说不了话的部署要音色没有用,且必须**这么说**,
// 而不是到上游那里才失败。
func TestUnavailableWhenSpeechIsOff(t *testing.T) {
	svc := New(Deps{
		Auth:   fakeAuth{},
		Config: &fakeCfg{c: config.Config{SpeechEnabled: false, QwenAPIKeys: []string{"k"}}},
		Quota:  &fakeQuota{},
		Store:  newStore(), Upstream: &fakeUpstream{}, Clock: fixedClock{}, IDs: &seqIDs{},
	})
	if svc.Available() {
		t.Fatal("Available() must be false when speech is off")
	}
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae != apierr.ErrVoiceUnavailable {
		t.Fatalf("ae = %v, want ErrVoiceUnavailable", ae)
	}
}

// TestBannedInstallIsRefused: the ban check runs before any inventory read or spend.
//
// TestBannedInstallIsRefused:封禁检查在任何库存读取或花钱之前跑。
func TestBannedInstallIsRefused(t *testing.T) {
	up := &fakeUpstream{}
	svc := New(Deps{
		Auth:   fakeAuth{status: dominstall.StatusBanned},
		Config: &fakeCfg{c: config.Config{SpeechEnabled: true, QwenAPIKeys: []string{"k"}}},
		Quota:  &fakeQuota{},
		Store:  newStore(), Upstream: up, Clock: fixedClock{}, IDs: &seqIDs{},
	})
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae != apierr.ErrAccountBanned {
		t.Fatalf("ae = %v, want ErrAccountBanned", ae)
	}
	if up.enrolled != 0 {
		t.Fatal("a banned install reached the paid upstream")
	}
}

// --- the wallet: what "tests green" could not see before ---------------------
//
// Every test above this line passed while the pUSD ledger was not wired at all. That is the point
// of this block: inventory ceilings answer "how many may I hold", and the answer to "how much may
// this cost" was, until these assertions existed, "no limit".
//
// 这条线以上的每个测试,在 pUSD 账本**完全没接**的时候都是绿的。这一组的意义正在于此:库存上限回答的是
// 「我能持有几个」,而「这总共可以花多少钱」的答案,在这些断言存在之前,是**没有上限**。

// TestEnroll_ReservesBeforeSpendingAndSettlesTheFullQuote: the wallet is the last gate before the
// money moves, and the price is fixed — so a success reserves once and settles exactly the quote.
//
// TestEnroll_ReservesBeforeSpendingAndSettlesTheFullQuote:钱包是钱动之前的最后一道闸,而价格固定
// ——故一次成功恰好预留一次、并按报价原额结算。
func TestEnroll_ReservesBeforeSpendingAndSettlesTheFullQuote(t *testing.T) {
	up := &fakeUpstream{}
	svc, _, q := newSvc(t, newStore(), up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "narrator", sample); ae != nil {
		t.Fatalf("ae = %v", ae)
	}
	if q.reserved != 1 || up.enrolled != 1 {
		t.Fatalf("reserved=%d enrolled=%d; want one of each", q.reserved, up.enrolled)
	}
	if len(q.settled) != 1 || q.settled[0] != 200_000_000_000 {
		t.Fatalf("settled = %v; a fixed-price purchase must settle the full quote", q.settled)
	}
	if q.rolledBack != 0 {
		t.Fatalf("a success rolled back the charge %d times", q.rolledBack)
	}
}

// TestEnroll_LocalRefusalsNeverTouchTheWallet: a request refused before the upstream call must not
// consume a daily unit — otherwise a user could exhaust tomorrow's allowance by typing a name that
// is already taken.
//
// TestEnroll_LocalRefusalsNeverTouchTheWallet:在上游调用之前被拒的请求绝不能消耗一个日 unit——否则
// 用户只要输一个已被占用的名字,就能把明天的额度耗掉。
func TestEnroll_LocalRefusalsNeverTouchTheWallet(t *testing.T) {
	full := newStore()
	for i := 0; i < domvoice.PerInstallInventory; i++ {
		full.rows["ins_1"] = append(full.rows["ins_1"], domvoice.Voice{ID: string(rune('a' + i)), Name: string(rune('a' + i))})
	}
	for _, tc := range []struct {
		name  string
		store *fakeStore
		vname string
		samp  string
	}{
		{"inventory full", full, "another", sample},
		{"bad sample shape", newStore(), "n", "https://internal.example/clip.wav"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, q := newSvc(t, tc.store, &fakeUpstream{}, 0)
			if _, ae := svc.Enroll(context.Background(), "ins_1", tc.vname, tc.samp); ae == nil {
				t.Fatal("expected a refusal")
			}
			if q.reserved != 0 {
				t.Fatalf("a local refusal consumed %d reservations", q.reserved)
			}
		})
	}
}

// TestEnroll_ProvablyUnbilledRejectionRollsBackTheCharge: GW-INV-50's admitting half — when the
// provider refuses BEFORE creating anything, the reservation is reversed so the wallet does not
// carry a charge nobody made.
//
// TestEnroll_ProvablyUnbilledRejectionRollsBackTheCharge:GW-INV-50 的**放行**那一半——上游在创建
// **之前**就拒绝时,预留被反转,使钱包不背一笔谁也没花的账。
func TestEnroll_ProvablyUnbilledRejectionRollsBackTheCharge(t *testing.T) {
	up := &fakeUpstream{enrollErr: apierr.UpstreamRejected(apierr.RejectedInvalid), enrollUnbill: true}
	svc, _, q := newSvc(t, newStore(), up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae == nil {
		t.Fatal("expected a refusal")
	}
	if q.rolledBack != 1 || len(q.settled) != 0 {
		t.Fatalf("rolledBack=%d settled=%v; a provably-unbilled rejection must reverse the charge",
			q.rolledBack, q.settled)
	}
}

// TestEnroll_AmbiguousUpstreamFailureKeepsTheCharge: GW-INV-50's refusing half — a timeout or a
// transport failure may well have created a voice we cannot see. Refunding here would let a
// systematic failure mode become free money spent on our card.
//
// TestEnroll_AmbiguousUpstreamFailureKeepsTheCharge:GW-INV-50 的**拒绝**那一半——超时或传输失败很可能
// 已经创建了一个我们看不见的音色。在这里退款,会让一种系统性失败变成「用我们的卡免费花钱」。
func TestEnroll_AmbiguousUpstreamFailureKeepsTheCharge(t *testing.T) {
	up := &fakeUpstream{enrollErr: apierr.ErrUpstreamTimeout, enrollUnbill: false}
	svc, _, q := newSvc(t, newStore(), up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae == nil {
		t.Fatal("expected a refusal")
	}
	if q.rolledBack != 0 {
		t.Fatal("an ambiguous outcome refunded the charge")
	}
	if len(q.settled) != 1 || q.settled[0] != 200_000_000_000 {
		t.Fatalf("settled = %v; an ambiguous outcome keeps the FULL quote", q.settled)
	}
}

// TestEnroll_RecordFailureKeepsTheChargeAndReclaimsOnlyTheSlot: our own bookkeeping failing cannot
// un-spend the provider's money. Deleting the registration reclaims the inventory SLOT; the fee
// stays settled, because it really was paid.
//
// TestEnroll_RecordFailureKeepsTheChargeAndReclaimsOnlyTheSlot:**我们自己**记账失败,退不掉上游已经
// 收走的钱。删掉那份登记收回的是库存**位置**;费用照样结算,因为它真的付了。
func TestEnroll_RecordFailureKeepsTheChargeAndReclaimsOnlyTheSlot(t *testing.T) {
	st := newStore()
	st.createErr = errors.New("disk on fire")
	up := &fakeUpstream{}
	svc, _, q := newSvc(t, st, up, 0)
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae == nil {
		t.Fatal("expected a failure")
	}
	if len(up.deleted) != 1 {
		t.Fatalf("the slot was not reclaimed: %v", up.deleted)
	}
	if q.rolledBack != 0 {
		t.Fatal("our own write failure refunded money the provider had already taken")
	}
	if len(q.settled) != 1 {
		t.Fatalf("settled = %v; the fee was really paid and must stay on the books", q.settled)
	}
}

// TestEnroll_QuotaDenialSurfacesItsOwnCode: the daily gate and the inventory ceiling are different
// refusals with different remedies. VOICE_QUOTA_EXHAUSTED resets tomorrow; VOICE_INVENTORY_FULL
// never does, so collapsing them would send a user to wait for a slot that will never open.
//
// TestEnroll_QuotaDenialSurfacesItsOwnCode:日闸与库存上限是**两条不同的拒绝、两种不同的补救**。
// VOICE_QUOTA_EXHAUSTED 明天会重置;VOICE_INVENTORY_FULL 永远不会,故合并它们会让用户去等一个永远不会
// 开的位置。
func TestEnroll_QuotaDenialSurfacesItsOwnCode(t *testing.T) {
	up := &fakeUpstream{}
	lg := &capturingLog{}
	svc := New(Deps{
		Auth:   fakeAuth{},
		Quota:  &fakeQuota{reserveErr: apierr.ErrVoiceQuotaExhausted},
		Config: &fakeCfg{c: config.Config{SpeechEnabled: true, QwenAPIKeys: []string{"k"}}},
		Store:  newStore(), Upstream: up, Clock: fixedClock{}, IDs: &seqIDs{}, Log: lg,
	})
	_, ae := svc.Enroll(context.Background(), "ins_1", "n", sample)
	if ae != apierr.ErrVoiceQuotaExhausted {
		t.Fatalf("ae = %v, want ErrVoiceQuotaExhausted", ae)
	}
	if up.enrolled != 0 {
		t.Fatal("a wallet denial still reached the paid upstream")
	}
}

// TestEnroll_UnclosedBooksAreObservable: a settle that fails leaves the operator's wallet
// under-reporting until the orphan scanner finalizes it. Silence here is how a money bug hides.
//
// TestEnroll_UnclosedBooksAreObservable:结算失败会让 operator 钱包在孤儿扫描收口之前一直**少报**。
// 在这里保持沉默,正是一个钱的 bug 藏起来的方式。
func TestEnroll_UnclosedBooksAreObservable(t *testing.T) {
	lg := &capturingLog{}
	svc := New(Deps{
		Auth:   fakeAuth{},
		Quota:  &fakeQuota{settleErr: errors.New("wallet down")},
		Config: &fakeCfg{c: config.Config{SpeechEnabled: true, QwenAPIKeys: []string{"k"}}},
		Store:  newStore(), Upstream: &fakeUpstream{}, Clock: fixedClock{}, IDs: &seqIDs{}, Log: lg,
	})
	if _, ae := svc.Enroll(context.Background(), "ins_1", "n", sample); ae != nil {
		t.Fatalf("a failed settle must not fail the enrollment the user already paid for: %v", ae)
	}
	if lg.settleFails != 1 {
		t.Fatalf("settleFails = %d; an unclosed book must be observable", lg.settleFails)
	}
}
