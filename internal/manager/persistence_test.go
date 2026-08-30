package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// A save that fails used to be a line on stderr and nothing else: the API had
// already answered 201, /readyz stayed green, and the dirty state waited for
// some future mutation to try again. Everything acknowledged since the failure
// then vanished at the next restart.
func TestFailedSaveIsVisibleAndRecovers(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	st, err := NewStore(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		items: make(map[string]*item),
		subs:  make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1),
		store: st,
	}

	if !m.flush() {
		t.Fatal("the first save should succeed")
	}
	if err := m.SaveError(); err != nil {
		t.Fatalf("SaveError after a good save = %v, want nil", err)
	}

	// Take the state directory away: writing the temp file now fails.
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if m.flush() {
		t.Fatal("a save into a missing directory should fail")
	}
	if m.SaveError() == nil {
		t.Fatal("a failed save must be reported by SaveError (and so by /readyz)")
	}

	// Give it back: the retry the saver loop performs must clear the error.
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !m.flush() {
		t.Fatalf("save after recovery failed: %v", m.SaveError())
	}
	if err := m.SaveError(); err != nil {
		t.Fatalf("SaveError after recovery = %v, want nil", err)
	}
}

// newSaverManager runs a manager with its background saver goroutine, which
// newTestManager deliberately leaves out.
func newSaverManager(t *testing.T, st *Store) (*Manager, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		ctx:       ctx,
		cancelCtx: cancel,
		max:       1,
		store:     st,
		runner:    newFakeRunner(),
		items:     make(map[string]*item),
		subs:      make(map[chan ipc.Event]struct{}),
		dirty:     make(chan struct{}, 1),
		saverStop: make(chan struct{}),
		saverDone: make(chan struct{}),
	}
	go m.saver()
	return m, func() {
		close(m.saverStop)
		<-m.saverDone
		cancel()
	}
}

// A failed save must not sit there until the next mutation: the saver marks
// the state dirty again so it comes back on its own.
func TestSaverRetriesAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	st, err := NewStore(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	m, cancel := newSaverManager(t, st)
	defer cancel()

	m.touch()
	waitFor(t, 2*time.Second, func() bool { return m.SaveError() != nil }, "the failing save is recorded")

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return m.SaveError() == nil },
		"the retry runs without another mutation and succeeds")
}
