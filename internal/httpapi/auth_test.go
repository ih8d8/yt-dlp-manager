package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yt-dlp-manager/internal/config"
)

// authedDeps builds a server with real password authentication.
func authedDeps(t *testing.T) (Deps, string) {
	t.Helper()
	d := testDeps(t)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "admin.json")
	a, err := NewAuthWithKey(credentialPath, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ChangePassword("correct-horse-battery", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	d.Auth = a
	d.Unauthenticated = false
	return d, "correct-horse-battery"
}

func login(t *testing.T, s *Server, pw string) (*http.Cookie, string, int) {
	t.Helper()
	rec := do(t, s, "POST", "/api/v1/session", map[string]any{"password": pw}, nil)
	var sr sessionResponse
	json.Unmarshal(rec.Body.Bytes(), &sr) //nolint:errcheck
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c, sr.CSRFToken, rec.Code
		}
	}
	return nil, sr.CSRFToken, rec.Code
}

func TestLoginSetsHardenedCookieAndCSRF(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)

	cookie, csrf, code := login(t, s, pw)
	if code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", code, "")
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie flags wrong: %+v", cookie)
	}
	if cookie.Secure {
		t.Error("Secure must be off unless configured")
	}
	if csrf == "" {
		t.Fatal("no csrf token returned")
	}
}

func TestSessionDiscoveryIsAnonymousWithoutAConsoleError(t *testing.T) {
	d, _ := authedDeps(t)
	s := testServer(t, d)
	rec := do(t, s, "GET", "/api/v1/session", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous session discovery = %d, want 200", rec.Code)
	}
	var response sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Authenticated || response.CSRFToken != "" {
		t.Fatalf("anonymous response exposed authenticated state: %+v", response)
	}
}

func TestAuthGateRejectsAndAccepts(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "GET", "/api/v1/downloads", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}

	cookie, csrf, _ := login(t, s, pw)
	hdr := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}

	rec = do(t, s, "GET", "/api/v1/downloads", nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET = %d", rec.Code)
	}

	// Mutation without CSRF must fail; with it must pass.
	rec = do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": "https://x/1"}, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mutation without csrf = %d", rec.Code)
	}
	hdr["X-CSRF-Token"] = csrf
	rec = do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": "https://x/1"}, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mutation with csrf = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWrongPasswordIdenticalResponse(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)

	ok1 := do(t, s, "POST", "/api/v1/session", map[string]any{"password": pw}, nil)
	if ok1.Code != 200 {
		t.Fatalf("good login = %d", ok1.Code)
	}

	// Distinct client addresses so the shared rate limiter cannot interfere;
	// the point here is response indistinguishability, not limiting.
	badReq := func(addr, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/session",
			strings.NewReader(`{"password":"`+pw+`"}`))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	bad1 := badReq("192.0.2.10:1", "wrong-wrong-wrong")
	bad2 := badReq("192.0.2.11:1", "also-also-also-wrong")
	stripID := func(rec *httptest.ResponseRecorder) string {
		var env struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &env) //nolint:errcheck
		env.Error.RequestID = ""
		b, _ := json.Marshal(env)
		return string(b)
	}
	if bad1.Code != bad2.Code || stripID(bad1) != stripID(bad2) {
		t.Error("wrong-password responses must be indistinguishable (modulo request id)")
	}
}

func TestSessionRotationAndLogout(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)

	c1, csrf1, _ := login(t, s, pw)
	hdr1 := map[string]string{"Cookie": c1.Name + "=" + c1.Value, "X-CSRF-Token": csrf1}

	// Second login rotates to a brand-new session id.
	c2, csrf2, _ := login(t, s, pw)
	if c1.Value == c2.Value {
		t.Fatal("session id must rotate on login")
	}

	// Old session still valid until logout destroys it explicitly.
	rec := do(t, s, "DELETE", "/api/v1/session", nil, hdr1)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	hdr1b := map[string]string{"Cookie": c1.Name + "=" + c1.Value}
	rec = do(t, s, "GET", "/api/v1/downloads", nil, hdr1b)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("logged-out session still accepted: %d", rec.Code)
	}

	// The rotated session remains usable.
	hdr2 := map[string]string{"Cookie": c2.Name + "=" + c2.Value}
	rec = do(t, s, "GET", "/api/v1/downloads", nil, hdr2)
	if rec.Code != http.StatusOK {
		t.Errorf("rotated session broken: %d", rec.Code)
	}
	_ = csrf2
}

func TestWeakPasswordRefused(t *testing.T) {
	a, err := NewAuth(filepath.Join(t.TempDir(), "admin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ChangePassword("short", "short"); err == nil {
		t.Fatal("weak password must be refused")
	}
}

func TestFirstRunPasswordSetupAndHashPersists(t *testing.T) {
	d := testDeps(t)
	path := filepath.Join(t.TempDir(), "admin.json")
	a, err := NewAuthWithKey(path, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	d.Auth, d.Unauthenticated = a, false
	s := testServer(t, d)

	rec := do(t, s, "GET", "/api/v1/session", nil, nil)
	var setup sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, candidate := range rec.Result().Cookies() {
		if candidate.Name == sessionCookieName {
			cookie = candidate
			break
		}
	}
	if rec.Code != http.StatusOK || cookie == nil || setup.Authenticated ||
		!setup.SetupRequired || setup.CSRFToken == "" || !a.SetupRequired() {
		t.Fatalf("setup discovery = %d cookie=%v response=%+v", rec.Code, cookie != nil, setup)
	}
	hdr := map[string]string{
		"Cookie":       cookie.Name + "=" + cookie.Value,
		"X-CSRF-Token": setup.CSRFToken,
	}
	if protected := do(t, s, "GET", "/api/v1/downloads", nil, hdr); protected.Code != http.StatusForbidden ||
		!strings.Contains(protected.Body.String(), codePasswordChange) {
		t.Fatalf("app was not locked during setup: %d %s", protected.Code, protected.Body.String())
	}
	if loginAttempt := do(t, s, "POST", "/api/v1/session", map[string]any{"password": "anything"}, nil); loginAttempt.Code != http.StatusConflict {
		t.Fatalf("login during setup = %d, want 409", loginAttempt.Code)
	}
	newPassword := "a-new-and-strong-admin-password"
	rec = do(t, s, "PUT", "/api/v1/session/password", map[string]any{
		"password": newPassword, "confirmation": newPassword,
	}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("password setup = %d %s", rec.Code, rec.Body.String())
	}
	var completed sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if !completed.Authenticated || completed.SetupRequired || completed.CSRFToken == "" || a.SetupRequired() {
		t.Fatalf("completed setup response = %+v", completed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), newPassword) {
		t.Fatal("credential file contains plaintext")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v err=%v", info.Mode().Perm(), err)
	}

	reloaded, err := NewAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	okNew, _ := reloaded.checkPassword(context.Background(), newPassword)
	okBad, _ := reloaded.checkPassword(context.Background(), "not-the-password")
	if reloaded.SetupRequired() || !okNew || okBad {
		t.Fatal("persisted first-run verifier is invalid")
	}
}

func TestFirstRunSetupCanOnlyCompleteOnce(t *testing.T) {
	a, err := NewAuth(filepath.Join(t.TempDir(), "admin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetupPassword("first-strong-password", "first-strong-password"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetupPassword("second-strong-password", "second-strong-password"); !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second setup = %v, want ErrSetupComplete", err)
	}
	okFirst, _ := a.checkPassword(context.Background(), "first-strong-password")
	okSecond, _ := a.checkPassword(context.Background(), "second-strong-password")
	if !okFirst || okSecond {
		t.Fatal("second setup replaced the password")
	}
}

func TestCredentialSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "admin.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuth(link); err == nil {
		t.Fatal("credential symlink must be refused")
	}
}

func TestSettingsRoundTripAndLiveConcurrency(t *testing.T) {
	d := testDeps(t)
	d.Store = config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	s := testServer(t, d)

	rec := do(t, s, "GET", "/api/v1/settings", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var view settingsView
	json.Unmarshal(rec.Body.Bytes(), &view) //nolint:errcheck
	if view.UI.Theme != "dark" {
		t.Fatalf("theme = %q", view.UI.Theme)
	}

	rec = do(t, s, "PUT", "/api/v1/settings", map[string]any{
		"ui":        map[string]any{"theme": "light"},
		"downloads": map[string]any{"max_concurrent": 7},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings = %d %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &view) //nolint:errcheck
	if view.UI.Theme != "light" || view.Downloads.MaxConcurrent != 7 {
		t.Fatalf("updated view = %+v", view)
	}

	// Invalid values rejected without corrupting state.
	rec = do(t, s, "PUT", "/api/v1/settings", map[string]any{
		"downloads": map[string]any{"max_concurrent": 999},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid max = %d", rec.Code)
	}
	rec = do(t, s, "PUT", "/api/v1/settings", map[string]any{"ui": map[string]any{"theme": "neon"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid theme = %d", rec.Code)
	}
}

func TestYtDlpManagedEndpointGatingAndWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "yt-dlp-config")
	os.WriteFile(cfgPath, []byte("# user stuff\n--retries 5\n"), 0o600) //nolint:errcheck

	d := testDeps(t)
	d.YtDlpConfigPath = cfgPath

	// Editing disabled by default.
	s := testServer(t, d)
	rec := do(t, s, "PUT", "/api/v1/settings/yt-dlp/managed", map[string]any{"format": "bv*+ba/b"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled edit = %d", rec.Code)
	}
	rec = do(t, s, "GET", "/api/v1/settings/yt-dlp", nil, nil)
	var resp ytDlpSettingsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if resp.EditEnabled {
		t.Error("edit flag must be reported disabled")
	}
	if resp.Settings != nil {
		t.Error("no block expected yet")
	}

	// Enabled via deployment setting.
	d.AllowYtDlpConfigEdit = true
	s2 := testServer(t, d)
	rec = do(t, s2, "PUT", "/api/v1/settings/yt-dlp/managed", map[string]any{
		"downloads_dir":   "/downloads/movies",
		"format":          `bv*+ba/b`,
		"output_template": `%(title)s [%(id)s].%(ext)s`,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("managed write = %d %s", rec.Code, rec.Body.String())
	}
	data, _ := os.ReadFile(cfgPath)
	txt := string(data)
	if !strings.HasPrefix(txt, "# user stuff\n--retries 5\n") {
		t.Errorf("outside bytes not preserved:\n%q", txt)
	}
	if !strings.Contains(txt, "--paths /downloads/movies") {
		t.Errorf("block missing:\n%q", txt)
	}

	// Unsafe values rejected.
	rec = do(t, s2, "PUT", "/api/v1/settings/yt-dlp/managed", map[string]any{
		"downloads_dir": "/etc",
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe dir = %d", rec.Code)
	}
}

func TestSystemEndpointsPrivacy(t *testing.T) {
	s := testServer(t, testDeps(t))

	rec := do(t, s, "GET", "/healthz", nil, nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "/") {
		t.Errorf("healthz leaked detail: %s", rec.Body.String())
	}

	rec = do(t, s, "GET", "/readyz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("readyz should be ready here: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, "GET", "/api/v1/system", nil, nil)
	var sys systemResponse
	json.Unmarshal(rec.Body.Bytes(), &sys) //nolint:errcheck
	if sys.Version.AppVersion == "" {
		t.Error("system missing version")
	}
}

func TestUnknownAPIRouteStaysJSON(t *testing.T) {
	s := testServer(t, testDeps(t))
	req := httptest.NewRequest("GET", "/api/v2/future", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("API 404 content type = %s", ct)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("API 404 returned HTML")
	}
}

func TestStaticSPAFallback(t *testing.T) {
	d := testDeps(t)
	d.Web = fakeWebFS(map[string]string{
		"index.html":         "<html>app</html>",
		"assets/app-abc.css": "body{}",
	})
	s := testServer(t, d)

	rec := do(t, s, "GET", "/", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("index = %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index cache = %q", cc)
	}

	rec = do(t, s, "GET", "/assets/app-abc.css", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("asset = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset cache = %q", cc)
	}

	rec = do(t, s, "GET", "/queue/some/route", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("spa fallback = %d %s", rec.Code, rec.Body.String())
	}
}

type fakeWebFS map[string]string

func (f fakeWebFS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if data, ok := f[name]; ok {
		w.Write([]byte(data)) //nolint:errcheck
		return
	}
	// Mirror webui.Handler behavior: SPA fallback for routes, 404 for assets.
	if strings.Contains(name, "assets/") || strings.Contains(name, ".css") {
		http.NotFound(w, r)
		return
	}
	w.Write([]byte(f["index.html"])) //nolint:errcheck
}
