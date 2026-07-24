package chat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	domchat "github.com/sunweilin/anselm/gateway/internal/domain/chat"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	dominstall "github.com/sunweilin/anselm/gateway/internal/domain/install"
	domquota "github.com/sunweilin/anselm/gateway/internal/domain/quota"
)

// fakeSink captures the use case's writes for assertions. Race-safe: the relay
// loop runs on the test goroutine but settle/rollback run on detached goroutines,
// so the test reads sink state only after WaitGroup drain.
type fakeSink struct {
	mu       sync.Mutex
	headers  map[string]string
	status   int
	body     bytes.Buffer
	flushes  int
	wroteHdr bool
}

func newFakeSink() *fakeSink { return &fakeSink{headers: map[string]string{}} }

func (s *fakeSink) SetHeader(k, v string) {
	s.mu.Lock()
	s.headers[k] = v
	s.mu.Unlock()
}
func (s *fakeSink) WriteHeader(status int) {
	s.mu.Lock()
	if !s.wroteHdr {
		s.status = status
		s.wroteHdr = true
	}
	s.mu.Unlock()
}
func (s *fakeSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}
func (s *fakeSink) Flush()                     { s.mu.Lock(); s.flushes++; s.mu.Unlock() }
func (s *fakeSink) SetWriteDeadline(time.Time) {}
func (s *fakeSink) bodyString() string         { s.mu.Lock(); defer s.mu.Unlock(); return s.body.String() }
func (s *fakeSink) statusCode() int            { s.mu.Lock(); defer s.mu.Unlock(); return s.status }
func (s *fakeSink) header(k string) string     { s.mu.Lock(); defer s.mu.Unlock(); return s.headers[k] }

// --- ports ---

type fakeAuth struct {
	id     string
	status dominstall.Status
	found  bool
	err    error
}

func (a fakeAuth) LookupInstall(context.Context, string) (string, dominstall.Status, bool, error) {
	return a.id, a.status, a.found, a.err
}

type fakeQuota struct {
	mu           sync.Mutex
	reserveErr   error
	plan         billing.Plan
	reservedPUSD int64
	settleCalls  []int64
	settleErr    error
	rollbackN    int
	rollbackErr  error
}

func (q *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-06", Day: "2026-06-20"}
}
func (q *fakeQuota) Reserve(_ context.Context, id string, plan billing.Plan, p domquota.Period) (*domquota.Reservation, error) {
	if q.reserveErr != nil {
		return nil, q.reserveErr
	}
	q.mu.Lock()
	q.plan = plan
	q.reservedPUSD = plan.ReservedPUSD
	q.mu.Unlock()
	return &domquota.Reservation{RequestID: "req_x", InstallID: id, Period: p, Plan: plan, ReservedPUSD: plan.ReservedPUSD}, nil
}
func (q *fakeQuota) Settle(_ context.Context, _ *domquota.Reservation, actual int64) error {
	q.mu.Lock()
	q.settleCalls = append(q.settleCalls, actual)
	q.mu.Unlock()
	return q.settleErr
}
func (q *fakeQuota) Rollback(_ context.Context, _ *domquota.Reservation) error {
	q.mu.Lock()
	q.rollbackN++
	q.mu.Unlock()
	return q.rollbackErr
}
func (q *fakeQuota) settles() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int64(nil), q.settleCalls...)
}
func (q *fakeQuota) rollbacks() int { q.mu.Lock(); defer q.mu.Unlock(); return q.rollbackN }

type fakeUpstream struct {
	mu        sync.Mutex
	body      string
	aerr      *apierr.APIError
	exposure  billing.ChargeExposure
	available map[billing.Provider]bool
	breakers  map[billing.Provider]bool
	calls     []upstreamCall
	// block, when non-nil, blocks Do until closed (simulates a slow upstream so a
	// second request finds no free slot).
	block <-chan struct{}
}

type upstreamCall struct {
	Provider billing.Provider
	Model    string
	Request  domchat.CompletionRequest
	Timeout  time.Duration
}

func (u *fakeUpstream) Open(ctx context.Context, provider billing.Provider, model string, req domchat.CompletionRequest, timeout time.Duration) (UpstreamStream, *UpstreamFailure) {
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{Provider: provider, Model: model, Request: req, Timeout: timeout})
	u.mu.Unlock()
	if u.block != nil {
		select {
		case <-u.block:
		case <-ctx.Done():
			return nil, &UpstreamFailure{APIError: apierr.ErrUpstreamBusy, Exposure: billing.DefinitelyUnbilled}
		}
	}
	if u.aerr != nil {
		return nil, &UpstreamFailure{APIError: u.aerr, Exposure: u.exposure}
	}
	return io.NopCloser(bytes.NewReader([]byte(u.body))), nil
}
func (u *fakeUpstream) Available(provider billing.Provider) bool {
	if u.available == nil {
		return true
	}
	return u.available[provider]
}
func (u *fakeUpstream) BreakerOpen(provider billing.Provider) bool {
	return u.breakers != nil && u.breakers[provider]
}
func (u *fakeUpstream) callSnapshot() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

type fakeRL struct {
	allow    bool
	setCalls int
}

func (r *fakeRL) Allow(string) bool                  { return r.allow }
func (r *fakeRL) SetKeyLimit(string, int, time.Time) { r.setCalls++ }

type fakeThrottle struct{ observe bool }

func (t fakeThrottle) Observe(string) bool { return t.observe }

type fakeDisk struct{ degraded bool }

func (d fakeDisk) Degraded() bool { return d.degraded }

type fakeConfig struct{ c *config.Config }

func (f fakeConfig) Load() *config.Config { return f.c }

type countingConfig struct {
	mu    sync.Mutex
	c     *config.Config
	loads int
}

func (f *countingConfig) Load() *config.Config {
	f.mu.Lock()
	f.loads++
	f.mu.Unlock()
	return f.c
}
func (f *countingConfig) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

type fakeLogger struct {
	mu    sync.Mutex
	warns [][]any
}

func (l *fakeLogger) Warn(_ context.Context, msg string, args ...any) {
	l.mu.Lock()
	entry := append([]any{msg}, args...)
	l.warns = append(l.warns, entry)
	l.mu.Unlock()
}
func (l *fakeLogger) warningCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

type fakeMetrics struct {
	mu          sync.Mutex
	settleFails int
	rollFails   int
	upstream    map[string]int
	drift       map[billing.Provider]int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{upstream: map[string]int{}, drift: map[billing.Provider]int{}}
}
func (m *fakeMetrics) Inflight(int) {}
func (m *fakeMetrics) Upstream(p billing.Provider, o string) {
	m.mu.Lock()
	m.upstream[string(p)+"/"+o]++
	m.mu.Unlock()
}
func (m *fakeMetrics) BillingDrift(p billing.Provider) {
	m.mu.Lock()
	m.drift[p]++
	m.mu.Unlock()
}
func (m *fakeMetrics) SettleFailure()      { m.mu.Lock(); m.settleFails++; m.mu.Unlock() }
func (m *fakeMetrics) RollbackFailure()    { m.mu.Lock(); m.rollFails++; m.mu.Unlock() }
func (m *fakeMetrics) settleFailures() int { m.mu.Lock(); defer m.mu.Unlock(); return m.settleFails }
func (m *fakeMetrics) upstreamCount(p billing.Provider, o string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upstream[string(p)+"/"+o]
}
func (m *fakeMetrics) driftCount(p billing.Provider) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drift[p]
}

// testCfg is a permissive config that passes every shape/cap gate.
func testCfg() *config.Config {
	return &config.Config{
		PublicModelID:           "anselm-auto",
		TextUpstreamModel:       billing.DeepSeekV4Flash,
		MultimodalUpstreamModel: billing.Qwen37Plus,
		MonthlyQuota:            1000,
		GlobalMonthlySpendPUSD:  100 * billing.PicoUSDPerUSD,
		MaxTokensCap:            1000,
		InputTokenCap:           1_000_000,
		MaxMessages:             100,
		MaxMessageChars:         100000,
		MaxMediaParts:           8,
		MediaPublicBaseURL:      "https://gw.example",
		MaxMediaDecodedBytes:    1 << 20,
		MaxBodyBytes:            4 << 20,
		NGlobalConcurrency:      2,
		QueueWait:               50 * time.Millisecond,
		UpstreamHeaderTimeout:   3 * time.Second,
		Location:                time.UTC,
	}
}

// build assembles a Service with sensible defaults; callers override fields on
// the returned deps' fakes before Handle. Returns the service + the bgWG so tests
// can await detached settle/rollback before asserting.
func build(d Deps) (*Service, *sync.WaitGroup) {
	wg := &sync.WaitGroup{}
	d.BgWG = wg
	if d.Config == nil {
		d.Config = fakeConfig{c: testCfg()}
	}
	if d.Clock == nil {
		d.Clock = fakeClock{t: time.Unix(1_700_000_000, 0)}
	}
	return New(d), wg
}

// disconnectSink fails Write after failAfter successful writes, simulating a
// client that disconnects mid-stream. Embeds fakeSink for the header/status side.
type disconnectSink struct {
	*fakeSink
	failAfter int
	writes    int
}

func (d *disconnectSink) appSink() Sink { return d }
func (d *disconnectSink) Write(p []byte) (int, error) {
	d.writes++
	if d.writes > d.failAfter {
		return 0, io.ErrClosedPipe // client gone.
	}
	return d.fakeSink.Write(p)
}

// payloadSpy records the marshaled upstream payload then succeeds with a usage
// body, so a test can assert the sanitized body's contents.
type payloadSpy struct {
	request domchat.CompletionRequest
}

func (s *payloadSpy) Open(_ context.Context, _ billing.Provider, _ string, req domchat.CompletionRequest, _ time.Duration) (UpstreamStream, *UpstreamFailure) {
	s.request = req
	return io.NopCloser(bytes.NewReader([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))), nil
}
func (s *payloadSpy) Available(billing.Provider) bool   { return true }
func (s *payloadSpy) BreakerOpen(billing.Provider) bool { return false }

var errBoom = errors.New("boom")

const goodBody = `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
const goodStreamBody = `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// fakeLeases stands in for the media service's VerifyLease. It records what chat asked so a test can
// assert the CALLING install is what gets checked — the predicate whose absence would let any
// install reference another's media.
//
// fakeLeases 顶替 media service 的 VerifyLease,并记录 chat 问了什么,使测试能断言被校验的是**发起请求的**
// install——少了这条谓词,任一 install 都能引用他人的媒体。
type fakeLeases struct {
	mu    sync.Mutex
	err   error
	calls []fakeLeaseCall
}

type fakeLeaseCall struct {
	InstallID string
	LeaseID   string
	Token     string
}

func (f *fakeLeases) VerifyLease(_ context.Context, installID, leaseID, token string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeLeaseCall{installID, leaseID, token})
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return "image/png", nil
}
