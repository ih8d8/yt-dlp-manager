package cli

import (
	"os"
	"path/filepath"
	"testing"

	"yt-dlp-manager/internal/inbox"
)

func TestIsClientCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"delete", false}, // file deletion was removed from every interface
		{"add", true},
		{"list", true},
		{"ls", true},
		{"rm", true},
		{"startnow", true},
		{"clear-finished", true},
		{"clear-all", true},
		{"clear", true},
		{"pause", true},
		{"resume", true},
		{"cancel", true},
		{"remove", true},
		{"start-now", true},
		{"daemon", false},
		{"tui", false},
		{"server", false},
		{"bogus", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsClientCommand(c.cmd); got != c.want {
			t.Errorf("IsClientCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	if code := Run(nil); code != 2 {
		t.Fatalf("Run(nil) = %d, want 2 (usage)", code)
	}
}

func TestOfflineAppendValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.jsonl")

	if code := appendOffline("https://example.com/video", path); code != 0 {
		t.Fatalf("appendOffline valid = %d, want 0", code)
	}
	urls, err := inbox.Drain(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "https://example.com/video" {
		t.Fatalf("drained = %v", urls)
	}

	if code := appendOffline("", path); code != 1 {
		t.Error("empty url must fail")
	}
	if code := appendOffline("-flag-like", path); code != 1 {
		t.Error("flag-looking url must fail")
	}
}

func TestResolveURLArgReadsStdinDash(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("https://example.com/from-stdin\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })

	raw, code := resolveURLArg([]string{"-"})
	if code != 0 {
		t.Fatalf("stdin resolve exit = %d, want 0", code)
	}
	if raw != "https://example.com/from-stdin" {
		t.Fatalf("raw = %q", raw)
	}
}
