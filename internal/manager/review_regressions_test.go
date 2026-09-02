package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// Terminal rows are history. A restart used to blank the failure reason and
// the completion time, which made every past failure look reasonless and
// dropped Library sorting back to added_at.
func TestRestorePreservesTerminalHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-2 * time.Hour)
	done := time.Now().Add(-time.Hour)
	items := []ipc.Item{
		{ID: "aaaaaaaa", URL: "https://example.com/ok", State: ipc.StateCompleted,
			AddedAt: started, StartedAt: &started, DoneAt: &done, Progress: 100},
		{ID: "bbbbbbbb", URL: "https://example.com/bad", State: ipc.StateFailed,
			AddedAt: started, StartedAt: &started, DoneAt: &done, Error: "HTTP Error 403: Forbidden"},
	}
	if err := st.Save(items, []string{"aaaaaaaa", "bbbbbbbb"}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{store: st, items: map[string]*item{}}
	m.restore()

	got, ok := m.items["bbbbbbbb"]
	if !ok {
		t.Fatal("failed row vanished on restore")
	}
	if got.Error == "" {
		t.Error("the failure reason was erased by restart")
	}
	if got.DoneAt == nil {
		t.Error("the failure time was erased by restart")
	}
	if c := m.items["aaaaaaaa"]; c == nil || c.DoneAt == nil {
		t.Error("the completion time was erased by restart")
	}
}

// A queued row carries no outcome, so restore must still clear its transients.
func TestRestoreClearsNonTerminalMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Hour)
	items := []ipc.Item{{
		ID: "cccccccc", URL: "https://example.com/q", Title: "x", State: ipc.StateQueued,
		AddedAt: when, StartedAt: &when, DoneAt: &when, Error: "stale",
	}}
	if err := st.Save(items, []string{"cccccccc"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: st, items: map[string]*item{}}
	m.restore()
	got := m.items["cccccccc"]
	if got.State != ipc.StatePaused || got.UserPaused {
		t.Fatalf("queued row was not restored as a recovery pause: %+v", got.Item)
	}
	if got.DoneAt != nil || got.StartedAt != nil || got.Error != "" {
		t.Fatalf("queued row kept stale outcome: %+v", got.Item)
	}
}

func TestClampMaxBoundsConcurrency(t *testing.T) {
	if got := ClampMax(100000); got != 100 {
		t.Errorf("ClampMax(100000) = %d, want 100", got)
	}
	if got := ClampMax(0); got != 1 {
		t.Errorf("ClampMax(0) = %d, want 1", got)
	}
	if got := ClampMax(-5); got != 1 {
		t.Errorf("ClampMax(-5) = %d, want 1", got)
	}
	if got := ClampMax(7); got != 7 {
		t.Errorf("ClampMax(7) = %d, want 7", got)
	}
}

// Load refuses oversized state, so Save must too — otherwise the next start
// quarantines the file and the whole history disappears.
// TestSaveAlwaysProducesALoadableFile: Save must never write something the
// next start would quarantine as corrupt. It used to guarantee this by
// refusing; it now guarantees it by trimming the oldest finished history, so
// the property under test is the same and the outcome is better.
func TestSaveAlwaysProducesALoadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 4 << 20
	huge := strings.Repeat("t", chunk)
	rows := maxStateBytes/chunk + 2
	var items []ipc.Item
	var order []string
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("%08x", i)
		items = append(items, ipc.Item{
			ID: id, URL: "https://example.com/x", Title: huge,
			State: ipc.StateCompleted, AddedAt: time.Now(),
		})
		order = append(order, id)
	}
	if _, err := st.SaveTrimmed(items, order); err != nil {
		t.Fatalf("Save failed instead of trimming: %v", err)
	}
	if _, err := st.Load(); err != nil {
		t.Fatalf("Save wrote a file Load rejects: %v", err)
	}
}

// "Start now" re-runs a row exactly like Resume does, so it must clear the
// previous outcome too — otherwise a force-started retry reports an old error
// and a completed_at while it is downloading.
func TestStartNowClearsPreviousOutcome(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &blockingRunner{release: make(chan struct{})}
	m, err := NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), r)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	id, err := m.Add("https://example.com/f")
	if err != nil {
		t.Fatal(err)
	}
	// Force the row into a failed state carrying a previous outcome.
	when := time.Now().Add(-time.Hour)
	m.mu.Lock()
	it := m.items[id]
	it.State = ipc.StateFailed
	it.Error = "HTTP Error 403"
	it.StartedAt, it.DoneAt = &when, &when
	m.mu.Unlock()

	if err := m.StartNow(id); err != nil {
		t.Fatal(err)
	}

	got, ok := m.Get(id)
	if !ok {
		t.Fatal("row vanished")
	}
	if got.Error != "" {
		t.Errorf("start-now kept the previous error %q", got.Error)
	}
	if got.DoneAt != nil {
		t.Errorf("start-now kept a completion time: %v", got.DoneAt)
	}
	// StartNow schedules the row, so run() may legitimately have stamped a
	// fresh StartedAt before this assertion — that is the retry starting, not
	// stale state. What must not survive is the OLD timestamp, so compare
	// against it rather than requiring nil.
	if got.StartedAt != nil && !got.StartedAt.After(when) {
		t.Errorf("start-now kept the stale start time %v (old was %v)", got.StartedAt, when)
	}
}
