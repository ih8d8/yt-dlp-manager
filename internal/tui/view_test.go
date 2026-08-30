package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type stubBackend struct{ ch chan ipc.Event }

func (b *stubBackend) Events() <-chan ipc.Event     { return b.ch }
func (b *stubBackend) Do(cmd, id, url string) error { return nil }

func TestViewLayoutColumns(t *testing.T) {
	be := &stubBackend{ch: make(chan ipc.Event, 16)}
	m := newModel(be)
	now := time.Now()
	m.apply(ipc.Event{Event: "snapshot", Items: []ipc.Item{
		{ID: "9f3c21", URL: "https://x/1", Title: "A Video", State: ipc.StateDownloading, Progress: 54.3, Speed: 4404019, ETA: 31, AddedAt: now},
		{ID: "71ab09", URL: "https://x/2", Title: "B Video", State: ipc.StateQueued, AddedAt: now.Add(time.Second)},
	}})
	m.width, m.height = 120, 18
	m.move(0)
	out := m.View()
	for _, want := range []string{"ID", "STATE", "PROGRESS", "%", "SPEED", "ETA", "NAME", "DOWNLOADING", "QUEUED"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q", want)
		}
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 17 {
		t.Errorf("frame lines = %d, want 17 (one terminal row reserved as bottom margin)", len(lines))
	}
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasPrefix(lines[16], "╰") {
		t.Error("frame must open/close with corner pieces")
	}
	for i, ln := range lines {
		if got := lipgloss.Width(ln); got != 119 {
			t.Errorf("line %d width = %d, want 119 (perfect bezel alignment)", i, got)
		}
	}
	if !strings.Contains(out, "PROGRESS") || !strings.Contains(out, "┈") {
		t.Error("header band with rule missing")
	}
}

func TestEdgeExactWidth(t *testing.T) {
	cases := []struct {
		w     int
		left  string
		right string
	}{
		{120, "⬇ yt-dlp-tui", "1 active 2 queued 0 done"},
		{120, "chips blob", ""},
		{80, "⬇ yt-dlp-tui", ""},
		{76, "x", "some long status message that overflows the available space easily"},
		{200, "l", strings.Repeat("r", 150)},
	}
	for _, c := range cases {
		got := edge("╭", "╮", c.left, c.right, c.w)
		if wd := lipgloss.Width(got); wd != c.w {
			t.Errorf("edge(w=%d,left=%q,right=%q) width=%d", c.w, c.left, c.right, wd)
		}
	}
}

func TestLocalBackendSeedsSnapshot(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	mgr, err := manager.New(ctx, 2, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Add("https://example.com/v"); err != nil {
		t.Fatal(err)
	}
	be := newLocalBackend(mgr)
	select {
	case ev := <-be.Events():
		if ev.Event != "snapshot" {
			t.Fatalf("first event = %s, want snapshot", ev.Event)
		}
		if len(ev.Items) != 1 || ev.Items[0].URL != "https://example.com/v" {
			t.Fatalf("snapshot items = %+v", ev.Items)
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot event")
	}
}
