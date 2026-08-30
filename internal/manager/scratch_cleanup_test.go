package manager

import (
	"os"
	"path/filepath"
	"testing"
)

// Reproduces the reported leftover: a cancelled video+audio download leaves
// "<stem>.f<id>.<ext>.part" behind. yt-dlp reports only the FINAL merged name,
// so cleanup keyed on "<base>.part" alone never found them.
func TestCancelledMergeLeavesNoScratch(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "Some Movie Reaction [CQu7paOfHts].mp4")
	leftovers := []string{
		base + ".part",
		filepath.Join(dir, "Some Movie Reaction [CQu7paOfHts].f616.mp4.part"),
		filepath.Join(dir, "Some Movie Reaction [CQu7paOfHts].f616.mp4.ytdl"),
		filepath.Join(dir, "Some Movie Reaction [CQu7paOfHts].f251.webm.part"),
	}
	keep := filepath.Join(dir, "Some Movie Reaction [CQu7paOfHts].f616.mp4")
	for _, p := range append(append([]string{}, leftovers...), keep) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupPartials([]string{base}); err != nil {
		t.Fatal(err)
	}
	for _, p := range leftovers {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("scratch survived: %s", filepath.Base(p))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a finished per-format file is media and must survive: %v", err)
	}
}
