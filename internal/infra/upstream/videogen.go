package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
)

// VideoGen is the ASYNC DashScope video client. Unlike its image and
// speech siblings it has two verbs, because the video family has no synchronous
// form at all: submission returns a task id, and the artifact appears minutes
// later. The gateway never waits — it hands the task back and the desktop polls,
// so a 3-minute generation does not occupy a gateway request slot (N_GLOBAL is 8;
// three simultaneous videos would be a third of the whole box).
//
// VideoGen 是**异步** DashScope 视频 client。与图像、语音两个兄弟不同,它有**两个**动词——
// 因为视频族**根本没有**同步形态:提交返回一个 task id,产物几分钟后才出现。网关**从不等待**——
// 它把任务交回、由桌面端轮询,于是一次 3 分钟的生成不会占住一个网关请求位(N_GLOBAL 是 8;三条
// 同时在跑的视频就是整台机器的三分之一)。
type VideoGen struct {
	c nativeClient
}

// videoGenTimeout bounds one SUBMIT or one POLL — not one generation. Both are
// ordinary short JSON round-trips; the minutes live entirely between them, on the
// client's clock.
//
// videoGenTimeout 界一次**提交**或一次**轮询**——不是一次生成。两者都是普通的短 JSON 往返;那几
// 分钟完全活在它们**之间**、活在客户端的钟上。
const videoGenTimeout = 30 * time.Second

// NewVideoGen builds the client over the native base and ONE key (same rule as
// the image route: a single deterministic reservation, a single upstream attempt).
//
// NewVideoGen 在原生 base 与**一把** key 上构建(与图像路由同律:单次确定性预留、单次上游尝试)。
func NewVideoGen(nativeBase, apiKey string) *VideoGen {
	return &VideoGen{c: newNativeClient(nativeBase, apiKey, videoGenTimeout)}
}

// VideoStatus is one poll's answer in the gateway's own vocabulary. URL is set
// only on PhaseSucceeded and is a bare pre-signed OSS link the client fetches
// WITHOUT any Authorization header — sending one can make the object store
// reject the request.
//
// VideoStatus 是一次轮询的答案,用网关自己的词表。URL 只在 PhaseSucceeded 时有值,且是一条裸的
// 预签名 OSS 链接,客户端取它时**不带**任何 Authorization 头——送一个反而可能被对象存储拒绝。
type VideoStatus struct {
	Phase domvideo.Phase
	URL   string
}

// SubmitVideo submits one generation and returns the upstream task id. The
// `X-DashScope-Async: enable` header is MANDATORY — without it the API answers
// "current user api does not support synchronous calls".
//
// unbilled follows the family rule (GW-INV-50): true ONLY for an explicit
// pre-generation rejection. Here it is stronger than for images — a rejected
// SUBMIT provably produced no video, because the provider had not even queued a
// task. Ambiguity (timeout, 5xx, unparseable 200) still keeps the charge.
//
// SubmitVideo 提交一次生成并返回上游 task id。`X-DashScope-Async: enable` 头是**强制**的——缺了
// 它 API 会答「current user api does not support synchronous calls」。
//
// unbilled 守本族规矩(GW-INV-50):**仅**显式的生成前拒绝为 true。这里比图像那边更强——一次被拒的
// **提交**可证明没产出任何视频,因为上游连任务都还没排。歧义(超时、5xx、200 却解析不出)仍保留计费。
func (g *VideoGen) SubmitVideo(ctx context.Context, model, prompt string, seconds int, ratio, resolution string) (string, bool, error) {
	if g == nil || !g.c.configured() {
		return "", false, apierr.ErrVideoUnavailable
	}
	params := map[string]any{
		"duration":  seconds,
		"watermark": false,
	}
	input := map[string]any{"prompt": prompt}
	switch {
	case strings.HasPrefix(model, "wan2.7"):
		// wan2.7 replaced the single `size` with resolution + ratio; 2.6 and earlier still
		// take `size`. Same provider, two shapes — branch on the model, never guess.
		// wan2.7 把单一 `size` 换成了 resolution + ratio;2.6 及更早仍吃 `size`。同一家两种形,
		// 按模型分支、绝不猜。
		params["resolution"] = resolution
		params["ratio"] = ratio
	default:
		params["size"] = legacyVideoSize(resolution, ratio)
	}
	return g.submit(ctx, model, input, params)
}

// SubmitAnimation speaks Wan 2.7's new image-to-video protocol. It shares the
// asynchronous endpoint with T2V, but not its input shape: the first frame is a
// typed media item, never the legacy img_url field. Resolution remains explicit
// so the provider cannot default a 720P-priced request to 1080P; ratio is omitted
// because the output follows the frame.
func (g *VideoGen) SubmitAnimation(ctx context.Context, model, prompt string, seconds int, resolution, firstFrame string) (string, bool, error) {
	if g == nil || !g.c.configured() {
		return "", false, apierr.ErrVideoUnavailable
	}
	input := map[string]any{
		"prompt": prompt,
		"media":  []map[string]string{{"type": "first_frame", "url": firstFrame}},
	}
	params := map[string]any{
		"duration": seconds, "resolution": resolution, "watermark": false,
	}
	return g.submit(ctx, model, input, params)
}

func (g *VideoGen) submit(ctx context.Context, model string, input, params map[string]any) (string, bool, error) {
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"input":      input,
		"parameters": params,
	})
	if err != nil {
		return "", false, apierr.Internal()
	}
	raw, unbilled, aerr := g.roundTrip(ctx, http.MethodPost,
		g.c.base+"/api/v1/services/aigc/video-generation/video-synthesis", payload, true)
	if aerr != nil {
		return "", unbilled, aerr
	}
	var wire struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || strings.TrimSpace(wire.Output.TaskID) == "" {
		// A 200 with no task id is ambiguous evidence: the provider may have queued
		// (and will bill) something we can no longer name — keep the charge.
		// 200 却没有 task id 是歧义证据:上游可能已经排了(并将计费)一个我们再也叫不出名字的
		// 任务——保留计费。
		return "", false, apierr.ErrUpstreamError
	}
	return wire.Output.TaskID, false, nil
}

// PollVideo reads one task's state. The poll deliberately does NOT carry the
// async header — that header belongs to submission only.
//
// An unrecognized upstream status is reported as RUNNING rather than FAILED: the
// closed set belongs to the VENDOR, and a new member appearing in it must not
// turn a healthy job into an error for every user at once.
//
// PollVideo 读一次任务状态。轮询刻意**不带**异步头——那个头只属于提交。
//
// 无法识别的上游状态报为 RUNNING、不报 FAILED:封闭集是**厂商**的,它里面冒出一个新成员,不该
// 在同一瞬间把所有用户的健康任务都变成错误。
func (g *VideoGen) PollVideo(ctx context.Context, taskID string) (VideoStatus, error) {
	if g == nil || !g.c.configured() {
		return VideoStatus{}, apierr.ErrVideoUnavailable
	}
	raw, _, aerr := g.roundTrip(ctx, http.MethodGet, g.c.base+"/api/v1/tasks/"+url.PathEscape(taskID), nil, false)
	if aerr != nil {
		return VideoStatus{}, aerr
	}
	var wire struct {
		Output struct {
			TaskStatus string `json:"task_status"`
			VideoURL   string `json:"video_url"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return VideoStatus{}, apierr.ErrUpstreamError
	}
	switch wire.Output.TaskStatus {
	case "PENDING":
		return VideoStatus{Phase: domvideo.PhasePending}, nil
	case "SUCCEEDED":
		u, err := url.Parse(strings.TrimSpace(wire.Output.VideoURL))
		if err != nil || u.Scheme != "https" || u.Host == "" {
			// Succeeded with nothing fetchable is an upstream contract break, not a
			// user-visible failure state — say so as an upstream error.
			// 成功却没有可取之物是上游契约破裂,不是一个用户可见的失败态——按上游错误说出来。
			return VideoStatus{}, apierr.ErrUpstreamError
		}
		return VideoStatus{Phase: domvideo.PhaseSucceeded, URL: u.String()}, nil
	case "FAILED", "CANCELED", "UNKNOWN":
		// The upstream's failure text is NOT relayed (redaction iron rule): it can
		// carry account, request and model identifiers the client has no business
		// reading. The phase alone is what the desktop acts on.
		// 上游的失败文本**不转发**(脱敏铁律):它可能带着账号、请求与模型标识,客户端没有理由读到。
		// 桌面端要据以行动的只是这个 phase。
		return VideoStatus{Phase: domvideo.PhaseFailed}, nil
	default:
		return VideoStatus{Phase: domvideo.PhaseRunning}, nil
	}
}

// roundTrip is the shared request/normalize path for both verbs — the ONE place
// the key is injected, the body is bounded, and every non-2xx becomes an apierr
// sentinel with its unbilled classification. Upstream body text never escapes it.
//
// roundTrip 是两个动词共用的请求/归一路径——key 只在此注入、body 只在此设界、非 2xx 只在此变成
// 带 unbilled 分类的 apierr sentinel。上游 body 文本绝不从这里逃出去。
func (g *VideoGen) roundTrip(ctx context.Context, method, endpoint string, payload []byte, async bool) ([]byte, bool, error) {
	body, status, ae := g.c.do(ctx, method, endpoint, payload, async)
	if ae != nil {
		return nil, false, ae
	}
	switch {
	case status == http.StatusOK:
		return body, false, nil
	case status == http.StatusNotFound:
		// Only reachable on a poll: a signed handle whose task the provider has
		// forgotten (expired). Distinct from a bad signature — the caller did own it.
		// 只在轮询时可达:签名有效、但上游已忘掉(过期)的任务。与坏签名不同——调用方**确实**拥有它。
		return nil, true, apierr.ErrVideoTaskNotFound
	case status == http.StatusTooManyRequests:
		// Explicit refusal of the request; nothing was queued and nothing was billed.
		// Same rule as the image and voice clients, and as the chat path.
		// 对请求的显式拒绝;什么也没排队、什么也没计费。与图像、音色 client 及 chat 路径同律。
		return nil, true, apierr.ErrUpstreamBusy
	case rejectedBeforeGeneration(status):
		return nil, true, apierr.UpstreamRejected(apierr.RejectedInvalid)
	default:
		return nil, false, apierr.ErrUpstreamError
	}
}

// legacyVideoSize renders the pre-2.7 `size` spelling from the 2.7 pair. Kept so
// an operator can pin an older wan model without the route silently sending
// parameters it ignores.
//
// legacyVideoSize 用 2.7 的那一对渲出 2.7 之前的 `size` 拼法。留着,是为了让运营者可以钉一个更老的
// wan 模型,而路由不会静默地送出它根本不看的参数。
func legacyVideoSize(resolution, ratio string) string {
	if resolution == "1080P" {
		switch ratio {
		case "9:16":
			return "1080*1920"
		case "1:1":
			return "1440*1440"
		default:
			return "1920*1080"
		}
	}
	switch ratio {
	case "9:16":
		return "720*1280"
	case "1:1":
		return "960*960"
	default:
		return "1280*720"
	}
}
