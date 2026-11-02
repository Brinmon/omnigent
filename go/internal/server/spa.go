package server

// Serving the built web client.
//
// The Go build serves the same SPA the Python build does, unchanged: openapi.json
// is the contract between them, so the client cannot tell which backend answered.
//
// Two rules govern the routing:
//
//   - an API path always wins, so a missing endpoint is a 404 from the API rather
//     than the SPA's HTML (which the client would fail to parse as JSON);
//   - any other path falls back to index.html, because the client owns its own
//     routes and a deep link like /c/<id> must render rather than 404.

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// spaHandler serves the web client from dir.
//
// It returns nil when dir holds no built client, which is an API-only deployment:
// the caller then leaves the root unmounted so a request for "/" is the API's 404
// rather than a confusing empty page.
func spaHandler(dir string) http.Handler {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	indexPath := path.Join(dir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		return nil
	}

	root := os.DirFS(dir)
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested == "" || requested == "." {
			serveIndex(w, r, indexPath)
			return
		}

		file, err := root.Open(requested)
		if err != nil {
			// Not a file: a client route. Serve the SPA so the router can handle
			// it; answering 404 here would break every deep link and reload.
			serveIndex(w, r, indexPath)
			return
		}
		defer file.Close()

		stat, err := file.Stat()
		if err != nil || stat.IsDir() {
			serveIndex(w, r, indexPath)
			return
		}

		// Hashed asset names are immutable, so they can be cached hard. The
		// index must not be, or a client keeps loading the previous build's
		// entry point after a deploy.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndex writes index.html without caching.
func serveIndex(w http.ResponseWriter, r *http.Request, indexPath string) {
	content, err := os.ReadFile(indexPath)
	if err != nil {
		http.Error(w, "web client is not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(content)
	}
}

// isAPIPath reports whether a path belongs to the API rather than the SPA.
//
// The check is on the prefixes the API owns: an unknown /v1/... must 404 as JSON,
// and serving HTML there would surface in the client as a parse error instead of
// "not found".
func isAPIPath(requested string) bool {
	for _, prefix := range []string{"/v1/", "/api/", "/health", "/openapi.json", "/.well-known/"} {
		if strings.HasPrefix(requested, prefix) {
			return true
		}
	}
	return false
}

// spaDirFromEnv resolves the built client's directory.
//
// OMNIGENT_WEB_UI_DIST overrides, matching the Python server's variable so one
// deployment description works for either build. An unset variable falls back to
// the repository's web/dist, which is where the monorepo build writes.
func spaDirFromEnv() string {
	if configured := os.Getenv("OMNIGENT_WEB_UI_DIST"); configured != "" {
		return configured
	}
	for _, candidate := range []string{"web/dist", "../web/dist", "../../web/dist"} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// dirExists reports whether a path is an existing directory.
func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// ensure fs is referenced: http.FileServer over an io/fs root is the point.
var _ fs.FS = os.DirFS(".")
