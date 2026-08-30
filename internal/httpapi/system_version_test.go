package httpapi

import "testing"

// Every string here was captured from the real binary; the About table renders
// whatever this returns, so a regression here puts prose back on screen.
func TestCleanVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ffmpeg version n9.0.1 Copyright (c) 2000-2026 the FFmpeg developers", "n9.0.1"},
		{"ffmpeg version 8.1.2 Copyright (c) 2000-2026 the FFmpeg developers", "8.1.2"},
		{"QuickJS version 2025-09-13", "2025-09-13"},
		{"deno 2.9.6 (stable, release, x86_64-unknown-linux-gnu)", "2.9.6"},
		{"v26.8.1", "v26.8.1"},
		{"2026.08.19", "2026.08.19"},
		{"1.2.15", "1.2.15"},
		// Unrecognised shapes fall back rather than returning nothing.
		{"some tool without a number", "some tool without a number"},
		{"", ""},
		// "version" as the final token must not read past the end.
		{"tool version", "tool version"},
	}
	for _, c := range cases {
		if got := cleanVersion(c.in); got != c.want {
			t.Errorf("cleanVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
