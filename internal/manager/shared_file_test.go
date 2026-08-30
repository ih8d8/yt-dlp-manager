package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// TestRemoveSparesSharedPartialOfSurvivingItem covers rows that reference the
// same output base — yt-dlp can reuse a name across attempts, and two queued
// entries for the same URL resolve identically. Removing one row clears its
// scratch, but a `.part` another live row is still writing must be left alone:
// deleting it would corrupt an in-flight download.
func TestRemoveSparesSharedPartialOfSurvivingItem(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := NewWithRunner(ctx, 2, filepath.Join(dir, "state.json"), sharedFileRunner{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)

	base := filepath.Join(dir, "shared video [abc].webm")
	part := base + ".part"

	idA, err := mgr.Add("https://v/a")
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, idA, ipc.StateCompleted, "A completes")

	// Both rows now reference the same base, and a partial exists for it.
	mgr.mu.Lock()
	mgr.items[idA].Files = []string{base}
	itB := &item{probed: true, Item: ipc.Item{
		ID: "bbb000", URL: "https://v/b", Title: "B",
		State:   ipc.StateFailed,
		AddedAt: time.Now().Add(-time.Minute),
		Files:   []string{base},
	}}
	mgr.items[itB.ID] = itB
	mgr.mu.Unlock()

	if err := os.WriteFile(part, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Removing B must not touch a partial A still claims.
	if err := mgr.Remove("bbb000"); err != nil {
		t.Fatalf("remove failed row: %v", err)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("surviving item's partial was deleted by removing the other row: %v", err)
	}
	if _, ok := mgr.Get("bbb000"); ok {
		t.Fatal("removed row still present")
	}

	// Once the last claimant goes, its scratch is cleared.
	if err := mgr.Remove(idA); err != nil {
		t.Fatalf("remove owner row: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		_, err := os.Stat(part)
		return os.IsNotExist(err)
	}, "partial cleared once its last owner is gone")
}

// sharedFileRunner completes instantly and records a deterministic @g path
// so tests can control which files an item claims.
type sharedFileRunner struct{ dir string }

func (f sharedFileRunner) Probe(ctx context.Context, url string) ([]Entry, error) {
	return []Entry{{URL: url, Title: "t"}}, nil
}

func (f sharedFileRunner) Run(ctx context.Context, url string, onLine func(string)) (string, error) {
	onLine(PrintLine("@g|", filepath.Join(f.dir, "ignored.bin")))
	return "", nil
}
