package httpapi

import (
	"net/http"
	"strings"
)

// serveStatic sets cache policy and delegates delivery (including the SPA
// fallback) to the embedded web handler.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if s.deps.Web == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
		return
	}
	p := r.URL.Path
	// Vite emits content-hashed files under /assets/: safe to cache forever.
	if strings.HasPrefix(p, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	s.deps.Web.ServeHTTP(w, r)
}
