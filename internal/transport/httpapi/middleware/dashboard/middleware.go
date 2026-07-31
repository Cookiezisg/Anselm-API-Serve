// Package dashboard holds the response-hardening middleware for the loopback
// admin listener.
//
// It deliberately contains NO authentication: the dashboard binds loopback-only
// (fail-fast) and a preceding IAP owns the login wall. The headers below are
// still worth setting on a loopback surface — a browser reaching it through a
// tunnel is a real browser, and clickjacking, MIME sniffing and a cached admin
// page do not care which interface served them.
//
// 本包只放 loopback 后台监听器的响应加固中间件,**刻意不含鉴权**:后台恒定 loopback 绑定
// (fail-fast),登录墙归前置的 IAP。下面这些头在 loopback 面上依然值得设——经隧道访问它的
// 浏览器是**真的**浏览器,而点击劫持、MIME 嗅探与被缓存的管理页面并不在乎是哪张网卡送出的。
package dashboard

import "net/http"

// SecurityHeaders sets nosniff + no-store (an admin surface must never be
// cached) + X-Frame-Options DENY + a strict CSP. script-src stays 'self' (no
// inline/eval); style-src adds 'unsafe-inline' — the one necessary relaxation for
// the AntD v5 CSS-in-JS runtime <style> injection (CSS cannot execute JS, so the
// script surface stays locked).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
