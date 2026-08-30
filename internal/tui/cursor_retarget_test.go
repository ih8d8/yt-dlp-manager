package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"yt-dlp-manager/internal/ipc"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func seedThree(m *model) {
	now := time.Now()
	m.apply(ipc.Event{Event: "snapshot", Items: []ipc.Item{
		{ID: "aaa", State: ipc.StateQueued, AddedAt: now},
		{ID: "bbb", State: ipc.StateQueued, AddedAt: now.Add(time.Millisecond)},
		{ID: "ccc", State: ipc.StateQueued, AddedAt: now.Add(2 * time.Millisecond)},
	}})
}

func TestCursorRetargetsToNextOnRemoval(t *testing.T) {
	be := &stubBackend{ch: make(chan ipc.Event, 16)}
	m := newModel(be)
	seedThree(&m)
	m.move(0)
	if m.selID != "aaa" {
		t.Fatalf("cursor = %s, want aaa", m.selID)
	}
	m.move(1)
	if m.selID != "bbb" {
		t.Fatalf("cursor = %s, want bbb", m.selID)
	}
	m.apply(ipc.Event{Event: "removed", ID: "bbb"})
	if m.selID != "ccc" {
		t.Fatalf("cursor = %s, want ccc (next item)", m.selID)
	}
	m.apply(ipc.Event{Event: "removed", ID: "ccc"})
	if m.selID != "aaa" {
		t.Fatalf("cursor = %s, want aaa (clamp to last)", m.selID)
	}
}

func TestCursorRowSpansFullWidth(t *testing.T) {
	be := &stubBackend{ch: make(chan ipc.Event, 16)}
	m := newModel(be)
	seedThree(&m)
	m.width, m.height = 120, 18
	m.move(0)
	out := m.View()
	cursorLine := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "aaa") && strings.HasPrefix(ln, "│") {
			cursorLine = ln
			break
		}
	}
	if cursorLine == "" {
		t.Fatal("cursor row not found")
	}
	stripped := stripANSI(cursorLine)
	if got := lipgloss.Width(stripped); got != 119 {
		t.Errorf("cursor row visible width = %d, want 119 (full row highlight)", got)
	}
}
