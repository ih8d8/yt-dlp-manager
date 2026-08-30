package manager

import (
	"testing"

	"yt-dlp-manager/internal/ipc"
)

func TestOnLineParsesRealProgressLine(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@p|1024|712445280|886108.3754899938|828")
	if it.Item.Got != 1024 || it.Item.Total != 712445280 || it.Item.Speed != 886108 || it.Item.ETA != 828 {
		t.Fatalf("got=%d total=%d speed=%d eta=%d", it.Item.Got, it.Item.Total, it.Item.Speed, it.Item.ETA)
	}
	if it.Item.Progress <= 0 || it.Item.Progress > 100 {
		t.Fatalf("progress=%f", it.Item.Progress)
	}
	// NA fields keep previous values rather than zeroing them.
	mgr.onLine(it, "@p|NA|NA|NA|NA")
	if it.Item.Got != 1024 || it.Item.Total != 712445280 {
		t.Fatalf("NA clobbered values: got=%d total=%d", it.Item.Got, it.Item.Total)
	}
}

func TestOnLineUsesYtDlpPercentBeforeTotalIsKnown(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@p|4096|NA|1024|30|  0.7%")
	if it.Total != 0 || it.Progress != 0.7 {
		t.Fatalf("total=%d progress=%f, want unknown/0.7", it.Total, it.Progress)
	}
}

func TestProbeTreatsMissingThumbnailMarkersAsEmpty(t *testing.T) {
	entries := parseProbeOutput(
		`@e|{"url":"https://example.test/v","title":"A title","thumbnail":"NA"}`+"\n",
		"https://example.test/v")
	if len(entries) != 1 || entries[0].Thumbnail != "" {
		t.Fatalf("entries = %+v, want empty thumbnail", entries)
	}
	// A yt-dlp that simply omits the key must behave the same way.
	omitted := parseProbeOutput(
		`@e|{"url":"https://example.test/v","title":"A title"}`+"\n",
		"https://example.test/v")
	if len(omitted) != 1 || omitted[0].Thumbnail != "" {
		t.Fatalf("entries = %+v, want empty thumbnail", omitted)
	}
}

// TestOnLineIgnoresUnencodedFilePaths documents that recorded paths must be
// JSON: they decide what a later purge deletes, so a line that did not come
// from our own "%(field)j" template is discarded rather than trusted.
func TestOnLineIgnoresUnencodedFilePaths(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@g|/home/user/.ssh/id_ed25519")
	if len(it.Files) != 0 {
		t.Fatalf("bare path recorded: %v", it.Files)
	}
	mgr.onLine(it, PrintLine("@g|", "/downloads/video.mp4"))
	if len(it.Files) != 1 || it.Files[0] != "/downloads/video.mp4" {
		t.Fatalf("encoded path not recorded: %v", it.Files)
	}
}
