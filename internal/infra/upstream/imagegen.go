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

// ImageGen is the sync DashScope image-generation client (WRK-082 批B, 代拍 B1):
// one POST to the NATIVE multimodal-generation endpoint returns the artifact's
// 24h OSS URL — no task polling. It follows the package's iron rules: the key is
// injected only into the outgoing request, upstream body/header text never
// passes through an error, and every non-2xx is normalized to an *apierr
// sentinel with a provable unbilled classification (explicit 4xx reject = the
// provider generated nothing; everything ambiguous keeps the charge).
//
// ImageGen 是同步 DashScope 图像生成 client(批B,代拍 B1):一次 POST 原生
// multimodal-generation 端点直接返回产物 24h OSS URL——无任务轮询。守本包铁律:key 只注入
// 出站请求、上游 body/header 文本绝不经错误透传、非 2xx 一律归一 apierr sentinel 并给出可证明
// 的 unbilled 分类(显式 4xx 拒=上游没生成;歧义一律保留计费)。
type ImageGen struct {
	base    string // NATIVE DashScope origin (DASHSCOPE_NATIVE_BASE), no trailing slash
	apiKey  string
	httpc   *http.Client
	timeout time.Duration
}

// imageGenTimeout bounds one whole sync generation. Generations run tens of
// seconds; 120s is far above healthy p99 while still ending a wedged upstream
// well inside the desktop's chat-turn ceiling.
//
// imageGenTimeout 界一次完整同步生成。生成十秒量级;120s 远高于健康 p99,又在桌面聊天回合
// 顶棚内了断卡死的上游。
const imageGenTimeout = 120 * time.Second

// NewImageGen builds the client over the native base and ONE key (the image
// route has no failover pool — a single deterministic reservation must map to a
// single upstream attempt).
//
// NewImageGen 在原生 base 与**一把** key 上构建(图像路由无 failover 池——单次确定性预留必须
// 对应单次上游尝试)。
func NewImageGen(nativeBase, apiKey string) *ImageGen {
	return &ImageGen{
		base:    strings.TrimRight(strings.TrimSpace(nativeBase), "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: imageGenTimeout},
		timeout: imageGenTimeout,
	}
}

type dashScopeImageReq struct {
	Model string `json:"model"`
	Input struct {
		Messages []dashScopeImageMsg `json:"messages"`
	} `json:"input"`
	Parameters dashScopeImageParams `json:"parameters"`
}

type dashScopeImageMsg struct {
	Role    string                   `json:"role"`
	Content []dashScopeImageMsgChunk `json:"content"`
}

type dashScopeImageMsgChunk struct {
	Text string `json:"text"`
}

type dashScopeImageParams struct {
	Size      string `json:"size,omitempty"`
	N         int    `json:"n"`
	Watermark bool   `json:"watermark"`
}

// GenerateImage runs one sync generation. size arrives in the gateway's WxH wire
// form and is translated to DashScope's W*H spelling here (the dialect boundary).
//
// GenerateImage 跑一次同步生成。size 以网关线缆 WxH 形到达,在此译成 DashScope 的 W*H 拼法
// (方言边界)。
func (g *ImageGen) GenerateImage(ctx context.Context, model, prompt, size string) (string, bool, error) {
	if g == nil || g.base == "" || g.apiKey == "" {
		return "", false, apierr.ErrImageUnavailable
	}
	reqBody := dashScopeImageReq{Model: model}
	reqBody.Input.Messages = []dashScopeImageMsg{{
		Role:    "user",
		Content: []dashScopeImageMsgChunk{{Text: prompt}},
	}}
	reqBody.Parameters = dashScopeImageParams{
		Size:      strings.ReplaceAll(size, "x", "*"),
		N:         1,
		Watermark: false,
	}
	payload, err := json.Marshal(reqBody)
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
		// Transport-level failure: the request may or may not have reached the
		// provider — ambiguous, keep the charge (GW-INV-50). Client cancellation is
		// equally ambiguous mid-generation.
		// 传输层失败:请求可能已达上游——歧义,保留计费(GW-INV-50)。生成中途的取消同样歧义。
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
		u, perr := parseDashScopeImageURL(body)
		if perr != nil {
			// A 200 without a parseable artifact is ambiguous evidence: the provider
			// may have generated (and billed) — keep the charge.
			// 200 却解析不出产物是歧义证据:上游可能已生成(已计费)——保留计费。
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
		// Explicit pre-generation rejection: provably unbilled → the caller rolls
		// back. Upstream text is discarded (redaction iron rule).
		// 显式生成前拒绝:可证明未计费 → 调用方回滚。上游原文丢弃(脱敏铁律)。
		return "", true, apierr.UpstreamRejected(apierr.RejectedInvalid)
	default:
		return "", false, apierr.ErrUpstreamError
	}
}

// parseDashScopeImageURL extracts the single artifact URL from the sync
// response (output.choices[0].message.content[*].image) and requires it to be a
// well-formed absolute https URL — the only thing the gateway relays (P13).
//
// parseDashScopeImageURL 从同步响应取唯一产物 URL,并要求是合法绝对 https URL——网关直通的
// 唯一之物(P13)。
func parseDashScopeImageURL(body []byte) (string, error) {
	var wire struct {
		Output struct {
			Choices []struct {
				Message struct {
					Content []struct {
						Image string `json:"image"`
					} `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return "", err
	}
	for _, c := range wire.Output.Choices {
		for _, chunk := range c.Message.Content {
			raw := strings.TrimSpace(chunk.Image)
			if raw == "" {
				continue
			}
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return "", fmt.Errorf("upstream image url malformed")
			}
			return raw, nil
		}
	}
	return "", fmt.Errorf("no image artifact in upstream response")
}
