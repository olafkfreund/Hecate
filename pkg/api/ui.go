package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// assets is the built UI, baked into the binary.
//
// **Why embedded rather than a second Deployment.** The UI and the API are the
// same origin, so there is no CORS to configure, no second image to build and
// tag in step, no second Service, and the session cookie set by /auth/callback
// is simply attached to every API call because the browser considers them the
// same site. One URL to give someone.
//
// `make ui` replaces this directory wholesale. A fresh checkout has only
// .gitkeep in it, which is enough for the embed to compile — so `go build ./...`
// works for anyone without Node, and the API's build never depends on a
// toolchain it does not otherwise need.
//
//go:embed all:ui
var assets embed.FS

// placeholder is what an unbuilt binary serves instead of the app.
//
// Embedded separately, outside the directory `make ui` wipes: keeping it inside
// meant the build deleted a tracked file every time it ran, which is the kind
// of thing that is only noticed once it has been committed.
//
//go:embed placeholder.html
var placeholder []byte

// uiHandler serves the built UI, or an honest placeholder if there is not one.
//
// A static export writes each route as a directory with an index.html, so the
// file server resolves /gates/ without this package knowing what routes exist.
// Anything it cannot find falls back to the export's own 404 page rather than
// Go's, so a mistyped URL still arrives inside the application.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(assets, "ui")
	if err != nil {
		// Only reachable if the embed directive above is wrong, which is a
		// build-time mistake rather than a runtime condition.
		panic("embedded UI is missing: " + err.Error())
	}

	if !built(sub) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			// Not an error status: the API is working, and a 500 here would
			// make a probe or a proxy treat the whole server as unhealthy.
			_, _ = w.Write(placeholder)
		})
	}

	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long-lived caching for the hashed bundles and nothing else. Next
		// fingerprints everything under /_next/static, so those are safe to
		// keep for a year; index.html must not be, or a browser will run last
		// week's app against this week's API.
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// built reports whether a real export is present rather than only the
// placeholder.
func built(sub fs.FS) bool {
	_, err := fs.Stat(sub, "index.html")
	return err == nil
}
