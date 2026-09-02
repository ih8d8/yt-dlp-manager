package manager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// progressLine builds one "@p|" line in the shape the real progress template
// produces, so these tests exercise the same parser production does.
func progressLine(got, total int64, status, formatID string) string {
	f := func(n int64) string {
		if n < 0 {
			return "NA"
		}
		return strconv.FormatInt(n, 10)
	}
	pct := "NA"
	if total > 0 {
		pct = " 50.0%"
	}
	id, err := json.Marshal(formatID)
	if err != nil {
		panic(err)
	}
	return "@p|" + f(got) + "|" + f(total) + "|NA|NA|" + pct + "|" + status +
		"|" + string(id)
}

// TestOnLineAccumulatesAcrossStreams is the regression test for a completed
// 134 MB download reporting "7.2 MB / 7.2 MB": yt-dlp downloads the video and
// the audio as separate transfers, each restarting its counters at zero, and
// the last one used to overwrite the item instead of adding to it.
func TestOnLineAccumulatesAcrossStreams(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}

	// Video stream: 127 MB.
	mgr.onLine(it, progressLine(1024, 133_000_000, "downloading", "137"))
	if it.Got != 1024 || it.Total != 133_000_000 {
		t.Fatalf("first stream: got=%d total=%d", it.Got, it.Total)
	}
	mgr.onLine(it, progressLine(133_000_000, 133_000_000, "finished", "137"))

	// Audio stream: 7.2 MB, counters back to zero.
	mgr.onLine(it, progressLine(1024, 7_200_000, "downloading", "140"))
	wantTotal := int64(133_000_000 + 7_200_000)
	if it.Total != wantTotal {
		t.Errorf("total = %d, want %d (video + audio)", it.Total, wantTotal)
	}
	if it.Got != 133_000_000+1024 {
		t.Errorf("got = %d, want the video's bytes plus the audio so far", it.Got)
	}
	mgr.onLine(it, progressLine(7_200_000, 7_200_000, "finished", "140"))
	if it.Got != wantTotal {
		t.Errorf("after both streams got = %d, want %d", it.Got, wantTotal)
	}
	if it.Progress < 99 {
		t.Errorf("progress = %f, want ~100 once both streams finished", it.Progress)
	}
}

// TestOnLineDetectsNewStreamWithoutStatusFields covers a yt-dlp too old to
// report status or format id: a byte count that goes backwards is the only
// remaining signal that a second transfer started.
func TestOnLineDetectsNewStreamWithoutStatusFields(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@p|5000000|5000000|NA|NA")
	mgr.onLine(it, "@p|1024|2000000|NA|NA")
	if it.Got != 5_000_000+1024 {
		t.Errorf("got = %d, want the first stream to have been kept", it.Got)
	}
	if it.Total != 7_000_000 {
		t.Errorf("total = %d, want 7000000", it.Total)
	}
}

// TestOnLineReportsUnknownTotalRatherThanAFalseOne: once a stream has
// finished, a following stream that never reports its size must leave the
// total unknown. Reporting the bytes so far as the total would render a
// running download as complete, which is the bug this all started from.
func TestOnLineReportsUnknownTotalRatherThanAFalseOne(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, progressLine(4_000_000, 4_000_000, "finished", "137"))
	mgr.onLine(it, progressLine(1024, -1, "downloading", "140"))
	if it.Got != 4_000_000+1024 {
		t.Errorf("got = %d", it.Got)
	}
	if it.Total != 0 {
		t.Errorf("total = %d, want 0 (unknown)", it.Total)
	}
}

// TestCompletedItemTakesItsSizeFromDisk: the streams yt-dlp reports do not add
// up to the merged file, so a finished item's size comes from the filesystem.
func TestCompletedItemTakesItsSizeFromDisk(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video.mkv")
	if err := os.WriteFile(out, make([]byte, 8_489_551), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	url := "https://example.test/v"
	fake.setLines(url,
		PrintLine("@g|", out),
		progressLine(4_639_733, 4_639_733, "finished", "137"),
		progressLine(3_931_453, 3_931_453, "finished", "251"),
	)
	mgr, _ := newTestManager(t, 2, fake)

	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "download to start")
	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateCompleted
	}, "download to complete")

	it, _ := mgr.Get(id)
	if it.Total != 8_489_551 || it.Got != 8_489_551 {
		t.Errorf("got=%d total=%d, want both to be the file's actual 8489551 bytes "+
			"(the streams sum to 8571186, which the merged file is not)", it.Got, it.Total)
	}
}

// TestSameURLIsOneLiveEntryWhateverTheOptions: identity is the URL alone. Two
// rows for one URL would resolve to one file on disk, because the output
// template rarely varies by format — the second would overwrite or skip the
// first, leaving a row claiming a quality it does not hold.
func TestSameURLIsOneLiveEntryWhateverTheOptions(t *testing.T) {
	mgr, _ := newTestManager(t, 0, newFakeRunner())
	url := "https://example.test/v"

	if _, err := mgr.AddWithOptions(url, ipc.Options{Preset: "1080p"}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	for _, opts := range []ipc.Options{
		{Preset: "720p"},
		{FormatID: "137"},
		{ExtraArgs: "--limit-rate 1M"},
		{},
	} {
		_, err := mgr.AddWithOptions(url, opts)
		if !errors.Is(err, ErrDuplicate) {
			t.Errorf("AddWithOptions(%+v) error = %v, want a duplicate refusal", opts, err)
		}
	}
}

// TestAddRejectsUnusableOptions keeps the validation at the door: nothing that
// could not be turned into arguments is allowed to reach the queue, where it
// would fail on every retry instead of once at the point of asking.
func TestAddRejectsUnusableOptions(t *testing.T) {
	mgr, _ := newTestManager(t, 0, newFakeRunner())

	bad := ipc.Options{FormatID: "137; rm -rf /"}
	if _, err := mgr.AddWithOptions("https://example.test/v", bad); err == nil {
		t.Fatal("a format id with shell metacharacters was accepted")
	}
}

// TestProgressNeverGoesBackwardsAcrossStreams is the regression test for a bar
// that fell from 100% to 54% when the video finished and the audio began.
// yt-dlp announces the combined size of every stream before the first byte, so
// the percentage measures the whole item and only ever climbs.
func TestProgressNeverGoesBackwardsAcrossStreams(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}

	// video 4,639,733 + audio 3,931,453, announced up front.
	mgr.onLine(it, "@n|8571186")
	if it.Total != 8_571_186 {
		t.Fatalf("announced total not adopted: %d", it.Total)
	}

	var last float64
	steps := []string{
		progressLine(1024, 4_639_733, "downloading", "394"),
		progressLine(2_000_000, 4_639_733, "downloading", "394"),
		progressLine(4_639_733, 4_639_733, "finished", "394"),
		progressLine(1024, 3_931_453, "downloading", "249"),
		progressLine(2_000_000, 3_931_453, "downloading", "249"),
		progressLine(3_931_453, 3_931_453, "finished", "249"),
	}
	for i, line := range steps {
		mgr.onLine(it, line)
		if it.Progress < last {
			t.Errorf("step %d: progress fell from %.1f to %.1f", i, last, it.Progress)
		}
		if it.Total < it.Got {
			t.Errorf("step %d: got %d exceeds total %d", i, it.Got, it.Total)
		}
		last = it.Progress
	}
	// The video alone must never read as the whole download.
	if it.Progress < 99 {
		t.Errorf("final progress = %.1f, want ~100", it.Progress)
	}
}

// TestAnnouncedTotalNeverUnderstatesMeasuredBytes: the announced figure is an
// estimate, so measurement wins wherever the two disagree. A download must
// never report more bytes than its own total.
func TestAnnouncedTotalNeverUnderstatesMeasuredBytes(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@n|1000")
	mgr.onLine(it, progressLine(5_000_000, 9_000_000, "downloading", "137"))
	if it.Total < it.Got {
		t.Errorf("got=%d total=%d: the estimate overrode measurement", it.Got, it.Total)
	}
	if it.Progress > 100 {
		t.Errorf("progress = %f", it.Progress)
	}
}

// TestProgressStillWorksWithoutAnAnnouncedTotal: extractors that report no
// size (HLS, live) must keep the accumulating behaviour rather than losing
// progress entirely.
func TestProgressStillWorksWithoutAnAnnouncedTotal(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@n|NA")
	mgr.onLine(it, progressLine(4_000_000, 4_000_000, "finished", "137"))
	mgr.onLine(it, progressLine(1024, 2_000_000, "downloading", "140"))
	if it.Got != 4_000_000+1024 || it.Total != 6_000_000 {
		t.Errorf("got=%d total=%d, want accumulation to still apply", it.Got, it.Total)
	}
}

// TestDiskSizeCountsOneFileOnce is the regression test for a finished 134 MB
// download whose details read 269 MB. An item records its output twice — the
// name predicted before downloading (relative to the working directory) and
// the final path after moving (absolute) — and summing both counted the same
// bytes twice.
func TestDiskSizeCountsOneFileOnce(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "video.mkv")
	if err := os.WriteFile(abs, make([]byte, 141_000_000/8), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(141_000_000 / 8)

	// The exact pair yt-dlp emits: a relative prediction and an absolute
	// final path. Resolving the relative one needs the working directory to
	// be the download directory, which is how the manager's own container
	// runs it.
	restore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })

	if got := diskSize([]string{"video.mkv", abs}); got != size {
		t.Errorf("diskSize = %d, want %d (the same file counted once)", got, size)
	}
	// A hard link is a second name for the same bytes, and must not add.
	link := filepath.Join(dir, "same.mkv")
	if err := os.Link(abs, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if got := diskSize([]string{abs, link}); got != size {
		t.Errorf("diskSize with a hard link = %d, want %d", got, size)
	}
	// Two genuinely different files still add up.
	other := filepath.Join(dir, "subs.srt")
	if err := os.WriteFile(other, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diskSize([]string{abs, other}); got != size+5 {
		t.Errorf("diskSize of two distinct files = %d, want %d", got, size+5)
	}
}

// TestAnnouncedTotalIsTakenOnlyOnce: an extractor that announced a per-format
// size after the first announcement would otherwise shrink the item's total
// mid-download, reintroducing the backwards progress this fixes.
func TestAnnouncedTotalIsTakenOnlyOnce(t *testing.T) {
	mgr := &Manager{}
	it := &item{Item: ipc.Item{ID: "x", State: ipc.StateDownloading}}
	mgr.onLine(it, "@n|8571186")
	mgr.onLine(it, "@n|3931453")
	if it.expectTotal != 8_571_186 {
		t.Errorf("expectTotal = %d, want the first announcement to stand", it.expectTotal)
	}
	mgr.onLine(it, progressLine(1024, 4_639_733, "downloading", "394"))
	if it.Total != 8_571_186 {
		t.Errorf("total = %d, want 8571186", it.Total)
	}
}

// TestSameURLDownloadsNeverRunConcurrently guards the retry window: a failed
// or deleted row does not reserve its URL, so a new entry can claim it and
// then the old one can be retried. Two runs against one output file is exactly
// what must not happen, whatever route the rows arrived by.
func TestSameURLDownloadsNeverRunConcurrently(t *testing.T) {
	fake := newFakeRunner()
	mgr, _ := newTestManager(t, 4, fake)
	url := "https://example.test/same"

	// Two rows holding one URL, as a retry race would produce them.
	mgr.mu.Lock()
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		it := &item{probed: true, Item: ipc.Item{
			ID: id, URL: url, State: ipc.StateQueued, AddedAt: time.Now(),
		}}
		mgr.items[id] = it
		mgr.order = append(mgr.order, id)
	}
	mgr.mu.Unlock()
	mgr.schedule()

	waitFor(t, 3*time.Second, func() bool { return fake.startedCount() >= 1 }, "the first to start")
	time.Sleep(150 * time.Millisecond)

	running := 0
	for _, it := range mgr.List() {
		if it.State == ipc.StateDownloading {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("%d downloads running for one URL, want 1", running)
	}

	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool { return fake.startedCount() == 2 }, "the second to start")
}

// TestRemovalDuringCompletionDoesNotResurrectTheRow: completion releases the
// lock to stat the output, and a removal landing in that window used to be
// followed by an update carrying a newer sequence number — which slipped past
// the removal tombstone and put a completed ghost row back in every UI.
func TestRemovalDuringCompletionDoesNotResurrectTheRow(t *testing.T) {
	fake := newFakeRunner()
	mgr, _ := newTestManager(t, 1, fake)
	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()

	url := "https://example.test/racy"
	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")

	// Remove and let the run finish; whichever order they interleave in, the
	// row must not come back.
	go func() { _ = mgr.Remove(id) }()
	fake.releaseURL(t, url)

	deadline := time.After(2 * time.Second)
	removed := false
	for {
		select {
		case ev := <-events:
			if ev.Event == "removed" && ev.ID == id {
				removed = true
			}
			if removed && ev.Event == "update" && ev.Item != nil && ev.Item.ID == id {
				t.Fatalf("row %s was published again after its removal (state %s)",
					id, ev.Item.State)
			}
		case <-deadline:
			if _, stillThere := mgr.Get(id); stillThere {
				t.Fatal("the removed row is still present")
			}
			return
		}
	}
}

// TestUnreadableOptionsCannotBeRetried: clearing invalid options and marking
// the row failed is not enough on its own — failed rows advertise Retry, and
// retrying would run the URL with today's defaults instead of the quality,
// container or arguments the row was created with. The row has to be
// un-runnable until someone re-adds it deliberately.
func TestUnreadableOptionsCannotBeRetried(t *testing.T) {
	// Every state a row can be re-run from. "failed" and "deleted" matter as
	// much as "queued": both advertise Retry and Start now, so leaving them
	// unmarked would let exactly the substitution this guards against happen.
	for _, state := range []ipc.State{
		ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused,
		ipc.StateFailed, ipc.StateDeleted,
	} {
		t.Run(string(state), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			raw := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
              {"id":"aaaaaaaa","url":"https://example.test/v","state":"` + string(state) + `",
               "progress":0,"added_at":"2026-01-01T00:00:00Z",
               "options":{"extra_args":"--exec id"}}],"order":["aaaaaaaa"]}`
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := st.Load()
			if err != nil {
				t.Fatalf("the snapshot was rejected outright: %v", err)
			}
			row := snap.Items[0]
			if row.State != ipc.StateFailed || !row.OptionsInvalid {
				t.Fatalf("row = %+v, want failed and marked unreadable", row)
			}
			if row.Options.ExtraArgs != "" {
				t.Errorf("the unusable arguments survived: %q", row.Options.ExtraArgs)
			}

			mgr, _ := newTestManager(t, 1, newFakeRunner())
			mgr.mu.Lock()
			mgr.items[row.ID] = &item{Item: row}
			mgr.order = append(mgr.order, row.ID)
			mgr.mu.Unlock()

			if err := mgr.Resume(row.ID); !errors.Is(err, ErrOptionsInvalid) {
				t.Errorf("Resume error = %v, want ErrOptionsInvalid", err)
			}
			if err := mgr.StartNow(row.ID); !errors.Is(err, ErrOptionsInvalid) {
				t.Errorf("StartNow error = %v, want ErrOptionsInvalid", err)
			}
		})
	}
}

// TestCompletedRowKeepsItsHistoryWhenOptionsAreUnreadable: a finished download
// is not re-run, so it keeps its row and simply loses the overrides rather
// than being turned into a failure the user has to clean up.
func TestCompletedRowKeepsItsHistoryWhenOptionsAreUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
      {"id":"bbbbbbbb","url":"https://example.test/v","state":"completed","progress":100,
       "added_at":"2026-01-01T00:00:00Z","options":{"extra_args":"--exec id"}}],
      "order":["bbbbbbbb"]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	row := snap.Items[0]
	if row.State != ipc.StateCompleted || row.OptionsInvalid {
		t.Errorf("row = %+v, want the completed row left alone", row)
	}
}
