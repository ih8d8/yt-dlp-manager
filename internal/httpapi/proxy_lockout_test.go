package httpapi

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the reverse-proxy lockout.
//
// Login rate limiting keys on the socket peer. Behind a reverse proxy — the
// deployment README.md and SECURITY.md both recommend for remote access — that
// address is the PROXY's for every client, so a single shared bucket holds
// everyone: one wrong guess turns every other client away too, and an attacker
// who paces guesses through the growing delays keeps the bucket pinned at its
// ceiling without ever learning the password.
//
// Two things address that, and both matter. loginMaxDelay is short (see
// auth.go), which bounds the damage to a nuisance for anyone who never
// configures a proxy. Naming the proxy removes it entirely by giving each
// client its own bucket.

// authServerWithPassword builds a server with authentication actually enforced
// and a real verifier, so a wrong password is a genuine failed attempt.
func authServerWithPassword(t *testing.T, password string, trustedProxies string) *Server {
	t.Helper()
	d := testDeps(t)
	d.Unauthenticated = false

	salt := []byte("0123456789abcdef")
	// Deliberately few iterations: this exercises the limiter, not the KDF, and
	// the production floor (100k) is enforced when loading a credential file.
	hash, err := pbkdf2.Key(sha256.New, password, salt, 1000, passwordHashBytes)
	if err != nil {
		t.Fatal(err)
	}
	d.Auth = &Auth{
		sessions:     NewSessions([]byte("0123456789abcdef0123456789abcdef")),
		limiter:      newLoginLimiter(),
		salt:         salt,
		passwordHash: hash,
		iterations:   1000,
	}

	prefixes, err := ParseTrustedProxies(trustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	d.TrustedProxies = prefixes
	return testServer(t, d)
}

// tryLogin posts one attempt arriving at proxyPeer on behalf of client.
func tryLogin(t *testing.T, s *Server, proxyPeer, client, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session",
		strings.NewReader(`{"password":`+strconv.Quote(password)+`}`))
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = proxyPeer
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", client)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

const proxyPeer = "127.0.0.1:44444"

// Without the setting, one client's failure is enough to turn the
// administrator away, because both share the proxy's address.
func TestUnconfiguredProxyLetsOneClientLockOutAnother(t *testing.T) {
	s := authServerWithPassword(t, "correct-horse-battery", "")

	if rec := tryLogin(t, s, proxyPeer, "203.0.113.7", "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("attacker attempt = %d, want 401", rec.Code)
	}

	admin := tryLogin(t, s, proxyPeer, "198.51.100.9", "correct-horse-battery")
	if admin.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the administrator to be turned away by another client's "+
			"failure without trusted proxies configured, got %d", admin.Code)
	}
}

// However long an attacker keeps guessing, the delay they can impose on
// everyone sharing the bucket stays bounded. This is what keeps the
// unconfigured proxy case a nuisance rather than an administrator lockout, so
// it is pinned: raising loginMaxDelay back into the minutes would quietly
// restore the denial of service.
func TestSharedBucketBackoffStaysBounded(t *testing.T) {
	l := newLoginLimiter()
	const shared = "127.0.0.1" // what every client looks like behind a proxy

	for i := 0; i < 40; i++ {
		l.fail(shared)
	}

	ok, retryIn := l.allow(shared)
	if ok {
		t.Fatal("expected the shared bucket to be throttled")
	}
	if retryIn > loginMaxDelay {
		t.Errorf("retryIn = %v, want at most %v", retryIn, loginMaxDelay)
	}
	if loginMaxDelay > 2*time.Minute {
		t.Errorf("loginMaxDelay = %v: a ceiling this long locks the operator out of "+
			"their own server and makes a shared bucket a denial of service", loginMaxDelay)
	}
	t.Logf("40 paced failures throttle every client behind the proxy for %v", retryIn.Truncate(1e9))
}

// With the proxy named, each client gets its own bucket: the attacker's
// failures no longer reach the administrator, who signs in normally.
func TestTrustedProxySeparatesLoginBuckets(t *testing.T) {
	s := authServerWithPassword(t, "correct-horse-battery", "127.0.0.1")

	for i := 0; i < 12; i++ {
		tryLogin(t, s, proxyPeer, "203.0.113.7", "wrong-password")
	}

	admin := tryLogin(t, s, proxyPeer, "198.51.100.9", "correct-horse-battery")
	if admin.Code != http.StatusOK {
		t.Fatalf("administrator login = %d, want 200: a different client's failures "+
			"must not throttle this one (body: %s)", admin.Code, admin.Body.String())
	}
}

// The attacker's own bucket must still tighten — separating clients must not
// turn into no rate limiting at all.
func TestTrustedProxyStillThrottlesTheFailingClient(t *testing.T) {
	s := authServerWithPassword(t, "correct-horse-battery", "127.0.0.1")

	if rec := tryLogin(t, s, proxyPeer, "203.0.113.7", "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt = %d, want 401", rec.Code)
	}
	rec := tryLogin(t, s, proxyPeer, "203.0.113.7", "wrong-password")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second attempt from the same client = %d, want 429", rec.Code)
	}
}

// A client that forges X-Forwarded-For while connecting directly must not be
// able to shed its own backoff by inventing a new identity per attempt.
func TestForgedForwardedHeaderCannotEvadeBackoff(t *testing.T) {
	// No trusted proxies: the peer is the only identity, whatever it claims.
	s := authServerWithPassword(t, "correct-horse-battery", "")

	if rec := tryLogin(t, s, "203.0.113.7:5000", "1.1.1.1", "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt = %d, want 401", rec.Code)
	}
	rec := tryLogin(t, s, "203.0.113.7:5000", "2.2.2.2", "wrong-password")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("a forged X-Forwarded-For bought a fresh bucket: got %d, want 429", rec.Code)
	}
}
