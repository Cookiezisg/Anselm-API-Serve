package image

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
		ReservedPUSD: plan.ReservedPUSD, CategoryApplied: domquota.CategoryImage, CategoryUnits: 1,
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

type fakeUpstream struct {
	url      string
	unbilled bool
	err      error
	// gotSource records what the edit path handed the upstream, so a test can assert the source
	// actually travelled rather than that the call merely happened.
	// gotSource 记下改图路径递给上游的东西,使测试断言的是「源图真的走到了」、而不只是「调用发生过」。
	gotSource *string
}

func (f fakeUpstream) GenerateImage(context.Context, string, string, string) (string, bool, error) {
	return f.url, f.unbilled, f.err
}

func (f fakeUpstream) EditImage(_ context.Context, _, _, _, source string) (string, bool, error) {
	if f.gotSource != nil {
		*f.gotSource = source
	}
	return f.url, f.unbilled, f.err
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_800_000_000, 0) }

func enabledCfg() *fakeCfg {
	return &fakeCfg{c: config.Config{
		ImageEnabled:       true,
		ImageUpstreamModel: billing.QwenImage20,
		QwenAPIKeys:        []string{"sk-test"},
	}}
}

func newSvc(cfg *fakeCfg, q *fakeQuota, up Upstream) *Service {
	return New(Deps{Auth: fakeAuth{status: dominstall.StatusActive}, Quota: q, Config: cfg, Upstream: up, Clock: fixedClock{}})
}

// TestGenerate_SuccessSettlesDeterministicCost: the happy path settles exactly the frozen
// per-image cost (reserve == settle) and relays the upstream URL untouched (P13).
func TestGenerate_SuccessSettlesDeterministicCost(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, fakeUpstream{url: "https://oss.example/img.png?Expires=1"})
	u, ae := svc.Generate(context.Background(), "ins_1", "a cat", "1024x1024")
	if ae != nil || u != "https://oss.example/img.png?Expires=1" {
		t.Fatalf("generate = %q, %v", u, ae)
	}
	if len(q.settled) != 1 || q.settled[0] != q.reservation.ReservedPUSD {
		t.Fatalf("settled = %v, want exactly the reserved amount", q.settled)
	}
	if q.rolledBack != 0 {
		t.Fatalf("unexpected rollback")
	}
}

// TestGenerate_UnavailableIsWholePath: the double-half rule — capability off OR missing
// credential each independently yield IMAGE_UNAVAILABLE before any reservation.
func TestGenerate_UnavailableIsWholePath(t *testing.T) {
	off := enabledCfg()
	off.c.ImageEnabled = false
	noKey := enabledCfg()
	noKey.c.QwenAPIKeys = nil
	for name, cfg := range map[string]*fakeCfg{"capability off": off, "no credential": noKey} {
		q := &fakeQuota{}
		svc := newSvc(cfg, q, fakeUpstream{url: "https://x/y.png"})
		if _, ae := svc.Generate(context.Background(), "ins_1", "p", "1024x1024"); ae != apierr.ErrImageUnavailable {
			t.Errorf("%s: err = %v, want IMAGE_UNAVAILABLE", name, ae)
		}
		if q.reservation != nil {
			t.Errorf("%s: reserved despite unavailable path", name)
		}
	}
}

// TestGenerate_UnbilledRejectionRollsBack: a provably-unbilled upstream rejection rolls the
// reservation back and surfaces the upstream sentinel.
func TestGenerate_UnbilledRejectionRollsBack(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, fakeUpstream{unbilled: true, err: apierr.UpstreamRejected(apierr.RejectedInvalid)})
	_, ae := svc.Generate(context.Background(), "ins_1", "p", "1024x1024")
	if ae == nil || ae.Code != apierr.CodeUpstreamRejected {
		t.Fatalf("err = %v, want UPSTREAM_REJECTED", ae)
	}
	if q.rolledBack != 1 || len(q.settled) != 0 {
		t.Fatalf("rollback/settle = %d/%v, want rollback only", q.rolledBack, q.settled)
	}
}

// TestGenerate_AmbiguousFailureKeepsFullQuote (GW-INV-50): a timeout after submission settles
// the FULL reservation — the provider may have billed; never roll an ambiguous outcome back.
func TestGenerate_AmbiguousFailureKeepsFullQuote(t *testing.T) {
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, fakeUpstream{err: apierr.ErrUpstreamTimeout})
	_, ae := svc.Generate(context.Background(), "ins_1", "p", "1024x1024")
	if ae != apierr.ErrUpstreamTimeout {
		t.Fatalf("err = %v, want UPSTREAM_TIMEOUT", ae)
	}
	if q.rolledBack != 0 || len(q.settled) != 1 || q.settled[0] != q.reservation.ReservedPUSD {
		t.Fatalf("rollback/settle = %d/%v, want full-quote settle only", q.rolledBack, q.settled)
	}
}

// TestGenerate_QuotaDenialPassesSentinelThrough: the mapped apierr (e.g. the category cap's
// IMAGE_QUOTA_EXHAUSTED) reaches the caller verbatim.
func TestGenerate_QuotaDenialPassesSentinelThrough(t *testing.T) {
	q := &fakeQuota{reserveErr: apierr.ErrImageQuotaExhausted}
	svc := newSvc(enabledCfg(), q, fakeUpstream{url: "https://x/y.png"})
	if _, ae := svc.Generate(context.Background(), "ins_1", "p", "1024x1024"); ae != apierr.ErrImageQuotaExhausted {
		t.Fatalf("err = %v, want IMAGE_QUOTA_EXHAUSTED", ae)
	}
}

// TestGenerate_BannedInstall: the shared auth tree applies before any money moves.
func TestGenerate_BannedInstall(t *testing.T) {
	q := &fakeQuota{}
	svc := New(Deps{Auth: fakeAuth{status: dominstall.StatusBanned}, Quota: q, Config: enabledCfg(), Upstream: fakeUpstream{}, Clock: fixedClock{}})
	if _, ae := svc.Generate(context.Background(), "ins_1", "p", "1024x1024"); ae != apierr.ErrAccountBanned {
		t.Fatalf("err = %v, want ACCOUNT_BANNED", ae)
	}
	if q.reservation != nil {
		t.Fatal("reserved for a banned install")
	}
}

var _ = errors.Is // keep errors imported if assertions change shape

// TestEdit_RefusesAnAddressShapedSource is the SSRF mitigation's test, and it is deliberately at the
// service layer rather than the handler: the shape guard is a security boundary, and a boundary
// that only holds when one particular caller is well-behaved is not a boundary. ADR 0011 forbids a
// managed media input carrying a scheme or a host because an address this gateway would fetch is an
// SSRF primitive aimed at our own network — including the addresses that look most innocent.
//
// TestEdit_RefusesAnAddressShapedSource 是那条 SSRF 缓解的测试,且**刻意**放在 service 层而非 handler:
// 形状闸是一条**安全边界**,而一条「只在某个特定调用方守规矩时才成立」的边界不是边界。ADR 0011 禁止带
// scheme 或 host 的受管媒体输入,因为一个本网关会去取的地址,是指向**我们自己网络**的 SSRF 原语——
// 包括那些看起来最人畜无害的地址。
func TestEdit_RefusesAnAddressShapedSource(t *testing.T) {
	for _, src := range []string{
		"https://example.com/cat.png",
		"http://169.254.169.254/latest/meta-data/", // the cloud metadata endpoint 云元数据端点
		"file:///etc/passwd",
		"//example.com/cat.png",
		"", // absent is not a source either 缺席同样不是源
	} {
		svc := newSvc(enabledCfg(), &fakeQuota{}, fakeUpstream{url: "https://cdn/x.png"})
		_, ae := svc.Edit(context.Background(), "ins_1", "make it night", "1024x1024", src)
		if ae == nil || ae.Code != "IMAGE_SOURCE_INVALID" {
			t.Fatalf("source %q: err = %v, want IMAGE_SOURCE_INVALID", src, ae)
		}
	}
}

// TestEdit_PassesTheDataURLThrough: the happy path, asserting the bytes reach the upstream. Without
// this the refusal test above could pass against a service that refuses everything.
//
// TestEdit_PassesTheDataURLThrough:顺利路径,断言字节抵达上游。少了它,上面那条拒绝测试在一个「拒绝
// 一切」的 service 上同样会绿。
func TestEdit_PassesTheDataURLThrough(t *testing.T) {
	var got string
	svc := newSvc(enabledCfg(), &fakeQuota{}, fakeUpstream{url: "https://cdn/edited.png", gotSource: &got})
	const src = "data:image/png;base64,iVBORw0KGgo="
	url, ae := svc.Edit(context.Background(), "ins_1", "make it night", "1024x1024", src)
	if ae != nil {
		t.Fatalf("edit failed: %v", ae)
	}
	if url != "https://cdn/edited.png" {
		t.Fatalf("url = %q, want the edited artifact", url)
	}
	if got != src {
		t.Fatalf("upstream received %q, want the data URL unchanged", got)
	}
}
