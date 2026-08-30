package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestIDFrom returns the per-request ID assigned by the middleware.
func RequestIDFrom(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'self'"

// securityHeaders apply to every response, static and API alike.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

// withRequestID assigns an id and echoes it back; control bytes can never
// enter because we only ever serve generated ids.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// recoverPanic converts handler panics into clean 500s without leaking
// stack traces or Go error text.
func recoverPanic(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if log != nil {
					log.Error("panic in handler", "path", pathOnly(r), "request_id", RequestIDFrom(r))
				}
				writeError(w, r, http.StatusInternalServerError, codeInternal, "unexpected server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// hostGuard rejects requests whose Host header is not expected. A specific
// non-wildcard bind locks hosts down exactly (host must match and the port
// must equal the bound port). Wildcard binds cannot know externally mapped
// ports (Docker publishing, reverse proxies), so they accept any
// syntactically valid host; operators can pin expected values via
// trustedHosts.
type hostGuard struct {
	listen       string   // configured listen address
	trustedHosts []string // extra accepted host:port values
	// loopbackOnly pins the guard to loopback names regardless of the bind
	// wildcard. Set in unauthenticated mode, where the session cookie that
	// normally defeats DNS rebinding does not exist: without it, any page the
	// user visits could drive the API through a rebound hostname.
	loopbackOnly bool
}

func (g *hostGuard) middleware(next http.Handler) http.Handler {
	listenHost, port, _ := net.SplitHostPort(g.listen)
	wildcard := (listenHost == "" || listenHost == "0.0.0.0" || listenHost == "::") && !g.loopbackOnly
	accept := func(hostPort string) bool {
		if strings.ContainsAny(hostPort, "\x00\n\r \t") {
			return false
		}
		for _, t := range g.trustedHosts {
			if strings.EqualFold(hostPort, t) {
				return true
			}
		}
		host, hport, err := net.SplitHostPort(hostPort)
		if err != nil {
			// No port in Host (default scheme ports).
			host = strings.Trim(strings.TrimSpace(hostPort), "[]")
			hport = ""
		}
		host = strings.Trim(host, "[]")
		if host == "" {
			return false
		}
		switch strings.ToLower(host) {
		case "localhost", "127.0.0.1", "::1":
			return wildcard || hport == "" || hport == port
		}
		if g.loopbackOnly {
			// Only the loopback names above are acceptable here.
			return false
		}
		if !wildcard {
			return host == listenHost && (hport == "" || hport == port)
		}
		return true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if h == "" || !accept(h) {
			writeError(w, r, http.StatusMisdirectedRequest, codeForbidden, "unexpected host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsLoopback reports whether a Host header names the local machine. Used
// to gate first-run administrator setup: a wildcard bind accepts any hostname
// (it cannot know the externally mapped one), which lets a malicious page use
// DNS rebinding to reach 127.0.0.1 under its own name, obtain a setup session
// — same-origin as far as the browser is concerned, so SameSite=Strict does
// not help — and claim the administrator account before the real user does.
func hostIsLoopback(hostPort string) bool {
	if strings.ContainsAny(hostPort, "\x00\n\r \t") {
		return false
	}
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = strings.TrimSpace(hostPort)
	}
	host = strings.Trim(host, "[]")
	// "localhost." is the same name in fully-qualified form. Only one
	// trailing dot is removed, so "localhost.evil.com" still does not match.
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originGuard protects mutations against cross-site browser requests. When
// an Origin header is present it must be same-origin with the request's own
// host over http(s). Absent Origin (curl, TUI-less scripts) still has to pass
// CSRF checks separately.
func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			origin := r.Header.Get("Origin")
			if origin != "" {
				u := originURL(origin)
				if u == "" || u != originURL("http://"+r.Host) && u != originURL("https://"+r.Host) {
					writeError(w, r, http.StatusForbidden, codeForbidden, "cross-origin request rejected")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originURL normalizes an absolute URL to lowercase scheme://host[:port].
func originURL(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	schemes := []string{"https://", "http://"}
	for _, s := range schemes {
		if rest, ok := strings.CutPrefix(raw, s); ok {
			host := rest
			if i := strings.IndexAny(rest, "/?#"); i >= 0 {
				host = rest[:i]
			}
			if strings.ContainsAny(host, "@ \t\r\n") {
				return ""
			}
			return s + host
		}
	}
	return ""
}

// accessLog writes one line per request. It logs only method + path (never
// query strings, which can carry secrets), status, duration, and request id.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if log != nil {
			log.Info("http",
				"method", r.Method,
				"path", pathOnly(r),
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r),
			)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.ResponseController pass deadline control (read/write)
// through to the underlying connection. Without this, SSE handlers cannot
// extend the server's global timeouts and streams die mid-flight.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush passthrough keeps SSE streaming working through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func pathOnly(r *http.Request) string {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	return p
}
