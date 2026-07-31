package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// nativeClient is the shape every generation capability reaches DashScope's
// NATIVE API with: one origin, one key, one timeout, and no failover pool — a
// single deterministic reservation must map to a single upstream attempt.
//
// The three clients that use it (image, video, voice) had written this struct,
// its constructor and its round trip out three times, line for line.
//
// nativeClient 是每个生成能力抵达 DashScope **原生** API 的那个形状:一个 origin、一把 key、一个
// 超时,且**无 failover 池**——单次确定性预留必须对应单次上游尝试。
//
// 用它的三个 client(image/video/voice)把这个结构、它的构造器和它的往返写了三遍,逐行相同。
type nativeClient struct {
	base    string // NATIVE DashScope origin (DASHSCOPE_NATIVE_BASE), no trailing slash
	apiKey  string
	httpc   *http.Client
	timeout time.Duration
}

// nativeReplyCap bounds what we will read back. A provider reply is a small JSON
// envelope; anything past this is not an answer we can use, and reading it
// unbounded is how one bad upstream turns into our OOM.
//
// nativeReplyCap 界住我们愿意读回来的量。上游回包是一个小 JSON 信封;超出这个的不是我们用得上的
// 答案,而不设界地读它正是「一个坏上游变成我们 OOM」的路子。
const nativeReplyCap = 1 << 20

func newNativeClient(nativeBase, apiKey string, timeout time.Duration) nativeClient {
	return nativeClient{
		base:    strings.TrimRight(strings.TrimSpace(nativeBase), "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

// configured reports whether this client can reach anything at all. A false here
// means nothing will leave the process.
//
// configured 报告本 client 究竟能不能抵达任何东西。false 意味着**什么也不会离开本进程**。
func (c nativeClient) configured() bool {
	return c.base != "" && c.apiKey != ""
}

// do performs one native round trip and hands back the provider's raw body and
// status code.
//
// The error it returns is ONLY ever a request-build or transport failure —
// Internal() (500) for the former, ErrUpstreamTimeout/ErrUpstreamError for the
// latter. Status mapping is deliberately NOT done here: what a 429 or a 404 costs
// differs per capability, and that is a decision about money, which must stay
// where a reader of that capability can see it rather than being buried in shared
// plumbing.
//
// do 跑一次原生往返,交回上游的原始 body 与状态码。
//
// 它返回的错误**只可能**是请求构造失败或传输失败——前者 Internal()(500),后者
// ErrUpstreamTimeout/ErrUpstreamError。状态码映射**刻意不在这里做**:一个 429 或一个 404 值多少钱
// 逐能力不同,而那是**关于钱的决定**,必须留在读那个能力的人看得见的地方,不能埋进共享管道里。
func (c nativeClient) do(ctx context.Context, method, endpoint string, payload []byte, async bool) ([]byte, int, *apierr.APIError) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(cctx, method, endpoint, body)
	if err != nil {
		return nil, 0, apierr.Internal()
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if async {
		req.Header.Set("X-DashScope-Async", "enable")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		// Transport-level failure: the request may or may not have reached the
		// provider — ambiguous, so the caller keeps the charge (GW-INV-50). A client
		// cancellation mid-generation is equally ambiguous.
		// 传输层失败:请求可能已达上游——歧义,故调用方保留计费(GW-INV-50)。生成中途的取消同样歧义。
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, apierr.ErrUpstreamTimeout
		}
		return nil, 0, apierr.ErrUpstreamError
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, nativeReplyCap))
	if err != nil {
		return nil, 0, apierr.ErrUpstreamError
	}
	return raw, resp.StatusCode, nil
}

// post is do's common case.
//
// post 是 do 的常见情形。
func (c nativeClient) post(ctx context.Context, endpoint string, payload []byte) ([]byte, int, *apierr.APIError) {
	return c.do(ctx, http.MethodPost, endpoint, payload, false)
}

// rejectedBeforeGeneration is the status set every capability agrees is a
// provably-unbilled explicit rejection: the provider refused the request itself,
// so nothing was generated and nothing was charged. Upstream text is discarded
// (redaction iron rule), so the reason is our own closed enum.
//
// rejectedBeforeGeneration 是每个能力都认同的那组状态:**可证明未计费的显式拒绝**——上游拒的是请求
// 本身,故什么也没生成、什么也没计费。上游原文丢弃(脱敏铁律),故 reason 用我们自己的封闭枚举。
func rejectedBeforeGeneration(status int) bool {
	switch status {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}
