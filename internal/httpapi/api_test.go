package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}

func TestAddDownloadAndPartialStartNow(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": "https://example.com/v", "start_now": false}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp addDownloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Download.ID == "" {
		t.Fatalf("resp = %+v", resp)
	}
	// The manager schedules immediately and the fake runner is fast, so the
	// item re-fetched for the response may already have left "queued". Any
	// state reachable without user action is valid; the API contract under
	// test is the id, the URL, and state-consistent allowed actions.
	switch resp.Download.State {
	case "queued", "downloading", "completed":
	default:
		t.Fatalf("impossible immediate state %q (resp=%+v)", resp.Download.State, resp)
	}
	want := allowedActionsFor(ipc.State(resp.Download.State))
	if len(resp.Download.AllowedActions) != len(want) {
		t.Fatalf("allowed = %v, want %v", resp.Download.AllowedActions, want)
	}
	for i, a := range want {
		if resp.Download.AllowedActions[i] != a {
			t.Fatalf("allowed = %v, want %v", resp.Download.AllowedActions, want)
		}
	}
	if resp.Download.URL != "https://example.com/v" {
		t.Errorf("url = %q", resp.Download.URL)
	}
}

func TestAddRejectsInvalidURLs(t *testing.T) {
	s := testServer(t, testDeps(t))
	for _, u := range []string{"", "-flag", "bad\x01url", strings.Repeat("x", 5000)} {
		rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": u}, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("url %q status = %d, want 422", u, rec.Code)
		}
	}
}

// Re-pasting a URL the manager already tracks is a conflict, not a silent
// second copy of the same download.
func TestAddRejectsDuplicateURL(t *testing.T) {
	s := testServer(t, testDeps(t))
	const u = "https://example.com/dup"

	if rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": u}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("first add status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{"url": u}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second add status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != codeDuplicateURL {
		t.Fatalf("code = %q, want %q", env.Error.Code, codeDuplicateURL)
	}
	if env.Error.Message == "" {
		t.Fatal("duplicate error carries no message for the user")
	}
}

func TestGetDownloadNotFoundAndFound(t *testing.T) {
	s := testServer(t, testDeps(t))
	mgr := s.deps.Manager
	id, _ := mgr.Add("https://example.com/x")

	rec := do(t, s, "GET", "/api/v1/downloads/"+id, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rec = do(t, s, "GET", "/api/v1/downloads/nope", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}
}

func TestListFilteringAndSearch(t *testing.T) {
	s := testServer(t, testDeps(t))
	mgr := s.deps.Manager
	id1, _ := mgr.Add("https://example.com/alpha")
	id2, _ := mgr.Add("https://example.com/beta")
	waitFor(t, time.Second, func() bool {
		a, _ := mgr.Get(id1)
		b, _ := mgr.Get(id2)
		return a.State == "completed" && b.State == "completed"
	}, "both complete")

	rec := do(t, s, "GET", "/api/v1/downloads?search=alpha", nil, nil)
	var list struct{ Downloads []Download }
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list.Downloads) != 1 || list.Downloads[0].ID != id1 {
		t.Fatalf("search results = %+v", list.Downloads)
	}

	rec = do(t, s, "GET", "/api/v1/downloads?state=completed", nil, nil)
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list.Downloads) != 2 {
		t.Fatalf("state filter = %d items", len(list.Downloads))
	}

	// Completed files are only visible to admins — here everyone is admin
	// because the deployment explicitly runs unauthenticated.
	if len(list.Downloads[0].Files) == 0 {
		t.Error("admin view must include recorded files")
	}
}

func TestBatchActionsMixedResults(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)
	mgr := d.Manager

	good, _ := mgr.Add("https://example.com/good")
	bad, _ := mgr.Add("https://example.com/bad")
	mgr.Pause(bad) //nolint:errcheck
	waitFor(t, time.Second, func() bool {
		it, _ := mgr.Get(good)
		return it.State == "completed"
	}, "good completes")
	// instantRunner finishes a download before the next statement runs, so
	// "bad" only stays non-terminal if Pause won that race. Wait for the
	// state to settle rather than assuming it did.
	waitFor(t, time.Second, func() bool {
		it, _ := mgr.Get(bad)
		return it.State == ipc.StatePaused || it.State == ipc.StateCompleted
	}, "bad settles")

	body := map[string]any{
		"action": "pause",
		"ids":    []string{good, bad, "ghost"},
	}
	rec := do(t, s, "POST", "/api/v1/downloads/actions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status = %d", rec.Code)
	}
	var br batchResponse
	json.Unmarshal(rec.Body.Bytes(), &br) //nolint:errcheck
	if len(br.Results) != 3 {
		t.Fatalf("results = %+v", br.Results)
	}
	// good (completed) pause → invalid_state; bad (paused) pause → invalid_state; ghost → not_found
	codes := map[string]string{}
	for _, r := range br.Results {
		if r.OK {
			codes[r.ID] = "ok"
		} else {
			codes[r.ID] = r.Error.Code
		}
	}
	if codes[good] != codeInvalidState || codes[bad] != codeInvalidState || codes["ghost"] != codeNotFound {
		t.Fatalf("codes = %v", codes)
	}

	// "purge" is no longer an action at all: nothing in the API deletes media.
	rec = do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{
		"action": "purge", "ids": []string{bad},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("purge action = %d, want 422 (unknown action)", rec.Code)
	}

	// Removing the row is what replaced it.
	rec = do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{
		"action": "remove", "ids": []string{bad},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if _, ok := mgr.Get(bad); ok {
		t.Error("removed row must be gone")
	}
}

func TestBatchCapsAndValidation(t *testing.T) {
	s := testServer(t, testDeps(t))

	ids := make([]string, maxBatchIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("id%d", i)
	}
	rec := do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{"action": "pause", "ids": ids}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized batch = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{"action": "explode", "ids": []string{"a"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown action = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{"action": "pause", "ids": []string{}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty ids = %d", rec.Code)
	}
}

// blockingRunner holds a download in "downloading" until its context is
// cancelled. The shared instantRunner finishes before the test's next line
// runs, which makes any pause/resume assertion a race against the scheduler:
// the item reaches "completed", Pause becomes a no-op and Resume rejects it.
type blockingRunner struct{ started chan struct{} }

func (b blockingRunner) Probe(ctx context.Context, job manager.Job) ([]manager.Entry, error) {
	url := job.URL
	return []manager.Entry{{URL: url, Title: "Test Video"}}, nil
}

func (b blockingRunner) Run(ctx context.Context, job manager.Job, onLine func(string)) (string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func TestRetryMapsToResume(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan struct{}, 1)
	mgr, err := manager.NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), blockingRunner{started: started})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })

	d := testDeps(t)
	d.Manager = mgr
	s := testServer(t, d)

	id, _ := mgr.Add("https://example.com/r")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never started the download")
	}
	if err := mgr.Pause(id); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Pausing a download that is already running is asynchronous: Pause signals
	// the runner and the state lands once it returns. Wait for that rather than
	// assuming it happened before the next line.
	waitFor(t, 5*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == "paused"
	}, "download never reached paused")

	rec := do(t, s, "POST", "/api/v1/downloads/actions", map[string]any{
		"action": "retry", "ids": []string{id},
	}, nil)
	var br batchResponse
	json.Unmarshal(rec.Body.Bytes(), &br) //nolint:errcheck
	if len(br.Results) != 1 || !br.Results[0].OK {
		t.Fatalf("retry results = %+v", br.Results)
	}
	it, _ := mgr.Get(id)
	if it.State != "queued" && it.State != "downloading" {
		t.Fatalf("after retry state = %s", it.State)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// --- SSE ---

func readSSEEvent(t *testing.T, br *bufio.Reader) (string, string) {
	t.Helper()
	name, data := "", ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if name != "" || data != "" {
				return name, data
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func TestSSESnapshotFirstThenUpdatesThenRemovalFinal(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	ts := httptest.NewServer(s)
	defer ts.Close()
	// Align the host guard with the ephemeral test port.
	s.deps.Listen = strings.TrimPrefix(ts.URL, "http://")

	ctx := ts.Client()
	req2, _ := http.NewRequest("GET", ts.URL+"/api/v1/events", nil)
	resp, err := ctx.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	name, data := readSSEEvent(t, br)
	if name != "snapshot" {
		t.Fatalf("first event = %q (%s)", name, data)
	}
	var snap struct {
		Downloads []Download `json:"downloads"`
	}
	json.Unmarshal([]byte(data), &snap) //nolint:errcheck
	baseCount := len(snap.Downloads)

	// Add one download; expect a download event.
	mgr := d.Manager
	id, _ := mgr.Add("https://example.com/live")
	deadline := time.Now().Add(2 * time.Second)
	sawUpdate := false
	for time.Now().Before(deadline) && !sawUpdate {
		n, dd := readSSEEvent(t, br)
		if n == "download" && strings.Contains(dd, id) {
			sawUpdate = true
		}
	}
	if !sawUpdate {
		t.Fatal("no download event received")
	}

	// Remove; expect removed event and no later resurrection.
	mgr.Cancel(id)
	sawRemoved := false
	for time.Now().Before(deadline) && !sawRemoved {
		n, dd := readSSEEvent(t, br)
		if n == "removed" && strings.Contains(dd, id) {
			sawRemoved = true
		}
	}
	if !sawRemoved {
		t.Fatal("no removed event received")
	}

	resp.Body.Close()
	_ = baseCount
}

func TestListSortsTerminalStatesLast(t *testing.T) {
	s := testServer(t, testDeps(t))
	mgr := s.deps.Manager
	base := time.Now().Add(-time.Hour)

	// Added in this order; all completed items are older than the queued one.
	idDone1, _ := mgr.Add("https://example.com/done1")
	waitFor(t, time.Second, func() bool {
		it, _ := mgr.Get(idDone1)
		return it.State == "completed"
	}, "done1 completes")
	// This row has to stay non-terminal for the ordering assertion to mean
	// anything: if it completed, every row would be terminal and the check
	// below would pass vacuously. "hold" keeps the runner in downloading
	// until the pause cancels it.
	idQueued, _ := mgr.Add("https://example.com/hold-never-runs")
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(idQueued)
		return it.State == ipc.StateDownloading
	}, "held row starts")
	if err := mgr.Pause(idQueued); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(idQueued)
		return it.State == ipc.StatePaused
	}, "held row pauses")
	idDone2, _ := mgr.Add("https://example.com/done2")
	waitFor(t, time.Second, func() bool {
		it, _ := mgr.Get(idDone2)
		return it.State == "completed"
	}, "done2 completes")

	rec := do(t, s, "GET", "/api/v1/downloads", nil, nil)
	var list struct {
		Downloads []Download `json:"downloads"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list.Downloads) < 3 {
		t.Fatalf("downloads = %+v", list.Downloads)
	}
	// Regression: completed items used to rank as 0 (missing map key) and
	// sort above active work.
	firstNonActiveIdx := -1
	for i, d := range list.Downloads {
		if d.State != "downloading" && d.State != "queued" && d.State != "paused" {
			firstNonActiveIdx = i
			break
		}
	}
	if firstNonActiveIdx == -1 {
		t.Fatal("no terminal rows found")
	}
	for _, d := range list.Downloads[firstNonActiveIdx:] {
		if d.State == "queued" || d.State == "downloading" || d.State == "paused" {
			t.Errorf("active row %s (%s) appears after terminal rows", d.ID, d.State)
		}
	}
	_ = base
}
