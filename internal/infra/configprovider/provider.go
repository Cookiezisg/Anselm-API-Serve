package configprovider

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// settingsLoader is the minimal read surface LoadWithOverlay needs (the boot-time
// overlay read). settingsStore is the minimal write surface ApplyOverrides needs
// (the all-or-nothing persist). Declared HERE as small interfaces so
// infra/store/settingsstore satisfies them structurally; the provider may still
// import settingsstore concretely (infra→infra is allowed).
//
// 在本包声明小接口,store 结构化满足之;读/写面分离,overlay 读只需 LoadAll。
type settingsLoader interface {
	LoadAll(ctx context.Context) (map[string]string, error)
}

type settingsStore interface {
	PersistAll(ctx context.Context, overlay map[string]string) error
}

// snapshot is the ONE thing the atomic pointer carries: the config as the
// operator configured it, and the same config after the GATEWAY_MODE rationing
// mask. Pairing them behind a SINGLE pointer (rather than two atomics) keeps the
// swap the single commit point it already was — a reader can never observe a new
// configured value against a stale effective one, or vice versa.
//
// snapshot 是原子指针携带的**唯一**一样东西:运营者配置的那份 + 过了 GATEWAY_MODE 掩码的
// 那份。把两者绑在**一个**指针后面(而不是两个 atomic),让 swap 仍是它本来的单一提交点——
// 读者绝不可能看到「新的 configured 配旧的 effective」。
type snapshot struct {
	configured config.Config // dashboard read/edit base, settings overlay base
	effective  config.Config // what every enforcement site reads
}

// Provider holds the live, hot-swappable effective Config behind an
// atomic.Pointer so the request hot path reads it lock-free (Load()), while
// ApplyOverrides serializes writes (validate → persist → atomic swap). Every
// request snapshots once via Load() and may hold the immutable pointer for its
// whole duration (overrides build a FRESH config and swap the pointer — the old
// one is never mutated, so there is no half-old/half-new tearing, GW-INV-35).
//
// The mask is applied HERE, once per swap, rather than at each enforcement site:
// there are a dozen readers of the rationing knobs across quota, rate limiting,
// install issuance and the capability catalog, and a mode that has to be re-checked
// at every one of them is a mode that will eventually be forgotten at one of them.
// Load() therefore returns the already-masked config and every existing caller
// stays byte-for-byte unchanged.
//
// 写经 mu 串行化;读 Load() 无锁。机密绝不进 overlay(domain ApplyOverrides 把机密
// 当未知键拒绝),持久化失败不 swap(生效态不被腐蚀)。掩码在**这里**每次 swap 算一次,
// 而不是在每个执行点各判一次:配额旋钮的读取方散布在 quota、限速、领号、能力面十几处,
// 一个需要在每处重新判断的模式,迟早会在其中某一处被忘掉。
type Provider struct {
	cur atomic.Pointer[snapshot]
	mu  sync.Mutex // serializes the write side (ApplyOverrides) only
}

// New seeds a Provider with the assembled startup config (env, or env+overlay).
func New(base config.Config) *Provider {
	p := &Provider{}
	p.publish(base)
	return p
}

// publish derives the effective config from configured and installs both as one
// atomic snapshot. The ONLY place the mask is applied.
func (p *Provider) publish(configured config.Config) *snapshot {
	s := &snapshot{configured: configured, effective: configured.EffectiveLimits()}
	p.cur.Store(s)
	return s
}

// Load returns the current EFFECTIVE config snapshot — configured values with the
// GATEWAY_MODE mask already applied. Lock-free; safe on the request hot path. The
// returned pointer is immutable — callers may hold it for the duration of a request.
func (p *Provider) Load() *config.Config { return &p.cur.Load().effective }

// Configured returns the config AS THE OPERATOR SET IT, mask not applied. It is
// the base for the override path and the dashboard read model — the two places
// that must show and edit real production values even while debug masks them.
// Enforcement must never read this.
//
// Configured 返回**运营者设的那份**(未掩码):override 路径与后台读模型的基准——这两处
// 必须在 debug 掩码生效期间照样显示/编辑真实生产值。执行路径绝不可读它。
func (p *Provider) Configured() *config.Config { return &p.cur.Load().configured }

// LoadWithOverlay assembles the boot config: env defaults (base) ← DB overlay
// (the runtime-tunable subset previously saved via the dashboard), re-running the
// cross-field semantic checks via config.ApplyOverrides so a persisted-then-stale
// combo can never bring up a guardrail-defeating config. A corrupt/invalid
// persisted overlay fails fast (never a silent fallback to env — that would mask a
// guardrail change ops believe is live). An empty overlay returns base unchanged.
//
// 加载顺序:env(base)→ settings 表覆盖运行时可改项 → 重跑跨字段校验 → 生效。
func LoadWithOverlay(ctx context.Context, base config.Config, ss settingsLoader) (config.Config, error) {
	overlay, err := ss.LoadAll(ctx)
	if err != nil {
		return config.Config{}, fmt.Errorf("read settings overlay: %w", err)
	}
	if len(overlay) == 0 {
		return base, nil
	}
	merged, err := config.ApplyOverrides(base, overlay)
	if err != nil {
		return config.Config{}, fmt.Errorf("apply settings overlay: %w", err)
	}
	return merged, nil
}

// ApplyOverrides validates the requested runtime-hot overrides against the CURRENT
// effective config (config.ApplyOverrides: clone → per-key apply rejecting
// secret/startup-hard keys → ValidateSemantics), persists them all-or-nothing, and
// only THEN atomically swaps in the rebuilt config. The swap is the commit point:
// on a validate OR persist failure it returns without mutating live state, so a
// half-written overlay can never diverge from the in-memory config. An empty batch
// is a no-op. Returns the new effective snapshot.
//
// 步骤:① domain 校验(拒机密/硬约束/越界 + 跨字段)② PersistAll 全或无持久化(失败
// 不 swap)③ 原子 swap。clone→apply→persist→swap。
func (p *Provider) ApplyOverrides(ctx context.Context, overrides map[string]string, ss settingsStore) (*config.Config, error) {
	if len(overrides) == 0 {
		return p.Load(), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// The base is the CONFIGURED config, never the masked one: cloning the mask
	// would bake debug's opened knobs into the persisted values, and the operator's
	// production numbers would be destroyed by the first unrelated dashboard edit
	// made while debug is live.
	// 基准恒取**未掩码**那份:拿掩码去 clone 会把 debug 开出来的值当成配置值固化,运营者的
	// 生产数值会被 debug 期间任何一次无关的后台编辑抹掉。
	cur := p.cur.Load()
	next, err := config.ApplyOverrides(cur.configured, overrides)
	if err != nil {
		return nil, err
	}
	// Persist BEFORE swapping so a write failure leaves live state untouched.
	if err := ss.PersistAll(ctx, overrides); err != nil {
		return nil, fmt.Errorf("persist overrides: %w", err)
	}
	return &p.publish(next).effective, nil
}
