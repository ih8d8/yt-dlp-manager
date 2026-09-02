package httpapi

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func goRuntimeVersion() string { return runtime.Version() }

type loginRequest struct {
	Password string `json:"password"`
}

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	SetupRequired bool   `json:"setup_required"`
	// SetupTokenRequired tells a first-run client that this request's origin
	// cannot claim the administrator account on its own and has to present the
	// setup token from the server log.
	SetupTokenRequired bool `json:"setup_token_required,omitempty"`
}

// setupDeniedMessage explains both ways to complete first-run setup. It is
// returned to anyone whose request fails the check, so it names no secret.
const setupDeniedMessage = "first-run administrator setup must be performed from the machine " +
	"running the server (browse to http://localhost:PORT), or by sending the setup token " +
	"printed in the server log as the " + SetupTokenHeader + " header"

type changePasswordRequest struct {
	Password     string `json:"password"`
	Confirmation string `json:"confirmation"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.deps.Unauthenticated {
		writeError(w, r, http.StatusConflict, codeInvalidState,
			"authentication is disabled on this server (unauthenticated loopback mode)")
		return
	}
	if s.deps.Auth.SetupRequired() {
		writeError(w, r, http.StatusConflict, codePasswordChange,
			"initial administrator password setup is required")
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}
	client := s.clientKey(r)

	ok, retryIn := s.deps.Auth.limiter.allow(client)
	if !ok {
		w.Header().Set("Retry-After", retryInCeil(retryIn))
		writeError(w, r, http.StatusTooManyRequests, codeRateLimited,
			"too many failed logins; try again later")
		return
	}

	valid, verr := s.deps.Auth.checkPassword(r.Context(), req.Password)
	if errors.Is(verr, ErrVerifierBusy) {
		// Shed load rather than queue behind unbounded PBKDF2 work. This is
		// not a failed attempt, so it must not feed the failure counters —
		// otherwise a burst would also trip the service-wide backoff.
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, codeRateLimited,
			"too many simultaneous login attempts; try again shortly")
		return
	}
	if verr != nil {
		// Client disconnected or the server is shutting down.
		return
	}
	if !valid {
		s.deps.Auth.limiter.fail(client)
		// Deliberately identical response for any wrong password: there is
		// only one admin account, and no username oracle exists.
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "invalid password")
		return
	}
	s.deps.Auth.limiter.success(client)

	value, csrf := s.deps.Auth.sessions.Create()
	setSessionCookie(w, s.deps.SecureCookie, value, int(sessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, CSRFToken: csrf})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.deps.Unauthenticated {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			s.deps.Auth.sessions.Destroy(c.Value)
		}
	}
	setSessionCookie(w, s.deps.SecureCookie, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Unauthenticated || s.deps.Auth == nil {
		writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true})
		return
	}
	if s.deps.Auth.SetupRequired() {
		// A setup session is not an authenticated administrator session. It
		// only supplies a signed same-origin cookie and CSRF token accepted by
		// the password endpoint while setupRequired remains true — which still
		// enforces setupAuthorized, so handing one out grants nothing on its
		// own.
		//
		// It is withheld from a Host that could have been rebound (unless the
		// caller already proves itself with the setup token), so a page that
		// points its own name at 127.0.0.1 never holds a usable CSRF token.
		//
		// tokenRequired tells the UI to ask for the bootstrap token up front
		// rather than present a password form whose submit would be refused.
		tokenRequired := !(peerIsLoopback(r) && s.setupHostAllowed(r))
		if !s.setupHostAllowed(r) && !s.setupTokenValid(r) {
			writeJSON(w, http.StatusOK, sessionResponse{
				SetupRequired: true, SetupTokenRequired: tokenRequired})
			return
		}
		if sess, err := s.deps.Auth.sessionFromRequest(r); err == nil {
			writeJSON(w, http.StatusOK, sessionResponse{
				CSRFToken: sess.csrf, SetupRequired: true, SetupTokenRequired: tokenRequired})
			return
		}
		value, csrf := s.deps.Auth.sessions.Create()
		setSessionCookie(w, s.deps.SecureCookie, value, int(sessionTTL.Seconds()))
		writeJSON(w, http.StatusOK, sessionResponse{
			CSRFToken: csrf, SetupRequired: true, SetupTokenRequired: tokenRequired})
		return
	}
	sess, err := s.deps.Auth.sessionFromRequest(r)
	if err != nil {
		// Session discovery is not a protected resource. Returning a normal
		// unauthenticated state lets the SPA render its login screen without a
		// failing network request (actual protected endpoints still return 401).
		writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, CSRFToken: sess.csrf})
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if s.deps.Unauthenticated {
		writeError(w, r, http.StatusConflict, codeInvalidState,
			"authentication is disabled on this server")
		return
	}
	var req changePasswordRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	var err error
	if setupRequest, _ := r.Context().Value(setupRequestKey{}).(bool); setupRequest {
		// Claiming the administrator account is the one mutation available
		// before any credential exists, so it is restricted to callers that
		// are demonstrably local, or that hold the startup setup token.
		if !s.setupAuthorized(r) {
			writeError(w, r, http.StatusForbidden, codeForbidden, setupDeniedMessage)
			return
		}
		err = s.deps.Auth.SetupPassword(req.Password, req.Confirmation)
	} else {
		err = s.deps.Auth.ChangePassword(req.Password, req.Confirmation)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrWeakSecret), errors.Is(err, ErrPasswordMismatch),
			errors.Is(err, ErrPasswordTooLong):
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState, err.Error())
		case errors.Is(err, ErrSetupComplete):
			writeError(w, r, http.StatusConflict, codeInvalidState, err.Error())
		default:
			writeError(w, r, http.StatusInternalServerError, codeInternal,
				"could not persist the administrator password")
		}
		return
	}
	// Password replacement is a security boundary: invalidate every existing
	// session and issue a fresh cookie/CSRF pair to the current browser.
	s.deps.Auth.sessions.LogoutAll()
	value, csrf := s.deps.Auth.sessions.Create()
	setSessionCookie(w, s.deps.SecureCookie, value, int(sessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, CSRFToken: csrf})
}

func retryInCeil(d time.Duration) string {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// setupAuthorized reports whether this request may claim the administrator
// account.
//
// The Host header is chosen by the client, so it can never establish that a
// caller is local: anything that can reach an unconfigured instance could
// otherwise send "Host: localhost" and set the password before the operator
// does. Locality is proven by the transport peer instead, which the server
// reads from the accepted socket.
//
// Two routes qualify:
//
//   - the peer is the local machine AND the Host is one that cannot have been
//     rebound (setupHostAllowed) — the desktop case, where the browser and the
//     server share a host;
//   - the caller presents the one-time setup token printed to the server log
//     at startup — the container/remote case, where the peer is a NAT gateway
//     or another machine and the log is the out-of-band channel.
func (s *Server) setupAuthorized(r *http.Request) bool {
	if peerIsLoopback(r) && s.setupHostAllowed(r) {
		return true
	}
	return s.setupTokenValid(r)
}

// SetupTokenHeader carries the first-run bootstrap secret.
const SetupTokenHeader = "X-Setup-Token"

// setupTokenValid compares the presented bootstrap token against the one this
// process generated. An empty configured token never matches, so a server that
// did not print a token cannot be set up remotely at all.
func (s *Server) setupTokenValid(r *http.Request) bool {
	want := s.deps.SetupToken
	if want == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get(SetupTokenHeader))
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// peerIsLoopback reports whether the request arrived from the local machine.
// RemoteAddr comes from the accepted connection and is not client-settable.
//
// Note that a proxy (including Docker's userland port proxy) appears as its
// own address here; forwarded-for headers are deliberately not consulted,
// since trusting them would restore exactly the spoofable check this replaces.
//
// This holds even when TrustedProxies is configured. That setting exists so
// login rate limiting can tell clients apart (see clientip.go); it is not a
// statement that a forwarded address proves locality. A proxy that appends to
// X-Forwarded-For rather than replacing it lets any client that can reach it
// put "127.0.0.1" in the chain, and first-run setup is precisely the decision
// that must not be reachable that way. Remote setup uses the token instead.
func peerIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(host), "[]"))
	return ip != nil && ip.IsLoopback()
}

// setupHostAllowed reports whether first-run administrator setup may be
// performed through this request's Host. This is only the anti-rebinding half
// of the decision — see setupAuthorized, which owns the check. A specific
// (non-wildcard) bind is already pinned by hostGuard, so only wildcard binds
// need the extra check.
func (s *Server) setupHostAllowed(r *http.Request) bool {
	if hostIsLoopback(r.Host) {
		return true
	}
	for _, t := range s.deps.TrustedHosts {
		if strings.EqualFold(strings.TrimSpace(t), r.Host) {
			return true
		}
	}
	host, _, err := net.SplitHostPort(s.deps.Listen)
	if err != nil {
		return false
	}
	// A bind to one specific interface cannot be rebound to: hostGuard has
	// already required Host to equal it.
	return host != "" && host != "0.0.0.0" && host != "::"
}
