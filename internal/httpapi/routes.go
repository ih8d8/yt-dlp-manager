package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
)

type setupRequestKey struct{}

// routes builds the API surface plus the static SPA fallback. Unknown /api/
// paths must stay JSON 404s and never fall through to HTML.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Every credential route must be inert when Deps.Auth is nil (explicitly
	// unauthenticated mode). Enforcing that in one wrapper rather than in each
	// handler is deliberate: three of the four remembered the check and
	// handleLogout did not, which made a nil dereference reachable without
	// authenticating. GET stays unwrapped because it has a real answer to give
	// in that mode.
	mux.HandleFunc("POST /api/v1/session", s.requireAuthEnabled(s.handleLogin))
	mux.HandleFunc("DELETE /api/v1/session", s.requireAuthEnabled(s.handleLogout))
	mux.HandleFunc("GET /api/v1/session", s.handleSessionGet)
	mux.HandleFunc("PUT /api/v1/session/password",
		s.requireAuthEnabled(s.requireAdminMode(s.handlePasswordChange, true)))

	mux.HandleFunc("GET /api/v1/downloads", s.requireAdmin(s.handleDownloadsList))
	mux.HandleFunc("POST /api/v1/downloads", s.requireAdmin(s.handleDownloadAdd))
	mux.HandleFunc("GET /api/v1/downloads/{id}", s.requireAdmin(s.handleDownloadGet))
	mux.HandleFunc("GET /api/v1/downloads/{id}/thumbnail", s.requireAdmin(s.handleThumbnail))
	mux.HandleFunc("POST /api/v1/downloads/actions", s.requireAdmin(s.handleBatchActions))
	mux.HandleFunc("POST /api/v1/downloads/clear", s.requireAdmin(s.handleDownloadsClear))
	mux.HandleFunc("POST /api/v1/formats", s.requireAdmin(s.handleFormats))

	mux.HandleFunc("GET /api/v1/events", s.requireAdmin(s.handleEvents))

	mux.HandleFunc("GET /api/v1/settings", s.requireAdmin(s.handleSettingsGet))
	mux.HandleFunc("PUT /api/v1/settings", s.requireAdmin(s.handleSettingsPut))
	mux.HandleFunc("GET /api/v1/settings/yt-dlp", s.requireAdmin(s.handleYtDlpGet))
	mux.HandleFunc("PUT /api/v1/settings/yt-dlp/managed", s.requireAdmin(s.handleYtDlpManagedPut))

	mux.HandleFunc("GET /api/v1/system", s.requireAdmin(s.handleSystem))

	// Machine-readable API documentation (authenticated, same origin only).
	mux.HandleFunc("GET /api/v1/openapi.json", s.requireAdmin(s.handleOpenAPI))

	// JSON 404 for anything else under /api/, SPA fallback for the rest.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "unknown API endpoint")
	})
	mux.HandleFunc("/", s.serveStatic)

	return mux
}

// requireAuthEnabled short-circuits credential endpoints when authentication
// is disabled. In that mode Deps.Auth is nil, so touching it would panic.
func (s *Server) requireAuthEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Unauthenticated || s.deps.Auth == nil {
			writeError(w, r, http.StatusConflict, codeInvalidState,
				"authentication is disabled on this server (unauthenticated loopback mode)")
			return
		}
		next(w, r)
	}
}

// requireAdmin enforces authentication (when enabled) and CSRF on mutating
// requests. In explicitly unauthenticated loopback mode every caller is
// treated as admin.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAdminMode(next, false)
}

func (s *Server) requireAdminMode(next http.HandlerFunc, allowPasswordChange bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.deps.Unauthenticated {
			sess, err := s.deps.Auth.sessionFromRequest(r)
			switch {
			case err != nil:
				writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
				return
			case r.Method != http.MethodGet && r.Method != http.MethodHead:
				token := r.Header.Get("X-CSRF-Token")
				// Constant-time comparison: the token gates every mutation.
				if token == "" ||
					subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
					writeError(w, r, http.StatusForbidden, codeForbidden, "missing or invalid CSRF token")
					return
				}
			}
			// Verify already slid the server-side expiry. The cookie was
			// issued with a fixed 24h Max-Age, so without re-issuing it the
			// browser would drop an active session exactly 24h after login
			// however recently it was used — "sliding" only on one side.
			if c, cerr := r.Cookie(sessionCookieName); cerr == nil && c.Value != "" {
				setSessionCookie(w, s.deps.SecureCookie, c.Value, int(sessionTTL.Seconds()))
			}
			if s.deps.Auth.SetupRequired() {
				if !allowPasswordChange {
					writeError(w, r, http.StatusForbidden, codePasswordChange,
						"administrator password setup is required before using the application")
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), setupRequestKey{}, true))
			}
		}
		next(w, r)
	}
}
