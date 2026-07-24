package config

import (
	"fmt"
	"net/url"
	"strings"
)

// validateMediaPublicBaseURL enforces that MEDIA_PUBLIC_BASE_URL is a bare, absolute https origin.
// It is the value the gateway prepends to a client-supplied RELATIVE lease path before handing the
// result to an upstream provider (ADR 0011), so a sloppy value here reintroduces exactly the SSRF
// the relative-only rule exists to eliminate: a path, a query or a fragment could let a crafted
// reference resolve somewhere unintended, and a non-https origin would hand a capability-bearing
// URL to the provider in clear text.
//
// validateMediaPublicBaseURL 强制 MEDIA_PUBLIC_BASE_URL 是**裸的、绝对的 https origin**。它是网关在把
// 客户端提供的**相对** lease 路径交给上游 provider 前所拼的前缀(ADR 0011),故此处一个随手写的值就会把
// 「只收相对形」本要消灭的那条 SSRF 重新引回来:带 path/query/fragment 会让精心构造的引用解析到意外位置,
// 非 https 的 origin 则等于把带能力的 URL 明文交给 provider。
func validateMediaPublicBaseURL(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fmt.Errorf("SEC-2 config: MEDIA_ENABLED requires MEDIA_PUBLIC_BASE_URL (this gateway's externally reachable https origin)")
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("SEC-2 config: MEDIA_PUBLIC_BASE_URL is not a valid URL")
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("SEC-2 config: MEDIA_PUBLIC_BASE_URL must be an https origin with a host and no userinfo")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("SEC-2 config: MEDIA_PUBLIC_BASE_URL must be a bare origin (no path, query or fragment)")
	}
	return nil
}
