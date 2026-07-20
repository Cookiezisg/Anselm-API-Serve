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
