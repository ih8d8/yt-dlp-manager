package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// An interrupted video+audio download must survive a restart. yt-dlp reports
// only the FINAL merged name through --print, but the bytes land in per-format
// scratch ("<stem>.f137.mp4.part"), so a restore that looked only for
// "<final>" and "<final>.part" saw nothing on disk and rewrote the row as
// deleted — making it vanish from Queue AND Library while still counting
// towards Clear finished.
func TestInterruptedMergeDownloadSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dl := filepath.Join(dir, "downloads")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}

	// yt-dlp reports only the FINAL merged name through --print before_dl.
	final := filepath.Join(dl, "Some Video.mp4")

	// ...but a video+audio download (the YouTube default) actually writes
	// per-format scratch files, NOT "<final>.part".
	for _, p := range []string{
		filepath.Join(dl, "Some Video.f137.mp4.part"),
		filepath.Join(dl, "Some Video.f251.webm.part"),
	} {
		if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	statePath := filepath.Join(dir, "state.json")
	snap := snapshotFile{
		Version: stateVersion,
		SavedAt: time.Now(),
		Items: []ipc.Item{{
			ID:       "aabbccdd",
			URL:      "https://www.youtube.com/watch?v=xxxxxxxxxxx",
			Title:    "Some Video",
			State:    ipc.StateDownloading, // interrupted at ~10%
			Progress: 10,
			Got:      4096,
			Total:    40960,
			Files:    []string{final},
			AddedAt:  time.Now().Add(-time.Minute),
		}},
		Order: []string{"aabbccdd"},
	}
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewWithRunner(context.Background(), 1, statePath, stubRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	items := mgr.List()
	if len(items) != 1 {
		t.Fatalf("expected the interrupted download to be restored, got %d items", len(items))
	}
	got := items[0]
	t.Logf("restored state = %q  progress = %.1f%%  files = %v", got.State, got.Progress, got.Files)

	if got.State == ipc.StateDeleted {
		t.Errorf("interrupted download came back as %q: it would vanish from Queue "+
			"and Library while still counting towards Clear finished", got.State)
	}
	if got.State != ipc.StatePaused {
		t.Errorf("want %q so the user can resume it, got %q", ipc.StatePaused, got.State)
	}
}

type stubRunner struct{}

func (stubRunner) Probe(ctx context.Context, job Job) ([]Entry, error) {
	url := job.URL
	return []Entry{{URL: url}}, nil
}
func (stubRunner) Run(ctx context.Context, job Job, onLine func(string)) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
