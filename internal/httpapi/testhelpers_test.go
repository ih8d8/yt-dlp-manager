package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/manager"
)

type instantRunner struct{ dir string }

func (f instantRunner) Probe(ctx context.Context, job manager.Job) ([]manager.Entry, error) {
	url := job.URL
	return []manager.Entry{{URL: url, Title: "Test Video"}}, nil
}

func (f instantRunner) Run(ctx context.Context, job manager.Job, onLine func(string)) (string, error) {
	url := job.URL
	if strings.Contains(url, "hold") {
		// Opt-in: a URL containing "hold" stays in "downloading" until it is
		// cancelled. Everything else finishes before the caller's next
		// statement, which makes Add-then-Pause a race the scheduler usually
		// wins — the row reaches "completed" and Pause becomes a no-op.
		<-ctx.Done()
		return "", ctx.Err()
	}
	target := filepath.Join(f.dir, "done.bin")
	onLine(manager.PrintLine("@g|", target))
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	return "", nil
}

// newTestManager builds a real manager over a fake yt-dlp runner.
func newTestManager(t *testing.T) *manager.Manager {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := manager.NewWithRunner(ctx, 4, filepath.Join(dir, "state.json"), instantRunner{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); mgr.Close() })
	return mgr
}

// newTestManagerWithRunner is newTestManager with a caller-supplied runner,
// for tests that need a capability instantRunner does not have.
func newTestManagerWithRunner(t *testing.T, r manager.Runner) *manager.Manager {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := manager.NewWithRunner(ctx, 4, filepath.Join(dir, "state.json"), r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); mgr.Close() })
	return mgr
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	auth := &Auth{sessions: NewSessions([]byte("0123456789abcdef0123456789abcdef")), limiter: newLoginLimiter()}
	testHash := sha256.Sum256([]byte("test-only-admin-password"))
	auth.passwordHash = testHash[:]
	return Deps{
		Manager:         newTestManager(t),
		Config:          &cfg,
		Sources:         map[string]config.Source{"downloads.max_concurrent": config.SourceDefault},
		Store:           store,
		YtDlpConfigPath: filepath.Join(dir, "ytdlp-config"),
		Auth:            auth,
		Listen:          "127.0.0.1:8080",
		StatePath:       filepath.Join(dir, "state.json"),
		Web:             nil,
		Version:         VersionInfo{AppVersion: "test"},
		Logger:          nil,
		StartTime:       time.Now(),
		// Handler tests run unauthenticated; auth flows get dedicated tests.
		Unauthenticated: true,
	}
}

func testServer(t *testing.T, d Deps) *Server {
	t.Helper()
	s := New(d)
	return s
}

// doJSON performs a request against the server handler.
func do(t *testing.T, s *Server, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	switch b := body.(type) {
	case nil:
		rd = strings.NewReader("")
	case string:
		rd = strings.NewReader(b)
	default:
		data, err := jsonMarshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "localhost:8080"
	// httptest defaults RemoteAddr to a public test-net address. These tests
	// model a browser on the same machine as the server, which is what
	// first-run setup authorization keys off; cases about remote peers set
	// their own RemoteAddr.
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
