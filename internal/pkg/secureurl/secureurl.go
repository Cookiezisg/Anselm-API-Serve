// Package secureurl contains URL transport-policy helpers shared by
// credential-bearing clients. It deliberately does not resolve hostnames: an
// HTTP exception must name a literal loopback address, never a DNS name that can
// be rebound to a different address after validation.
package secureurl

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// AllowsCredentialTransport reports whether u may carry an API key or other
// bearer credential. HTTPS is valid for any explicit host. Plain HTTP is valid
// only for a literal loopback IP, which keeps local tests and development
// servers possible without trusting hosts/NSS resolution for a credential sent
// over cleartext.
func AllowsCredentialTransport(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		// Parse the hostname exactly as it will be handed to net/http. In
		// particular, do not canonicalize a trailing dot: ProxyFromEnvironment
		// does not recognize "127.0.0.1." as loopback, so accepting it here could
		// send a cleartext bearer request through HTTP_PROXY (or DNS/NSS).
		ip := net.ParseIP(u.Hostname())
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// PublicFetchURL builds the absolute address an UPSTREAM may fetch a media lease's bytes from.
//
// **It exists for exactly one upstream.** `voice-enrollment` accepts no base64 — only a fetchable
// URL — so this is the narrow re-opening of something ADR 0012 deliberately closed. Chat and image
// inputs still inline their bytes; nothing else calls this.
//
// **The host must not be `api.`-prefixed, and that is empirical, not stylistic.** ADR 0012's
// production experiment put the identical lease path and token on two hosts in one DNS zone: on
// `api.<domain>` it failed 400 three times over while Caddy's access log proved the origin never
// received a request, and on a plain host it answered 200. The fetcher blacklists API-shaped hosts
// at its own edge — invisible and uncontestable — so a misconfiguration fails with no diagnostic
// anywhere. Callers must reject an api.* host before they get here (config validation and
// render-caddy.sh both do); this function refuses it too, because the one guard that runs on the
// actual value beats two that run on config.
//
// PublicFetchURL 构造**上游**可以来取媒体 lease 字节的绝对地址。
//
// **它只为一个上游而存在。** `voice-enrollment` 不收 base64、只收可取的 URL,故这是对 ADR 0012
// 刻意关掉的那扇门的一次**窄缝**重开。chat 与图像输入仍然内联字节;没有别的东西调它。
//
// **主机不得以 `api.` 开头,而这是实证、不是风格。** ADR 0012 的生产实验把**完全相同**的 lease 路径
// 与 token 放在同一个 DNS zone 的两台主机上:在 `api.<域>` 上连续三次 400,而 Caddy 访问日志证明
// 源站**从未收到请求**;在普通主机上答 200。拉取器在它自己的边缘把 API 形主机拉黑——不可见、不可
// 申诉——故配错会**在任何地方都不留下诊断**地失败。调用方应在到这里之前就拒掉 api.* 主机(config
// 校验与 render-caddy.sh 都做了);本函数**也**拒,因为一道跑在真实值上的闸,胜过两道跑在配置上的。
func PublicFetchURL(host, leaseID, token string) (string, error) {
	host = strings.TrimSpace(host)
	switch {
	case host == "":
		return "", errors.New("secureurl: media fetch host is not configured")
	case strings.Contains(host, "://"), strings.ContainsAny(host, "/?#"):
		return "", errors.New("secureurl: media fetch host must be a bare host, not a URL")
	case strings.HasPrefix(host, "api."):
		return "", errors.New("secureurl: media fetch host must not be an api.* name")
	}
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(token) == "" {
		return "", errors.New("secureurl: lease id and token are both required")
	}
	u := url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     "/v1/media/leases/" + leaseID + "/content",
		RawQuery: url.Values{"token": []string{token}}.Encode(),
	}
	return u.String(), nil
}
