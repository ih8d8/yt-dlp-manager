package manager

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"yt-dlp-manager/internal/ipc"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	added := time.Now()
	items := []ipc.Item{
		{ID: "id1", URL: "https://a", Title: "A", State: ipc.StateQueued, AddedAt: added},
		{ID: "id2", URL: "https://b", State: ipc.StateCompleted, AddedAt: added, DoneAt: &added},
	}
	order := []string{"id1"}
	if err := st.Save(items, order); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	if got.Items[0].ID != "id1" || got.Items[0].Title != "A" {
		t.Errorf("item0 = %+v", got.Items[0])
	}
	if len(got.Order) != 1 || got.Order[0] != "id1" {
		t.Errorf("order = %v", got.Order)
	}
}

func TestStoreFilePerms(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "state.json"))
	if err := st.Save(nil, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestLoadMissingReturnsErr(t *testing.T) {
	st, _ := NewStore(filepath.Join(t.TempDir(), "none.json"))
	if _, err := st.Load(); err != ErrNoState {
		t.Fatalf("err = %v, want ErrNoState", err)
	}
}

func TestNewStoreRejectsNonRegularAndBrokenStatePaths(t *testing.T) {
	dir := t.TempDir()

	directory := filepath.Join(dir, "state-dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(directory); err == nil {
		t.Fatal("directory accepted as a state file")
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("rejected state directory was changed: info=%v err=%v", info, err)
	}

	fifo := filepath.Join(dir, "state-fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	if _, err := NewStore(fifo); err == nil {
		t.Fatal("FIFO accepted as a state file")
	}
	if _, err := readFileLimited(fifo, 1024); err == nil {
		t.Fatal("FIFO accepted by the descriptor-level read guard")
	}

	broken := filepath.Join(dir, "broken-state")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), broken); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := NewStore(broken); err == nil {
		t.Fatal("dangling state symlink accepted")
	}
}

func TestQuarantineRefusesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-stay")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &Store{path: dir}
	if _, err := st.Quarantine(); err == nil {
		t.Fatal("Quarantine renamed a directory supplied as the state path")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("state directory was moved: info=%v err=%v", info, err)
	}
}

func TestLoadCorruptFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	st, _ := NewStore(p)
	if _, err := st.Load(); err == nil {
		t.Fatal("expected error for corrupt state")
	}
}

func TestLoadRejectsSemanticallyInvalidSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	data := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[{"id":"dup","url":"https://a","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"},{"id":"dup","url":"https://b","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"}],"order":["dup","dup"]}`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := NewStore(p)
	if _, err := st.Load(); err == nil {
		t.Fatal("duplicate IDs/order entries must reject the snapshot")
	}
}

func TestReadFileLimitedRejectsOversizedInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(p, []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileLimited(p, 8); err == nil {
		t.Fatal("oversized state input must be rejected during the bounded read")
	}
}

func TestParseNum(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", -1},
		{"NA", -1},
		{"None", -1},
		{"0", 0},
		{"42", 42},
		{"1048576.5", 1048576},
		{"abc", -1},
		{" 7 ", 7},
	}
	for _, c := range cases {
		if got := ParseNum(c.in); got != c.want {
			t.Errorf("ParseNum(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSaveTrimsOldestHistoryRatherThanWedging: rows this build admits are
// bounded, but a state file written by a build with looser limits is not.
// Persistence degrades by dropping the oldest completed rows rather than
// refusing to write, so it can never wedge.
func TestSaveTrimsOldestHistoryRatherThanWedging(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Rows with every remote-controlled field at its bound.
	filler := strings.Repeat("w", 4096-16)
	var items []ipc.Item
	var order []string
	base := time.Now().Add(-100 * time.Hour)
	// Enough rows to cross the 64 MiB limit at roughly 4 KB each. This models
	// a state file written by a build with looser limits, which is the only
	// way a snapshot gets here oversized now that admission bounds rows.
	rows := (maxStateBytes / 4000) + 500
	for i := range rows {
		id := fmt.Sprintf("%08x", i)
		items = append(items, ipc.Item{
			ID: id, URL: "https://a.test/" + filler,
			Title: strings.Repeat("t", MaxTitleBytes),
			State: ipc.StateCompleted, AddedAt: base.Add(time.Duration(i) * time.Minute),
		})
		order = append(order, id)
	}
	// One live row, which must survive whatever else is dropped.
	items = append(items, ipc.Item{
		ID: "ffffffff", URL: "https://a.test/live", State: ipc.StateQueued,
		AddedAt: base.Add(-time.Hour),
	})
	order = append(order, "ffffffff")

	trimmed, err := st.SaveTrimmed(items, order)
	if err != nil {
		t.Fatalf("Save refused an oversized snapshot instead of trimming it: %v", err)
	}
	if len(trimmed) == 0 {
		t.Fatal("nothing was trimmed, so this test no longer exercises trimming")
	}

	// What was written must load back — the whole point of trimming.
	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("the trimmed snapshot does not load: %v", err)
	}
	live := false
	for _, it := range loaded.Items {
		if it.ID == "ffffffff" {
			live = true
		}
	}
	if !live {
		t.Error("trimming dropped a queued row; only finished history may be trimmed")
	}
	// Oldest-first: the survivors must be the newest of the trimmed category.
	for _, it := range loaded.Items {
		if it.State == ipc.StateCompleted && it.AddedAt.Before(base.Add(time.Duration(len(trimmed))*time.Minute)) {
			t.Errorf("kept %s (added %s) while trimming newer rows", it.ID, it.AddedAt)
			break
		}
	}
}

// TestRemoteFieldsAreBounded proves the terms above are enforced rather than
// hoped for: an extractor answering with a megabyte of title, a huge thumbnail
// URL and a flood of output paths must not be able to grow one row without
// limit.
func TestRemoteFieldsAreBounded(t *testing.T) {
	huge := strings.Repeat("A", 1<<20)
	fake := newFakeRunner()
	url := "https://example.test/huge"
	fake.probeFn = func(string) ([]Entry, error) {
		return []Entry{{URL: url, Title: huge, Thumbnail: "https://x.test/" + huge}}, nil
	}
	lines := []string{PrintLine("@t|", huge)}
	for i := range MaxFilesPerItem + 20 {
		lines = append(lines, PrintLine("@g|", "/downloads/f"+strconv.Itoa(i)+".mp4"))
	}
	lines = append(lines, PrintLine("@g|", "/downloads/"+strings.Repeat("p", MaxFilePathBytes)+".mp4"))
	fake.setLines(url, lines...)

	mgr, _ := newTestManager(t, 1, fake)
	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")
	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateCompleted
	}, "the download to finish")

	it, _ := mgr.Get(id)
	if got := encodedLen(it.Title); got > MaxTitleBytes {
		t.Errorf("title encodes to %d bytes, over the %d bound", got, MaxTitleBytes)
	}
	if got := encodedLen(it.ThumbURL); got > MaxThumbURLBytes {
		t.Errorf("thumbnail URL encodes to %d bytes, over the %d bound", got, MaxThumbURLBytes)
	}
	if len(it.Files) > MaxFilesPerItem {
		t.Errorf("recorded %d files, over the %d bound", len(it.Files), MaxFilesPerItem)
	}
	for _, f := range it.Files {
		if got := encodedLen(f); got > MaxFilePathBytes {
			t.Errorf("recorded a path encoding to %d bytes, over the %d bound", got, MaxFilePathBytes)
		}
	}
}

// TestQueueAlwaysFitsTheStateLimit is the invariant the per-item budget exists
// for: a queue the manager will accept must always be persistable, even when
// every row is live and every row is as large as admission allows. Trimming
// cannot help there — an all-live queue has no expendable history — so this is
// what makes the limit real rather than hoped for.
func TestQueueAlwaysFitsTheStateLimit(t *testing.T) {
	// Arithmetic first: the two ceilings must agree with the state limit
	// whatever a row happens to contain.
	if budget := int64(MaxQueueItems)*MaxItemBytes + snapshotOverhead; budget > maxStateBytes {
		t.Fatalf("%d rows of up to %d bytes need %d, over the %d byte state limit",
			MaxQueueItems, MaxItemBytes, budget, maxStateBytes)
	}

	// Then a measurement of the largest row the bounds can actually produce.
	// The options must be a combination the API would ACCEPT — mutually
	// exclusive fields and duplicate languages are rejected or normalized
	// away, so a row built from them would not be a real worst case.
	pad := strings.Repeat("w", 1<<16)
	langs := make([]string, 0, ipc.MaxSubLangs)
	for i := range ipc.MaxSubLangs {
		langs = append(langs, fmt.Sprintf("%031d", i)) // distinct, 32 bytes each
	}
	opts := ipc.Options{
		FormatID:       strings.Repeat("a", 64),
		AudioFormatID:  strings.Repeat("b", 64),
		MergeContainer: "webm",
		Subtitles:      "on",
		SubLangs:       langs,
	}

	// Grow the free-form arguments until the WHOLE options key sits on its
	// ceiling — that key, not the argument string, is what admission measures.
	// They have to be arguments the allowlist actually accepts: a run of
	// padding characters is a bare positional and would be refused, so this
	// row would never exist.
	// Grow the free-form arguments until the WHOLE options key sits on its
	// ceiling — that key, not the argument string, is what admission measures.
	// They have to be arguments the allowlist actually accepts: a run of
	// padding characters is a bare positional and would be refused, so a row
	// built from one could never exist.
	base := opts
	base.ExtraArgs = "--add-header X:v"
	if encodedLen(base.Key()) <= MaxOptionsBytes {
		opts = base
		for {
			trial := opts
			trial.ExtraArgs += "v" // extend the header's value, one byte at a time
			if encodedLen(trial.Key()) > MaxOptionsBytes {
				break
			}
			opts = trial
		}
	}

	// Only now is the row's option set complete, so this is where it is
	// checked: a "worst case" the API would reject proves nothing.
	if err := opts.Normalize().Validate(); err != nil {
		t.Fatalf("the test's options are not a request the API would accept: %v", err)
	}
	if _, err := ipc.ExtraArgs(opts.ExtraArgs); err != nil {
		t.Fatalf("the test's extra arguments would be refused: %v", err)
	}

	started := time.Date(2099, 12, 31, 23, 59, 59, 999999999, time.UTC)
	row := ipc.Item{
		ID:             "ffffffff",
		URL:            truncateEncoded("https://a.test/"+pad, MaxURLBytes),
		Title:          truncateEncoded(pad, MaxTitleBytes),
		ThumbURL:       truncateEncoded(pad, MaxThumbURLBytes),
		Error:          truncateEncoded(pad, MaxErrorBytes),
		State:          ipc.StateDownloading,
		Forced:         true,
		UserPaused:     true,
		Progress:       99.99999999999999,
		Got:            math.MaxInt64,
		Total:          math.MaxInt64,
		Speed:          math.MaxInt64,
		ETA:            math.MaxInt64,
		AddedAt:        started,
		StartedAt:      &started,
		DoneAt:         &started,
		Options:        opts,
		OptionsInvalid: true,
	}
	for len(row.Files) < MaxFilesPerItem {
		row.Files = append(row.Files, truncateEncoded(pad, MaxFilePathBytes))
	}

	// Every bounded field must actually sit on its ceiling, or this is not the
	// worst case it claims to be.
	for _, f := range []struct {
		name string
		got  int
		want int
	}{
		{"url", encodedLen(row.URL), MaxURLBytes},
		{"title", encodedLen(row.Title), MaxTitleBytes},
		{"thumbnail", encodedLen(row.ThumbURL), MaxThumbURLBytes},
		{"error", encodedLen(row.Error), MaxErrorBytes},
	} {
		if f.got != f.want {
			t.Errorf("%s is %d bytes, not at its %d ceiling: this is not the worst case",
				f.name, f.got, f.want)
		}
	}
	// Maximal by construction rather than by arithmetic: the options are at
	// their ceiling exactly when one more byte of argument would exceed it.
	if got := encodedLen(row.Options.Key()); got > MaxOptionsBytes {
		t.Errorf("options key is %d bytes, over the %d ceiling", got, MaxOptionsBytes)
	} else {
		oneMore := row.Options
		oneMore.ExtraArgs += "v"
		if encodedLen(oneMore.Key()) <= MaxOptionsBytes {
			t.Errorf("options key is %d bytes and still has room: not the worst case", got)
		}
		t.Logf("options key: %d of %d bytes", got, MaxOptionsBytes)
	}
	for i, p := range row.Files {
		if got := encodedLen(p); got != MaxFilePathBytes {
			t.Errorf("path %d is %d bytes, not at its %d ceiling", i, got, MaxFilePathBytes)
		}
	}

	maxRow := itemBytes(row)
	t.Logf("largest valid row the bounds allow: %d of %d bytes", maxRow, MaxItemBytes)
	if maxRow > MaxItemBytes {
		t.Fatalf("a row with every field at its ceiling is %d bytes, over the %d budget: "+
			"the field bounds do not fit MaxItemBytes", maxRow, MaxItemBytes)
	}

	items := make([]ipc.Item, 0, MaxQueueItems)
	order := make([]string, 0, MaxQueueItems)
	for i := range MaxQueueItems {
		r := row
		r.ID = fmt.Sprintf("%08x", i)
		items = append(items, r)
		order = append(order, r.ID)
	}
	size, err := snapshotSize(items, order)
	if err != nil {
		t.Fatal(err)
	}
	if size > maxStateBytes {
		t.Errorf("a full live queue serializes to %d bytes, over the %d byte limit: "+
			"lower MaxQueueItems (%d) or MaxItemBytes (%d), or raise maxStateBytes",
			size, maxStateBytes, MaxQueueItems, MaxItemBytes)
	}
}

// TestAdmissionRefusesAnOversizedRow: the budget is enforced where the row is
// created, so nothing that cannot be persisted ever enters the queue.
func TestAdmissionRefusesAnOversizedRow(t *testing.T) {
	mgr, _ := newTestManager(t, 0, newFakeRunner())
	// Legal by every other rule — http(s), no control characters — but past
	// what one row may spend on a URL.
	huge := "https://a.test/" + strings.Repeat("u", MaxURLBytes+100)
	if _, err := mgr.Add(huge); !errors.Is(err, ErrItemTooLarge) {
		t.Errorf("Add error = %v, want ErrItemTooLarge", err)
	}
	if _, err := mgr.Add("https://a.test/ordinary"); err != nil {
		t.Errorf("an ordinary URL was refused: %v", err)
	}
}

// TestTrimmingNeverRemovesARevivedRow pins the removal-side guard: even if a
// stale trimming decision names an id that is now running, only a row that is
// still completed under the manager lock may be removed.
func TestTrimmingNeverRemovesARevivedRow(t *testing.T) {
	fake := newFakeRunner()
	mgr, _ := newTestManager(t, 1, fake)
	url := "https://example.test/revived"

	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")

	// The save decided to trim this id while it was still terminal; by the
	// time the removal runs it is live again.
	mgr.dropTrimmedHistory([]string{id})

	it, ok := mgr.Get(id)
	if !ok {
		t.Fatal("a running download's row was removed by history trimming")
	}
	if it.State != ipc.StateDownloading {
		t.Errorf("state = %s, want the running row untouched", it.State)
	}
	fake.releaseURL(t, url)
}

// TestTrimmingSparesFailedRows: a failed row is pending work — the API offers
// Retry and Start now for it — so evicting it as "history" would silently
// discard a download the user still means to make. Only completed rows are
// selectable; this pins that, since the selector is one line away from being
// widened by accident.
func TestTrimmingSparesFailedRows(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	items := []ipc.Item{
		{ID: "aaaaaaaa", URL: "https://a.test/failed", State: ipc.StateFailed, AddedAt: base},
		{ID: "bbbbbbbb", URL: "https://a.test/done", State: ipc.StateCompleted, AddedAt: base},
	}
	// Force trimming by making the snapshot enormous. Only the completed row
	// is eligible, so the failed one must survive whatever is dropped.
	items[1].Title = strings.Repeat("t", maxStateBytes)
	_, _, trimmed, err := fitToStateLimit(items, []string{"aaaaaaaa", "bbbbbbbb"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range trimmed {
		if id == "aaaaaaaa" {
			t.Error("a failed row was trimmed; only completed rows are expendable")
		}
	}
}

// TestFieldBoundsFitTheItemBudget is what makes MaxItemBytes hold for a row's
// whole life rather than only at admission. Every field is bounded in encoded
// bytes at its own ingestion point, so if the bounds sum to less than the
// budget, no later mutation — a title arriving from stdout, a recorded path,
// an error, timestamps — can push an admitted row over.
func TestFieldBoundsFitTheItemBudget(t *testing.T) {
	sum := MaxURLBytes + MaxOptionsBytes + MaxTitleBytes + MaxThumbURLBytes +
		MaxFilesPerItem*MaxFilePathBytes + MaxErrorBytes + itemFixedBytes
	if sum > MaxItemBytes {
		t.Errorf("the field bounds sum to %d bytes, over the %d per-item budget: "+
			"an admitted row could grow past what the snapshot can hold", sum, MaxItemBytes)
	}
}

// TestAnAdmittedRowStaysWithinBudgetAsItGrows walks a row through the whole
// lifecycle — probe metadata, a title from stdout, recorded outputs, a failure
// message, timestamps — with every remote field hostile, and checks the budget
// after each step.
func TestAnAdmittedRowStaysWithinBudgetAsItGrows(t *testing.T) {
	hostile := strings.Repeat("<", 1<<16)
	fake := newFakeRunner()
	url := "https://example.test/" + strings.Repeat("a", 900)
	fake.probeFn = func(string) ([]Entry, error) {
		return []Entry{{URL: url, Title: hostile, Thumbnail: "https://x.test/" + hostile}}, nil
	}
	lines := []string{PrintLine("@t|", hostile)}
	for i := range MaxFilesPerItem + 4 {
		lines = append(lines, PrintLine("@g|", "/downloads/"+strings.Repeat("p", 400)+strconv.Itoa(i)+".mp4"))
	}
	fake.setLines(url, lines...)
	fake.mu.Lock()
	fake.errs[url] = fmt.Errorf("%s", strings.Repeat("<", 4096))
	fake.mu.Unlock()

	mgr, _ := newTestManager(t, 1, fake)
	id, err := mgr.AddWithOptions(url, ipc.Options{
		FormatID: strings.Repeat("a", 64), AudioFormatID: strings.Repeat("b", 64),
		ExtraArgs: "--limit-rate 2M --user-agent " + strings.Repeat("u", 400),
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		it, ok := mgr.Get(id)
		if !ok {
			return
		}
		if got := itemBytes(it); got > MaxItemBytes {
			t.Fatalf("%s: row is %d bytes, over the %d budget", stage, got, MaxItemBytes)
		}
	}
	check("admitted")
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")
	check("downloading")
	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && (it.State == ipc.StateFailed || it.State == ipc.StateCompleted)
	}, "the download to finish")
	check("finished")
}

// TestZeroExitWithoutOutputIsNotCompleted: options that skip a download exit 0
// and print nothing, so treating every zero exit as success put entries in the
// library with no media behind them.
func TestZeroExitWithoutOutputIsNotCompleted(t *testing.T) {
	fake := newFakeRunner()
	url := "https://example.test/skipped"
	fake.setLines(url) // exits cleanly, names no file
	mgr, _ := newTestManager(t, 1, fake)

	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")
	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State != ipc.StateDownloading
	}, "the download to finish")

	it, _ := mgr.Get(id)
	if it.State != ipc.StateFailed {
		t.Errorf("state = %s, want failed: yt-dlp produced no output file", it.State)
	}
	if it.Progress == 100 {
		t.Error("progress reported as complete for a download that never happened")
	}
}

// TestLongOutputPathDoesNotFailASuccessfulDownload: a valid filename can be
// longer than a row is allowed to store. Recording the path and knowing that
// output happened are different questions, and conflating them turned a real
// download into a false failure.
func TestLongOutputPathDoesNotFailASuccessfulDownload(t *testing.T) {
	fake := newFakeRunner()
	url := "https://example.test/longpath"
	long := "/downloads/" + strings.Repeat("n", MaxFilePathBytes+200) + ".mp4"
	fake.setLines(url, PrintLine("@g|", long))
	mgr, _ := newTestManager(t, 1, fake)

	id, err := mgr.Add(url)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State == ipc.StateDownloading
	}, "the download to start")
	fake.releaseURL(t, url)
	waitFor(t, 3*time.Second, func() bool {
		it, ok := mgr.Get(id)
		return ok && it.State != ipc.StateDownloading
	}, "the download to finish")

	it, _ := mgr.Get(id)
	if it.State != ipc.StateCompleted {
		t.Errorf("state = %s, want completed: yt-dlp did produce a file", it.State)
	}
	if len(it.Files) != 0 {
		t.Errorf("a path over the storage budget was recorded anyway: %q", it.Files)
	}
}

// TestUnstorableRestoredRowsArePreservedAndReported: an upgrade that cannot
// store a saved entry must not delete it silently — the URL is written out so
// it can be re-added.
func TestUnstorableRestoredRowsArePreservedAndReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// A URL that is legal for the API (http(s), no control characters) but
	// larger than one row is allowed to spend on it.
	huge := "https://a.test/" + strings.Repeat("u", MaxURLBytes+500)
	raw := fmt.Sprintf(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
      {"id":"aaaaaaaa","url":%q,"state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"},
      {"id":"bbbbbbbb","url":"https://a.test/ok","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"}],
      "order":["aaaaaaaa","bbbbbbbb"]}`, huge)
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
	if len(snap.Items) != 1 || snap.Items[0].ID != "bbbbbbbb" {
		t.Fatalf("kept = %+v, want only the storable row", snap.Items)
	}
	if len(snap.Dropped) != 1 || snap.Dropped[0].Item.ID != "aaaaaaaa" {
		t.Fatalf("dropped = %+v, want the oversized row reported", snap.Dropped)
	}
	// Its order entry goes with it, or the snapshot would not validate.
	for _, id := range snap.Order {
		if id == "aaaaaaaa" {
			t.Error("a dropped row is still referenced by the queue order")
		}
	}
	// And it is written somewhere the operator can recover it from.
	where, err := st.SaveDropped(snap.Dropped)
	if err != nil {
		t.Fatalf("dropped rows were not written out: %v", err)
	}
	data, err := os.ReadFile(where)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Dropped[0].Reason != DropTooLarge {
		t.Errorf("reason = %q, want %q", snap.Dropped[0].Reason, DropTooLarge)
	}
	if !strings.Contains(string(data), huge) {
		t.Error("the dropped row's URL was not preserved")
	}
}

// TestRestoreEnforcesTheQueueCeiling: a snapshot from a build with a larger
// ceiling must not restore more rows than this one admits. Every restored row
// is live after a restart, so the excess would grow as titles and paths arrive
// until the snapshot no longer fits — the wedge the ceiling prevents.
func TestRestoreEnforcesTheQueueCeiling(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[`)
	var order []string
	total := MaxQueueItems + 250
	for i := range total {
		id := fmt.Sprintf("%08x", i)
		if i > 0 {
			b.WriteString(",")
		}
		// A mix: older completed history, then queued work.
		state := "completed"
		if i >= total/2 {
			state = "queued"
		}
		fmt.Fprintf(&b, `{"id":%q,"url":"https://a.test/%d","state":%q,"progress":0,`+
			`"added_at":"2026-01-01T00:%02d:00Z"}`, id, i, state, i%60)
		order = append(order, id)
	}
	b.WriteString(`],"order":[`)
	for i, id := range order {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q", id)
	}
	b.WriteString(`]}`)

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
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
	if len(snap.Items) > MaxQueueItems {
		t.Errorf("restored %d rows, over the %d ceiling", len(snap.Items), MaxQueueItems)
	}
	if len(snap.Dropped) != total-MaxQueueItems {
		t.Errorf("reported %d dropped rows, want %d", len(snap.Dropped), total-MaxQueueItems)
	}
	// History goes first: the queued work must survive.
	queued := 0
	for _, it := range snap.Items {
		if it.State == ipc.StateQueued {
			queued++
		}
	}
	if queued != total/2 {
		t.Errorf("kept %d queued rows, want all %d — history should be dropped first",
			queued, total/2)
	}
	if len(snap.Order) != len(snap.Items) {
		t.Errorf("order has %d ids for %d rows", len(snap.Order), len(snap.Items))
	}
}

// TestRestoreEnforcesOneEntryPerURL: strict identity has to survive a restart.
// Resuming a paused row deliberately skips the duplicate check, so a snapshot
// carrying two rows for one URL would put two downloads on one file with
// nothing downstream to catch it.
func TestRestoreEnforcesOneEntryPerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
      {"id":"aaaaaaaa","url":"https://a.test/v","state":"paused","progress":0,"added_at":"2026-01-01T00:00:00Z"},
      {"id":"bbbbbbbb","url":"https://a.test/v","state":"queued","progress":0,"added_at":"2026-01-01T00:01:00Z"},
      {"id":"cccccccc","url":"https://a.test/other","state":"queued","progress":0,"added_at":"2026-01-01T00:02:00Z"}],
      "order":["aaaaaaaa","bbbbbbbb","cccccccc"]}`
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
	seen := map[string]int{}
	for _, it := range snap.Items {
		seen[it.URL]++
	}
	if seen["https://a.test/v"] != 1 {
		t.Errorf("restored %d rows for one URL, want 1", seen["https://a.test/v"])
	}
	if seen["https://a.test/other"] != 1 {
		t.Error("an unrelated row was dropped")
	}
	if len(snap.Dropped) != 1 || snap.Dropped[0].Item.ID != "bbbbbbbb" {
		t.Errorf("dropped = %+v, want the later duplicate reported", snap.Dropped)
	}
}

// TestSiblingPathStaysWithinAFilenameLimit: the sidecar and the backup are
// built by appending to the state file's name, and a state file can already
// sit near the limit. Appending past it would fail exactly where the data
// matters most, so the base is shortened instead.
func TestSiblingPathStaysWithinAFilenameLimit(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("n", maxNameLen-4)+".json")
	got, err := siblingPath(long, ".dropped-1234567890.json")
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(got); len(base) > maxNameLen {
		t.Errorf("produced a %d byte filename, over the %d limit", len(base), maxNameLen)
	}
	if !strings.HasSuffix(got, ".dropped-1234567890.json") {
		t.Errorf("suffix lost: %q", got)
	}
	// And a real write to that name must succeed.
	dir := t.TempDir()
	target, err := siblingPath(filepath.Join(dir, strings.Repeat("n", maxNameLen-4)+".json"),
		".before-prune-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileDurable(target, []byte("{}")); err != nil {
		t.Errorf("writing the sibling failed: %v", err)
	}
}

// TestRestorePreservesTheOriginalStateWhenRowsAreDropped: the reduced snapshot
// overwrites the file the rows came from, so the original is renamed aside
// first. Without that, a failure to write the per-entry record would leave the
// dropped downloads with no durable copy at all.
func TestRestorePreservesTheOriginalStateWhenRowsAreDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	huge := "https://a.test/" + strings.Repeat("u", MaxURLBytes+500)
	raw := fmt.Sprintf(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
      {"id":"aaaaaaaa","url":%q,"state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"},
      {"id":"bbbbbbbb","url":"https://a.test/ok","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"}],
      "order":["aaaaaaaa","bbbbbbbb"]}`, huge)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := NewWithRunner(ctx, 0, path, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// The oversized row is gone from the queue…
	if len(mgr.List()) != 1 {
		t.Fatalf("restored %d rows, want 1", len(mgr.List()))
	}
	// …but a copy of the original state survives, containing its URL.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var preserved, sidecar bool
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(e.Name(), ".before-prune-") && strings.Contains(string(data), huge) {
			preserved = true
		}
		if strings.Contains(e.Name(), ".dropped-") && strings.Contains(string(data), huge) {
			sidecar = true
		}
	}
	if !preserved {
		t.Error("the original state file was not preserved before being reduced")
	}
	if !sidecar {
		t.Error("the dropped rows were not written to a per-entry record")
	}
}

// TestPersistenceStopsRatherThanOverwriteTheLastCopy: if neither the backup
// nor the per-entry record can be written, saving must stop. Overwriting the
// state file would destroy the only remaining record of those downloads.
func TestPersistenceStopsRatherThanOverwriteTheLastCopy(t *testing.T) {
	mgr, _ := newTestManager(t, 0, newFakeRunner())
	mgr.disablePersistence(errors.New("nowhere to put the dropped rows"))

	if mgr.flush() {
		t.Error("flush wrote state after persistence was disabled")
	}
	if mgr.SaveError() == nil {
		t.Error("no error reported, so readiness would still claim state is saved")
	}
}

// TestPruningLeavesAUsablePrimaryStateFile is the regression test for a
// migration that renamed the original state aside and then left nothing in its
// place: a crash before the next mutation would have started the following run
// from no state at all, while readiness still claimed everything was fine.
func TestPruningLeavesAUsablePrimaryStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	huge := "https://a.test/" + strings.Repeat("u", MaxURLBytes+500)
	raw := fmt.Sprintf(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[
      {"id":"aaaaaaaa","url":%q,"state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"},
      {"id":"bbbbbbbb","url":"https://a.test/keep","state":"paused","progress":0,"added_at":"2026-01-01T00:00:00Z"}],
      "order":["aaaaaaaa","bbbbbbbb"]}`, huge)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := NewWithRunner(ctx, 0, path, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	// The primary must exist NOW — before any later mutation, and before any
	// graceful shutdown, because neither is guaranteed to happen.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no primary state file after pruning: %v", err)
	}
	if err := mgr.SaveError(); err != nil {
		t.Errorf("save error after pruning: %v", err)
	}
	// Kill it the way a crash would, then start again from what is on disk.
	cancel()
	mgr.Close()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	restarted, err := NewWithRunner(ctx2, 0, path, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	rows := restarted.List()
	if len(rows) != 1 || rows[0].URL != "https://a.test/keep" {
		t.Errorf("after restart the queue is %+v, want the surviving row", rows)
	}
}

// TestLockPathIsUniqueAndFits covers the three ways a sibling name can go
// wrong when the state file's own name is long: it can overflow the component
// limit, it can collide with the state file itself (a name ending in ".lock"
// truncating back to itself), and two different state files can end up sharing
// one lock. Any of those hands two processes the same state.
func TestLockPathIsUniqueAndFits(t *testing.T) {
	dir := t.TempDir()

	t.Run("fits and is creatable", func(t *testing.T) {
		st, err := NewStore(filepath.Join(dir, strings.Repeat("s", maxNameLen)))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := st.LockPath()
		if err != nil {
			t.Fatal(err)
		}
		if base := filepath.Base(lock); len(base) > maxNameLen {
			t.Fatalf("lock name is %d bytes, over the %d limit", len(base), maxNameLen)
		}
		f, err := os.Create(lock)
		if err != nil {
			t.Fatalf("cannot create the lock beside a maximal state name: %v", err)
		}
		f.Close()
	})

	t.Run("never aliases the state file", func(t *testing.T) {
		// A maximal name that already ends in ".lock": appending would
		// overflow, and truncating naively lands back on the state file.
		name := strings.Repeat("s", maxNameLen-len(".lock")) + ".lock"
		st, err := NewStore(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := st.LockPath()
		if err != nil {
			t.Fatal(err)
		}
		if lock == st.Path() {
			t.Fatal("the lock path IS the state file: a save would replace the locked inode")
		}
	})

	t.Run("distinct state files get distinct locks", func(t *testing.T) {
		shared := strings.Repeat("p", maxNameLen-8)
		a, err := NewStore(filepath.Join(dir, shared+"-aaaaaaa"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewStore(filepath.Join(dir, shared+"-bbbbbbb"))
		if err != nil {
			t.Fatal(err)
		}
		la, err := a.LockPath()
		if err != nil {
			t.Fatal(err)
		}
		lb, err := b.LockPath()
		if err != nil {
			t.Fatal(err)
		}
		if la == lb {
			t.Fatalf("two state files share one lock (%s): single-owner protection is gone", la)
		}
	})

	t.Run("quarantine works for a maximal name", func(t *testing.T) {
		st, err := NewStore(filepath.Join(dir, strings.Repeat("q", maxNameLen)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st.Path(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Quarantine(); err != nil {
			t.Errorf("quarantine failed for a maximal state name: %v", err)
		}
	})
}

// TestUnquarantinableStateStopsPersistence: if unreadable state cannot even be
// moved aside, that file is still the only copy of whatever it holds. Starting
// with an empty queue is acceptable; overwriting it is not, so saving stops
// and readiness reports it rather than showing green over a silent loss.
func TestUnquarantinableStateStopsPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A read-only directory makes the rename fail while the file stays
	// readable — the transient-failure shape this guards against.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := NewWithRunner(ctx, 0, path, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	if mgr.SaveError() == nil {
		t.Error("no save error, so readiness would report ready over unreadable state")
	}
	if mgr.flush() {
		t.Error("state was written despite the unreadable file being unrecoverable")
	}
	// The original must still be there, untouched.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the unreadable state file is gone: %v", err)
	}
	if string(data) != "this is not json" {
		t.Errorf("the unreadable state file was overwritten: %q", data)
	}
}

// TestLockPathCannotBeAnotherStateFile is the sharper form of lock
// uniqueness. Naming a lock "<state>.lock" is not enough: a state file
// literally named "foo.lock" IS the lock for a state file named "foo", so the
// second instance's ordinary save would atomically replace the inode the first
// one holds and quietly end its ownership. Locks therefore live in their own
// directory, which state files may not.
func TestLockPathCannotBeAnotherStateFile(t *testing.T) {
	dir := t.TempDir()

	a, err := NewStore(filepath.Join(dir, "foo"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStore(filepath.Join(dir, "foo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lockA, err := a.LockPath()
	if err != nil {
		t.Fatal(err)
	}
	lockB, err := b.LockPath()
	if err != nil {
		t.Fatal(err)
	}
	if lockA == b.Path() {
		t.Errorf("A's lock (%s) is B's state file: B's next save would revoke A's ownership", lockA)
	}
	if lockB == a.Path() {
		t.Errorf("B's lock (%s) is A's state file", lockB)
	}
	if lockA == lockB {
		t.Error("two state files share one lock")
	}

	// And the crossing cannot be made from the other side either.
	if _, err := NewStore(filepath.Join(dir, lockDirName, "state.json")); err == nil {
		t.Error("a state file was allowed inside the lock directory")
	}
}

// TestSiblingNamesStayShortForSmallerFilesystems: eCryptfs and some network
// mounts cap a filename well below 255. A threshold at 255 would happily emit
// a 145-byte sibling that such a filesystem rejects, so shortening starts far
// earlier and every sibling stays comfortably short.
func TestSiblingNamesStayShortForSmallerFilesystems(t *testing.T) {
	const restrictiveLimit = 143 // eCryptfs
	dir := t.TempDir()
	for _, base := range []string{
		"state.json",
		strings.Repeat("s", 140),
		strings.Repeat("s", 250),
	} {
		st, err := NewStore(filepath.Join(dir, base))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := st.LockPath()
		if err != nil {
			t.Fatal(err)
		}
		names := []string{filepath.Base(lock)}
		for _, suffix := range []string{".corrupt-1788888888888888888", ".before-prune-1788888888888888888",
			".dropped-1788888888888888888.json"} {
			p, err := siblingPath(st.Path(), suffix)
			if err != nil {
				t.Fatalf("siblingPath(%d byte base, %q): %v", len(base), suffix, err)
			}
			names = append(names, filepath.Base(p))
		}
		for _, n := range names {
			if len(n) > restrictiveLimit {
				t.Errorf("base %d bytes produced a %d byte sibling %q, over a %d byte limit",
					len(base), len(n), n, restrictiveLimit)
			}
		}
	}
}
