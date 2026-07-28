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
