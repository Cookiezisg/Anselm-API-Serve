// Package apierr is the single source of truth for the gateway's structured
// error type and every stable wire code. It is pure domain: zero net/http,
// zero DB. The transport layer (transport/httpapi/response) renders an
// *APIError into the {"error":{"code","message"[,"details"]}} envelope; the
// type itself carries no rendering. Keeping wire codes here — the innermost
// layer — lets domain/app/infra/transport all import them with no reverse dep.
//
// Wire codes are an external contract (SPA, Caddy, operators depend on the
// exact values), so their status/code/message are spec, not arbitrary.
package apierr

import "strconv"

// APIError is a typed gateway error carrying both an HTTP status and a stable
// UPPER_SNAKE wire code. Details is omitempty and carries a machine-actionable
// field for the rare case that needs one (currently only LOGIN_LOCKED's
// retryAfterSec). Secrets MUST NOT enter Message (GW-INV-11/17).
type APIError struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Error reports code + message; never includes Details (which may be logged).
func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// NewError builds an APIError without details.
func NewError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// NewErrorWithDetails builds an APIError carrying a structured details map,
// e.g. the dashboard login lockout's retryAfterSec.
func NewErrorWithDetails(status int, code, message string, details map[string]any) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Details: details}
}

// LoginLocked returns the dashboard per-IP backoff error carrying retryAfterSec
// (delta-seconds). The transport layer also sets the Retry-After header from
// this value; the details field lets the SPA act without parsing the message.
func LoginLocked(retryAfterSec int) *APIError {
	return NewErrorWithDetails(StatusLoginLocked, "LOGIN_LOCKED",
		"too many attempts, retry later",
		map[string]any{"retryAfterSec": retryAfterSec})
}

// HTTP status constants for the wire codes. Kept as untyped ints (not
// net/http.Status*) so this package never imports net/http and stays pure
// domain; the values are the spec.
const (
	statusBadRequest          = 400
	statusNotFound            = 404
	statusUnprocessableEntity = 422
	statusRequestTooLarge     = 413
	statusUnauthorized        = 401
	statusPaymentRequired     = 402
	statusForbidden           = 403
	statusConflict            = 409
	statusTooManyRequests     = 429
	statusBadGateway          = 502
	statusServiceUnavailable  = 503
	statusGatewayTimeout      = 504
	statusInternalServerError = 500

	// StatusLoginLocked is exported for the dashboard backoff helper.
	StatusLoginLocked = statusTooManyRequests
)

// Sentinel wire codes — the §5.2 status/code table, verbatim. Messages are
// client-safe (no upstream body, no key). These are the machine contract;
// clients branch on Code (UPPER_SNAKE), never on an OpenAI-style type/param.
var (
	// ErrInvalidInstall — the public install id is absent or unknown.
	ErrInvalidInstall = NewError(statusUnauthorized, "INVALID_INSTALL", "missing or invalid install id")
	// ErrDeviceProofRequired — request has no device-bound proof.
	ErrDeviceProofRequired = NewError(statusUnauthorized, "DEVICE_PROOF_REQUIRED", "device proof is required")
	// ErrDeviceProofInvalid — signature or signed request material is invalid.
	ErrDeviceProofInvalid = NewError(statusUnauthorized, "DEVICE_PROOF_INVALID", "device proof is invalid")
	// ErrDeviceProofNonceInvalid — challenge is forged, expired, or from a prior process.
	ErrDeviceProofNonceInvalid = NewError(statusUnauthorized, "DEVICE_PROOF_NONCE_INVALID", "device proof challenge expired; fetch a fresh challenge")
	// ErrDeviceProofReplayed — the signed request id was already consumed.
	ErrDeviceProofReplayed = NewError(statusConflict, "DEVICE_PROOF_REPLAYED", "device proof was already used")
	// ErrAccountBanned — this install has been disabled.
	ErrAccountBanned = NewError(statusForbidden, "ACCOUNT_BANNED", "this install has been disabled")
	// ErrRateLimited — per-minute rate or daily sublimit exceeded.
	ErrRateLimited = NewError(statusTooManyRequests, "RATE_LIMITED", "rate or daily sub-limit exceeded")
	// ErrRequestBodyTooLarge — the exact HTTP request body crossed the
	// configured memory-safety envelope. This is deliberately distinct from a
	// model context rejection.
	ErrRequestBodyTooLarge = NewError(statusRequestTooLarge, "REQUEST_BODY_TOO_LARGE", "request body exceeds the configured size limit")
	// ErrQuotaExhausted — monthly free-tier quota exhausted.
	ErrQuotaExhausted = NewError(statusTooManyRequests, "QUOTA_EXHAUSTED", "monthly free-tier quota exhausted")
	// ErrUpstreamBusy — upstream capacity busy (queue full/timeout, breaker open,
	// upstream 429). Its own class so it is never retried/breakered (GW-INV-23).
	ErrUpstreamBusy = NewError(statusTooManyRequests, "UPSTREAM_BUSY", "upstream capacity is busy, retry shortly")
	// ErrBudgetExhausted — daily free-tier budget reached. Note 402, not 429.
	ErrBudgetExhausted = NewError(statusPaymentRequired, "BUDGET_EXHAUSTED", "daily service budget reached, try again tomorrow")
	// ErrBadRequest — invalid request body.
	ErrBadRequest = NewError(statusBadRequest, "BAD_REQUEST", "invalid request body")
	// ErrUpstreamError — upstream model provider error.
	ErrUpstreamError = NewError(statusBadGateway, "UPSTREAM_ERROR", "upstream model provider error")
	// ErrUpstreamTimeout — upstream model provider timeout.
	ErrUpstreamTimeout = NewError(statusGatewayTimeout, "UPSTREAM_TIMEOUT", "upstream model provider timeout")
	// ErrMultimodalUnavailable — this deployment has no configured multimodal
	// provider credential. It is deliberately distinct from UPSTREAM_BUSY: retrying
	// cannot help until the operator enables the visual provider, and text remains available.
	ErrMultimodalUnavailable = NewError(statusServiceUnavailable, "MULTIMODAL_UNAVAILABLE", "multimodal input is unavailable on this deployment")
	// ErrAudioUnavailable — audio is a valid public content part, but the current fixed routing
	// table deliberately has no audio-capable upstream. It is distinct from malformed input and from
	// a missing visual-provider credential, so clients can preserve their attachment and retry after an upgrade.
	ErrAudioUnavailable = NewError(statusServiceUnavailable, "AUDIO_UNAVAILABLE", "audio input is not available on this deployment")
	// ErrSpeechUnavailable — realtime microphone transcription is unavailable on this deployment.
	// It is distinct from AUDIO_UNAVAILABLE: speech input is an I/O helper that produces editable text,
	// while audio content parts are raw multimodal chat input.
	ErrSpeechUnavailable = NewError(statusServiceUnavailable, "SPEECH_UNAVAILABLE", "speech transcription is not available on this deployment")
	// ErrMediaUnavailable means durable media ingress has not been explicitly
	// configured on this gateway. It is not a malformed attachment and retrying
	// the same bytes cannot repair it.
	ErrMediaUnavailable = NewError(statusServiceUnavailable, "MEDIA_UNAVAILABLE", "durable media upload is not enabled on this deployment")
	// ErrImageUnavailable — image generation is not available on this deployment
	// (IMAGE_ENABLED off or no upstream credential). Distinct from a quota denial:
	// retrying tomorrow cannot repair a disabled capability.
	ErrImageUnavailable = NewError(statusServiceUnavailable, "IMAGE_UNAVAILABLE", "image generation is not available on this deployment")
	// ErrImageSourceInvalid — the edit source is not a base64 data URL. It is a SHAPE
	// refusal, not a validation nicety: ADR 0011 forbids a managed media input carrying a scheme or
	// a host, because an address this gateway would fetch is an SSRF primitive aimed at our own
	// network. Requiring `data:` IS the mitigation — a data URL cannot be fetched, it is the bytes.
	//
	// ErrImageSourceInvalid——改图的源不是 base64 data URL。这是一次**形状**拒绝、不是校验的
	// 客套:ADR 0011 禁止带 scheme 或 host 的受管媒体输入,因为一个本网关会去取的地址,是指向**我们自己
	// 网络**的 SSRF 原语。要求 `data:` **就是**那个缓解——data URL 取不了,它就是字节本身。
	ErrImageSourceInvalid = NewError(statusBadRequest, "IMAGE_SOURCE_INVALID", "the edit source must be a base64 data URL, not an address")
	// ErrVideoFrameInvalid — the image-to-video first frame is not a base64 data URL.
	// Its own code rather than reusing IMAGE_SOURCE_INVALID: a client animating a picture that gets
	// told "the EDIT source is invalid" would go looking at the wrong request.
	//
	// ErrVideoFrameInvalid——图生视频的首帧不是 base64 data URL。**自己的码**而非复用
	// IMAGE_SOURCE_INVALID:一个在让图动起来的客户端被告知「**改图**的源无效」,会去查错的那个请求。
	ErrVideoFrameInvalid = NewError(statusBadRequest, "VIDEO_FRAME_INVALID", "the first frame must be a base64 data URL, not an address")
	// ErrVoiceUnavailable — voice cloning is not configured on this deployment (it rides the speech
	// capability; a deployment that cannot speak has no use for a voice).
	// ErrVoiceUnavailable——本部署没配音色克隆(它搭在语音能力上;说不了话的部署要音色没用)。
	ErrVoiceUnavailable = NewError(statusServiceUnavailable, "VOICE_UNAVAILABLE", "voice cloning is not available on this deployment")
	// ErrVoiceSampleInvalid — the reference sample is not a base64 data URL. Same shape guard, same
	// SSRF reason, as the image editor's source and the video's first frame.
	// ErrVoiceSampleInvalid——参考样本不是 base64 data URL。与改图的源、图生视频的首帧同一条形状闸、
	// 同一个 SSRF 理由。
	ErrVoiceSampleInvalid = NewError(statusBadRequest, "VOICE_SAMPLE_INVALID", "the voice sample must be a base64 data URL, not an address")
	// ErrVoiceInventoryFull — this install already holds its maximum number of voices. INVENTORY,
	// not quota: nothing frees a slot tomorrow, so the message says "delete one" — "try again
	// later" would send the user away to wait for something that never happens.
	// ErrVoiceInventoryFull——本 install 已持有上限数量的音色。**库存**不是配额:明天不会腾出位置,
	// 故消息说「删一个」——「过会儿再试」会打发用户去等一件永远不会发生的事。
	ErrVoiceInventoryFull = NewError(statusConflict, "VOICE_INVENTORY_FULL", "voice inventory is full — delete a voice to make room")
	// ErrVoiceNameTaken — that name already points at an upstream registration; enrolling over it
	// would orphan the first one in our provider account.
	// ErrVoiceNameTaken——该名已指向一个上游登记;覆盖它会让第一个在我们的 provider 账号里变成孤儿。
	ErrVoiceNameTaken = NewError(statusConflict, "VOICE_NAME_TAKEN", "a voice with this name already exists — delete it first")
	// ErrVoiceCapacityReached — OUR provider account is at its voice ceiling, not this install's.
	// Distinct from VOICE_INVENTORY_FULL because the remedy is different and NOT the user's:
	// deleting their own voice will not help, and the operator is the one who has to act.
	// ErrVoiceCapacityReached——**我们的** provider 账号到了音色上限,不是这个 install 的。与
	// VOICE_INVENTORY_FULL 分开,因为补救办法不同、**且不在用户手里**:删掉他自己的音色也没用,
	// 该动手的是运营者。
	ErrVoiceCapacityReached = NewError(statusServiceUnavailable, "VOICE_CAPACITY_REACHED", "the service cannot register new voices right now")
	// ErrVoiceNotFound — no such voice for this install.
	ErrVoiceNotFound = NewError(statusNotFound, "VOICE_NOT_FOUND", "voice not found")
	// ErrImageQuotaExhausted — the per-install daily image-generation cap is
	// reached. Distinct from RATE_LIMITED (a pacing denial) and from
	// QUOTA_EXHAUSTED (the monthly request entitlement): the client should offer
	// "try again tomorrow", not back off and retry.
	ErrImageQuotaExhausted = NewError(statusTooManyRequests, "IMAGE_QUOTA_EXHAUSTED", "daily image generation quota reached, try again tomorrow")
	// ErrTTSUnavailable — speech SYNTHESIS is not available on this deployment
	// (SPEECH_ENABLED off or no upstream credential). Deliberately NOT named
	// SPEECH_UNAVAILABLE: that code is already the realtime ASR (speech→text)
	// denial, and one code answering for both directions would leave a client
	// that lost its microphone indistinguishable on the wire from one that
	// cannot read a message aloud.
	//
	// ErrTTSUnavailable —— 本部署不提供语音**合成**。刻意不叫 SPEECH_UNAVAILABLE:那个码已是
	// 实时 ASR(语音→文本)的拒绝,一个码答两个方向,会让「麦克风没了」与「读不出这条消息」在线缆上
	// 无从区分。
	ErrTTSUnavailable = NewError(statusServiceUnavailable, "TTS_UNAVAILABLE", "speech synthesis is not available on this deployment")
	// ErrTTSQuotaExhausted — the per-install daily speech-character cap is
	// reached. The image twin's reasoning: distinct from
	// RATE_LIMITED (pacing) and QUOTA_EXHAUSTED (the monthly entitlement)
	// because the honest client action is "try again tomorrow".
	ErrTTSQuotaExhausted = NewError(statusTooManyRequests, "TTS_QUOTA_EXHAUSTED", "daily speech synthesis quota reached, try again tomorrow")
	// ErrVideoUnavailable — video generation is not available on this deployment
	// (VIDEO_ENABLED off, no upstream credential, or no handle-signing material).
	// Its own code, not IMAGE_UNAVAILABLE: an operator may run image and speech
	// without video, and a client told "image is unavailable" when it asked for a
	// video would degrade the wrong feature.
	ErrVideoUnavailable = NewError(statusServiceUnavailable, "VIDEO_UNAVAILABLE", "video generation is not available on this deployment")
	// ErrVideoQuotaExhausted — the per-install daily video-CLIP cap is reached
	// (用户拍板 10/day). The unit is clips, not seconds: the honest
	// client message is "you have used today's 10 videos", not a duration budget.
	ErrVideoQuotaExhausted = NewError(statusTooManyRequests, "VIDEO_QUOTA_EXHAUSTED", "daily video generation quota reached, try again tomorrow")

	// ErrVoiceQuotaExhausted — the per-install daily voice-ENROLLMENT cap is reached. "tomorrow" is
	// honest here in a way the inventory refusal is not: this ledger DOES reset with the day, while
	// VOICE_INVENTORY_FULL never does and therefore says "delete one" instead. Two refusals, two
	// remedies; collapsing them would send a user to wait for a slot that will never open.
	// ErrVoiceQuotaExhausted——逐 install 的每日**登记**次数上限到顶。「明天」在这里是诚实的,而在库存
	// 拒绝那里不是:**这个**账本真的会随天重置,而 VOICE_INVENTORY_FULL 永远不会、故它说的是「删一个」。
	// 两条拒绝、两种补救;合并它们会让用户去等一个永远不会开的位置。
	ErrVoiceQuotaExhausted = NewError(statusTooManyRequests, "VOICE_QUOTA_EXHAUSTED", "daily voice enrollment quota reached, try again tomorrow")
	// ErrVideoTaskNotFound — the polled handle names no task this install can
	// read. It answers BOTH "the signature does not verify" and "the provider has
	// forgotten this task": distinguishing them would confirm to a caller that
	// some OTHER install owns the handle it just guessed.
	//
	// ErrVideoTaskNotFound —— 被轮询的句柄指不出任何本 install 读得到的任务。它同时回答「签名验不过」
	// 与「上游已忘掉此任务」:区分两者等于向调用方确认「它刚猜中的那个句柄属于**别的** install」。
	ErrVideoTaskNotFound = NewError(statusNotFound, "VIDEO_TASK_NOT_FOUND", "video task was not found")
	// Media upload failures intentionally distinguish client-fixable lifecycle
	// mistakes from a verified object whose bytes/digest do not match.
	ErrMediaUploadInvalid   = NewError(statusBadRequest, "MEDIA_UPLOAD_INVALID", "invalid media upload request")
	ErrMediaUploadNotFound  = NewError(statusNotFound, "MEDIA_UPLOAD_NOT_FOUND", "media upload was not found")
	ErrMediaLeaseNotFound   = NewError(statusNotFound, "MEDIA_LEASE_NOT_FOUND", "media lease was not found")
	ErrMediaUploadConflict  = NewError(statusConflict, "MEDIA_UPLOAD_CONFLICT", "media upload is not writable in its current state")
	ErrMediaIntegrityFailed = NewError(statusUnprocessableEntity, "MEDIA_INTEGRITY_FAILED", "media upload failed integrity verification")

	// /install reject paths use DISTINCT codes for audit separation (GW-INV-20):

	// ErrInstallRateLimited — per-IP install rate exceeded (INSTALL_PER_IP_HOUR).
	ErrInstallRateLimited = NewError(statusTooManyRequests, "INSTALL_RATE_LIMITED", "too many installs from this address, retry later")
	// ErrInstallCapReached — global daily coarse cap reached (INSTALL_GLOBAL_DAILY_CAP).
	ErrInstallCapReached = NewError(statusTooManyRequests, "INSTALL_CAP_REACHED", "install issuance is temporarily at capacity, retry later")
	// ErrInstallFPLimited — per-fingerprint daily limit / cooldown window.
	ErrInstallFPLimited = NewError(statusTooManyRequests, "INSTALL_FP_LIMITED", "too many installs for this client, retry later")
	// ErrInstallPoWRequired — enforce mode, missing X-PoW (403: re-fetch challenge,
	// do not back off and retry).
	ErrInstallPoWRequired = NewError(statusForbidden, "INSTALL_POW_REQUIRED", "proof-of-work is required: solve GET /v1/install/challenge and resubmit with X-PoW")
	// ErrInstallPoWInvalid — HMAC forgery / expired challenge / difficulty miss /
	// nonce reuse.
	ErrInstallPoWInvalid = NewError(statusForbidden, "INSTALL_POW_INVALID", "invalid proof-of-work: fetch a fresh challenge from GET /v1/install/challenge and retry")

	// ErrDiskLow — REL-6 low-disk read-only degradation (GW-INV-29).
	ErrDiskLow = NewError(statusServiceUnavailable, "DISK_LOW", "service temporarily read-only: low disk space")
	// ErrQuotaResetBusy — a dashboard-wide quota reset must wait until all
	// current-month reservations have reached a terminal ledger state.
	ErrQuotaResetBusy = NewError(statusConflict, "QUOTA_RESET_BUSY", "quota reset is waiting for active requests to settle")
)

// CodeUpstreamRejected is the wire code for an upstream-originated request
// rejection; exported as a const so callers branch without a magic string.
const CodeUpstreamRejected = "UPSTREAM_REJECTED"

// Coarse machine reasons an UPSTREAM_REJECTED carries in details.reason — a
// CLOSED enum derived by the upstream layer from the provider's 4xx error shape.
// Upstream TEXT never passes through (GW-INV-11); only one of these values does.
const (
	// RejectedContextLength — input (+ max_tokens) exceeds the model's context window.
	RejectedContextLength = "context_length"
	// RejectedMaxTokens — max_tokens outside the model's accepted range.
	RejectedMaxTokens = "max_tokens"
	// RejectedInvalid — any other request-shaped upstream rejection.
	RejectedInvalid = "invalid_request"
)

// UpstreamRejected — the upstream model provider rejected the request as invalid
// (e.g. context length exceeded). 400: the CLIENT must change the request, so it
// is neither retried nor a breaker fault (ADR-011). The message is fixed and the
// details carry only the coarse reason enum — never the upstream body (GW-INV-11).
func UpstreamRejected(reason string) *APIError {
	return NewErrorWithDetails(statusBadRequest, CodeUpstreamRejected,
		"upstream rejected the request: reduce input size or max_tokens, or fix request parameters",
		map[string]any{"reason": reason})
}

// Internal is the normalization target for any non-*APIError reaching the
// transport renderer — never leak internal detail/upstream body/key (§4.3).
// The renderer constructs this; defined here so the contract has one home.
func Internal() *APIError {
	return NewError(statusInternalServerError, "INTERNAL", "internal error")
}

// retryAfterString formats a delta-seconds value for the Retry-After header.
// Exposed for the transport layer so the header and details stay in lockstep.
func retryAfterString(sec int) string { return strconv.Itoa(sec) }

// RetryAfterSeconds returns the retryAfterSec from an APIError's Details (if
// present) plus its string form for the Retry-After header. ok is false when
// the error carries no such field. Lets transport render the header without
// re-deriving the value.
func (e *APIError) RetryAfterSeconds() (sec int, header string, ok bool) {
	if e == nil || e.Details == nil {
		return 0, "", false
	}
	v, present := e.Details["retryAfterSec"]
	if !present {
		return 0, "", false
	}
	n, isInt := v.(int)
	if !isInt {
		return 0, "", false
	}
	return n, retryAfterString(n), true
}
