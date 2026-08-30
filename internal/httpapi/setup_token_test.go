package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// setupTokenServer builds a server that still needs its first-run password —
// wildcard bind, real credential store so a successful setup can persist —
// with a bootstrap token configured the way internal/app wires one whenever
// the bind is reachable from off-box.
func setupTokenServer(t *testing.T, token string) *Server {
	t.Helper()
	d := testDeps(t)
	a, err := NewAuthWithKey(filepath.Join(t.TempDir(), "admin.json"),
		[]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	d.Auth, d.Unauthenticated = a, false
	d.Listen = "0.0.0.0:8080"
	d.SetupToken = token
	return testServer(t, d)
}

// claimAccount runs the whole first-run flow — fetch a setup session, then PUT
// the password with its CSRF token — from one peer, host and token, and
// returns the status of the password request.
func claimAccount(t *testing.T, s *Server, peer, host, token string) int {
	t.Helper()

	get := httptest.NewRequest("GET", "/api/v1/session", nil)
	get.Host = host
	get.RemoteAddr = peer
	if token != "" {
		get.Header.Set(SetupTokenHeader, token)
	}
	grec := httptest.NewRecorder()
	s.ServeHTTP(grec, get)
	var sess sessionResponse
	if err := json.Unmarshal(grec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}

	put := httptest.NewRequest("PUT", "/api/v1/session/password",
		strings.NewReader(`{"password":"correct horse","confirmation":"correct horse"}`))
	put.Host = host
	put.RemoteAddr = peer
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("X-CSRF-Token", sess.CSRFToken)
	if token != "" {
		put.Header.Set(SetupTokenHeader, token)
	}
	for _, c := range grec.Result().Cookies() {
		put.AddCookie(c)
	}
	prec := httptest.NewRecorder()
	s.ServeHTTP(prec, put)
	return prec.Code
}

// The Host header is chosen by the client, so it cannot be evidence that a
// caller is local. Before this, anyone who could reach an unconfigured
// instance could send "Host: localhost:8080" and claim the administrator
// account ahead of the operator.
func TestFirstRunSetupRejectsSpoofedLocalhostFromRemotePeer(t *testing.T) {
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		s := setupTokenServer(t, "0123456789abcdef0123456789abcdef")
		if code := claimAccount(t, s, "203.0.113.7:41234", host, ""); code != 403 {
			t.Errorf("Host %q from a remote peer = %d, want 403", host, code)
		}
		if !s.deps.Auth.SetupRequired() {
			t.Fatalf("Host %q let a remote client claim the administrator account", host)
		}
	}
}

func TestFirstRunSetupAcceptsBootstrapTokenFromRemotePeer(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	s := setupTokenServer(t, token)
	if code := claimAccount(t, s, "203.0.113.7:41234", "nas.lan:8080", token); code != 200 {
		t.Fatalf("setup with the bootstrap token = %d, want 200", code)
	}
	if s.deps.Auth.SetupRequired() {
		t.Fatal("a valid setup token should have completed first-run setup")
	}
}

func TestFirstRunSetupRejectsWrongOrMissingToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	for name, presented := range map[string]string{
		"wrong":  "ffffffffffffffffffffffffffffffff",
		"prefix": "0123456789abcdef",
		"none":   "",
	} {
		s := setupTokenServer(t, token)
		// 401 rather than 403 when the origin was never handed a setup session
		// to begin with; either way the account must stay unclaimed.
		if code := claimAccount(t, s, "203.0.113.7:41234", "nas.lan:8080", presented); code != 403 && code != 401 {
			t.Errorf("%s token = %d, want 401 or 403", name, code)
		}
		if !s.deps.Auth.SetupRequired() {
			t.Fatalf("%s token claimed the administrator account", name)
		}
	}
}

// A server with no token configured (a loopback bind) must not be settable up
// remotely by presenting an empty one.
func TestFirstRunSetupWithoutConfiguredTokenRefusesRemotePeers(t *testing.T) {
	s := setupTokenServer(t, "")
	if code := claimAccount(t, s, "203.0.113.7:41234", "localhost:8080", ""); code != 403 {
		t.Fatalf("remote setup without a configured token = %d, want 403", code)
	}
	if !s.deps.Auth.SetupRequired() {
		t.Fatal("remote client claimed the account on a server with no setup token")
	}
}

// The desktop path is unchanged: a browser on the same machine sets the
// password with no token at all.
func TestFirstRunSetupFromLoopbackPeerNeedsNoToken(t *testing.T) {
	s := setupTokenServer(t, "0123456789abcdef0123456789abcdef")
	if code := claimAccount(t, s, "127.0.0.1:54321", "localhost:8080", ""); code != 200 {
		t.Fatalf("loopback setup = %d, want 200", code)
	}
	if s.deps.Auth.SetupRequired() {
		t.Fatal("loopback setup did not complete")
	}
}

// The UI needs to know whether to ask for a token before it shows a form it
// could not submit.
func TestSessionReportsWhenSetupTokenIsRequired(t *testing.T) {
	s := setupTokenServer(t, "0123456789abcdef0123456789abcdef")
	for _, tc := range []struct {
		peer, host string
		want       bool
	}{
		{"127.0.0.1:54321", "localhost:8080", false},
		{"203.0.113.7:41234", "localhost:8080", true},
		{"172.17.0.1:41234", "127.0.0.1:8080", true},
	} {
		req := httptest.NewRequest("GET", "/api/v1/session", nil)
		req.Host = tc.host
		req.RemoteAddr = tc.peer
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var got sessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.SetupTokenRequired != tc.want {
			t.Errorf("peer %s host %s: setup_token_required = %v, want %v",
				tc.peer, tc.host, got.SetupTokenRequired, tc.want)
		}
	}
}
