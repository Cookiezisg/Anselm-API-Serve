// Package webassets embeds and serves the self-contained dashboard SPA — the
// Vite-built React+AntD bundle (index.html + hashed assets under ui/dist/),
// committed into the repo so build/deploy needs NO Node toolchain and the public
// surface pulls nothing from a third-party origin (a CDN could be GFW-blocked and
// widens the attack surface). The dashboard router mounts Handler() at "/".
package webassets

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:ui/dist
var webFS embed.FS

// spaIndex is index.html, read once at startup. The SPA fallback serves it for
// any unmatched GET so the client hash-router (#/overview …) works without a
// history-API server, and we never re-read the embed FS per request.
var spaIndex []byte

func init() {
	b, err := webFS.ReadFile("ui/dist/index.html")
	if err != nil {
		// An embed read failure is a packaging error, not a runtime condition —
		// fail loudly at startup rather than serve a blank shell.
		panic("webassets: embedded index.html missing: " + err.Error())
	}
	spaIndex = b
}

// Handler serves the embedded SPA. /static/<file> → the hashed Vite asset
// (explicit content-type, traversal-safe); any other path → index.html so the
// client hash-router owns every non-API GET. The dashboard router mounts this at
// "/", below the concrete /api/*,/login,… routes (they win ServeMux precedence),
// so the SPA fallback can never shadow an authenticated route or bypass auth.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			serveStatic(w, r)
			return
		}
		serveSPA(w, r)
	})
}

// serveStatic maps /static/<file> to ui/dist/<file>. path.Clean collapses any
// "."/".." so a traversal like /static/../x cannot escape the ui/dist subtree;
// an unknown asset falls through to the SPA shell (never a bare 404).
func serveStatic(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/static/"))
	name := "ui/dist" + clean
	b, err := fs.ReadFile(webFS, name)
	if err != nil {
		serveSPA(w, r)
		return
	}
	// Explicit content-type: the dashboard sets X-Content-Type-Options: nosniff,
	// so a wrong/empty type would make the browser refuse the JS/CSS.
	w.Header().Set("Content-Type", contentTypeFor(clean))
	http.ServeContent(w, r, path.Base(clean), time.Time{}, bytes.NewReader(b))
}

func serveSPA(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spaIndex)
}

// contentTypeFor maps a static asset path to its content type (set explicitly
// because nosniff is on). Covers what the Vite build can emit.
func contentTypeFor(p string) string {
	switch {
	case strings.HasSuffix(p, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".json"), strings.HasSuffix(p, ".map"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(p, ".woff"):
		return "font/woff"
	case strings.HasSuffix(p, ".ttf"):
		return "font/ttf"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(p, ".webp"):
		return "image/webp"
	case strings.HasSuffix(p, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}
