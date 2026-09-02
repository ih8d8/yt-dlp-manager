package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

// clearRunner completes instantly and writes a file on disk so tests can
// prove that clear operations never delete completed output files.
type clearRunner struct {
	dir string
}

func (c clearRunner) Probe(ctx context.Context, job manager.Job) ([]manager.Entry, error) {
	url := job.URL
	return []manager.Entry{{URL: url, Title: "Clear Video"}}, nil
}

func (c clearRunner) Run(ctx context.Context, job manager.Job, onLine func(string)) (string, error) {
	url := job.URL
	if strings.Contains(url, "hold") {
		// Stays in "downloading" until cancelled. A runner that finishes
		// instantly makes Add-then-Pause a race against the scheduler: the
		// row reaches "completed" and Pause becomes a no-op, so a test that
		// needs a genuinely unfinished row cannot rely on it.
		<-ctx.Done()
		return "", ctx.Err()
	}
	target := filepath.Join(c.dir, "done.bin")
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	onLine(manager.PrintLine("@g|", target))
	return "", nil
}

func newClearTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	d := testDeps(t)
	mediaDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := manager.NewWithRunner(ctx, 4, filepath.Join(mediaDir, "state.json"), clearRunner{dir: mediaDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); mgr.Close() })
	d.Manager = mgr
	s := testServer(t, d)
	return s, mediaDir
}

func clearDo(t *testing.T, s *Server, body any) *http.Response {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/v1/downloads/clear", body, nil)
	return rec.Result()
}

func clearCall(t *testing.T, s *Server, body any) (int, clearResponse) {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/v1/downloads/clear", body, nil)
	var resp clearResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, resp
}

// TestClearFinishedRemovesHistoryKeepsFiles: finished scope drops completed,
// failed and deleted rows, keeps queued/downloading rows, and never deletes
// completed output files.
func TestClearFinishedRemovesHistoryKeepsFiles(t *testing.T) {
	s, mediaDir := newClearTestServer(t)
	mgr := s.deps.Manager

	done, _ := mgr.Add("https://example.com/done")
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(done)
		return it.State == ipc.StateCompleted
	}, "download to finish")
	waitFor(t, 2*time.Second, func() bool {
		it, _ := mgr.Get(done)
		return len(it.Files) > 0
	}, "completed file to be recorded")

	queued, _ := mgr.Add("https://example.com/hold")
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(queued)
		return it.State == ipc.StateDownloading
	}, "second download to start")
	// Pause it so it is a non-finished row that must survive.
	if err := mgr.Pause(queued); err != nil {
		t.Fatal(err)
	}
	// Pausing a running download is asynchronous: the state lands when the
	// runner returns. Wait for it rather than racing the clear against it.
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(queued)
		return it.State == ipc.StatePaused
	}, "second download to pause")

	code, resp := clearCall(t, s, map[string]any{"scope": "finished"})
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, "n/a")
	}
	if resp.Scope != "finished" || resp.Removed != 1 {
		t.Fatalf("resp = %+v, want scope=finished removed=1", resp)
	}
	if _, ok := mgr.Get(queued); !ok {
		t.Error("paused item must survive clear finished")
	}
	for _, it := range mgr.List() {
		switch it.State {
		case ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
			t.Errorf("finished state %q survived clear finished", it.State)
		}
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "done.bin")); err != nil {
		t.Errorf("completed output file must be kept: %v", err)
	}
}

// TestClearAllCancelsActiveAndKeepsFiles: clear all stops the active download
// through its normal cancellation path, removes every row, and keeps
// completed files on disk.
func TestClearAllCancelsActiveAndKeepsFiles(t *testing.T) {
	s, mediaDir := newClearTestServer(t)
	mgr := s.deps.Manager

	done, _ := mgr.Add("https://example.com/done")
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(done)
		return it.State == ipc.StateCompleted
	}, "download to finish")

	code, resp := clearCall(t, s, map[string]any{"scope": "all"})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Scope != "all" {
		t.Fatalf("scope = %q", resp.Scope)
	}
	waitFor(t, 5*time.Second, func() bool {
		return len(mgr.List()) == 0
	}, "queue to empty after clear all")
	if resp.Removed < 1 {
		t.Fatalf("removed = %d, want >= 1", resp.Removed)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "done.bin")); err != nil {
		t.Errorf("completed output file must be kept: %v", err)
	}
}

// TestClearValidation: malformed/unknown scopes fail cleanly with the
// standard error envelope.
func TestClearValidation(t *testing.T) {
	s, _ := newClearTestServer(t)

	cases := []struct {
		name string
		body any
		want int
	}{
		{"missing scope", map[string]any{}, http.StatusUnprocessableEntity},
		{"empty scope", map[string]any{"scope": ""}, http.StatusUnprocessableEntity},
		{"unknown scope", map[string]any{"scope": "everything"}, http.StatusUnprocessableEntity},
		{"malformed json", "{not json", http.StatusBadRequest},
		{"trailing json", `{"scope":"all"} {}`, http.StatusBadRequest},
		{"unknown field", map[string]any{"scope": "all", "extra": 1}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := do(t, s, http.MethodPost, "/api/v1/downloads/clear", tc.body, nil)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// TestDeleteAllEndpointIsGone pins the removal of file deletion from the API.
// The app downloads into a directory the operator owns; deleting media there
// belongs to a file manager, not to a network service.
func TestDeleteAllEndpointIsGone(t *testing.T) {
	s, mediaDir := newClearTestServer(t)
	id, _ := s.deps.Manager.Add("https://example.com/delete-all")
	waitFor(t, 5*time.Second, func() bool {
		it, ok := s.deps.Manager.Get(id)
		return ok && it.State == ipc.StateCompleted && len(it.Files) > 0
	}, "download to finish")
	target := filepath.Join(mediaDir, "done.bin")

	rec := do(t, s, http.MethodPost, "/api/v1/downloads/delete-all", map[string]any{
		"confirmation": "DELETE ALL FILES",
	}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete-all = %d, want 404 (endpoint removed)", rec.Code)
	}

	// "purge" is no longer an action either.
	rec = do(t, s, http.MethodPost, "/api/v1/downloads/actions", map[string]any{
		"action": "purge", "ids": []string{id},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("purge action = %d, want 422 (unknown action)", rec.Code)
	}

	// And clearing the queue leaves the media alone.
	rec = do(t, s, http.MethodPost, "/api/v1/downloads/clear", map[string]any{"scope": "all"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear all = %d", rec.Code)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("finished file must survive clearing the queue: %v", err)
	}
	if len(s.deps.Manager.List()) != 0 {
		t.Fatal("queue entry survived clear all")
	}
}

// TestClearRequiresAuthAndCSRF: with authentication enabled the clear
// endpoint must reject anonymous callers and mutations without the session's
// CSRF token. The response body is the standard error envelope.
func TestClearRequiresAuthAndCSRF(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)
	cookie, csrf, code := login(t, s, pw)
	if code != http.StatusOK || cookie == nil {
		t.Fatalf("login failed: code=%d", code)
	}

	// Anonymous: 401 envelope.
	rec := do(t, s, http.MethodPost, "/api/v1/downloads/clear", map[string]any{"scope": "all"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
	// Authenticated but no CSRF token: 403.
	rec = do(t, s, http.MethodPost, "/api/v1/downloads/clear", map[string]any{"scope": "all"},
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-csrf status = %d, want 403", rec.Code)
	}
	// Wrong token: 403.
	rec = do(t, s, http.MethodPost, "/api/v1/downloads/clear", map[string]any{"scope": "all"},
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "X-CSRF-Token": "nope"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad-csrf status = %d, want 403", rec.Code)
	}
	// Correct token: 200 with an accurate (zero) count.
	rec = do(t, s, http.MethodPost, "/api/v1/downloads/clear", map[string]any{"scope": "finished"},
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("with-csrf status = %d, want 200", rec.Code)
	}
	var resp clearResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope != "finished" || resp.Removed != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}
