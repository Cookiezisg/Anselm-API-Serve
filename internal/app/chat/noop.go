package chat

import (
	"context"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// No-op port defaults so optional collaborators can be omitted at wiring without
// the hot path nil-checking. Each is the inert form of its port (the
// dormant-by-default safety posture: an absent throttle/disk/metrics changes
// nothing).

type noopThrottle struct{}

func (noopThrottle) Observe(string) bool { return false }

type noopDisk struct{}

func (noopDisk) Degraded() bool { return false }

type noopLogger struct{}

func (noopLogger) Warn(context.Context, string, ...any) {}

type noopMetrics struct{}

func (noopMetrics) Inflight(int)                      {}
func (noopMetrics) Upstream(billing.Provider, string) {}
func (noopMetrics) BillingDrift(billing.Provider)     {}
func (noopMetrics) SettleFailure()                    {}
func (noopMetrics) RollbackFailure()                  {}

type noopWG struct{}

func (noopWG) Add(int) {}
func (noopWG) Done()   {}
