package bootstrap

// listeners.go owns the bind policy: the business listener prefers a systemd
// socket-activation fd (so a restart never drops connections queued in the
// kernel backlog on :8080, OPS-2), with a net.Listen fallback; admin + dashboard
// self-bind. requireLoopback fail-fasts a non-loopback ADMIN_ADDR (GW-INV-13:
// /metrics + pprof + expvar must never be reachable off-box). Ported from
// _legacy/cmd/server/main.go + _legacy/internal/metrics/admin.go.

import (
	"fmt"
	"net"

	"github.com/coreos/go-systemd/v22/activation"
)

// businessListener returns the listener the public business server serves on. It
// prefers a systemd socket-activation fd so connections queued in the kernel
// backlog on the business port survive a restart (OPS-2); without systemd it
// self-binds addr (dev / non-systemd, identical behavior). The bool reports
// whether the fd came from socket activation (for the startup log).
func businessListener(addr string) (net.Listener, bool, error) {
	if lns, err := activation.Listeners(); err == nil && len(lns) > 0 {
		for _, ln := range lns {
			if ln != nil {
				return ln, true, nil // single socket unit ⇒ take the first fd.
			}
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, false, fmt.Errorf("bind business listener %q: %w", addr, err)
	}
	return ln, false, nil
}

// selfBind binds a self-managed listener (admin / dashboard) — these get a brief
// gap across a restart (only the business port is connection-preserving).
func selfBind(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind listener %q: %w", addr, err)
	}
	return ln, nil
}

// requireLoopback validates that addr's host resolves to a loopback IP, so the
// admin/metrics surface is never reachable off-box. An empty host (":9090" =
// all interfaces) is rejected; an IP literal is judged directly; a hostname is
// resolved and EVERY resolved IP must be loopback — so a hosts/NSS entry pointing
// it at a routable address can never expose admin.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ADMIN_ADDR %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("ADMIN_ADDR %q must bind a loopback host (e.g. 127.0.0.1), not all interfaces", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("ADMIN_ADDR host %q is not loopback (metrics must never be public)", host)
		}
		return nil
	}
	ips, lerr := net.LookupIP(host)
	if lerr != nil || len(ips) == 0 {
		return fmt.Errorf("ADMIN_ADDR host %q does not resolve to a loopback address: %v", host, lerr)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("ADMIN_ADDR host %q resolves to non-loopback %s (metrics must never be public)", host, ip)
		}
	}
	return nil
}
