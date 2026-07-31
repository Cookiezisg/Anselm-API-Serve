package secureurl

import (
	"net/url"
	"testing"
)

func TestAllowsCredentialTransport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "remote HTTPS", raw: "https://api.example.com/v1", want: true},
		{name: "uppercase HTTPS", raw: "HTTPS://api.example.com/v1", want: true},
		{name: "localhost HTTP", raw: "http://localhost:8080/v1", want: false},
		{name: "localhost dot HTTP", raw: "http://LOCALHOST.:8080/v1", want: false},
		{name: "IPv4 loopback HTTP", raw: "http://127.0.0.2:8080/v1", want: true},
		{name: "IPv4 loopback dot HTTP", raw: "http://127.0.0.1.:8080/v1", want: false},
		{name: "IPv6 loopback HTTP", raw: "http://[::1]:8080/v1", want: true},
		{name: "remote hostname HTTP", raw: "http://api.example.com/v1", want: false},
		{name: "remote IPv4 HTTP", raw: "http://192.0.2.10/v1", want: false},
		{name: "localhost lookalike HTTP", raw: "http://localhost.example.com/v1", want: false},
		{name: "relative", raw: "/v1", want: false},
		{name: "port without hostname", raw: "https://:443/v1", want: false},
		{name: "wrong scheme", raw: "ftp://127.0.0.1/v1", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.raw, err)
			}
			if got := AllowsCredentialTransport(u); got != tc.want {
				t.Fatalf("AllowsCredentialTransport(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestAllowsCredentialTransportRejectsNil(t *testing.T) {
	t.Parallel()
	if AllowsCredentialTransport(nil) {
		t.Fatal("nil URL must not be credential-safe")
	}
}

// TestPublicFetchURL_RefusesTheOneShapeThatFailsInvisibly: an api.* host is the exact shape ADR
// 0012 proved the upstream fetcher rejects at its own edge — three 400s while the origin's access
// log showed no request ever arrived. That failure leaves no diagnostic anywhere, so it has to be
// caught before the URL is handed over, not debugged afterwards.
//
// TestPublicFetchURL_RefusesTheOneShapeThatFailsInvisibly:api.* 主机正是 ADR 0012 证明过的、拉取器
// 在自己边缘拒绝的那个形状——三次 400,而源站访问日志显示请求从未到达。那种失败在任何地方都不留
// 诊断,故必须在 URL 递出去**之前**拦住,而不是事后去查。
func TestPublicFetchURL_RefusesTheOneShapeThatFailsInvisibly(t *testing.T) {
	for _, host := range []string{"api.example.net", "api.example.com"} {
		if _, err := PublicFetchURL(host, "mls_1", "tok"); err == nil {
			t.Fatalf("PublicFetchURL accepted %q — that host fails with no diagnostic anywhere", host)
		}
	}
}

// TestPublicFetchURL_RefusesAnythingButABareHost: the gateway builds the scheme and path, so a URL
// here would let a typo point the upstream at somebody else's server.
//
// TestPublicFetchURL_RefusesAnythingButABareHost:scheme 与路径由网关自己拼,故这里收一个 URL 等于
// 让一个笔误把上游指向别人的服务器。
func TestPublicFetchURL_RefusesAnythingButABareHost(t *testing.T) {
	for _, host := range []string{"", "https://media.example.com", "media.example.com/x", "media.example.com?a=1"} {
		if _, err := PublicFetchURL(host, "mls_1", "tok"); err == nil {
			t.Fatalf("PublicFetchURL accepted %q", host)
		}
	}
}

// TestPublicFetchURL_BuildsTheLeaseRoute: https, the lease content path, and the token escaped into
// the query — the shape the public route already serves.
//
// TestPublicFetchURL_BuildsTheLeaseRoute:https、lease 内容路径、token 转义进 query——公开路由本来
// 就服务的那个形状。
func TestPublicFetchURL_BuildsTheLeaseRoute(t *testing.T) {
	got, err := PublicFetchURL("media.example.com", "mls_abc", "t o+k/en")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	const want = "https://media.example.com/v1/media/leases/mls_abc/content?token=t+o%2Bk%2Fen"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := PublicFetchURL("media.example.com", "mls_abc", ""); err == nil {
		t.Fatal("a lease URL without its token is a 404 waiting to happen; it must not be built")
	}
}
