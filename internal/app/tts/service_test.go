package tts

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

type fakeAuth struct{ status dominstall.Status }

func (f fakeAuth) LookupInstall(_ context.Context, id string) (string, dominstall.Status, bool, error) {
	if id == "" {
		return "", "", false, nil
	}
	return id, f.status, true, nil
}

type fakeCfg struct{ c config.Config }

func (f *fakeCfg) Load() *config.Config { return &f.c }

type fakeQuota struct {
	reserveErr  error
	settled     []int64
	rolledBack  int
	reservation *domquota.Reservation
}

func (f *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-07", Day: "2026-07-27"}
}

func (f *fakeQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	f.reservation = &domquota.Reservation{
		RequestID: "req_t", InstallID: installID, Period: p, Plan: plan,
		ReservedPUSD:    plan.ReservedPUSD,
		CategoryApplied: domquota.CategorySpeech, CategoryUnits: plan.PromptQuote,
	}
	return f.reservation, nil
}

func (f *fakeQuota) Settle(_ context.Context, _ *domquota.Reservation, actual int64) error {
	f.settled = append(f.settled, actual)
	return nil
}

func (f *fakeQuota) Rollback(context.Context, *domquota.Reservation) error {
	f.rolledBack++
	return nil
}

// wantAudio stands in for a synthesized utterance. It is BYTES because the upstream is a duplex
// WebSocket that streams frames — there is no artifact URL to assert on any more (H9).
// wantAudio 代表一段合成出来的话。它是**字节**,因为上游是一条流帧的双工 WebSocket——已经没有产物
// URL 可供断言了(H9)。
var wantAudio = []byte("RIFF....WAVEfake-pcm")

type fakeUpstream struct {
	audio    []byte
	unbilled bool
	err      error
	gotText  string
	gotVoice string
}

func (f *fakeUpstream) GenerateSpeech(_ context.Context, _, text, voice string) ([]byte, bool, error) {
	f.gotText, f.gotVoice = text, voice
	return f.audio, f.unbilled, f.err
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_800_000_000, 0) }

func enabledCfg() *fakeCfg {
	return &fakeCfg{c: config.Config{
		SpeechEnabled:    true,
		TTSUpstreamModel: billing.QwenAudio30TTSFlash,
		TTSDefaultVoice:  "longanhuan_v3.6",
		QwenAPIKeys:      []string{"sk-test"},
	}}
}

func newSvc(cfg *fakeCfg, q *fakeQuota, up Upstream) *Service {
	return New(Deps{Auth: fakeAuth{status: dominstall.StatusActive}, Quota: q, Config: cfg, Upstream: up, Clock: fixedClock{}})
}

// TestSynthesize_SuccessSettlesDeterministicCost: the happy path settles exactly the frozen
// per-character cost (reserve == settle) and relays the upstream URL untouched (P13).
func TestSynthesize_SuccessSettlesDeterministicCost(t *testing.T) {
	q := &fakeQuota{}
	up := &fakeUpstream{audio: wantAudio}
	svc := newSvc(enabledCfg(), q, up)
	u, ae := svc.Synthesize(context.Background(), "ins_1", "hello there", "")
	if ae != nil || string(u) != string(wantAudio) {
		t.Fatalf("synthesize = %q, %v", u, ae)
	}
	if len(q.settled) != 1 || q.settled[0] != q.reservation.ReservedPUSD {
		t.Fatalf("settled = %v, want exactly the reserved amount", q.settled)
	}
	if q.rolledBack != 0 {
		t.Fatalf("unexpected rollback")
	}
}

// TestSynthesize_BillsRunesNotBytes: the whole point of a per-character price is that one CJK
// character costs one character. A byte count would charge it three, silently tripling every
// Chinese request — the single most likely and least visible pricing bug in this path.
//
// 字符计价的全部意义就是一个汉字算一个字符。按字节会算成三个,把每一条中文请求静默乘三——这条
// 路上最可能发生、也最不容易被看见的计费错误。
func TestSynthesize_BillsRunesNotBytes(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{audio: wantAudio})
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "你好世界", ""); ae != nil {
		t.Fatalf("synthesize: %v", ae)
	}
	if got := q.reservation.CategoryUnits; got != 4 {
		t.Fatalf("charged %d units for 4 CJK characters (12 bytes) — want 4", got)
	}
}

// TestSynthesize_DefaultVoiceFillsIn: an omitted voice becomes the configured default rather
// than an empty string on the wire (P10: the parameter stays, the picker does not).
func TestSynthesize_DefaultVoiceFillsIn(t *testing.T) {
	up := &fakeUpstream{audio: wantAudio}
	svc := newSvc(enabledCfg(), &fakeQuota{}, up)
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "hi", "  "); ae != nil {
		t.Fatalf("synthesize: %v", ae)
	}
	if up.gotVoice != "longanhuan_v3.6" {
		t.Fatalf("upstream got voice %q, want the configured default", up.gotVoice)
	}
	// An explicit voice is passed through untouched. 显式音色原样透传。
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "hi", "Dylan"); ae != nil {
		t.Fatalf("synthesize: %v", ae)
	}
	if up.gotVoice != "Dylan" {
		t.Fatalf("upstream got voice %q, want the explicit one", up.gotVoice)
	}
}

// TestSynthesize_UnavailableIsWholePath: the double-half rule — capability off OR missing
// credential each independently yield TTS_UNAVAILABLE before any reservation, and the code is
// NOT the ASR one (SPEECH_UNAVAILABLE answers a different question).
func TestSynthesize_UnavailableIsWholePath(t *testing.T) {
	off := enabledCfg()
	off.c.SpeechEnabled = false
	noKey := enabledCfg()
	noKey.c.QwenAPIKeys = nil

	for name, cfg := range map[string]*fakeCfg{"capabilityOff": off, "noCredential": noKey} {
		q := &fakeQuota{}
		svc := newSvc(cfg, q, &fakeUpstream{audio: wantAudio})
		_, ae := svc.Synthesize(context.Background(), "ins_1", "hi", "")
		if ae == nil || ae.Code != "TTS_UNAVAILABLE" {
			t.Fatalf("%s: err = %v, want TTS_UNAVAILABLE", name, ae)
		}
		if q.reservation != nil {
			t.Fatalf("%s: reserved despite an unavailable path", name)
		}
	}
}

// TestSynthesize_UnbilledRejectionRollsBack: a provably-unbilled upstream rejection frees the
// characters; the client is not charged for audio that was never made.
func TestSynthesize_UnbilledRejectionRollsBack(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{unbilled: true, err: apierr.UpstreamRejected(apierr.RejectedInvalid)})
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "hi", ""); ae == nil {
		t.Fatal("want the rejection surfaced")
	}
	if q.rolledBack != 1 || len(q.settled) != 0 {
		t.Fatalf("rollbacks=%d settles=%v, want exactly one rollback", q.rolledBack, q.settled)
	}
}

// TestSynthesize_AmbiguousFailureKeepsFullQuote: a timeout may or may not have reached the
// provider, so the full quote is settled and NOTHING is rolled back (GW-INV-50).
func TestSynthesize_AmbiguousFailureKeepsFullQuote(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{err: apierr.ErrUpstreamTimeout})
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "hi", ""); ae == nil {
		t.Fatal("want the timeout surfaced")
	}
	if q.rolledBack != 0 || len(q.settled) != 1 || q.settled[0] != q.reservation.ReservedPUSD {
		t.Fatalf("rollbacks=%d settles=%v, want one full-quote settle", q.rolledBack, q.settled)
	}
}

// TestSynthesize_QuotaDenialPassesSentinelThrough: the speech category's own wire code reaches
// the caller — never the image one, and never a generic internal error.
func TestSynthesize_QuotaDenialPassesSentinelThrough(t *testing.T) {
	q := &fakeQuota{reserveErr: apierr.ErrTTSQuotaExhausted}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{audio: wantAudio})
	_, ae := svc.Synthesize(context.Background(), "ins_1", "hi", "")
	if ae == nil || ae.Code != "TTS_QUOTA_EXHAUSTED" {
		t.Fatalf("err = %v, want TTS_QUOTA_EXHAUSTED", ae)
	}
	var target *apierr.APIError
	if !errors.As(error(ae), &target) {
		t.Fatal("denial must stay a typed APIError")
	}
}

// TestSynthesize_BannedInstall: a banned install is refused before any reservation.
func TestSynthesize_BannedInstall(t *testing.T) {
	q := &fakeQuota{}
	svc := New(Deps{
		Auth: fakeAuth{status: dominstall.StatusBanned}, Quota: q, Config: enabledCfg(),
		Upstream: &fakeUpstream{audio: wantAudio}, Clock: fixedClock{},
	})
	if _, ae := svc.Synthesize(context.Background(), "ins_1", "hi", ""); ae == nil || ae.Code != "ACCOUNT_BANNED" {
		t.Fatalf("err = %v, want ACCOUNT_BANNED", ae)
	}
	if q.reservation != nil {
		t.Fatal("reserved for a banned install")
	}
}

// stubVoices is one install's inventory, keyed by install so the ownership half can be asserted.
// stubVoices 是逐 install 的库存,按 install 索引,使**归属**那半可断言。
type stubVoices map[string][]domvoice.Voice

func (s stubVoices) ListVoices(_ context.Context, installID string) ([]domvoice.Voice, error) {
	return s[installID], nil
}

// TestResolveVoice_HandleIsScopedToTheInstall guards both halves of the voice handle contract.
//
// The FIRST half is why cloning works at all: `GET /v1/voices` hands out this gateway's own row id,
// and the provider has never heard of it — so without this resolution every synthesis in a cloned
// voice fails at the upstream with a generic engine error, which is exactly what the real-money
// acceptance found after enrollment had already succeeded and been paid for.
//
// The SECOND half is why the handle is not simply the provider's id: another install's handle must
// NOT resolve. It travels on as a literal name and the provider rejects it — refusing loudly here
// would confirm to the caller that the id exists and belongs to someone else (the same reasoning
// that made video hand back a signed handle, ADR 0015).
//
// TestResolveVoice_HandleIsScopedToTheInstall 守住音色句柄契约的两半。
//
// **第一半**是克隆为什么能用:`GET /v1/voices` 给出的是本网关自己的行 id,而供应商从没听说过它——没有这
// 一次解析,每一次用克隆音色的合成都会在上游以一个笼统的引擎错误失败,而那正是真钱验收在**登记已经成功
// 且已付费之后**撞见的东西。
//
// **第二半**是句柄为什么不直接是供应商的 id:**别人的**句柄必须解析不出来。它作为字面名字继续走、被供应商
// 拒掉——在这里大声拒绝,等于向调用方确认那个 id 存在且属于另一个人(与视频交回签名句柄同一条理由,ADR 0015)。
func TestResolveVoice_HandleIsScopedToTheInstall(t *testing.T) {
	voices := stubVoices{
		"ins_mine":  {{ID: "vce_1", Name: "narrator", UpstreamID: "qwen-audio-3.0-tts-flash-anselm-abc"}},
		"ins_other": {{ID: "vce_9", Name: "theirs", UpstreamID: "qwen-audio-3.0-tts-flash-anselm-xyz"}},
	}
	s := &Service{voices: voices}
	for _, tc := range []struct{ install, in, want string }{
		{"ins_mine", "vce_1", "qwen-audio-3.0-tts-flash-anselm-abc"},
		{"ins_mine", " vce_1 ", "qwen-audio-3.0-tts-flash-anselm-abc"},
		{"ins_mine", "vce_9", "vce_9"},                     // another install's handle — never resolved
		{"ins_mine", "longanhuan_v3.6", "longanhuan_v3.6"}, // a preset — never ours to rewrite
		{"ins_mine", "", ""},
		{"ins_nobody", "vce_1", "vce_1"},
	} {
		if got := s.resolveVoice(context.Background(), tc.install, tc.in); got != tc.want {
			t.Fatalf("resolveVoice(%q, %q) = %q, want %q", tc.install, tc.in, got, tc.want)
		}
	}
	// No inventory port at all (older assemblies, tests) must not panic and must not eat the choice.
	// 完全没有库存端口时(旧装配、测试)不得 panic,也不得吞掉调用方的选择。
	if got := (&Service{}).resolveVoice(context.Background(), "ins_mine", "vce_1"); got != "vce_1" {
		t.Fatalf("a service without a voice port must pass the handle through, got %q", got)
	}
}
