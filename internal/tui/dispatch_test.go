package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

// growRunner completes instantly, recording its output file via @g like a real
// merged yt-dlp run.
type growRunner struct{ dir string }

func (f growRunner) Probe(ctx context.Context, url string) ([]manager.Entry, error) {
	return []manager.Entry{{URL: url, Title: "t"}}, nil
}

func (f growRunner) Run(ctx context.Context, url string, onLine func(string)) (string, error) {
	target := filepath.Join(f.dir, "done.bin")
	onLine(manager.PrintLine("@g|", target))
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	return "", nil
}

func TestLocalBackendHasNoDeleteCommand(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := manager.NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), growRunner{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	be := newLocalBackend(mgr)
	id, err := mgr.Add("https://v/x")
	if err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateCompleted
	}, "completed")

	// File deletion was removed from every interface: the TUI can remove a row
	// but never destroy media.
	if err := be.Do("delete", id, ""); err == nil {
		t.Fatal("delete must no longer be a backend command")
	}
	if _, ok := mgr.Get(id); !ok {
		t.Fatal("rejected command must not have removed the row")
	}
}

func waitForCond(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}

// --- D-key double-confirm flow ---

type recordingBackend struct {
	ch   chan ipc.Event
	mu   chan struct{}
	cmds [][3]string
}

func newRecordingBackend() *recordingBackend {
	return &recordingBackend{ch: make(chan ipc.Event, 8), mu: make(chan struct{}, 1)}
}

func (b *recordingBackend) Events() <-chan ipc.Event { return b.ch }
func (b *recordingBackend) Do(cmd, id, url string) error {
	b.mu <- struct{}{}
	b.cmds = append(b.cmds, [3]string{cmd, id, url})
	<-b.mu
	return nil
}

func keyD(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// TestShiftDNoLongerDeletes pins the removal of the destructive TUI flow. "D"
// used to arm a two-press confirmation that deleted files from disk; it is now
// an unbound key and must dispatch nothing at all.
func TestShiftDNoLongerDeletes(t *testing.T) {
	be := newRecordingBackend()
	m := newModel(be)
	now := time.Now()
	m.apply(ipc.Event{Event: "snapshot", Items: []ipc.Item{
		{ID: "aaa111", URL: "u1", State: ipc.StateCompleted, AddedAt: now},
	}})
	m.width, m.height = 120, 20
	m.move(0)

	for i := 0; i < 3; i++ {
		nm, cmd := m.Update(keyD('D'))
		m = nm.(model)
		if cmd != nil {
			executeCmd(t, cmd)
		}
	}
	for _, c := range be.cmds {
		if c[0] == "delete" {
			t.Fatalf("D dispatched a delete: %v", c)
		}
	}
}

// executeCmd runs a bubbletea Cmd to completion, flattening tea.Batch.
func executeCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			c() // actionMsg results are ignored by the test backend
		}
	}
}
