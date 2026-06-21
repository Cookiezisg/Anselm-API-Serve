// Package clientip derives the trustworthy client IP behind the Caddy hop and
// collapses it to a low-cardinality, privacy-preserving rate-limit key.
//
// 信任边界 (GW-INV-16): 网关 bind 127.0.0.1,Caddy 是唯一直连 peer 并把真实 client
// IP append 到 XFF 末尾。故只有当直连 peer 为 loopback (可信 Caddy) 时才信任 XFF,
// 且取最右一段;否则攻击者直连即可伪造左侧 XFF 拿到不同 Key、绕过 per-IP Sybil 防御。
package clientip

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
)

// ClientIP returns the address driving the per-IP install limit.
//
// remoteAddr is the direct peer (r.RemoteAddr, host or host:port). xff is the raw
// X-Forwarded-For header value. XFF is honored ONLY when remoteAddr is loopback —
// then the RIGHTMOST hop (the real peer Caddy appended) is used; otherwise the XFF
// is ignored entirely and remoteAddr is returned verbatim. Never panics on any input.
func ClientIP(remoteAddr, xff string) string {
	if isLoopback(remoteAddr) && xff != "" {
		parts := strings.Split(xff, ",")
		right := strings.TrimSpace(parts[len(parts)-1])
		if right != "" {
			return right
		}
	}
	return remoteAddr
}

// Key collapses an IP to a rate-limit bucket key, then hashes it (SHA-256, hex) so
// no plaintext address reaches storage/logs. IPv6 collapses to its /64 prefix first
// (else one user's vast address space defeats per-IP limiting); IPv4 keys on the
// full address. A non-parseable input keys on its raw string (still hashed) so even
// garbage peers bucket deterministically without leaking the value.
func Key(ip string) string {
	return hashKey(bucket(ip))
}

// bucket reduces ip (host or host:port) to its cardinality-collapsed form before
// hashing: IPv4 → full address, IPv6 → /64 prefix, unparseable → raw host.
func bucket(ip string) string {
	host := ip
	if h, _, err := net.SplitHostPort(ip); err == nil {
		host = h
	}
	parsed := net.ParseIP(host)
	if parsed == nil {
		return host
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	masked := parsed.Mask(net.CIDRMask(64, 128))
	return masked.String() + "/64"
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// isLoopback reports whether addr (host or host:port) parses to a loopback IP.
func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
