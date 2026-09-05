package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

type fakeRunner struct {
	mu      sync.Mutex
	started []string
	runCnt  map[string]int
	release map[string]chan struct{}
	lines   map[string][]string
	errs    map[string]error
	probeFn func(url string) ([]Entry, error)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		runCnt:  map[string]int{},
		release: map[string]chan struct{}{},
		lines:   map[string][]string{},
		errs:    map[string]error{},
	}
}

func (f *fakeRunner) Probe(ctx context.Context, job Job) ([]Entry, error) {
	url := job.URL
	f.mu.Lock()
	fn := f.probeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(url)
	}
	return []Entry{{URL: url, Title: "title-" + url}}, nil
}

func (f *fakeRunner) Run(ctx context.Context, job Job, onLine func(string)) (string, error) {
	url := job.URL
	f.mu.Lock()
	f.runCnt[url]++
	f.started = append(f.started, url)
	ch := make(chan struct{})
	f.release[url] = ch
	lines, configured := f.lines[url]
	if !configured {
		// A real successful yt-dlp run always names its output — both for a
		// fresh download and for one that was already on disk. Modelling that
		// by default keeps every test that just wants "this finishes" honest,
		// while a test can still opt into the no-output case with setLines(url).
		lines = []string{PrintLine("@g|", "/downloads/fake-"+strings.TrimPrefix(url, "https://")+".mp4")}
	}
	for _, l := range lines {
		onLine(l)
	}
	f.mu.Unlock()

	select {
	case <-ch:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "", f.errs[url]
}

// runCount reports how many times a URL's download has been started, read
// under the lock so a test can poll it while the runner is working.
func (f *fakeRunner) runCount(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCnt[url]
}

func (f *fakeRunner) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// releaseURL unblocks a running fake download. It waits for the run to have
// registered itself rather than assuming it already has: the manager marks an
// item "downloading" BEFORE calling Run, so a test that waits on the item
// state can easily arrive first.
//
// It fails the test rather than returning quietly if the run never appears. A
// silent return turns a real problem into a hang that only shows up as a
// timeout minutes later, on whichever machine happened to be slow.
func (f *fakeRunner) releaseURL(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		ch := f.release[url]
		if ch != nil {
			delete(f.release, url)
		}
		f.mu.Unlock()
		if ch != nil {
			close(ch)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("releaseURL(%q): the fake runner never started this download", url)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeRunner) setLines(url string, lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[url] = lines
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func newTestManager(t *testing.T, max int, r Runner) (*Manager, *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		ctx:    ctx,
		max:    max,
		store:  st,
		runner: r,
		items:  make(map[string]*item),
		subs:   make(map[chan ipc.Event]struct{}),
		dirty:  make(chan struct{}, 1),
		fmtSem: make(chan struct{}, maxConcurrentFormatProbes),
	}
	return m, st
}

func TestValidURL(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://example.com/v", true},
		{"http://x.test", true},
		// yt-dlp has no magnet extractor, and the API contract, OpenAPI
		// document and README all promise http(s) only.
		{"magnet:?xt=urn:x", false},
		{"file:///etc/passwd", false},
		{"ytsearch10:cats", false},
		{"/etc/hosts", false},
		{"//example.com/v", false},
		{"http://", false},
		{"", false},
		{"-o/tmp/evil", false},
		{"--exec=boom", false},
		{"line1\nline2", false},
		{strings.Repeat("a", 5000), false},
	}
	for _, c := range cases {
		if got := ValidURL(c.url); got != c.ok {
			t.Errorf("ValidURL(%q) = %v, want %v", c.url, got, c.ok)
		}
	}
}

func TestAddRejectsInvalid(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	if _, err := m.Add(""); err == nil {
		t.Fatal("expected error for empty url")
	}
	if _, err := m.Add("-o/etc/passwd"); err == nil {
		t.Fatal("expected error for flag-like url")
	}
}

func TestMutationsAreRejectedAfterClose(t *testing.T) {
	m, err := NewWithRunner(context.Background(), 1,
		filepath.Join(t.TempDir(), "state.json"), newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err := m.Add("https://v/late"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Add after Close = %v, want ErrShuttingDown", err)
	}
	if err := m.Pause("missing"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Pause after Close = %v, want ErrShuttingDown", err)
	}
	if _, err := m.ClearAll(); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("ClearAll after Close = %v, want ErrShuttingDown", err)
	}
	if _, err := m.ClearFinished(); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("ClearFinished after Close = %v, want ErrShuttingDown", err)
	}
	if err := m.SetMaxConcurrent(2); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("SetMaxConcurrent after Close = %v, want ErrShuttingDown", err)
	}
}

func TestQueueRespectsMaxConcurrent(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 2, fr)
	var ids []string
	for i := 0; i < 4; i++ {
		id, err := m.Add(fmt.Sprintf("https://v/%d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 2 }, "two downloads started")
	// Probes for items 3 and 4 finish asynchronously; queued only counts
	// items whose probe has completed.
	waitFor(t, time.Second, func() bool {
		running, queued := m.Stats()
		return running == 2 && queued == 2
	}, "stats settle to (2,2)")

	fr.releaseURL(t, "https://v/0")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 3 }, "third download starts after slot frees")
}

func TestFIFOOrder(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	for i := 0; i < 3; i++ {
		if _, err := m.Add(fmt.Sprintf("https://v/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "first started")
	fr.mu.Lock()
	first := fr.started[0]
	fr.mu.Unlock()
	if first != "https://v/0" {
		t.Fatalf("first started = %q, want https://v/0", first)
	}
	fr.releaseURL(t, first)
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 2 }, "second started")
	fr.mu.Lock()
	second := fr.started[1]
	fr.mu.Unlock()
	if second != "https://v/1" {
		t.Fatalf("second started = %q, want https://v/1", second)
	}
}

func TestForceStartBypassesLimit(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	idA, _ := m.Add("https://v/a")
	idB, _ := m.Add("https://v/b")

	waitFor(t, time.Second, func() bool {
		itA, ok := m.Get(idA)
		return ok && itA.State == ipc.StateDownloading
	}, "a started first (FIFO)")

	if err := m.StartNow(idB); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		itB, ok := m.Get(idB)
		return ok && itB.State == ipc.StateDownloading
	}, "b force-started")
	running, _ := m.Stats()
	if running != 2 {
		t.Fatalf("running = %d, want 2 (over limit)", running)
	}
	_ = idA
}

func TestPauseResumeDownload(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/x")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")

	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State == ipc.StatePaused
	}, "paused")

	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		return fr.runCnt["https://v/x"] >= 2
	}, "resumed downloading (second run started)")

	it, _ := m.Get(id)
	if it.State != ipc.StateDownloading && it.State != ipc.StateCompleted {
		t.Fatalf("state = %s, want downloading/completed", it.State)
	}

	fr.releaseURL(t, "https://v/x")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State == ipc.StateCompleted
	}, "completed after release")
}

func TestPauseQueuedItem(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	idA, _ := m.Add("https://v/a")
	idB, _ := m.Add("https://v/b")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "a started")

	if err := m.Pause(idB); err != nil {
		t.Fatal(err)
	}
	itB, ok := m.Get(idB)
	if !ok || itB.State != ipc.StatePaused {
		t.Fatalf("b state = %v ok=%v, want paused", itB.State, ok)
	}
	_ = idA

	fr.releaseURL(t, "https://v/a")
	time.Sleep(50 * time.Millisecond)
	if _, queued := m.Stats(); queued != 0 {
		t.Fatalf("paused item must not start; queued=%d", queued)
	}

	if err := m.Resume(idB); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 2 }, "b started after resume")
}

func TestCancelRemovesAndCleansPartials(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "video.mp4")
	part := target + ".part"
	frag := target + ".part-Frag0001"
	ytdl := target + ".ytdl"
	for _, p := range []string{part, frag, ytdl} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	full := filepath.Join(tmp, "other.mkv")
	if err := os.WriteFile(full, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	fr := newFakeRunner()
	fr.setLines("https://v/c", PrintLine("@t|", "Cool Video"), PrintLine("@f|", target))
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/c")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")

	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		_, ok := m.Get(id)
		return !ok
	}, "item removed after cancel")

	for _, p := range []string{part, frag, ytdl} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", p)
		}
	}
	if _, err := os.Stat(full); err != nil {
		t.Errorf("unrelated file must survive: %v", err)
	}
}

func TestRemoveWhileRunning(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/r")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")

	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		_, ok := m.Get(id)
		return !ok
	}, "removed while running")
}

func TestRemoveWhileRunningKeepsCompletedBaseFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "already-finished.mp4")
	if err := os.WriteFile(base, []byte("finished video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".part", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := newFakeRunner()
	fr.setLines("https://v/postprocessing", PrintLine("@f|", base))
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/postprocessing")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")
	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		_, ok := m.Get(id)
		return !ok
	}, "removed")
	if data, err := os.ReadFile(base); err != nil || string(data) != "finished video" {
		t.Fatalf("ordinary remove deleted completed base: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(base + ".part"); !os.IsNotExist(err) {
		t.Fatal("ordinary remove must still delete partial data")
	}
}

func TestPlaylistExpansion(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.probeFn = func(url string) ([]Entry, error) {
		return []Entry{
			{URL: "https://e/1", Title: "Ep1"},
			{URL: "https://e/2", Title: "Ep2"},
			{URL: "https://e/3", Title: "Ep3"},
		}, nil
	}
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)

	parentID, err := m.Add("https://playlist")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		items := m.List()
		return len(items) == 3
	}, "playlist expanded to 3 children")

	if _, ok := m.Get(parentID); ok {
		t.Fatal("parent should be removed")
	}
	titles := map[string]bool{}
	for _, it := range m.List() {
		titles[it.Title] = true
		if it.State != ipc.StateQueued && it.State != ipc.StateDownloading {
			t.Errorf("child %s state = %s, want queued or downloading", it.Title, it.State)
		}
	}
	for _, want := range []string{"Ep1", "Ep2", "Ep3"} {
		if !titles[want] {
			t.Errorf("missing child title %q", want)
		}
	}
}

func TestProbeSingleSetsTitle(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://single")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.Title == "title-https://single"
	}, "title set from probe")
}

func TestProbeFailureMarksFailed(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.probeFn = func(url string) ([]Entry, error) {
		return nil, fmt.Errorf("unsupported url")
	}
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://bad")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State == ipc.StateFailed
	}, "failed state")
	it, _ := m.Get(id)
	if !strings.Contains(it.Error, "unsupported url") {
		t.Errorf("error = %q", it.Error)
	}
}

func TestFailedRunCapturesError(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.errs["https://x.test/f"] = fmt.Errorf("HTTP Error 404")
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://x.test/f")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")
	fr.releaseURL(t, "https://x.test/f")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State == ipc.StateFailed
	}, "failed")
	it, _ := m.Get(id)
	if !strings.Contains(it.Error, "404") {
		t.Errorf("error = %q, want contains 404", it.Error)
	}
}

func TestClearFinished(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.errs["https://v/bad"] = fmt.Errorf("boom")
	fr.mu.Unlock()
	m, _ := newTestManager(t, 5, fr)
	idGood, _ := m.Add("https://v/good")
	idBad, _ := m.Add("https://v/bad")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 2 }, "both started")
	fr.releaseURL(t, "https://v/good")
	fr.releaseURL(t, "https://v/bad")
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(idGood)
		it2, _ := m.Get(idBad)
		return it.State == ipc.StateCompleted && it2.State == ipc.StateFailed
	}, "both finished")

	if n, err := m.ClearFinished(); err != nil || n != 2 {
		t.Fatalf("cleared = %d, want 2", n)
	}
	if len(m.List()) != 0 {
		t.Fatal("list should be empty")
	}
}

func TestProgressParsingAndThrottleState(t *testing.T) {
	fr := newFakeRunner()
	fr.setLines("https://v/p",
		"@p|100|1000|50|10",
		PrintLine("@t|", "Prog Video"),
		"@p|500|1000|25|5",
	)
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/p")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.Got == 500 && it.Total == 1000 && it.Speed == 25 && it.ETA == 5
	}, "progress fields updated")

	it, _ := m.Get(id)
	if it.Progress < 49.9 || it.Progress > 50.1 {
		t.Errorf("progress = %f, want ~50", it.Progress)
	}
	if it.Title != "Prog Video" {
		t.Errorf("title = %q", it.Title)
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	ch, unsub := m.Subscribe()
	defer unsub()
	id, _ := m.Add("https://v/s")
	found := false
	deadline := time.After(time.Second)
	for !found {
		select {
		case ev := <-ch:
			if ev.Item != nil && ev.Item.ID == id {
				found = true
			}
		case <-deadline:
			t.Fatal("no event for added item")
		}
	}
}

func TestRestoreMarksUnfinishedWorkAsPaused(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	st, err := NewStore(stPath)
	if err != nil {
		t.Fatal(err)
	}
	items := []ipc.Item{
		{ID: "aaa111", URL: "https://v/was-running", State: ipc.StateDownloading},
		{ID: "bbb222", URL: "https://v/was-queued", State: ipc.StateQueued},
		{ID: "ccc333", URL: "https://v/done", State: ipc.StateCompleted},
	}
	if err := st.Save(items, []string{"bbb222"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := newFakeRunner()
	m := &Manager{
		ctx: ctx, max: 4, store: st, runner: fr,
		items: make(map[string]*item),
		subs:  make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1),
	}
	m.restore()
	m.schedule()

	it, ok := m.Get("aaa111")
	if !ok || it.State != ipc.StatePaused {
		t.Fatalf("was-running state = %v ok=%v, want paused", it.State, ok)
	}
	it, ok = m.Get("bbb222")
	if !ok || it.State != ipc.StatePaused || it.UserPaused {
		t.Fatalf("was-queued state = %+v ok=%v, want recovery pause", it, ok)
	}
	time.Sleep(50 * time.Millisecond)
	if got := fr.startedCount(); got != 0 {
		t.Fatalf("restore started %d unfinished download(s), want 0", got)
	}

	if err := m.Resume("aaa111"); err != nil {
		t.Fatalf("resume restored item: %v", err)
	}
	if err := m.Resume("bbb222"); err != nil {
		t.Fatalf("resume restored queued item: %v", err)
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() >= 2 }, "explicitly resumed items run")
}

func TestDuplicateAddAllowedDistinctIDs(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	id1, _ := m.Add("https://v/same")
	id2, _ := m.Add("https://v/same")
	if id1 == id2 {
		t.Fatal("ids must be unique")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 3); got != "ab" {
		t.Errorf("truncate = %q", got)
	}
}

func TestClearAll(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 5, fr)
	idDone, _ := m.Add("https://v/done")
	idRun, _ := m.Add("https://v/run")
	idQueue, _ := m.Add("https://v/queue")
	// Wait for THIS row, not for "any runner started". With a concurrency of
	// five all three are eligible at once and the scheduler may reach another
	// one first, which made this assertion fail intermittently under -race.
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(idDone)
		return ok && it.State == ipc.StateDownloading
	}, "the first item to be downloading")
	n, err := m.ClearAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("ClearAll = %d, want 3", n)
	}
	waitFor(t, time.Second, func() bool { return len(m.List()) == 0 }, "list empty after clear all")
	waitFor(t, time.Second, func() bool {
		running, queued := m.Stats()
		return running == 0 && queued == 0
	}, "stats settle to zero after clear all")
	_, ok := m.Get(idRun)
	_, ok2 := m.Get(idQueue)
	if ok || ok2 {
		t.Fatal("items must be gone")
	}
}

func TestClearAllWhilePausedAndFailed(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.errs["https://v/bad"] = fmt.Errorf("boom")
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)
	idA, _ := m.Add("https://v/a")
	idB, _ := m.Add("https://v/bad")
	_, _ = m.Add("https://v/c")
	m.Pause(idB)
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(idA)
		return it.State == ipc.StateDownloading
	}, "a running")
	if n, err := m.ClearAll(); err != nil || n != 3 {
		t.Fatalf("ClearAll = %d, want 3", n)
	}
	waitFor(t, time.Second, func() bool { return len(m.List()) == 0 }, "all gone")
}

func TestInterruptedCountSkipsUserPaused(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 5, fr)
	idA, _ := m.Add("https://v/a")
	idB, _ := m.Add("https://v/b")
	m.Pause(idB)
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(idA)
		it2, _ := m.Get(idB)
		return it.State == ipc.StateDownloading && it2.State == ipc.StatePaused
	}, "a running, b paused")

	// A row the user paused is not "interrupted work": it is exactly where
	// they left it, so it must not be counted as waiting.
	if n := m.InterruptedCount(); n != 0 {
		t.Fatalf("InterruptedCount = %d, want 0 (a user pause is not an interruption)", n)
	}
	itB, _ := m.Get(idB)
	if itB.State != ipc.StatePaused || !itB.UserPaused {
		t.Fatalf("b = %s userPaused=%v", itB.State, itB.UserPaused)
	}
	// b may have reached Run before Pause cancelled it. Compare against the
	// count immediately before Resume rather than assuming exactly one earlier
	// start across both rows; an exact total made this test scheduler-dependent.
	startedBeforeResume := fr.startedCount()
	if err := m.Resume(idB); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return fr.startedCount() > startedBeforeResume
	}, "manual resume starts b")
}

// A download interrupted by shutdown comes back paused and STAYS paused:
// startup must never begin pulling bandwidth the operator did not ask for.
func TestShutdownPauseStaysPausedAndIsCounted(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	st, err := NewStore(stPath)
	if err != nil {
		t.Fatal(err)
	}
	items := []ipc.Item{
		{ID: "aaa111", URL: "https://v/was-running", State: ipc.StateDownloading},
		{ID: "ccc333", URL: "https://v/was-queued", State: ipc.StateQueued},
		{ID: "bbb222", URL: "https://v/user-paused", State: ipc.StatePaused, UserPaused: true},
	}
	if err := st.Save(items, []string{"ccc333"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := newFakeRunner()
	m := &Manager{
		ctx: ctx, max: 4, store: st, runner: fr,
		items: make(map[string]*item),
		subs:  make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1),
	}
	m.restore()

	if n := m.InterruptedCount(); n != 2 {
		t.Fatalf("InterruptedCount = %d, want 2", n)
	}
	m.schedule()
	time.Sleep(150 * time.Millisecond)
	if got := fr.startedCount(); got != 0 {
		t.Fatalf("started %d download(s) on restore; interrupted work must stay paused", got)
	}
	if it, ok := m.Get("aaa111"); !ok || it.State != ipc.StatePaused || it.UserPaused {
		t.Fatalf("interrupted item = %+v ok=%v, want paused with UserPaused=false", it, ok)
	}
	if it, ok := m.Get("ccc333"); !ok || it.State != ipc.StatePaused || it.UserPaused {
		t.Fatalf("queued item = %+v ok=%v, want paused with UserPaused=false", it, ok)
	}
	it, ok := m.Get("bbb222")
	if !ok || it.State != ipc.StatePaused || !it.UserPaused {
		t.Fatalf("user-paused item = %+v ok=%v, must stay paused", it, ok)
	}
}

func TestSingleEntryKeepsOriginalURL(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.probeFn = func(url string) ([]Entry, error) {
		return []Entry{{URL: "/static/hls/video.m3u8", Title: "PeerTube Video"}}, nil
	}
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://videos.example.com/w/abc")
	waitFor(t, time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.Title == "PeerTube Video"
	}, "title adopted")
	it, _ := m.Get(id)
	if it.URL != "https://videos.example.com/w/abc" {
		t.Fatalf("url rewritten to %q; must keep original", it.URL)
	}
}

func TestParseProbeOutput(t *testing.T) {
	out := `@e|{"url":"https://e/1","title":"Ep1"}` + "\n" +
		`@e|{"url":"NA","title":"Ep2"}` + "\n" +
		`@e|{"url":"","title":"Ep3"}` + "\n"
	got := parseProbeOutput(out, "orig")
	if len(got) != 1 || got[0].URL != "https://e/1" || got[0].Title != "Ep1" {
		t.Fatalf("multi = %+v", got)
	}
	fb := parseProbeOutput(`@e|{"url":"NA","title":"Only Title"}`+"\n", "orig")
	if len(fb) != 1 || fb[0].URL != "orig" || fb[0].Title != "Only Title" {
		t.Fatalf("fallback = %+v", fb)
	}
	empty := parseProbeOutput("", "orig")
	if len(empty) != 1 || empty[0].URL != "orig" {
		t.Fatalf("empty = %+v", empty)
	}
}

// TestProbeOutputResistsMetadataInjection pins the reason probe output is JSON.
// With the old "@e|%(url)s|%(title)s|%(thumbnail)s" framing, a title carrying a
// newline forged an extra entry — letting a page inject a URL of its choosing
// into the queue — and a title carrying "|" shifted every following field.
func TestProbeOutputResistsMetadataInjection(t *testing.T) {
	hostile := "Cats\n@e|{\"url\":\"https://attacker.test/x\",\"title\":\"injected\"}"
	line, err := json.Marshal(map[string]string{
		"url":   "https://legit.test/v",
		"title": hostile,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := parseProbeOutput("@e|"+string(line)+"\n", "orig")
	if len(got) != 1 {
		t.Fatalf("injection produced %d entries: %+v", len(got), got)
	}
	if got[0].URL != "https://legit.test/v" {
		t.Fatalf("url = %q, want the real one", got[0].URL)
	}
	if got[0].Title != hostile {
		t.Fatalf("title = %q, want it carried intact as data", got[0].Title)
	}

	// A pipe in the title must not shift fields either.
	piped, _ := json.Marshal(map[string]string{
		"url": "https://legit.test/v", "title": "a|b|c", "thumbnail": "https://t/1.jpg",
	})
	p := parseProbeOutput("@e|"+string(piped)+"\n", "orig")
	if len(p) != 1 || p[0].Title != "a|b|c" || p[0].Thumbnail != "https://t/1.jpg" {
		t.Fatalf("pipe handling = %+v", p)
	}
}

func TestResumeFailedRetriesDownload(t *testing.T) {
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.errs["https://v/f"] = fmt.Errorf("HTTP Error 404")
	fr.mu.Unlock()
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/f")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "first attempt started")
	fr.releaseURL(t, "https://v/f")
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(id)
		return it.State == ipc.StateFailed
	}, "failed")

	if err := m.Resume(id); err != nil {
		t.Fatalf("resume failed item: %v", err)
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 2 }, "second attempt started")
	fr.mu.Lock()
	delete(fr.errs, "https://v/f")
	fr.mu.Unlock()
	fr.releaseURL(t, "https://v/f")
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(id)
		return it.State == ipc.StateCompleted
	}, "completed on retry")
	it, _ := m.Get(id)
	if it.Error != "" {
		t.Errorf("error not cleared after successful retry: %q", it.Error)
	}
}

func TestPauseStillRejectsFailed(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)
	m.mu.Lock()
	m.items["zzz"] = &item{Item: ipc.Item{ID: "zzz", URL: "https://x", State: ipc.StateFailed}}
	m.mu.Unlock()
	if err := m.Pause("zzz"); err != ErrInvalidState {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
	if err := m.StartNow("zzz"); err != nil {
		t.Fatalf("start-now on failed should retry: %v", err)
	}
}

func TestRestoreHydratesProgressFromPartFile(t *testing.T) {
	dir := t.TempDir()
	dlDir := filepath.Join(dir, "dl")
	os.MkdirAll(dlDir, 0o700)
	part := filepath.Join(dlDir, "video.mp4.part")
	os.WriteFile(part, make([]byte, 600), 0o600)

	stPath := filepath.Join(dir, "state.json")
	st, _ := NewStore(stPath)
	items := []ipc.Item{
		{ID: "p1", URL: "https://v/1", Title: "video.mp4", State: ipc.StatePaused,
			Total: 1000, Files: []string{part[:len(part)-len(".part")]}},
		{ID: "p2", URL: "https://v/2", Title: "gone.mp4", State: ipc.StatePaused, Total: 500},
	}
	if err := st.Save(items, []string{"p1"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{
		ctx: ctx, max: 4, store: st, runner: newFakeRunner(),
		items: make(map[string]*item),
		subs:  make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1),
	}
	m.restore()

	it, ok := m.Get("p1")
	if !ok {
		t.Fatal("item missing")
	}
	if it.Got != 600 || it.Progress < 59.9 || it.Progress > 60.1 {
		t.Errorf("hydrated = got %d progress %.1f, want 600/~60", it.Got, it.Progress)
	}
	it2, _ := m.Get("p2")
	if it2.Progress != 0 || it2.Got != 0 {
		t.Errorf("missing part must stay at zero, got %+v", it2.Got)
	}
}

func TestRestoreDetectsFinishedFile(t *testing.T) {
	dir := t.TempDir()
	dlDir := filepath.Join(dir, "dl")
	os.MkdirAll(dlDir, 0o700)
	full := filepath.Join(dlDir, "done.mp4")
	os.WriteFile(full, make([]byte, 1000), 0o600)

	st, _ := NewStore(filepath.Join(dir, "state.json"))
	items := []ipc.Item{
		{ID: "f1", URL: "https://v/1", State: ipc.StatePaused, Total: 1000, Files: []string{full}},
	}
	st.Save(items, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{ctx: ctx, max: 2, store: st, runner: newFakeRunner(),
		items: make(map[string]*item), subs: make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1)}
	m.restore()
	it, ok := m.Get("f1")
	if !ok {
		t.Fatal("missing")
	}
	if it.Progress != 100 || it.Got != 1000 {
		t.Errorf("progress=%.1f got=%d, want 100/1000 (file already complete)", it.Progress, it.Got)
	}
}

// TestRemoveKeepsFinishedFile pins the guarantee that replaced file deletion:
// removing a row is a list operation, never a filesystem one. The app writes
// into a directory the operator owns, so deleting media there is deliberately
// not something a network service can be asked to do.
func TestRemoveKeepsFinishedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "done.mp4")
	os.WriteFile(target, []byte("video-bytes"), 0o600) //nolint:errcheck

	m, _ := newTestManager(t, 1, newFakeRunner())
	m.mu.Lock()
	m.items["keep1"] = &item{Item: ipc.Item{
		ID: "keep1", URL: "https://v/1", Title: "t", State: ipc.StateCompleted,
		Files: []string{target},
	}}
	m.mu.Unlock()

	if err := m.Remove("keep1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("keep1"); ok {
		t.Error("row must be removed")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "video-bytes" {
		t.Fatalf("finished file must survive a row removal: %v %q", err, data)
	}
}

// TestCleanupSkipsUnsafePaths keeps the degenerate-path guard honest even
// though only scratch files are ever removed now.
func TestCleanupSkipsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	neighbor := filepath.Join(dir, "keepme.txt")
	os.WriteFile(neighbor, []byte("important"), 0o600) //nolint:errcheck

	if err := cleanupPartials([]string{"..", "", ".", "-rf", "/"}); err != nil {
		t.Fatalf("degenerate paths must be skipped, not attempted: %v", err)
	}
	data, err := os.ReadFile(neighbor)
	if err != nil || string(data) != "important" {
		t.Errorf("neighbor must survive untouched: %v %q", err, data)
	}
}

func TestRemoveRunningStopsAndClearsPartials(t *testing.T) {
	tmp := t.TempDir()
	part := filepath.Join(tmp, "v.mp4.part")
	os.WriteFile(part, []byte("partial"), 0o600)
	fr := newFakeRunner()
	fr.setLines("https://v/p", PrintLine("@f|", part[:len(part)-len(".part")]))
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/p")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")

	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		_, ok := m.Get(id)
		return !ok
	}, "row gone")
	waitFor(t, time.Second, func() bool {
		_, err := os.Stat(part)
		return os.IsNotExist(err)
	}, "partial deleted")
}

func TestRestoreMarksVanishedFilesAsDeleted(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "state.json"))
	items := []ipc.Item{
		{ID: "gone1", URL: "https://v/1", State: ipc.StateCompleted, Files: []string{filepath.Join(dir, "moved-away.mp4")}},
		{ID: "part2", URL: "https://v/2", State: ipc.StatePaused, Total: 100, Files: []string{filepath.Join(dir, "half.mp4")}},
		{ID: "keep3", URL: "https://v/3", State: ipc.StateCompleted},
	}
	st.Save(items, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{ctx: ctx, max: 2, store: st, runner: newFakeRunner(),
		items: make(map[string]*item), subs: make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1)}
	m.restore()

	it1, ok1 := m.Get("gone1")
	it3, _ := m.Get("keep3")
	if !ok1 || it1.State != ipc.StateDeleted {
		t.Fatalf("vanished completed should become deleted, got %+v", it1.State)
	}
	if it3.State != ipc.StateCompleted {
		t.Fatalf("completed without files info must stay completed, got %s", it3.State)
	}

	it2, _ := m.Get("part2")
	if it2.State != ipc.StateDeleted {
		t.Fatalf("vanished partial should become deleted, got %s", it2.State)
	}
	if n, err := m.ClearFinished(); err != nil || n != 3 {
		t.Fatalf("ClearFinished = %d, want 3 (completed+deleted+deleted-partial)", n)
	}
}

func TestResumeDeletedRestartsFromScratch(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "state.json"))
	items := []ipc.Item{
		{ID: "del9", URL: "https://v/9", State: ipc.StateCompleted, Files: []string{filepath.Join(dir, "nope.mp4")}},
	}
	st.Save(items, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := newFakeRunner()
	m := &Manager{ctx: ctx, max: 2, store: st, runner: fr,
		items: make(map[string]*item), subs: make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1)}
	m.restore()

	it, ok := m.Get("del9")
	if !ok || it.State != ipc.StateDeleted {
		t.Fatalf("want deleted state, got %v/%s", ok, it.State)
	}
	if err := m.Resume("del9"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "fresh download started")
	it, _ = m.Get("del9")
	if it.State != ipc.StateDownloading && it.State != ipc.StateCompleted {
		t.Fatalf("state = %s", it.State)
	}
}

func TestRestoreQuarantinesCorruptState(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	os.WriteFile(stPath, []byte("{corrupt json"), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := New(ctx, 2, stPath)
	if err != nil {
		t.Fatalf("manager must survive corrupt state: %v", err)
	}
	if _, err := os.Stat(stPath); !os.IsNotExist(err) {
		t.Error("corrupt file should be moved aside")
	}
	entries, _ := filepath.Glob(stPath + ".corrupt-*")
	if len(entries) != 1 {
		t.Errorf("quarantine copy missing: %v", entries)
	}
	id, err := mgr.Add("https://v/works")
	if err != nil || id == "" {
		t.Fatalf("manager usable after quarantine: %v", err)
	}
	mgr.Close()
}

func TestDeletedStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	st, _ := NewStore(stPath)
	items := []ipc.Item{
		{ID: "del1", URL: "https://v/1", State: ipc.StateDeleted, Files: []string{filepath.Join(dir, "gone.mp4")}},
	}
	st.Save(items, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{ctx: ctx, max: 2, store: st, runner: newFakeRunner(),
		items: make(map[string]*item), subs: make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1)}
	m.restore()

	it, ok := m.Get("del1")
	if !ok || it.State != ipc.StateDeleted {
		t.Fatalf("deleted state = %v/%s, must persist across restart", ok, it.State)
	}
}

func TestFinalMergedFileCaptured(t *testing.T) {
	tmp := t.TempDir()
	final := filepath.Join(tmp, "video final.mp4")
	intermediate := filepath.Join(tmp, "video.f137.mp4")
	os.WriteFile(final, []byte("merged"), 0o600)
	os.WriteFile(intermediate, []byte("stream"), 0o600)

	fr := newFakeRunner()
	fr.setLines("https://v/g",
		PrintLine("@f|", intermediate),
		PrintLine("@g|", final),
	)
	m, _ := newTestManager(t, 1, fr)
	id, _ := m.Add("https://v/g")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "started")

	fr.mu.Lock()
	var hasFinal bool
	m.mu.Lock()
	for _, f := range m.items[id].Files {
		if f == final {
			hasFinal = true
		}
	}
	m.mu.Unlock()
	fr.mu.Unlock()
	if !hasFinal {
		t.Fatal("final merged path not captured from @g")
	}

	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(m.List()) == 0 }, "row removed")
	// The merged output survives; only yt-dlp's own scratch is cleared.
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("finished file must survive a removal: %v", err)
	}
}

func TestOnLineShortProgressLineIgnored(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	m.mu.Lock()
	it := &item{Item: ipc.Item{ID: "x", URL: "https://v/x", State: ipc.StateDownloading}}
	m.items["x"] = it
	m.mu.Unlock()
	m.onLine(it, "@p|100")
	if it.Got != 0 || it.Total != 0 {
		t.Fatalf("malformed progress line must be ignored, got %+v", it.Item)
	}
}

// A second Add of a URL a live entry already holds is rejected instead of
// queueing the same video twice, and the error names the blocking state.
func TestAddRejectsDuplicateURL(t *testing.T) {
	blocking := []ipc.State{
		ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused, ipc.StateCompleted,
	}
	for _, st := range blocking {
		t.Run(string(st), func(t *testing.T) {
			// Concurrency 0: nothing is scheduled, so the state forced below
			// stays put. With a slot free, the scheduler could move the row to
			// "downloading" between the assignment and the assertion, which
			// made this fail intermittently under -race.
			m, _ := newTestManager(t, 0, newFakeRunner())
			const u = "https://example.com/dup"
			id, err := m.Add(u)
			if err != nil {
				t.Fatalf("first add: %v", err)
			}
			m.mu.Lock()
			m.items[id].State = st
			m.mu.Unlock()

			if _, err := m.Add(u); err == nil {
				t.Fatal("second add succeeded; want duplicate rejection")
			} else if !errors.Is(err, ErrDuplicate) {
				t.Fatalf("second add: got %v, want ErrDuplicate", err)
			} else {
				var dup *DuplicateError
				if !errors.As(err, &dup) || dup.ID != id || dup.State != st {
					t.Fatalf("duplicate details = %+v, want id %s state %s", dup, id, st)
				}
			}
			if n := len(m.items); n != 1 {
				t.Fatalf("items = %d, want 1", n)
			}
		})
	}
}

// Failed and deleted entries are finished work the user may want to run again,
// so pasting the URL a second time must still queue it.
func TestAddAllowsRetryOfFinishedFailures(t *testing.T) {
	for _, st := range []ipc.State{ipc.StateFailed, ipc.StateDeleted} {
		t.Run(string(st), func(t *testing.T) {
			m, _ := newTestManager(t, 1, newFakeRunner())
			const u = "https://example.com/again"
			id, err := m.Add(u)
			if err != nil {
				t.Fatalf("first add: %v", err)
			}
			m.mu.Lock()
			m.items[id].State = st
			m.mu.Unlock()

			if _, err := m.Add(u); err != nil {
				t.Fatalf("re-add after %s: %v", st, err)
			}
			if n := len(m.items); n != 2 {
				t.Fatalf("items = %d, want 2", n)
			}
		})
	}
}

// Whitespace around a pasted URL must not defeat the check: Add trims before
// both the duplicate lookup and the store, so " url " and "url" are one entry.
func TestAddDuplicateIgnoresSurroundingWhitespace(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	if _, err := m.Add("https://example.com/ws"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := m.Add("  https://example.com/ws  "); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("got %v, want ErrDuplicate", err)
	}
}

// Playlist children never go through Add, so expansion has to enforce the
// duplicate rule itself: an entry the queue already holds, and an entry the
// playlist lists twice, must both be dropped rather than started a second
// time against the same output file.
func TestExpandDeduplicatesPlaylistEntries(t *testing.T) {
	r := newFakeRunner()
	r.probeFn = func(url string) ([]Entry, error) {
		if url != "https://example.com/list" {
			return []Entry{{URL: url}}, nil
		}
		return []Entry{
			{URL: "https://example.com/a"},   // already queued below
			{URL: "https://example.com/b"},   //
			{URL: "  https://example.com/b"}, // same entry, whitespace only
			{URL: "https://example.com/c"},
		}, nil
	}
	// max 3 so the placeholder can actually be probed; the fake runner blocks,
	// so whatever starts stays "downloading" and every row remains inspectable.
	m, _ := newTestManager(t, 3, r)

	if _, err := m.Add("https://example.com/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("https://example.com/list"); err != nil {
		t.Fatal(err)
	}
	m.schedule()

	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, it := range m.items {
			if it.URL == "https://example.com/list" {
				return false // placeholder not yet replaced
			}
		}
		return len(m.items) == 3
	}, "playlist expansion")

	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]int{}
	for _, it := range m.items {
		seen[it.URL]++
	}
	want := map[string]int{
		"https://example.com/a": 1,
		"https://example.com/b": 1,
		"https://example.com/c": 1,
	}
	if len(seen) != len(want) {
		t.Fatalf("urls = %v, want %v", seen, want)
	}
	for u, n := range want {
		if seen[u] != n {
			t.Errorf("url %s appears %d times, want %d", u, seen[u], n)
		}
	}
}

// A failed row does not reserve its URL, so another entry may claim it before
// the user retries. Resume and StartNow must then refuse rather than aim two
// runs at one output file.
func TestRetryRefusedWhenAnotherEntryClaimedTheURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(m *Manager, id string) error
	}{
		{"resume", func(m *Manager, id string) error { return m.Resume(id) }},
		{"start_now", func(m *Manager, id string) error { return m.StartNow(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t, 0, newFakeRunner())
			const u = "https://example.com/contested"

			failedID, err := m.Add(u)
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			m.items[failedID].State = ipc.StateFailed
			m.mu.Unlock()

			// Allowed precisely because the first row failed.
			liveID, err := m.Add(u)
			if err != nil {
				t.Fatalf("re-add after failure: %v", err)
			}

			err = tc.act(m, failedID)
			if !errors.Is(err, ErrDuplicate) {
				t.Fatalf("retry of stale failed row: got %v, want ErrDuplicate", err)
			}
			var dup *DuplicateError
			if !errors.As(err, &dup) || dup.ID != liveID {
				t.Fatalf("duplicate points at %+v, want id %s", dup, liveID)
			}
			m.mu.Lock()
			state := m.items[failedID].State
			m.mu.Unlock()
			if state != ipc.StateFailed {
				t.Fatalf("refused retry still changed state to %s", state)
			}
		})
	}
}

// The guard must not make ordinary retries unreachable: with no competing
// entry, a failed row still resumes.
func TestRetryAllowedWhenURLIsFree(t *testing.T) {
	m, _ := newTestManager(t, 0, newFakeRunner())
	id, err := m.Add("https://example.com/free")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.items[id].State = ipc.StateFailed
	m.items[id].Error = "boom"
	m.mu.Unlock()

	if err := m.Resume(id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.items[id].State; got != ipc.StateQueued {
		t.Fatalf("state = %s, want queued", got)
	}
	if m.items[id].Error != "" {
		t.Fatalf("stale error kept: %q", m.items[id].Error)
	}
}

// A paused row holds its own URL. Resuming it must not trip over itself.
func TestResumePausedRowIsNotBlockedByItself(t *testing.T) {
	m, _ := newTestManager(t, 0, newFakeRunner())
	id, err := m.Add("https://example.com/paused")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.items[id].State = ipc.StatePaused
	m.mu.Unlock()

	if err := m.Resume(id); err != nil {
		t.Fatalf("resume: %v", err)
	}
}
