// Package secureurl contains URL transport-policy helpers shared by
// credential-bearing clients. It deliberately does not resolve hostnames: an
// HTTP exception must name a literal loopback address, never a DNS name that can
// be rebound to a different address after validation.
package secureurl

import (
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
