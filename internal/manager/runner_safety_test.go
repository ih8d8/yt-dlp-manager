package manager

import (
	"bytes"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yt-dlp-manager/internal/ipc"
)

// --- scratch cleanup: glob-metacharacter safety ---

func TestCleanupFilesHandlesGlobMetachars(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "video [1080p].mp4")
	// Scratch that must go. The per-format intermediates matter: yt-dlp reports
	// only the FINAL merged name, so a cancelled video+audio download leaves
	// "<stem>.f<id>.<ext>.part" behind and nothing else knows to remove it.
	own := []string{
		base + ".part",
		base + ".ytdl",
		base + ".part-Frag1",
		base + ".part-Frag2",
		filepath.Join(dir, "video [1080p].f137.mp4.part"),
		filepath.Join(dir, "video [1080p].f140.m4a.part"),
		filepath.Join(dir, "video [1080p].f137.mp4.ytdl"),
	}
	for _, p := range own {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Siblings whose names the old filepath.Glob(base+".part-*") pattern would
	// match (the [1080p] char class matches '1', '0', '8', 'p') or break on
	// with ErrBadPattern.
	neighbors := []string{
		base, // the finished output itself is never removed
		filepath.Join(dir, "video 1.mp4"),
		filepath.Join(dir, "video 1.mp4.part-Frag9"),
		filepath.Join(dir, "video 8.mp4.part-Frag7"),
		filepath.Join(dir, "video [1080.mp4"), // unmatched bracket = bad pattern
		filepath.Join(dir, "video ?x.mp4.part-1"),
		// Looks like a per-format intermediate but the segment after ".f" is
		// not a format id, so it is the user's file, not ours.
		filepath.Join(dir, "video [1080p].final.mp4.part"),
		// A completed per-format file (no scratch suffix) is media too.
		filepath.Join(dir, "video [1080p].f137.mp4"),
	}
	for _, p := range neighbors {
		if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cleanupPartials([]string{base}) //nolint:errcheck

	for _, p := range own {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must be deleted", p)
		}
	}
	for _, p := range neighbors {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("neighbor %s must survive: %v", p, err)
		}
	}
}

// --- bounded line scanning ---

func TestScanLinesSkipsOversizedLineKeepsFraming(t *testing.T) {
	input := strings.Repeat("a", maxLineLen+5) + "\n" + "@p|1|2|3|4\n" + "tail-no-newline"
	var got []string
	var wg sync.WaitGroup
	wg.Add(1)
	scanLines(strings.NewReader(input), func(l string) { got = append(got, l) }, &wg)
	wg.Wait()
	if len(got) != 2 || got[0] != "@p|1|2|3|4" || got[1] != "tail-no-newline" {
		t.Fatalf("got = %q", got)
	}
}

func TestCollectStderrSkipsOversizedLine(t *testing.T) {
	ringLen := 3
	input := strings.Repeat("e", maxLineLen*2) + "\n" + strings.Repeat("short ", 1) + "err\n"
	ring := make([]string, 0, ringLen)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	collectStderr(strings.NewReader(input), &ring, &mu, &wg)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(ring) != 1 || !strings.HasPrefix(ring[0], "short") {
		t.Fatalf("ring = %q", ring)
	}
}

func TestCappedBufferDiscardsOverflowButDrains(t *testing.T) {
	b := &cappedBuffer{max: 10}
	n, err := b.Write(bytes.Repeat([]byte("x"), 100))
	if err != nil || n != 100 {
		t.Fatalf("Write = %d, %v; want 100, nil", n, err)
	}
	if b.buf.Len() != 10 {
		t.Fatalf("retained %d bytes, want 10", b.buf.Len())
	}
	if !b.truncated {
		t.Fatal("overflow must be recorded so callers never parse partial output")
	}
}

// --- number parsing / truncation robustness ---

func TestParseNumSaturatesInsteadOfUndefined(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1e300", math.MaxInt64},
		{"-1e300", math.MinInt64},
		{"NaN", -1},
		{"Inf", math.MaxInt64},
	}
	for _, c := range cases {
		if got := ParseNum(c.in); got != c.want {
			t.Errorf("ParseNum(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTruncateStaysOnRuneBoundary(t *testing.T) {
	s := "héllo" // é is two bytes
	got := truncate(s, 2)
	if got != "h" {
		t.Errorf("truncate(%q, 2) = %q, want %q", s, got, "h")
	}
	if got := truncate(s, 3); got != "hé" {
		t.Errorf("truncate(%q, 3) = %q, want %q", s, got, "hé")
	}
}

func TestSanitizeStripsTerminalEscapes(t *testing.T) {
	in := "\x1b]0;pwned\x07\x1b[2Jhello\x1b[K world\x07"
	if got := sanitize(in); got != "hello world" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitize("plain title"); got != "plain title" {
		t.Errorf("sanitize mangled plain text: %q", got)
	}
	if got := sanitize("tab\tkept"); got != "tabkept" {
		t.Errorf("control chars must be stripped, got %q", got)
	}
	if got := sanitize("safe\u009b31m\u202espoof"); got != "safe31mspoof" {
		t.Errorf("C1/bidi controls must be stripped, got %q", got)
	}
}

// --- probe pipe drain with capped buffers ---

func TestProbeOutputCapped(t *testing.T) {
	b := cappedBuffer{max: 16}
	io.Copy(&b, strings.NewReader(strings.Repeat("y", 1000))) //nolint:errcheck
	if b.buf.Len() != 16 {
		t.Fatalf("capped to %d bytes, want 16", b.buf.Len())
	}
}

// Regression: the progress template used to ask only for
// total_bytes_estimate, which yt-dlp's generic downloader leaves NA —
// progress bars stayed at 0% for plain HTTP downloads. The template now
// falls back from total_bytes to total_bytes_estimate; this pins the @p
// parsing for both variants.
func TestProgressLineTotalBytesFallback(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	m.mu.Lock()
	it := &item{Item: ipc.Item{ID: "tt", URL: "https://v/x", State: ipc.StateDownloading}}
	m.items["tt"] = it
	m.mu.Unlock()

	// total_bytes variant (generic downloader).
	m.onLine(it, "@p|1024|25165824|5304299.7|4")
	if it.Got != 1024 || it.Total != 25165824 {
		t.Fatalf("got=%d total=%d, want 1024/25165824", it.Got, it.Total)
	}
	if it.Progress < 0.004 || it.Progress > 0.0041 {
		t.Errorf("progress = %f, want ~0.004", it.Progress)
	}

	// total_bytes_estimate-only variant (streaming formats).
	it2 := &item{Item: ipc.Item{ID: "tt2", URL: "https://v/y", State: ipc.StateDownloading}}
	m.onLine(it2, "@p|500|100000|NA|NA")
	if it2.Got != 500 || it2.Total != 100000 {
		t.Fatalf("got=%d total=%d, want 500/100000", it2.Got, it2.Total)
	}
}
