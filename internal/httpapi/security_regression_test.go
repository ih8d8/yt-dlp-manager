package httpapi

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestThumbnailIDCannotEscapeStateDir is the regression for the traversal that
// let a single GET delete files outside the state directory.
//
// The mechanism is subtle enough to be worth restating: http.ServeMux matches
// on the ESCAPED path and unescapes only the captured wildcard, so "%2F"
// survives routing and arrives in PathValue as a real separator. The handler
// then joined that value into a filesystem path and, for an id with no live
// item, removed it — and an id that traverses is exactly an id with no live
// item.
func TestThumbnailIDCannotEscapeStateDir(t *testing.T) {
	stateDir := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "secret.img")
	if err := os.WriteFile(victim, []byte("IMPORTANT"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := testDeps(t)
	d.StateDir = stateDir
	s := testServer(t, d)

	// Walk from <stateDir>/thumbs back up to the victim file.
	up := strings.Repeat("..%2F", strings.Count(filepath.Clean(stateDir), string(filepath.Separator))+1)
	escaped := up + strings.TrimPrefix(strings.ReplaceAll(victimDir, string(filepath.Separator), "%2F"), "%2F") + "%2Fsecret"

	for _, id := range []string{
		escaped,
		"..%2F..%2F..%2Fetc%2Fpasswd",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"../../etc/passwd",
		"", // empty segment
		strings.Repeat("a", 200),
		"NOT-HEX",
	} {
		req := httptest.NewRequest("GET", "/api/v1/downloads/"+id+"/thumbnail", nil)
		req.Host = "localhost:8080"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Errorf("id %q unexpectedly served a thumbnail", id)
		}
	}

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("file outside the state directory was deleted: %v", err)
	}
}

func TestValidDownloadIDShape(t *testing.T) {
	valid := []string{"0123abcd", "deadbeef", strings.Repeat("a", 64)}
	for _, id := range valid {
		if !validDownloadID(id) {
			t.Errorf("id %q should be valid", id)
		}
	}
	invalid := []string{
		"", "abc", "ABCDEF12", "../../x", "0123abc/", "0123abc.", "g0000000",
		strings.Repeat("a", 65), "0123abcd ",
	}
	for _, id := range invalid {
		if validDownloadID(id) {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

// TestSessionRoutesAreInertWithoutAuth covers the mode where Deps.Auth is nil.
// Three of the four session handlers guarded for it and handleLogout did not,
// which made a nil dereference reachable from an unauthenticated request.
func TestSessionRoutesAreInertWithoutAuth(t *testing.T) {
	d := testDeps(t)
	d.Unauthenticated = true
	d.Auth = nil
	s := testServer(t, d)

	cases := []struct {
		method, path string
		body         any
	}{
		{"DELETE", "/api/v1/session", nil},
		{"POST", "/api/v1/session", map[string]string{"password": "x"}},
		{"PUT", "/api/v1/session/password", map[string]string{"password": "abcdefgh", "confirmation": "abcdefgh"}},
	}
	for _, c := range cases {
		rec := do(t, s, c.method, c.path, c.body,
			map[string]string{"Cookie": sessionCookieName + "=abc.def"})
		if rec.Code >= 500 {
			t.Errorf("%s %s = %d, want a clean client-visible status (no panic)",
				c.method, c.path, rec.Code)
		}
	}

	// GET stays available because it has a real answer in this mode.
	rec := do(t, s, "GET", "/api/v1/session", nil,
		map[string]string{"Cookie": sessionCookieName + "=abc.def"})
	if rec.Code != 200 {
		t.Errorf("GET /api/v1/session = %d, want 200", rec.Code)
	}
}

// TestHostGuardIsLoopbackOnlyWithoutAuth pins the second half of the
// unauthenticated-mode hardening. With no session cookie to defeat DNS
// rebinding, a wildcard bind must not accept an arbitrary Host.
func TestHostGuardIsLoopbackOnlyWithoutAuth(t *testing.T) {
	d := testDeps(t)
	d.Unauthenticated = true
	d.Auth = nil
	d.Listen = "0.0.0.0:8080"
	s := testServer(t, d)

	for host, want := range map[string]int{
		"evil.example.com":   421,
		"attacker.test:8080": 421,
		"localhost:8080":     200,
		"127.0.0.1:8080":     200,
	} {
		req := httptest.NewRequest("GET", "/api/v1/downloads", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q = %d, want %d", host, rec.Code, want)
		}
	}
}

// TestLimiterEvictionKeepsLivePenalties covers the flush an attacker could
// perform by filling the table from many source addresses: eviction used to
// fall back to deleting in (randomized) map order, which could drop a penalty
// that had not expired.
func TestLimiterEvictionKeepsLivePenalties(t *testing.T) {
	l := newLoginLimiter()
	const victim = "10.0.0.1"
	l.fail(victim)
	l.fail(victim)
	l.fail(victim) // now has a multi-second penalty

	if ok, _ := l.allow(victim); ok {
		t.Fatal("victim should be penalised before the flood")
	}

	// Flood well past the table cap with distinct live clients.
	for i := 0; i < maxTrackedClient*2; i++ {
		l.fail("192.0.2." + itoa(i))
	}

	if ok, _ := l.allow(victim); ok {
		t.Error("a flood of new clients must not clear an existing penalty")
	}
	if len(l.attempts) > maxTrackedClient {
		t.Errorf("attempt table grew to %d, want <= %d", len(l.attempts), maxTrackedClient)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
