package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// VoiceGen is the DashScope voice-CLONING client (WRK-082 H9).
//
// **Every line of this file was written against the real API, because the documented shape we
// first built to did not exist.** The first implementation posted `model: "qwen-tts"` with
// `action: create` and a base64 `audio.data`; the live endpoint answers `Model not exist.` to that
// model id and rejects data URLs outright. Mock tests were green throughout — the whole contract
// was fiction. What the endpoint actually serves (真机实测 2026-07-28):
//
//	model   voice-enrollment
//	action  create_voice / query_voice / delete_voice   (`create`/`delete` → "invalid action")
//	input   url  — a PUBLICLY FETCHABLE address. A data: URL makes it run ASR on the literal
//	             string and 500. There is no base64 path at all.
//	async   create_voice returns immediately with a voice_id in status DEPLOYING; it is not usable
//	             until query_voice reports OK.
//
// **The user's name never reaches the upstream.** `create_voice` takes a `prefix` (a namespace the
// provider prepends to the id it mints), not a name. Our name lives in our own table; sending it
// here would leak a user's words into a shared provider account for no benefit.
//
// VoiceGen 是 DashScope 音色**克隆** client(H9)。
//
// **本文件每一行都是对着真 API 写的,因为我们最初照着建的那个「文档形状」根本不存在。** 第一版发的是
// `model: "qwen-tts"` + `action: create` + base64 的 `audio.data`;线上端点对那个 model id 答
// `Model not exist.`,并且**直接拒绝** data URL。而 mock 测试全程是绿的——整份契约是虚构的。
// 端点真正提供的(真机实测 2026-07-28):
//
//	model   voice-enrollment
//	action  create_voice / query_voice / delete_voice  (`create`/`delete` → "invalid action")
//	input   url —— 一个**公网可取**的地址。data: URL 会让它把那串字面量拿去跑 ASR 然后 500。
//	             **根本没有 base64 这条路。**
//	async   create_voice 立刻返回一个状态为 DEPLOYING 的 voice_id;在 query_voice 报 OK 之前不可用。
//
// **用户起的名字永远不上游。** `create_voice` 收的是 `prefix`(供应商拼在它铸出的 id 前面的命名空间),
// 不是名字。我们的名字住在我们自己的表里;把它发上去等于把用户的措辞白白泄进一个共享的 provider
// 账号。
type VoiceGen struct {
	c nativeClient
}

// voiceGenTimeout bounds one customization call.
//
// voiceGenTimeout 界一次 customization 调用。
const voiceGenTimeout = 90 * time.Second

const (
	// voiceEnrollmentModel is the ENROLLMENT service, not a synthesis model.
	// voiceEnrollmentModel 是**登记服务**,不是合成模型。
	voiceEnrollmentModel = "voice-enrollment"

	// voiceTargetModel is the synthesis model a minted voice will be usable with. It must match the
	// model the TTS route actually calls — a voice enrolled against one family cannot be spoken by
	// another, and the mismatch only shows up at synthesis time, long after the money is spent.
	// voiceTargetModel 是铸出的音色**将来能被谁用**的那个合成模型。它必须与 TTS 路由真正调用的模型
	// 一致——登记在一支上的音色,另一支说不出来,而这个不匹配**只在合成时才暴露**,那时钱早花完了。
	voiceTargetModel = "qwen-audio-3.0-tts-flash"

	// voicePrefix namespaces the ids this deployment mints. It is a constant rather than anything
	// per-install: the provider appends its own entropy, and threading an install id through here
	// would put our tenant identifiers inside a shared provider account.
	// voicePrefix 给本部署铸出的 id 做命名空间。它是**常量**而不是逐 install 的东西:供应商自己会
	// 追加熵,而把 install id 穿过来等于把我们的租户标识放进一个共享的 provider 账号里。
	voicePrefix = "anselm"
)

// NewVoiceGen builds the client over the native base and ONE key.
//
// NewVoiceGen 在原生 base 与**一把** key 上构建。
func NewVoiceGen(nativeBase, apiKey string) *VoiceGen {
	return &VoiceGen{c: newNativeClient(nativeBase, apiKey, voiceGenTimeout)}
}

// EnrollVoice registers a reference clip reachable at sampleURL and returns the upstream voice id.
//
// The returned voice is NOT yet usable — see AwaitVoiceReady. Returning here without waiting is
// deliberate: the money is already spent at this point, so the caller must be able to record the
// id before it starts waiting on anything.
//
// EnrollVoice 登记一段位于 sampleURL 的参考音频,返回上游 voice id。
//
// 返回的音色**还不能用**——见 AwaitVoiceReady。在这里不等就返回是刻意的:钱到这一步已经花掉了,
// 故调用方必须能在开始等任何东西**之前**先把 id 记下来。
func (g *VoiceGen) EnrollVoice(ctx context.Context, sampleURL string) (string, bool, error) {
	body, unbilled, err := g.customization(ctx, map[string]any{
		"model": voiceEnrollmentModel,
		"input": map[string]any{
			"action":       "create_voice",
			"target_model": voiceTargetModel,
			"prefix":       voicePrefix,
			"url":          sampleURL,
		},
	})
	if err != nil {
		return "", unbilled, err
	}
	id, err := parseVoiceID(body)
	if err != nil {
		// A 200 we cannot read is AMBIGUOUS: the provider may well have created (and billed for) a
		// voice we now cannot name — the one outcome nothing can clean up. Keep the charge.
		// 一个读不懂的 200 是**歧义**:上游很可能已经创建了(并已计费)一个我们此刻叫不出名字的音色
		// ——那正是没有任何东西清理得了的结局。保留计费。
		return "", false, apierr.ErrUpstreamError
	}
	return id, false, nil
}

// voiceReadyPoll / voiceReadyBudget bound the wait for deployment. Enrollment took ~15s in the
// real-money probe; the budget is generous because a voice that is merely slow must not be
// abandoned — we have already paid for it.
//
// voiceReadyPoll / voiceReadyBudget 界等待部署的过程。真钱探测里登记约 15 秒完成;预算给得宽,因为
// 一个**只是慢**的音色绝不能被丢下——我们已经为它付过钱了。
const (
	voiceReadyPoll   = 3 * time.Second
	voiceReadyBudget = 3 * time.Minute
)

// AwaitVoiceReady blocks until the upstream reports the voice deployed.
//
// **This step is why the first implementation would have shipped broken.** `create_voice` answers
// 200 with a usable-looking id while the voice is still DEPLOYING; synthesizing with it before OK
// fails. A client that recorded the id and returned would hand the user a voice that does not work
// yet — and the failure would look like a synthesis bug, not an enrollment one.
//
// AwaitVoiceReady 阻塞到上游报告音色已部署。
//
// **正是这一步,会让第一版实现带着 bug 发出去。** `create_voice` 在音色还处于 DEPLOYING 时就答 200
// 并给出一个看着能用的 id;在 OK 之前拿它合成会失败。一个记下 id 就返回的客户端,会把一个**还不能用**
// 的音色交给用户——而那个失败看起来像是合成的 bug、不像登记的。
func (g *VoiceGen) AwaitVoiceReady(ctx context.Context, upstreamID string) error {
	deadline := time.Now().Add(voiceReadyBudget)
	for {
		body, _, err := g.customization(ctx, map[string]any{
			"model": voiceEnrollmentModel,
			"input": map[string]any{"action": "query_voice", "voice_id": upstreamID},
		})
		if err != nil {
			return err
		}
		switch parseVoiceStatus(body) {
		case "OK":
			return nil
		case "FAILED":
			return apierr.ErrUpstreamError
		}
		if time.Now().After(deadline) {
			// Out of patience, not out of voice: the registration exists and is paid for, so the
			// caller keeps the record and the user can use it once the provider finishes.
			// 是我们没耐心了,不是音色没了:那份登记存在且已付费,故调用方**保留记录**,用户在上游
			// 完成之后照样能用。
			return apierr.ErrUpstreamTimeout
		}
		select {
		case <-ctx.Done():
			return apierr.ErrUpstreamTimeout
		case <-time.After(voiceReadyPoll):
		}
	}
}

// DeleteVoice removes a registration upstream. The caller deletes its own record only after this
// succeeds — see app/voice for why that ordering is the whole point.
//
// DeleteVoice 删掉上游的一份登记。调用方**只在这一步成功之后**才删自己的记录——为什么这个顺序就是
// 全部要害,见 app/voice。
func (g *VoiceGen) DeleteVoice(ctx context.Context, upstreamID string) error {
	_, _, err := g.customization(ctx, map[string]any{
		"model": voiceEnrollmentModel,
		"input": map[string]any{"action": "delete_voice", "voice_id": upstreamID},
	})
	return err
}

func parseVoiceID(body []byte) (string, error) {
	var wire struct {
		Output struct {
			VoiceID string `json:"voice_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return "", err
	}
	if id := strings.TrimSpace(wire.Output.VoiceID); id != "" {
		return id, nil
	}
	return "", errors.New("upstream minted no voice id")
}

func parseVoiceStatus(body []byte) string {
	var wire struct {
		Output struct {
			Status string `json:"status"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(wire.Output.Status))
}

// customization is the shared transport for the three verbs — one resource's lifecycle over one
// endpoint, discriminated by `input.action`, not three services.
//
// The bool is `unbilled`: true ONLY for an explicit pre-creation rejection, where the provider
// provably registered nothing and charged nothing. Everything else — transport failure, timeout,
// unreadable 200 — is ambiguous and keeps the charge (GW-INV-50).
//
// customization 是那三个动词共用的传输——**一个**资源在**一条**端点上的生命周期,由 `input.action`
// 判别,不是三个服务。
//
// 那个 bool 是 `unbilled`:**只有**显式的创建前拒绝才为 true,那时上游可证明什么也没登记、什么也没收。
// 其余一切——传输失败、超时、读不懂的 200——都是歧义,**保留计费**(GW-INV-50)。
func (g *VoiceGen) customization(ctx context.Context, payload map[string]any) ([]byte, bool, error) {
	if g == nil || !g.c.configured() {
		// Nothing left this process, so nothing was billed.
		// 什么也没离开本进程,故什么也没被计费。
		return nil, true, apierr.ErrVoiceUnavailable
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, true, apierr.Internal()
	}
	body, status, ae := g.c.post(ctx, g.c.base+"/api/v1/services/audio/tts/customization", raw)
	if ae != nil {
		// A request we could not even build never left the process; a transport failure may
		// already have. 构造不出来的请求从未离开进程;传输失败则可能已经出去了。
		return nil, ae.Status == http.StatusInternalServerError, ae
	}
	switch {
	case status == http.StatusOK:
		return body, false, nil
	case status == http.StatusTooManyRequests:
		return nil, true, apierr.ErrUpstreamBusy
	case rejectedBeforeGeneration(status):
		// Explicit pre-creation rejection: provably nothing was registered and nothing billed.
		// Upstream text is discarded (redaction iron rule) — including the fetch diagnostics, which
		// is why the SAMPLE URL is validated before we get here rather than debugged from a reply.
		// 显式创建前拒绝:可证明什么也没登记、什么也没计费。上游原文丢弃(脱敏铁律)——**包括**它的
		// 抓取诊断,这正是**样本 URL 必须在到这里之前就验好**、而不是从回包里去 debug 的理由。
		return nil, true, apierr.UpstreamRejected(apierr.RejectedInvalid)
	default:
		return nil, false, apierr.ErrUpstreamError
	}
}
