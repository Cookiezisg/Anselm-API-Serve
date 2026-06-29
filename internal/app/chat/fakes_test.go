package chat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
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
	mu          sync.Mutex
	reserveErr  error
	reserved    int64
	settleCalls []int64
	settleErr   error
	rollbackN   int
	rollbackErr error
}

func (q *fakeQuota) SnapshotPeriod(time.Time) domquota.Period {
	return domquota.Period{Month: "2026-06", Day: "2026-06-20"}
}
func (q *fakeQuota) Reserve(_ context.Context, id string, est int64, p domquota.Period) (*domquota.Reservation, error) {
	if q.reserveErr != nil {
		return nil, q.reserveErr
	}
	q.mu.Lock()
	q.reserved = est
	q.mu.Unlock()
	return &domquota.Reservation{RequestID: "req_x", InstallID: id, Period: p, Reserved: est}, nil
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
	body        string
	aerr        *apierr.APIError
	breakerOpen bool
	// block, when non-nil, blocks Do until closed (simulates a slow upstream so a
	// second request finds no free slot).
	block <-chan struct{}
}

func (u *fakeUpstream) Do(ctx context.Context, _ []byte, _ bool, _ *config.Config) (UpstreamStream, *apierr.APIError) {
	if u.block != nil {
		select {
		case <-u.block:
		case <-ctx.Done():
			return nil, apierr.ErrUpstreamBusy
		}
	}
	if u.aerr != nil {
		return nil, u.aerr
	}
	return io.NopCloser(bytes.NewReader([]byte(u.body))), nil
}
func (u *fakeUpstream) BreakerOpen() bool { return u.breakerOpen }

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

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

type fakeMetrics struct {
	mu          sync.Mutex
	settleFails int
	rollFails   int
	upstream    map[string]int
}

func newFakeMetrics() *fakeMetrics         { return &fakeMetrics{upstream: map[string]int{}} }
func (m *fakeMetrics) Inflight(int)        {}
func (m *fakeMetrics) Upstream(o string)   { m.mu.Lock(); m.upstream[o]++; m.mu.Unlock() }
func (m *fakeMetrics) SettleFailure()      { m.mu.Lock(); m.settleFails++; m.mu.Unlock() }
func (m *fakeMetrics) RollbackFailure()    { m.mu.Lock(); m.rollFails++; m.mu.Unlock() }
func (m *fakeMetrics) settleFailures() int { m.mu.Lock(); defer m.mu.Unlock(); return m.settleFails }

// testCfg is a permissive config that passes every shape/cap gate.
func testCfg() *config.Config {
	return &config.Config{
		ModelAllowlist:     []string{"deepseek-chat"},
		DefaultModel:       "deepseek-chat",
		MonthlyQuota:       1000,
		MaxTokensCap:       1000,
		InputTokenCap:      1_000_000,
		MaxMessages:        100,
		MaxMessageChars:    100000,
		NGlobalConcurrency: 2,
		QueueWait:          50 * time.Millisecond,
		Location:           time.UTC,
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
	payload []byte
}

func (s *payloadSpy) Do(_ context.Context, payload []byte, _ bool, _ *config.Config) (UpstreamStream, *apierr.APIError) {
	s.payload = append([]byte(nil), payload...)
	return io.NopCloser(bytes.NewReader([]byte(`{"usage":{"total_tokens":1}}`))), nil
}
func (s *payloadSpy) BreakerOpen() bool { return false }

var errBoom = errors.New("boom")

const goodBody = `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
const goodStreamBody = `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
