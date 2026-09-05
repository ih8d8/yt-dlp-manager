package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// Terminal rows are history. A restart used to blank the failure reason and
// the completion time, which made every past failure look reasonless and
// dropped Library sorting back to added_at.
func TestRestorePreservesTerminalHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-2 * time.Hour)
	done := time.Now().Add(-time.Hour)
	items := []ipc.Item{
		{ID: "aaaaaaaa", URL: "https://example.com/ok", State: ipc.StateCompleted,
			AddedAt: started, StartedAt: &started, DoneAt: &done, Progress: 100},
		{ID: "bbbbbbbb", URL: "https://example.com/bad", State: ipc.StateFailed,
			AddedAt: started, StartedAt: &started, DoneAt: &done, Error: "HTTP Error 403: Forbidden"},
	}
	if err := st.Save(items, []string{"aaaaaaaa", "bbbbbbbb"}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{store: st, items: map[string]*item{}}
	m.restore()

	got, ok := m.items["bbbbbbbb"]
	if !ok {
		t.Fatal("failed row vanished on restore")
	}
	if got.Error == "" {
		t.Error("the failure reason was erased by restart")
	}
	if got.DoneAt == nil {
		t.Error("the failure time was erased by restart")
	}
	if c := m.items["aaaaaaaa"]; c == nil || c.DoneAt == nil {
		t.Error("the completion time was erased by restart")
	}
}

// A queued row carries no outcome, so restore must still clear its transients.
func TestRestoreClearsNonTerminalMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Hour)
	items := []ipc.Item{{
		ID: "cccccccc", URL: "https://example.com/q", Title: "x", State: ipc.StateQueued,
		AddedAt: when, StartedAt: &when, DoneAt: &when, Error: "stale",
	}}
	if err := st.Save(items, []string{"cccccccc"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: st, items: map[string]*item{}}
	m.restore()
	got := m.items["cccccccc"]
	if got.State != ipc.StatePaused || got.UserPaused {
		t.Fatalf("queued row was not restored as a recovery pause: %+v", got.Item)
	}
	if got.DoneAt != nil || got.StartedAt != nil || got.Error != "" {
		t.Fatalf("queued row kept stale outcome: %+v", got.Item)
	}
}

func TestClampMaxBoundsConcurrency(t *testing.T) {
	if got := ClampMax(100000); got != 100 {
		t.Errorf("ClampMax(100000) = %d, want 100", got)
	}
	if got := ClampMax(0); got != 1 {
		t.Errorf("ClampMax(0) = %d, want 1", got)
	}
	if got := ClampMax(-5); got != 1 {
		t.Errorf("ClampMax(-5) = %d, want 1", got)
	}
	if got := ClampMax(7); got != 7 {
		t.Errorf("ClampMax(7) = %d, want 7", got)
	}
}

// Load refuses oversized state, so Save must too — otherwise the next start
// quarantines the file and the whole history disappears.
// TestSaveAlwaysProducesALoadableFile: Save must never write something the
// next start would quarantine as corrupt. It used to guarantee this by
// refusing; it now guarantees it by trimming the oldest finished history, so
// the property under test is the same and the outcome is better.
func TestSaveAlwaysProducesALoadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 4 << 20
	huge := strings.Repeat("t", chunk)
	rows := maxStateBytes/chunk + 2
	var items []ipc.Item
	var order []string
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("%08x", i)
		items = append(items, ipc.Item{
			ID: id, URL: "https://example.com/x", Title: huge,
			State: ipc.StateCompleted, AddedAt: time.Now(),
		})
		order = append(order, id)
	}
	if _, err := st.SaveTrimmed(items, order); err != nil {
		t.Fatalf("Save failed instead of trimming: %v", err)
	}
	if _, err := st.Load(); err != nil {
		t.Fatalf("Save wrote a file Load rejects: %v", err)
	}
}

// "Start now" re-runs a row exactly like Resume does, so it must clear the
// previous outcome too — otherwise a force-started retry reports an old error
// and a completed_at while it is downloading.
func TestStartNowClearsPreviousOutcome(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &blockingRunner{release: make(chan struct{})}
	m, err := NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), r)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	id, err := m.Add("https://example.com/f")
	if err != nil {
		t.Fatal(err)
	}
	// Force the row into a failed state carrying a previous outcome.
	when := time.Now().Add(-time.Hour)
	m.mu.Lock()
	it := m.items[id]
	it.State = ipc.StateFailed
	it.Error = "HTTP Error 403"
	it.StartedAt, it.DoneAt = &when, &when
	m.mu.Unlock()

	if err := m.StartNow(id); err != nil {
		t.Fatal(err)
	}

	got, ok := m.Get(id)
	if !ok {
		t.Fatal("row vanished")
	}
	if got.Error != "" {
		t.Errorf("start-now kept the previous error %q", got.Error)
	}
	if got.DoneAt != nil {
		t.Errorf("start-now kept a completion time: %v", got.DoneAt)
	}
	// StartNow schedules the row, so run() may legitimately have stamped a
	// fresh StartedAt before this assertion — that is the retry starting, not
	// stale state. What must not survive is the OLD timestamp, so compare
	// against it rather than requiring nil.
	if got.StartedAt != nil && !got.StartedAt.After(when) {
		t.Errorf("start-now kept the stale start time %v (old was %v)", got.StartedAt, when)
	}
}

// A probe marks its row `probed` before it runs, so a probe that FAILED left
// the row flagged as done. Retrying then downloaded a URL whose title and
// thumbnail were never learned: the entry kept showing the raw URL with no
// image preview no matter how often it was retried. Resume and StartNow must
// re-arm the probe for a row that has no metadata at all.
func TestRetryReprobesRowThatNeverProbed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry func(*Manager, string) error
	}{
		{"resume", (*Manager).Resume},
		{"start_now", (*Manager).StartNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			var probeFails atomic.Bool
			probeFails.Store(true)
			r.probeFn = func(url string) ([]Entry, error) {
				if probeFails.Load() {
					return nil, fmt.Errorf("probe unavailable")
				}
				return []Entry{{
					URL:       url,
					Title:     "Real Title",
					Thumbnail: "https://images.example/cover.jpg",
				}}, nil
			}
			m, err := NewWithRunner(context.Background(), 1,
				filepath.Join(t.TempDir(), "state.json"), r)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()

			id, err := m.Add("https://example.com/probe-fails")
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				it, ok := m.Get(id)
				return ok && it.State == ipc.StateFailed
			}, "the probe to fail the row")

			probeFails.Store(false)
			if err := tc.retry(m, id); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				it, ok := m.Get(id)
				return ok && it.ThumbURL != ""
			}, "the retry to re-probe and learn the thumbnail")

			it, _ := m.Get(id)
			if it.Title != "Real Title" {
				t.Errorf("title after retry = %q, want %q", it.Title, "Real Title")
			}
		})
	}
}

// The other half of the same rule: a row that probed fine and then failed
// while DOWNLOADING already has its metadata, so retrying it must not spend
// another probe — and must not risk a second expansion of a row that is
// already the resolved single entry.
func TestRetryDoesNotReprobeRowThatAlreadyHasMetadata(t *testing.T) {
	r := newFakeRunner()
	var probes atomic.Int64
	r.probeFn = func(url string) ([]Entry, error) {
		probes.Add(1)
		return []Entry{{URL: url, Title: "Known", Thumbnail: "https://images.example/a.jpg"}}, nil
	}
	m, err := NewWithRunner(context.Background(), 1,
		filepath.Join(t.TempDir(), "state.json"), r)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	id, err := m.Add("https://example.com/download-fails")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.Title == "Known"
	}, "the probe to complete")
	after := probes.Load()

	// Fail the row the way a download failure does, leaving metadata in place.
	m.mu.Lock()
	it := m.items[id]
	it.State = ipc.StateFailed
	it.Error = "HTTP Error 403"
	m.mu.Unlock()

	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State != ipc.StateFailed
	}, "the retry to re-queue the row")
	if got := probes.Load(); got != after {
		t.Errorf("retry ran %d extra probe(s) for a row that already had metadata", got-after)
	}
}

// ClearStates is the one bulk history path, so it has to refuse a state that
// is not terminal: an active row has a process behind it that must be
// cancelled first, which is ClearAll's job. Silently dropping such a row would
// leak a running yt-dlp with nothing left to stop it.
func TestClearStatesRefusesNonTerminalStates(t *testing.T) {
	m, err := NewWithRunner(context.Background(), 1,
		filepath.Join(t.TempDir(), "state.json"), newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	for _, st := range []ipc.State{ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused} {
		if _, err := m.ClearStates(st); !errors.Is(err, ErrInvalidState) {
			t.Errorf("ClearStates(%q) = %v, want ErrInvalidState", st, err)
		}
	}
	// A mixed set is refused as a whole rather than partly applied.
	if _, err := m.ClearStates(ipc.StateCompleted, ipc.StatePaused); !errors.Is(err, ErrInvalidState) {
		t.Errorf("mixed set = %v, want ErrInvalidState", err)
	}
	for _, st := range []ipc.State{ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted} {
		if _, err := m.ClearStates(st); err != nil {
			t.Errorf("ClearStates(%q) = %v, want nil", st, err)
		}
	}
}

// Re-probing on retry must not become a one-way door. Some extractors cannot
// answer the --flat-playlist probe for a URL they download perfectly well;
// before retries re-probed, such a row went straight to yt-dlp on retry and
// that was the only thing that ever worked for it. A retry whose probe fails
// again therefore falls through to the download instead of failing the row.
func TestRetryProbeFailureFallsThroughToTheDownload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(string) ([]Entry, error)
	}{
		{"probe errors", func(string) ([]Entry, error) {
			return nil, fmt.Errorf("extractor does not support --flat-playlist")
		}},
		{"probe reports nothing", func(string) ([]Entry, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.probeFn = tc.probe
			// newTestManager's context cleanup tears this down; Close is for
			// managers built by NewWithRunner, which own a cancel func.
			m, _ := newTestManager(t, 1, r)

			id, err := m.Add("https://example.com/probe-hostile")
			if err != nil {
				t.Fatal(err)
			}
			m.schedule()
			waitFor(t, 5*time.Second, func() bool {
				it, _ := m.Get(id)
				return it.State == ipc.StateFailed
			}, "the first probe to fail the row")

			if err := m.Resume(id); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				return r.runCount("https://example.com/probe-hostile") > 0
			}, "the retry to attempt the download anyway")

			it, _ := m.Get(id)
			if it.State == ipc.StateFailed {
				t.Fatalf("retry left the row failed (%q) instead of downloading", it.Error)
			}
		})
	}
}

// The fingerprint of the original bug: a row whose probe failed downloaded
// anyway and ended up with files but no title. Counting files as evidence of
// a successful probe would leave exactly those rows title-less forever, so
// the retry decision looks only at what a probe actually produces.
func TestRetryReprobesARowThatHasFilesButNoMetadata(t *testing.T) {
	r := newFakeRunner()
	var probes atomic.Int64
	r.probeFn = func(url string) ([]Entry, error) {
		probes.Add(1)
		return []Entry{{URL: url, Title: "Recovered Title", Thumbnail: "https://img.example/c.jpg"}}, nil
	}
	m, _ := newTestManager(t, 1, r)

	// A row exactly as the old bug left it: failed, with a partial file
	// recorded and no metadata at all.
	m.mu.Lock()
	m.items["legacyrow"] = &item{probed: true, Item: ipc.Item{
		ID:      "legacyrow",
		URL:     "https://example.com/legacy",
		State:   ipc.StateFailed,
		Files:   []string{"/downloads/legacy.mp4"},
		AddedAt: time.Now(),
	}}
	m.mu.Unlock()

	if err := m.Resume("legacyrow"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		it, ok := m.Get("legacyrow")
		return ok && it.Title == "Recovered Title"
	}, "the retry to re-probe a row that only had files")
	if probes.Load() == 0 {
		t.Error("no probe ran for a row with files but no metadata")
	}
}

// The restore predicate and the retry rule have to agree on what "probed"
// means. Restore used to look at title-or-files while the retry rule looks at
// title-or-thumbnail, so a queued row restored with a thumbnail but no title
// was marked unprobed and re-probed on resume — and a probe that failed then
// killed a row that already had the metadata it needed.
func TestRestoredRowWithOnlyAThumbnailCountsAsProbed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]ipc.Item{{
		ID:       "aaaabbbb",
		URL:      "https://example.com/thumb-only",
		ThumbURL: "https://img.example/only.jpg",
		State:    ipc.StateQueued,
		AddedAt:  time.Now().Add(-time.Hour),
	}}, []string{"aaaabbbb"}); err != nil {
		t.Fatal(err)
	}

	r := newFakeRunner()
	var probes atomic.Int64
	r.probeFn = func(string) ([]Entry, error) {
		probes.Add(1)
		return nil, fmt.Errorf("probe unavailable")
	}
	m, err := NewWithRunner(context.Background(), 1, path, r)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	waitFor(t, 5*time.Second, func() bool {
		it, ok := m.Get("aaaabbbb")
		return ok && it.State == ipc.StatePaused
	}, "the row to be restored paused")

	if err := m.Resume("aaaabbbb"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return r.runCount("https://example.com/thumb-only") > 0
	}, "the resume to start the download")

	if n := probes.Load(); n != 0 {
		t.Errorf("re-probed a row that already had a thumbnail (%d probes)", n)
	}
	it, _ := m.Get("aaaabbbb")
	if it.State == ipc.StateFailed {
		t.Fatalf("resume failed the row instead of downloading it: %q", it.Error)
	}
	if it.ThumbURL == "" {
		t.Error("the restored thumbnail was lost")
	}
}

// The API's start_now path calls Add and then StartNow immediately, so a real
// yt-dlp probe is still running when StartNow lands — and StartNow accepts
// already-queued rows. Re-arming the probe there treated a row's FIRST probe
// as a retry: a success left the row unprobed and cost a second probe, and a
// failure quietly downloaded an unprobed URL instead of failing the row.
//
// The probe blocks until this test releases it, so the interleaving is forced
// rather than raced for.
func TestStartNowDuringTheInitialProbeIsNotARetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		probeErr  error
		wantRuns  int
		wantState ipc.State
	}{
		// A probe that succeeds must be the only one, and its metadata must
		// be what the download proceeds with.
		{"probe succeeds", nil, 1, ipc.StateDownloading},
		// A first probe that fails must fail the row. The retry fallback is
		// for re-probes of rows that already reached a terminal state; using
		// it here would download a URL nothing has ever validated.
		{"probe fails", errors.New("no suitable extractor"), 0, ipc.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const url = "https://example.com/start-now-race"
			r := newFakeRunner()
			release := make(chan struct{})
			started := make(chan struct{})
			var probes atomic.Int64
			r.probeFn = func(u string) ([]Entry, error) {
				if probes.Add(1) == 1 {
					close(started)
				}
				<-release
				if tc.probeErr != nil {
					return nil, tc.probeErr
				}
				return []Entry{{URL: u, Title: "Probed Title", Thumbnail: "https://img.example/p.jpg"}}, nil
			}
			m, _ := newTestManager(t, 2, r)

			id, err := m.Add(url)
			if err != nil {
				t.Fatal(err)
			}
			m.schedule()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the initial probe never started")
			}

			// The row is queued with a probe in flight — exactly the state the
			// HTTP handler force-starts from.
			if err := m.StartNow(id); err != nil {
				t.Fatal(err)
			}
			close(release)

			waitFor(t, 5*time.Second, func() bool {
				it, _ := m.Get(id)
				return it.State == tc.wantState
			}, "the row to reach "+string(tc.wantState))

			if n := probes.Load(); n != 1 {
				t.Errorf("ran %d probes for one force-started add, want 1", n)
			}
			if got := r.runCount(url); got != tc.wantRuns {
				t.Errorf("downloads started = %d, want %d", got, tc.wantRuns)
			}
			it, _ := m.Get(id)
			if !it.Forced {
				t.Error("the force-start flag was lost")
			}
			if tc.probeErr == nil && it.Title != "Probed Title" {
				t.Errorf("title = %q, want the probe's metadata", it.Title)
			}
		})
	}
}
