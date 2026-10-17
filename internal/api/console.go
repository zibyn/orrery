package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// consoleHandler serves the embedded SPA at / (api.md §8). A path that names
// no static file falls back to index.html so client-side routes survive a
// reload; index.html is never cached, while built assets are content-hashed.
func consoleHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// The console runs no inline script; Vite's modulepreload polyfill is
		// turned off in vite.config.ts to keep this policy closed.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		http.ServeFileFS(w, r, fsys, "index.html")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			writeProblem(w, "not_found", "no such endpoint")
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "index.html" {
			serveIndex(w, r)
			return
		}
		if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r)
	})
}
