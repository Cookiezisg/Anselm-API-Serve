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

// Provider holds the live, hot-swappable effective Config behind an
// atomic.Pointer so the request hot path reads it lock-free (Load()), while
// ApplyOverrides serializes writes (validate → persist → atomic swap). Every
// request snapshots once via Load() and may hold the immutable pointer for its
// whole duration (overrides build a FRESH config and swap the pointer — the old
// one is never mutated, so there is no half-old/half-new tearing, GW-INV-35).
//
// 写经 mu 串行化;读 Load() 无锁。机密绝不进 overlay(domain ApplyOverrides 把机密
// 当未知键拒绝),持久化失败不 swap(生效态不被腐蚀)。
type Provider struct {
	cur atomic.Pointer[config.Config]
	mu  sync.Mutex // serializes the write side (ApplyOverrides) only
}

// New seeds a Provider with the assembled startup config (env, or env+overlay).
func New(base config.Config) *Provider {
	p := &Provider{}
	c := base
	p.cur.Store(&c)
	return p
}

// Load returns the current effective config snapshot. Lock-free; safe on the
// request hot path. The returned pointer is immutable — callers may hold it for
// the duration of a request.
func (p *Provider) Load() *config.Config { return p.cur.Load() }

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

	cur := p.cur.Load()
	next, err := config.ApplyOverrides(*cur, overrides)
	if err != nil {
		return nil, err
	}
	// Persist BEFORE swapping so a write failure leaves live state untouched.
	if err := ss.PersistAll(ctx, overrides); err != nil {
		return nil, fmt.Errorf("persist overrides: %w", err)
	}
	p.cur.Store(&next)
	return &next, nil
}
