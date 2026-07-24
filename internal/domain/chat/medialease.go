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
}

// parseMediaLeaseRef recognises the gateway's own relative lease fetch path. It is deliberately
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
	return MediaLeaseRef{LeaseID: id, Token: token}, true
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
