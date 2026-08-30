package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/manager"
)

// newHTTPTestServer builds a Server bound to a real loopback listener and
// configures Deps.Listen to match before any request is served, so the host
// guard cannot race test setup.
func newHTTPTestServer(t *testing.T, d *Deps) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.Listen = ln.Addr().String()
	s := New(*d)
	srv := http.Server{Handler: s}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s, "http://" + ln.Addr().String()
}

// stagedRunner emits spaced-out progress lines so the manager's 200ms
// publish throttle cannot coalesce them all into one update. It simulates
// what real yt-dlp prints during a multi-second download.
type stagedRunner struct{ dir string }

func (s stagedRunner) Probe(ctx context.Context, url string) ([]manager.Entry, error) {
	return []manager.Entry{{URL: url, Title: "Staged Video"}}, nil
}

func (s stagedRunner) Run(ctx context.Context, url string, onLine func(string)) (string, error) {
	onLine(manager.PrintLine("@t|", "Staged Video"))
	for _, line := range []string{
		"@p|100|1000|50|18",
		"@p|300|1000|60|12",
		"@p|600|1000|70|6",
	} {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(350 * time.Millisecond):
		}
		onLine(line)
	}
	target := filepath.Join(s.dir, "staged.bin")
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	onLine(manager.PrintLine("@g|", target))
	return "", nil
}

type sseFrame struct {
	name string
	data string
}

// readSSE consumes the stream until the terminal frame arrives or timeout.
func readSSE(t *testing.T, url string, done func(f sseFrame) bool) []sseFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	var frames []sseFrame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var name, data string
	flush := func() {
		if name != "" {
			f := sseFrame{name: name, data: data}
			frames = append(frames, f)
			name, data = "", ""
			if done(f) {
				cancel()
			}
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			flush()
		}
	}
	return frames
}

type wireDownload struct {
	ID              string  `json:"id"`
	State           string  `json:"state"`
	Progress        float64 `json:"progress"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
}

// Regression for the reported frozen-progress bug: while a download runs,
// the SSE transport must deliver at least one intermediate "download" event
// whose progress is strictly between 0% and 100%, followed by a completion
// event that moves the item to the Library's completed state.
func TestEventsStreamEmitsIntermediateProgressBeforeCompletion(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := manager.NewWithRunner(ctx, 2, filepath.Join(dir, "state.json"), stagedRunner{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); mgr.Close() })

	d := testDeps(t)
	d.Manager = mgr
	s, base := newHTTPTestServer(t, &d)

	addBody := strings.NewReader(`{"url":"https://example.com/video","start_now":true}`)
	addReq, err := http.NewRequest(http.MethodPost, base+"/api/v1/downloads", addBody)
	if err != nil {
		t.Fatal(err)
	}
	addReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, addReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d body %s", rec.Code, rec.Body.String())
	}
	var added struct {
		Download struct {
			ID string `json:"id"`
		} `json:"download"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	id := added.Download.ID

	frames := readSSE(t, base+"/api/v1/events", func(f sseFrame) bool {
		return f.name == "download" && strings.Contains(f.data, `"state":"completed"`)
	})

	var (
		sawSnapshot       bool
		intermediate      []wireDownload
		sawCompleted      bool
		completedProgress float64
	)
	for _, f := range frames {
		switch f.name {
		case "snapshot":
			sawSnapshot = true
		case "download":
			var payload struct {
				Download wireDownload `json:"download"`
			}
			if err := json.Unmarshal([]byte(f.data), &payload); err != nil {
				t.Fatalf("bad download frame %q: %v", f.data, err)
			}
			dl := payload.Download
			if dl.ID != id {
				continue
			}
			switch dl.State {
			case "downloading":
				if dl.Progress > 0 && dl.Progress < 100 && dl.DownloadedBytes > 0 && dl.TotalBytes > 0 {
					intermediate = append(intermediate, dl)
				}
			case "completed":
				sawCompleted = true
				completedProgress = dl.Progress
			}
		}
	}
	if !sawSnapshot {
		t.Error("first SSE frame must be a snapshot")
	}
	if len(intermediate) == 0 {
		t.Fatalf("no intermediate downloading update with 0<progress<100 reached the stream; frames=%d", len(frames))
	}
	if !sawCompleted || completedProgress != 100 {
		t.Fatalf("completion event missing or wrong progress=%v sawCompleted=%v", completedProgress, sawCompleted)
	}
	// Progress must be monotonically meaningful: last intermediate below 100,
	// proving updates arrived during the download rather than only at the end.
	last := intermediate[len(intermediate)-1]
	if last.Progress >= 100 {
		t.Fatalf("expected a sub-100%% intermediate update, got %.1f", last.Progress)
	}
}
