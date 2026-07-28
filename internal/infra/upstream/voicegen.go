package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// VoiceGen is the DashScope voice-CLONING client (WRK-082 H9) — the third sibling of ImageGen and
// TTSGen, and the one whose resource outlives its request.
//
// **`qwen-tts` is the only reachable family, and the reason is the shape of the input.** DashScope
// has two cloning routes; CosyVoice's takes the reference clip ONLY as a publicly fetchable URL,
// while qwen-tts also accepts base64 (官方文档核准 2026-07-28). ADR 0011 forbids a managed media
// input that carries an address — a fetchable URL is an SSRF primitive aimed at our own network —
// so the free-but-URL-only family is not merely inconvenient here, it is unusable by construction.
// The reachable one costs $0.2 per voice created, charged at CREATE and never refunded, because
// what it produces is a registration that persists in our account until someone deletes it.
//
// VoiceGen 是 DashScope 音色**克隆** client(H9)——ImageGen 与 TTSGen 的第三个兄弟,也是唯一一个
// **资源比请求活得久**的那个。
//
// **`qwen-tts` 是唯一够得着的一支,而理由在于输入的形状。** DashScope 有两条克隆路由;CosyVoice 那条
// **只收公开可取的 URL**,而 qwen-tts 还收 base64(官方文档核准 2026-07-28)。ADR 0011 禁止受管媒体
// 输入携带地址——一个可取的 URL 就是一枚指向我们自己网络的 SSRF 原语——故那支「免费但只收 URL」的家族
// 在这里不只是不方便,而是**构造上不可用**。够得着的那支每创建一个音色 $0.2,**在创建时收、永不退**,
// 因为它产出的是一份留在我们账号里、直到有人删掉才消失的登记。
type VoiceGen struct {
	base    string // NATIVE DashScope origin, no trailing slash
	apiKey  string
	httpc   *http.Client
	timeout time.Duration
}

// voiceGenTimeout bounds one enrollment or deletion. Enrollment is an upload plus a synchronous
// answer, so this is a wedged-upstream ceiling rather than an expected duration.
//
// voiceGenTimeout 界一次登记或删除。登记是一次上传 + 同步应答,故这是「卡死上游」的顶棚、非预期时长。
const voiceGenTimeout = 90 * time.Second

// voiceCloneModel is the ENROLLMENT model id — the `model` field of the customization call, not the
// synthesis model. The voice it mints is then addressable by the ordinary TTS route.
//
// voiceCloneModel 是**登记**模型 id——customization 调用的 `model` 字段,不是合成模型。它铸出的音色
// 随后由普通 TTS 路径寻址。
const voiceCloneModel = "qwen-tts"

// NewVoiceGen builds the client over the native base and ONE key.
//
// NewVoiceGen 在原生 base 与**一把** key 上构建。
func NewVoiceGen(nativeBase, apiKey string) *VoiceGen {
	return &VoiceGen{
		base:    strings.TrimRight(strings.TrimSpace(nativeBase), "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: voiceGenTimeout},
		timeout: voiceGenTimeout,
	}
}

// EnrollVoice registers a reference clip and returns the upstream voice id.
//
// **An empty id on a 200 is treated as a failure, not as a voice.** Recording a row that points at
// nothing would consume an inventory slot the user can see but never use, and no later call could
// tell us whether a registration was actually created — the one outcome nothing can clean up.
//
// EnrollVoice 登记一段参考音频并返回上游 voice id。
//
// **200 但 id 为空,按失败处理、不当作一个音色。** 记下一行指向虚无的记录,会吃掉一个用户看得见却永远
// 用不了的库存位,而此后没有任何调用能告诉我们那边到底有没有真的建成——那正是没有任何东西清理得了的
// 那一种结局。
func (g *VoiceGen) EnrollVoice(ctx context.Context, name, sampleDataURL string) (string, bool, error) {
	body, unbilled, err := g.customization(ctx, map[string]any{
		"model": voiceCloneModel,
		"input": map[string]any{
			"action": "create",
			"voice":  name,
			"audio":  map[string]any{"data": sampleDataURL},
		},
	})
	if err != nil {
		return "", unbilled, err
	}
	var wire struct {
		Output struct {
			Voice   string `json:"voice"`
			VoiceID string `json:"voice_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		// A 200 we cannot read is AMBIGUOUS evidence: the provider may well have created (and
		// billed for) a voice we now cannot name. Keep the charge (GW-INV-50).
		// 一个读不懂的 200 是**歧义**证据:上游很可能已经创建了(并已计费)一个我们此刻叫不出名字的
		// 音色。**保留计费**(GW-INV-50)。
		return "", false, apierr.ErrUpstreamError
	}
	// Both spellings are read: the two cloning families answer with different keys, and a model swap
	// must not silently yield an empty id.
	// 两种拼法都读:两支克隆答的键不同,而换模型绝不能静默给出一个空 id。
	if id := strings.TrimSpace(wire.Output.VoiceID); id != "" {
		return id, false, nil
	}
	if id := strings.TrimSpace(wire.Output.Voice); id != "" {
		return id, false, nil
	}
	return "", false, apierr.ErrUpstreamError
}

// DeleteVoice removes a registration upstream. The caller deletes its own record only after this
// succeeds — see app/voice for why that ordering is the whole point.
//
// DeleteVoice 删掉上游的一份登记。调用方**只在这一步成功之后**才删自己的记录——为什么这个顺序就是全部
// 要害,见 app/voice。
func (g *VoiceGen) DeleteVoice(ctx context.Context, upstreamID string) error {
	_, _, err := g.customization(ctx, map[string]any{
		"model": voiceCloneModel,
		"input": map[string]any{"action": "delete", "voice": upstreamID},
	})
	return err
}

// customization is the shared transport for the two verbs — one resource's lifecycle over one
// endpoint, discriminated by `input.action`, not two services.
//
// customization 是那两个动词共用的传输——**一个**资源在**一条**端点上的生命周期,由 `input.action`
// 判别,不是两个服务。
// The bool is `unbilled`: true ONLY for an explicit pre-creation rejection, where the provider
// provably registered nothing and charged nothing. Everything else — transport failure, timeout,
// unreadable 200 — is ambiguous and keeps the charge (GW-INV-50).
//
// 那个 bool 是 `unbilled`:**只有**显式的创建前拒绝才为 true,那时上游可证明什么也没登记、什么也没收。
// 其余一切——传输失败、超时、读不懂的 200——都是歧义,**保留计费**(GW-INV-50)。
func (g *VoiceGen) customization(ctx context.Context, payload map[string]any) ([]byte, bool, error) {
	if g == nil || g.base == "" || g.apiKey == "" {
		// Nothing left this process, so nothing was billed.
		// 什么也没离开本进程,故什么也没被计费。
		return nil, true, apierr.ErrVoiceUnavailable
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, true, apierr.Internal()
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		g.base+"/api/v1/services/audio/tts/customization", bytes.NewReader(raw))
	if err != nil {
		return nil, true, apierr.Internal()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.httpc.Do(req)
	if err != nil {
		// Transport-level failure: the request may or may not have reached the provider —
		// ambiguous, keep the charge (GW-INV-50).
		// 传输层失败:请求可能已达上游——歧义,保留计费(GW-INV-50)。
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, false, apierr.ErrUpstreamTimeout
		}
		return nil, false, apierr.ErrUpstreamError
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, apierr.ErrUpstreamError
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, false, nil
	case http.StatusTooManyRequests:
		return nil, true, apierr.ErrUpstreamBusy
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		// Explicit pre-creation rejection: provably nothing was registered and nothing billed.
		// Upstream text is discarded (redaction iron rule).
		// 显式创建前拒绝:可证明什么也没登记、什么也没计费。上游原文丢弃(脱敏铁律)。
		return nil, true, apierr.UpstreamRejected(apierr.RejectedInvalid)
	default:
		return nil, false, apierr.ErrUpstreamError
	}
}
