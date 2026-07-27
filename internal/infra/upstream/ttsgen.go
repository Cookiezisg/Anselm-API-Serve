package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// TTSGen is the sync DashScope speech-synthesis client (WRK-082 批C). It is the
// ImageGen twin and follows the same iron rules — key only on the outgoing
// request, upstream text never relayed through an error, every non-2xx
// normalized to an *apierr sentinel with a provable unbilled classification.
//
// It shares ImageGen's endpoint (`multimodal-generation/generation`) because
// DashScope has NO OpenAI-compatible TTS route: `/compatible-mode/v1/audio/speech`
// does not exist (三处独立证据,含第三方对该端点的 404 实测). The native path is
// the only one there is, so the "OpenAI shape" the working doc originally
// assumed was never available here.
//
// TTSGen 是同步 DashScope 语音合成 client(批C),ImageGen 的孪生件、守同一套铁律。它与
// ImageGen 共用端点,因为 DashScope **没有** OpenAI 兼容的 TTS 路由——
// `/compatible-mode/v1/audio/speech` 不存在(三处独立证据,含第三方 404 实测)。原生路径是
// 唯一存在的那条,故工单原先假设的「OpenAI 形」在这里从来就不可得。
type TTSGen struct {
	base    string // NATIVE DashScope origin (DASHSCOPE_NATIVE_BASE), no trailing slash
	apiKey  string
	httpc   *http.Client
	timeout time.Duration
}

// ttsGenTimeout bounds one whole sync synthesis. Synthesis is seconds, not tens
// of seconds like image generation — but the ceiling exists to end a wedged
// upstream, not to express the happy path, so it stays generous.
//
// ttsGenTimeout 界一次完整同步合成。合成是秒级、不像出图十秒级——但这个顶棚是为了了断卡死的
// 上游、不是为了表达顺利路径,故留宽。
const ttsGenTimeout = 60 * time.Second

// NewTTSGen builds the client over the native base and ONE key (no failover
// pool — a single deterministic reservation must map to a single attempt).
//
// NewTTSGen 在原生 base 与**一把** key 上构建(无 failover 池——单次确定性预留必须对应单次尝试)。
func NewTTSGen(nativeBase, apiKey string) *TTSGen {
	return &TTSGen{
		base:    strings.TrimRight(strings.TrimSpace(nativeBase), "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: ttsGenTimeout},
		timeout: ttsGenTimeout,
	}
}

// dashScopeTTSReq is the native request shape: a nested `input` object, NO
// `parameters` block and NO format field — qwen3-tts always answers 24kHz
// 16-bit mono WAV. A `format` on the gateway wire would therefore be a promise
// the upstream cannot keep (代拍 C3).
//
// dashScopeTTSReq 是原生请求形:嵌套 `input` 对象,**无** `parameters` 块、**无** format 字段——
// qwen3-tts 恒返 24kHz/16bit/mono WAV。故网关线缆上放一个 `format` 等于许一个上游兑现不了的诺
// (代拍 C3)。
type dashScopeTTSReq struct {
	Model string            `json:"model"`
	Input dashScopeTTSInput `json:"input"`
}

type dashScopeTTSInput struct {
	Text  string `json:"text"`
	Voice string `json:"voice"`
}

// GenerateSpeech runs one sync synthesis and returns the artifact's OSS URL —
// the same P13 URL-passthrough contract as images: the gateway never holds the
// bytes, so a long article's audio never becomes gateway memory or egress.
//
// GenerateSpeech 跑一次同步合成、返回产物 OSS URL——与图像同一条 P13 URL 直通契约:网关从不
// 持有字节,故一篇长文的音频永远不会变成网关的内存与出口流量。
func (g *TTSGen) GenerateSpeech(ctx context.Context, model, text, voice string) (string, bool, error) {
	if g == nil || g.base == "" || g.apiKey == "" {
		return "", false, apierr.ErrTTSUnavailable
	}
	payload, err := json.Marshal(dashScopeTTSReq{
		Model: model,
		Input: dashScopeTTSInput{Text: text, Voice: voice},
	})
	if err != nil {
		return "", false, apierr.Internal()
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost,
		g.base+"/api/v1/services/aigc/multimodal-generation/generation", bytes.NewReader(payload))
	if err != nil {
		return "", false, apierr.Internal()
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.httpc.Do(httpReq)
	if err != nil {
		// Ambiguous: the request may or may not have reached the provider — keep the
		// charge (GW-INV-50). 歧义:请求可能已达上游——保留计费(GW-INV-50)。
		if errors.Is(err, context.DeadlineExceeded) {
			return "", false, apierr.ErrUpstreamTimeout
		}
		return "", false, apierr.ErrUpstreamError
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false, apierr.ErrUpstreamError
	}

	switch resp.StatusCode {
	case http.StatusOK:
		u, perr := parseDashScopeAudioURL(body)
		if perr != nil {
			// A 200 without a parseable artifact is ambiguous evidence: the provider may
			// have synthesized (and billed) — keep the charge.
			// 200 却解析不出产物是歧义证据:上游可能已合成(已计费)——保留计费。
			return "", false, apierr.ErrUpstreamError
		}
		return u, false, nil
	case http.StatusTooManyRequests:
		return "", false, apierr.ErrUpstreamBusy
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		// Explicit pre-synthesis rejection: provably unbilled → the caller rolls back.
		// 显式合成前拒绝:可证明未计费 → 调用方回滚。
		return "", true, apierr.UpstreamRejected(apierr.RejectedInvalid)
	default:
		return "", false, apierr.ErrUpstreamError
	}
}

// parseDashScopeAudioURL extracts the artifact URL from the sync response
// (output.audio.url) and normalizes it to https.
//
// The scheme normalization is deliberate (代拍 C6): DashScope hands audio back
// on an OSS result host that may answer with an `http://` URL, while BOTH ends
// of this system refuse plaintext artifact fetches — the desktop's downloader is
// https-only by iron rule. An OSS pre-signed signature covers the path and query,
// not the scheme, so upgrading is safe; the real-money smoke test is what proves
// it, and a 403 there would be the signal that this assumption is wrong.
//
// parseDashScopeAudioURL 从同步响应取产物 URL(output.audio.url)并归一到 https。
// scheme 归一是刻意的(代拍 C6):DashScope 可能返回 `http://` 的 OSS 结果 URL,而本系统**两端**
// 都拒绝明文取产物——桌面下载器按铁律只认 https。OSS 预签名覆盖 path 与 query、不覆盖 scheme,
// 故升级是安全的;真钱冒烟测试才是证明,那里出 403 就是这条假设被推翻的信号。
func parseDashScopeAudioURL(body []byte) (string, error) {
	var wire struct {
		Output struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(wire.Output.Audio.URL)
	if raw == "" {
		return "", fmt.Errorf("no audio artifact in upstream response")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("upstream audio url malformed")
	}
	u.Scheme = "https"
	return u.String(), nil
}
