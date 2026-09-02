package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeJSONRejectsBadRequests(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &body, MaxBodyBytes); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
	}

	cases := []struct {
		name    string
		body    string
		ct      string
		wantErr bool
	}{
		{"valid", `{"name":"x"}`, "application/json", false},
		{"unknown field", `{"nope":1}`, "application/json", true},
		{"trailing value", `{"name":"x"} {"a":1}`, "application/json", true},
		{"non-object", `[1,2,3]`, "application/json", true},
		{"wrong content type", `{"name":"x"}`, "text/plain", true},
		{"garbage", `{`, "application/json", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/x", strings.NewReader(c.body))
			req.Header.Set("Content-Type", c.ct)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if got := rec.Code; (got != http.StatusOK) != c.wantErr {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDecodeJSONSizeLimit(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSON(w, r, &body, 64)
	}
	big := `{"pad":"` + strings.Repeat("x", 200) + `"}`
	req := httptest.NewRequest("POST", "/x", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	s := testServer(t, testDeps(t))
	req := httptest.NewRequest("GET", "/api/v1/downloads/zzzz", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != codeNotFound || env.Error.Message == "" || env.Error.RequestID == "" {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if rec.Header().Get("X-Request-ID") != env.Error.RequestID {
		t.Error("request id must be echoed in header and envelope")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("API error cache policy = %q, want no-store", got)
	}
}

func TestSessionJSONIsNeverCacheable(t *testing.T) {
	s := testServer(t, testDeps(t))
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("session cache policy = %q, want no-store", got)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	s := testServer(t, testDeps(t))
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	for _, h := range []string{
		"Content-Security-Policy", "X-Content-Type-Options",
		"Referrer-Policy", "Permissions-Policy", "X-Request-ID",
	} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("weak CSP: %s", csp)
	}
}

func TestHostGuardRejectsUnexpectedHost(t *testing.T) {
	d := testDeps(t)
	d.Listen = "127.0.0.1:8080"
	s := New(d)

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Host = "evil.example.com:8080"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unexpected host status = %d, want 421", rec.Code)
	}

	req.Host = "localhost:8080"
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("localhost host status = %d", rec2.Code)
	}
}

func TestOriginGuardBlocksCrossSiteMutation(t *testing.T) {
	s := testServer(t, testDeps(t))
	req := httptest.NewRequest("POST", "/api/v1/downloads/actions", strings.NewReader(`{"action":"pause","ids":["x"]}`))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation status = %d, want 403", rec.Code)
	}
}

func TestSessionsLifecycle(t *testing.T) {
	key := make([]byte, 32)
	ss := NewSessions(key)
	value, csrf := ss.Create()

	sess, err := ss.Verify(value)
	if err != nil {
		t.Fatal(err)
	}
	if sess.csrf != csrf {
		t.Error("csrf mismatch")
	}

	// Tampered signature must fail.
	bad := value[:len(value)-2] + "xx"
	if _, err := ss.Verify(bad); err == nil {
		t.Error("tampered cookie accepted")
	}
	if !ss.Destroy(value) {
		t.Error("destroy failed")
	}
	if _, err := ss.Verify(value); err == nil {
		t.Error("destroyed session still valid")
	}
}

func TestLoadOrCreateKeyPersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.key")

	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key perms = %v", fi.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("existing key was not reused")
	}
}

func TestLoadOrCreateKeyRotatesOversizedFileWithBoundedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.key")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxSessionKeyFileBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != sessionKeyBytes {
		t.Fatalf("key length = %d, want %d", len(key), sessionKeyBytes)
	}
	if info, err := os.Stat(path); err != nil || info.Size() > maxSessionKeyFileBytes {
		t.Fatalf("oversized key file was not replaced: info=%v err=%v", info, err)
	}
}

func TestLoginLimiterExponentialCap(t *testing.T) {
	l := newLoginLimiter()
	if d := delayFor(0); d != 0 {
		t.Errorf("delay(0) = %v", d)
	}
	if d := delayFor(3); d != 2*time.Second {
		t.Errorf("delay(3) = %v", d)
	}
	if d := delayFor(40); d != loginMaxDelay {
		t.Errorf("delay(40) = %v, want cap", d)
	}
	if ok, _ := l.allow("1.2.3.4"); !ok {
		t.Error("fresh client must be allowed")
	}
	l.fail("1.2.3.4")
	if ok, retry := l.allow("1.2.3.4"); ok || retry <= 0 {
		t.Error("client after failure must wait")
	}
	l.success("1.2.3.4")
	if ok, _ := l.allow("1.2.3.4"); !ok {
		t.Error("successful login must clear limiter")
	}
}
