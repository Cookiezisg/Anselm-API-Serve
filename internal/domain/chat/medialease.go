package chat

import (
	"net/url"
	"strings"
)

// ADR 0011 (Anselm/docs/decisions/0011-gateway-media-handle-contract.md) — the ONE non-data-URI
// media reference this gateway accepts: a lease IT issued, named by the RELATIVE `fetchPath` the
// media `complete` response returned.
//
// Why relative and never absolute. Ownership alone is not enough: an attacker holding a LEGITIMATE
// lease of their own could post `https://evil.example/v1/media/leases/{their-id}/content?token=…`.
// Ownership would verify, and then the UPSTREAM PROVIDER would fetch evil.example — an SSRF routed
// through the provider. Taking only the relative form means the host is never a client-supplied
// value at all: the gateway absolutizes against its own configured public base. The risk is not
// checked away, it is unrepresentable.
//
// ADR 0011 —— 本网关接受的唯一非 data-URI 媒体引用:它**自己签发**的 lease,以 media `complete` 响应
// 返回的**相对** `fetchPath` 指称。
//
// 为什么只收相对形、绝不收绝对形:光有归属校验不够——攻击者拿自己**合法**的 lease 就能发
// `https://evil.example/v1/media/leases/{自己的id}/content?token=…`;归属能过,然后**上游 provider 会去
// 拉 evil.example**,这是一条经 provider 的 SSRF。只收相对形意味着 host 根本不是客户端能提供的值:
// 网关用自己配置的公开 base 绝对化。风险不是被检查掉,而是**不可表达**。

// leasePathPrefix is the media fetch route's fixed prefix. It must stay in lockstep with the route
// registered in the HTTP router. 媒体 fetch 路由的固定前缀,须与路由注册处保持一致。
const leasePathPrefix = "/v1/media/leases/"

const leasePathSuffix = "/content"

// MediaLeaseRef names a lease the client asked the gateway to hand upstream. Both members come from
// a reference the gateway itself minted; neither is trusted until the app layer verifies ownership,
// state and expiry.
//
// MediaLeaseRef 指称客户端要求网关转交上游的一个 lease。两个成员都来自网关自己铸的引用;在 app 层校验
// 归属/状态/时效之前,**一个都不可信**。
type MediaLeaseRef struct {
	LeaseID string
	Token   string

	// Raw is the reference exactly as it appeared on the wire — the key WithInlinedMediaLeases
	// replaces by. Raw 是引用在线缆上的原样——WithInlinedMediaLeases 以它为替换键。
	Raw string
}

// parseMediaLeaseRef recognizes the gateway's own relative lease fetch path. It is deliberately
// strict: any scheme, any host, a missing token, a path that is not exactly
// `/v1/media/leases/{id}/content`, or an id containing a path separator all fail. A caller that
// gets ok=false must treat the value as an ordinary (and therefore rejected) URL — never as a
// "probably fine" lease.
//
// parseMediaLeaseRef 识别网关自己的相对 lease fetch 路径。刻意严格:带 scheme、带 host、缺 token、
// 路径不是恰好 `/v1/media/leases/{id}/content`、或 id 里含路径分隔符,一律失败。拿到 ok=false 的调用方
// 必须把它当作普通(因而被拒的)URL——**绝不能**当成「大概没问题」的 lease。
func parseMediaLeaseRef(rawURL string) (MediaLeaseRef, bool) {
	if !strings.HasPrefix(rawURL, leasePathPrefix) {
		return MediaLeaseRef{}, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return MediaLeaseRef{}, false
	}
	if !strings.HasPrefix(u.Path, leasePathPrefix) || !strings.HasSuffix(u.Path, leasePathSuffix) {
		return MediaLeaseRef{}, false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(u.Path, leasePathPrefix), leasePathSuffix)
	// A lease id is one path segment. Rejecting separators here is what stops `../` and nested
	// paths from ever reaching the store lookup. lease id 是**单个**路径段;在此拒分隔符,正是让
	// `../` 与嵌套路径永远到不了仓储查询的那一步。
	if id == "" || strings.ContainsAny(id, "/\\") {
		return MediaLeaseRef{}, false
	}
	token := u.Query().Get("token")
	if token == "" {
		return MediaLeaseRef{}, false
	}
	return MediaLeaseRef{LeaseID: id, Token: token, Raw: rawURL}, true
}

// MediaLeaseRefs returns every lease reference in the request, in wire order. The app layer must
// verify each one (ownership, state, expiry) before any of them reaches a provider; a reference is
// a CLAIM, and this function does not validate the claim.
//
// MediaLeaseRefs 按线缆顺序返回请求中的全部 lease 引用。app 层必须**逐个**校验(归属/状态/时效)后才可
// 让它们抵达 provider;引用只是一个**主张**,本函数不校验该主张。
func (in InboundRequest) MediaLeaseRefs() []MediaLeaseRef {
	var refs []MediaLeaseRef
	for _, message := range in.Messages {
		parts, ok := message.Content.Parts()
		if !ok {
			continue
		}
		for _, part := range parts {
			var raw string
			switch {
			case part.Type == PartTypeImageURL && part.ImageURL != nil:
				raw = part.ImageURL.URL
			case part.Type == PartTypeVideoURL && part.VideoURL != nil:
				raw = part.VideoURL.URL
			default:
				continue
			}
			if ref, ok := parseMediaLeaseRef(raw); ok {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// InlinedLease is one verified lease's content, ready to travel as a data URI.
// InlinedLease 是一个已校验 lease 的内容,以 data URI 形态随行。
type InlinedLease struct {
	MIMEType string
	Base64   string
}

// WithInlinedMediaLeases returns a copy of the request whose recognized lease references are replaced
// by `data:` URIs carrying the lease's own bytes — the map is keyed by the RAW reference string. It
// lives in this package because ContentPart slices hang off Content's unexported field: rewriting
// anywhere else would mean exporting the innards of the content union just to mutate two strings.
//
// Why INLINE rather than absolutize (which is what this function's predecessor did): the upstream
// multimodal provider's fetcher refuses to download from this gateway's public host. Measured on the
// live deployment (2026-07-25, ADR 0012): the same bytes, the same lease-shaped path and the same
// token query all fetch fine from the bare site host, while every URL on the `api.` host fails with
// "Failed to download multimodal content" and the origin's access log shows the fetcher NEVER
// CONNECTED — the block is at their edge or policy layer, invisible and out of our control. The
// gateway already holds the verified bytes locally, so handing the provider a URL it must fetch back
// from us was a dependency on third-party fetcher policy we never needed.
//
// It must run AFTER the app layer has verified every reference (an unverified reference must never
// reach the upstream in any form) and BEFORE Sanitize builds the forwarded body.
//
// A recognized reference with no map entry is left untouched — this function never invents content,
// and the stale relative URL then fails upstream validation loudly rather than silently.
//
// InlinedLease 随行的是**已校验** lease 的内容。WithInlinedMediaLeases 返回一份副本,其中被识别的 lease
// 引用替换为携带 lease 自身字节的 `data:` URI——map 以**原始引用串**为键。它住在本包,因为 ContentPart
// 切片挂在 Content 的未导出字段上。
//
// 为什么**内联**而非绝对化(本函数的前身做的事):上游多模态 provider 的拉取器拒绝从本网关的公开主机下载。
// 线上实测(2026-07-25,ADR 0012):同样的字节、同样的 lease 形路径、同样的 token query,放在裸域全部拉取
// 正常;而 `api.` 主机上的任何 URL 一律 "Failed to download multimodal content",且源站访问日志显示拉取器
// **从未连入**——拦截发生在对方边缘或策略层,不可见、不可控。网关本地本就持有已校验的字节,让 provider 再
// 回头拉我们一趟,是一条我们从不需要的对第三方拉取策略的依赖。
//
// 必须在 app 层校验完**每一个**引用之后(未经校验的引用不得以任何形态抵达上游)、在 Sanitize 构造转发体
// 之前运行。
//
// 被识别但 map 里没有的引用原样不动——本函数从不凭空造内容,残留的相对 URL 会在上游校验处大声失败,
// 而非静默。
func (in InboundRequest) WithInlinedMediaLeases(inline map[string]InlinedLease) InboundRequest {
	if len(inline) == 0 {
		return in
	}
	out := in
	out.Messages = nil
	changedAny := false
	messages := make([]Message, len(in.Messages))
	copy(messages, in.Messages)
	for mi := range messages {
		parts, ok := messages[mi].Content.Parts()
		if !ok {
			continue
		}
		rewritten := make([]ContentPart, len(parts))
		copy(rewritten, parts)
		changed := false
		for pi := range rewritten {
			switch {
			case rewritten[pi].Type == PartTypeImageURL && rewritten[pi].ImageURL != nil:
				if data, ok := inline[rewritten[pi].ImageURL.URL]; ok {
					clone := *rewritten[pi].ImageURL
					clone.URL = "data:" + data.MIMEType + ";base64," + data.Base64
					rewritten[pi].ImageURL = &clone
					changed = true
				}
			case rewritten[pi].Type == PartTypeVideoURL && rewritten[pi].VideoURL != nil:
				if data, ok := inline[rewritten[pi].VideoURL.URL]; ok {
					clone := *rewritten[pi].VideoURL
					clone.URL = "data:" + data.MIMEType + ";base64," + data.Base64
					rewritten[pi].VideoURL = &clone
					changed = true
				}
			}
		}
		if changed {
			messages[mi].Content = PartsContent(rewritten)
			changedAny = true
		}
	}
	if !changedAny {
		return in
	}
	out.Messages = messages
	return out
}
