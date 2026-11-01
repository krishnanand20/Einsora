// Package webui serves the dashboard built into the binary: Solo is one binary with its dashboard
// (ADR-0012). `make build` copies web/dist into dist/ before compiling. A binary built without it
// serves a short page saying so, and its API works either way.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Files is the dashboard built into this binary.
func Files() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // dist is embedded at build time; it always exists
	}
	return sub
}

// Built reports whether files hold a built dashboard.
func Built(files fs.FS) bool {
	_, err := fs.Stat(files, "index.html")
	return err == nil
}

// csp keeps the dashboard to its own origin: no scripts, styles, or connections from anywhere else,
// and no framing, so another site can't overlay it.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// Handler serves files: each file as it is, and index.html for every other path, so the
// dashboard's own routes (/workloads/<id>) load on a refresh. A missing file (a path with an
// extension) is a 404, not the dashboard.
func Handler(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if fi, err := fs.Stat(files, name); err == nil && !fi.IsDir() {
				if strings.HasPrefix(name, "assets/") { // file names carry a content hash
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, notBuilt)
			return
		}
		_, _ = w.Write(index)
	})
}

const notBuilt = `<!doctype html><title>Einsora</title><p>This einsora binary was built without its dashboard.
Build it with <code>make build</code>, or run the dashboard from source with <code>make web-dev</code>.
The API works either way.`
