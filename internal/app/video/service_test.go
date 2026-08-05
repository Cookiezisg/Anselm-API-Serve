package video

import (
	"context"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
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
	reserveErr error
	settled    []int64
	rolledBack int
}

func (f *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-07", Day: "2026-07-27"}
}

func (f *fakeQuota) Reserve(_ context.Context, installID string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return &domquota.Reservation{
		RequestID: "req_t", InstallID: installID, Period: p, Plan: plan,
		ReservedPUSD: plan.ReservedPUSD, CategoryApplied: domquota.CategoryVideo, CategoryUnits: 1,
	}, nil
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
	gotModel      string
	gotRatio      string
	gotResolution string
	gotFrame      string
	taskID        string
	unbilled      bool
	submitErr     error

	status    VideoStatus
	pollErr   error
	polledFor string
}

func (f *fakeUpstream) SubmitVideo(_ context.Context, model, _ string, _ int, ratio, resolution string) (string, bool, error) {
	f.gotModel, f.gotRatio, f.gotResolution = model, ratio, resolution
	return f.taskID, f.unbilled, f.submitErr
}

func (f *fakeUpstream) SubmitAnimation(_ context.Context, model, _ string, _ int, resolution, firstFrame string) (string, bool, error) {
	f.gotModel, f.gotResolution, f.gotFrame = model, resolution, firstFrame
	return f.taskID, f.unbilled, f.submitErr
}

// gotRatio/gotResolution/gotFrame record what actually reached the upstream — the animate path's
// contract is as much about what it DROPS as about what it sends.
// gotRatio/gotResolution/gotFrame 记下真正抵达上游的东西——图生视频那条路径的契约,**丢掉什么**与
// **发送什么**同样重要。
func (f *fakeUpstream) PollVideo(_ context.Context, taskID string) (VideoStatus, error) {
	f.polledFor = taskID
	return f.status, f.pollErr
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_800_000_000, 0) }

func enabledCfg() *fakeCfg {
	return &fakeCfg{c: config.Config{
		VideoEnabled:          true,
		VideoUpstreamModel:    billing.Wan27T2V,
		VideoI2VUpstreamModel: billing.Wan27I2V,
		QwenAPIKeys:           []string{"sk-test"},
		VideoHandleKey:        domvideo.DeriveKey([]byte("media-signing-secret-at-least-32b!!")),
	}}
}

func newSvc(cfg *fakeCfg, q *fakeQuota, up *fakeUpstream) *Service {
	return New(Deps{Auth: fakeAuth{status: dominstall.StatusActive}, Quota: q, Config: cfg, Upstream: up, Clock: fixedClock{}})
}

// The money shape stated in the package doc: reserve == settle at SUBMIT, and
// the handle comes back signed for the caller.
//
// 包注释里说明的钱的形状:在**提交**处 reserve == settle,句柄签给调用方后返回。
func TestSubmitSettlesAtSubmitAndSignsForCaller(t *testing.T) {
	t.Parallel()
	cfg, q := enabledCfg(), &fakeQuota{}
	svc := newSvc(cfg, q, &fakeUpstream{taskID: "task-abc"})

	handle, ae := svc.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P")
	if ae != nil {
		t.Fatalf("submit: %v", ae)
	}
	if len(q.settled) != 1 || q.settled[0] != 5*83_000_000_000 {
		t.Fatalf("settled = %v, want one full per-second charge", q.settled)
	}
	if q.rolledBack != 0 {
		t.Fatal("an accepted submission must not roll back")
	}
	got, err := domvideo.ParseHandle(cfg.c.VideoHandleKey, "ins_1", handle)
	if err != nil || got != "task-abc" {
		t.Fatalf("handle → %q, %v", got, err)
	}
}

// A provably-unbilled upstream rejection is the ONE path that refunds. Anything
// ambiguous keeps the charge (GW-INV-50).
//
// 可证明未计费的上游拒绝是**唯一**会退的路径。一切歧义保留计费(GW-INV-50)。
func TestSubmitRollsBackOnlyWhenProvablyUnbilled(t *testing.T) {
	t.Parallel()
	q := &fakeQuota{}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{unbilled: true, submitErr: apierr.UpstreamRejected(apierr.RejectedInvalid)})
	if _, ae := svc.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P"); ae == nil {
		t.Fatal("want an error")
	}
	if q.rolledBack != 1 || len(q.settled) != 0 {
		t.Fatalf("rolledBack=%d settled=%v — want a refund and no charge", q.rolledBack, q.settled)
	}

	q2 := &fakeQuota{}
	svc2 := newSvc(enabledCfg(), q2, &fakeUpstream{submitErr: apierr.ErrUpstreamTimeout})
	if _, ae := svc2.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P"); ae == nil {
		t.Fatal("want an error")
	}
	if q2.rolledBack != 0 || len(q2.settled) != 1 || q2.settled[0] != 5*83_000_000_000 {
		t.Fatalf("rolledBack=%d settled=%v — a timeout must keep the full quote", q2.rolledBack, q2.settled)
	}
}

// Polling must never move money — not on success, not on failure. A client
// waiting three minutes will ask a dozen times.
//
// 轮询绝不动钱——成功不动、失败也不动。一个等三分钟的客户端会问上十几次。
func TestStatusTouchesNoMoney(t *testing.T) {
	t.Parallel()
	cfg, q := enabledCfg(), &fakeQuota{}
	up := &fakeUpstream{status: VideoStatus{Phase: domvideo.PhaseSucceeded, URL: "https://oss.example/v.mp4"}}
	svc := newSvc(cfg, q, up)
	handle := domvideo.SignHandle(cfg.c.VideoHandleKey, "ins_1", "task-abc")

	for i := 0; i < 3; i++ {
		st, ae := svc.Status(context.Background(), "ins_1", handle)
		if ae != nil || st.Phase != domvideo.PhaseSucceeded || st.URL == "" {
			t.Fatalf("status = %+v, %v", st, ae)
		}
	}
	if len(q.settled) != 0 || q.rolledBack != 0 {
		t.Fatalf("polling moved money: settled=%v rolledBack=%d", q.settled, q.rolledBack)
	}
	if up.polledFor != "task-abc" {
		t.Fatalf("polled %q, want the handle's task", up.polledFor)
	}
}

// The cross-install read is what the signature exists to stop, and it must fail
// with the SAME answer as a forgotten task.
//
// 跨 install 读取正是签名存在的目的所要阻止的,且它必须与「已忘掉的任务」**同一个答案**。
func TestStatusRefusesAnotherInstallsHandle(t *testing.T) {
	t.Parallel()
	cfg := enabledCfg()
	up := &fakeUpstream{status: VideoStatus{Phase: domvideo.PhaseSucceeded, URL: "https://oss.example/v.mp4"}}
	svc := newSvc(cfg, &fakeQuota{}, up)
	alices := domvideo.SignHandle(cfg.c.VideoHandleKey, "ins_alice", "task-secret")

	_, ae := svc.Status(context.Background(), "ins_bob", alices)
	if ae != apierr.ErrVideoTaskNotFound {
		t.Fatalf("bob got %v, want VIDEO_TASK_NOT_FOUND", ae)
	}
	if up.polledFor != "" {
		t.Fatal("a rejected handle must never reach the upstream")
	}
}

// Availability needs all three halves; the handle key is the one that is easy to
// forget, and forgetting it would spend a daily clip on a video nobody can poll.
//
// 可用性需要三半齐全;句柄密钥是最容易漏的那一半,而漏了它会花掉一条日额度去换一条没人轮询得到的片子。
func TestUnavailableWhenAnyHalfIsMissing(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Config){
		"capability off": func(c *config.Config) { c.VideoEnabled = false },
		"no credential":  func(c *config.Config) { c.QwenAPIKeys = nil },
		"no handle key":  func(c *config.Config) { c.VideoHandleKey = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := enabledCfg()
			mutate(&cfg.c)
			q := &fakeQuota{}
			svc := newSvc(cfg, q, &fakeUpstream{taskID: "task-abc"})
			if _, ae := svc.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P"); ae != apierr.ErrVideoUnavailable {
				t.Fatalf("submit → %v, want VIDEO_UNAVAILABLE", ae)
			}
			if _, ae := svc.Status(context.Background(), "ins_1", "x.y"); ae != apierr.ErrVideoUnavailable {
				t.Fatalf("status → %v, want VIDEO_UNAVAILABLE", ae)
			}
			if len(q.settled) != 0 {
				t.Fatal("an unavailable route must not charge")
			}
		})
	}
}

// The category denial must surface as the VIDEO sentinel, not the umbrella —
// "you have used today's 10 videos" is a different sentence from "out of quota".
//
// 品类拒绝必须以**视频**的 sentinel 露出、而非伞 sentinel——「今天的 10 条用完了」与「额度用尽」
// 不是同一句话。
func TestQuotaDenialPropagates(t *testing.T) {
	t.Parallel()
	q := &fakeQuota{reserveErr: apierr.ErrVideoQuotaExhausted}
	svc := newSvc(enabledCfg(), q, &fakeUpstream{taskID: "task-abc"})
	_, ae := svc.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P")
	if ae != apierr.ErrVideoQuotaExhausted {
		t.Fatalf("submit → %v, want VIDEO_QUOTA_EXHAUSTED", ae)
	}
}

func TestInstallGates(t *testing.T) {
	t.Parallel()
	q := &fakeQuota{}
	banned := New(Deps{
		Auth: fakeAuth{status: dominstall.StatusBanned}, Quota: q, Config: enabledCfg(),
		Upstream: &fakeUpstream{taskID: "t"}, Clock: fixedClock{},
	})
	if _, ae := banned.Submit(context.Background(), "ins_1", "a cat", 5, "16:9", "720P"); ae != apierr.ErrAccountBanned {
		t.Fatalf("banned → %v", ae)
	}
	if _, ae := banned.Status(context.Background(), "ins_1", "x.y"); ae != apierr.ErrAccountBanned {
		t.Fatalf("banned status → %v", ae)
	}
	unknown := newSvc(enabledCfg(), q, &fakeUpstream{taskID: "t"})
	if _, ae := unknown.Submit(context.Background(), "", "a cat", 5, "16:9", "720P"); ae != apierr.ErrInvalidInstall {
		t.Fatalf("no install → %v", ae)
	}
	if len(q.settled) != 0 || q.rolledBack != 0 {
		t.Fatal("a rejected install must never touch money")
	}
}

// TestAnimate_RefusesAnAddressShapedFrame — the SSRF mitigation, at the service boundary, for the
// same reason as the image editor's: an address this gateway would fetch is a primitive aimed at
// our own network, and a boundary that only holds for well-behaved callers is not a boundary.
//
// TestAnimate_RefusesAnAddressShapedFrame——SSRF 缓解,在 service 边界上,理由与改图那条相同:
// 一个本网关会去取的地址是指向我们自己网络的原语,而只对守规矩的调用方成立的边界不是边界。
func TestAnimate_RefusesAnAddressShapedFrame(t *testing.T) {
	for _, frame := range []string{
		"https://example.com/cat.png",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"",
	} {
		up := &fakeUpstream{taskID: "task_1"}
		svc := newSvc(enabledCfg(), &fakeQuota{}, up)
		_, ae := svc.Animate(context.Background(), "ins_1", "make it move", 5, "720P", frame)
		if ae == nil || ae.Code != "VIDEO_FRAME_INVALID" {
			t.Fatalf("frame %q: err = %v, want VIDEO_FRAME_INVALID", frame, ae)
		}
		if up.gotFrame != "" {
			t.Fatalf("frame %q reached the upstream despite the refusal", frame)
		}
	}
}

func TestAnimate_RefusesUnpricedResolutionBeforeUpstream(t *testing.T) {
	up := &fakeUpstream{taskID: "task_1"}
	svc := newSvc(enabledCfg(), &fakeQuota{}, up)
	_, ae := svc.Animate(context.Background(), "ins_1", "make it move", 5, "1080P", "data:image/png;base64,iVBORw0KGgo=")
	if ae != apierr.ErrBadRequest {
		t.Fatalf("1080P animation = %v, want BAD_REQUEST", ae)
	}
	if up.gotModel != "" {
		t.Fatal("unpriced animation reached the upstream")
	}
}

// Animation uses the independently priced model, inherits only the frame's
// aspect, and keeps 720P explicit so the provider cannot choose a pricier tier.
//
// 动画使用独立定价模型,只继承首帧比例,并显式发送 720P,不让上游自行选择更贵档位。
func TestAnimate_UsesI2VModelAndKeepsPricedResolution(t *testing.T) {
	up := &fakeUpstream{taskID: "task_1"}
	svc := newSvc(enabledCfg(), &fakeQuota{}, up)
	const frame = "data:image/png;base64,iVBORw0KGgo="
	if _, ae := svc.Animate(context.Background(), "ins_1", "make it move", 5, "720P", frame); ae != nil {
		t.Fatalf("animate failed: %v", ae)
	}
	if up.gotFrame != frame {
		t.Fatalf("upstream frame = %q, want the data URL unchanged", up.gotFrame)
	}
	if up.gotModel != billing.Wan27I2V {
		t.Fatalf("upstream model = %q, want %q", up.gotModel, billing.Wan27I2V)
	}
	if up.gotRatio != "" || up.gotResolution != "720P" {
		t.Fatalf("animation geometry = ratio %q resolution %q, want inherited ratio and explicit 720P",
			up.gotRatio, up.gotResolution)
	}
}
