package manager

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

func TestFreshIDNeverCollidesUnderVolume(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	const n = 200000 // birthday bound: ~99% collision rate without regeneration
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		id, err := m.freshIDLocked()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := m.items[id]; dup {
			t.Fatalf("freshIDLocked returned duplicate %q", id)
		}
		m.items[id] = nil
	}
}

func TestNewIDHasLongLivedEntropy(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	// Sixteen random bytes, hex encoded. This pins the lifetime-collision
	// guarantee separately from freshIDLocked's live-row collision retry.
	if len(id) != 32 {
		t.Fatalf("newID length = %d, want 32: %q", len(id), id)
	}
}

func TestAddSurfacesRandomIDFailure(t *testing.T) {
	old := randomRead
	randomRead = func([]byte) (int, error) { return 0, context.Canceled }
	defer func() { randomRead = old }()
	m, _ := newTestManager(t, 1, newFakeRunner())
	if _, err := m.Add("https://v/id-failure"); err == nil {
		t.Fatal("crypto/rand failure must be returned instead of looping on a zero ID")
	}
	if len(m.List()) != 0 {
		t.Fatal("failed ID generation must not insert an item")
	}
}

func TestFlushSurfacesStateSaveFailure(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	m.store = &Store{path: filepath.Join(t.TempDir(), "missing", "state.json")}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	m.flush()
	_ = w.Close()
	os.Stderr = oldStderr
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "save state") {
		t.Fatalf("save failure was not surfaced: %q", data)
	}
}

func TestPublishSuppressesUpdateAfterRemoved(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	ch, unsub := m.Subscribe()
	defer unsub()

	m.mu.Lock()
	it := &item{Item: ipc.Item{ID: "g1", URL: "https://v/x", State: ipc.StateDownloading}}
	m.items["g1"] = it
	seq := m.stampLocked()
	m.mu.Unlock()

	m.publish(seq, ipc.Event{Event: "update", Item: &it.Item}) // live: delivered

	m.mu.Lock()
	m.noteRemovedLocked("g1")
	delete(m.items, "g1")
	m.mu.Unlock()
	m.publish(0, ipc.Event{Event: "removed", ID: "g1"}) // delivered

	// The stale update that was stamped before the removal must never reach
	// the UI — it would resurrect a deleted row.
	m.publish(seq, ipc.Event{Event: "update", Item: &it.Item})

	deadline := time.After(300 * time.Millisecond)
	var events []ipc.Event
collect:
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
			if len(events) > 3 {
				t.Fatalf("too many events: %+v", events)
			}
		case <-time.After(80 * time.Millisecond):
			break collect
		case <-deadline:
			break collect
		}
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v, want exactly [snapshot update removed]", events)
	}
	if events[0].Event != "snapshot" || events[1].Event != "update" || events[2].Event != "removed" {
		t.Fatalf("events = %s,%s,%s", events[0].Event, events[1].Event, events[2].Event)
	}
}

func TestSubscribeSnapshotAlwaysPrecedesLaterUpdates(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	ch, unsub := m.Subscribe()
	defer unsub()
	id, err := m.Add("https://v/after-subscribe")
	if err != nil {
		t.Fatal(err)
	}
	first := <-ch
	if first.Event != "snapshot" {
		t.Fatalf("first event = %q, want atomic snapshot", first.Event)
	}
	for _, it := range first.Items {
		if it.ID == id {
			t.Fatal("snapshot must represent the instant before the later Add")
		}
	}
}

type slotCountingRunner struct {
	mu        sync.Mutex
	active    int
	maxActive int
	probes    int
	release   chan struct{}
}

func (r *slotCountingRunner) enter() {
	r.mu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
}

func (r *slotCountingRunner) leave() {
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
}

func (r *slotCountingRunner) Probe(ctx context.Context, job Job) ([]Entry, error) {
	url := job.URL
	r.enter()
	r.mu.Lock()
	r.probes++
	r.mu.Unlock()
	defer r.leave()
	select {
	case <-r.release:
		return []Entry{{URL: url, Title: url}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *slotCountingRunner) Run(ctx context.Context, job Job, onLine func(string)) (string, error) {
	r.enter()
	defer r.leave()
	return "", nil
}

func TestConcurrencyLimitCountsMetadataProbes(t *testing.T) {
	r := &slotCountingRunner{release: make(chan struct{})}
	m, _ := newTestManager(t, 2, r)
	for i := 0; i < 4; i++ {
		if _, err := m.Add("https://v/" + string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.probes == 2 && r.active == 2
	}, "exactly two probes occupy the two slots")
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	maxActive := r.maxActive
	probes := r.probes
	r.mu.Unlock()
	if maxActive != 2 || probes != 2 {
		t.Fatalf("before release: max active=%d probes=%d, want 2/2", maxActive, probes)
	}
	close(r.release)
	waitFor(t, time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.probes == 4
	}, "remaining probes eventually scheduled")
	r.mu.Lock()
	maxActive = r.maxActive
	r.mu.Unlock()
	if maxActive > 2 {
		t.Fatalf("combined probes/downloads exceeded limit: %d", maxActive)
	}
}

func TestCloseCancelsWorkWithoutCallerCancellingContext(t *testing.T) {
	dir := t.TempDir()
	r := &slotCountingRunner{release: make(chan struct{})}
	m, err := NewWithRunner(context.Background(), 1, filepath.Join(dir, "state.json"), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("https://v/blocked-probe"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.active == 1
	}, "probe entered")
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close must cancel its own manager context")
	}
}

func TestScheduleDoesNotStartAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := newFakeRunner()
	m := &Manager{
		ctx: ctx, max: 1, store: st, runner: fr,
		items: make(map[string]*item), subs: make(map[chan ipc.Event]struct{}),
		dirty: make(chan struct{}, 1),
	}
	idA, _ := m.Add("https://v/a")
	idB, _ := m.Add("https://v/b")
	waitFor(t, time.Second, func() bool { return fr.startedCount() == 1 }, "a started")

	cancel() // simulate shutdown before A finishes

	fr.releaseURL(t, "https://v/a")
	waitFor(t, time.Second, func() bool {
		it, _ := m.Get(idA)
		return it.State == ipc.StatePaused // shutdown downgrade
	}, "a downgraded to paused")

	time.Sleep(60 * time.Millisecond)
	if fr.startedCount() != 1 {
		t.Fatalf("started = %d; B must NOT start after ctx cancel", fr.startedCount())
	}
	itB, _ := m.Get(idB)
	if itB.State != ipc.StateQueued {
		t.Fatalf("b state = %s, want still queued", itB.State)
	}
}

func TestCloseWaitsForInFlightPlaylistExpansion(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	probeEntered := make(chan struct{}, 1)
	fr := newFakeRunner()
	fr.mu.Lock()
	fr.probeFn = func(url string) ([]Entry, error) {
		select {
		case probeEntered <- struct{}{}:
		default:
		}
		<-release
		return []Entry{
			{URL: "https://e/1", Title: "Ep1"},
			{URL: "https://e/2", Title: "Ep2"},
		}, nil
	}
	fr.mu.Unlock()

	mgr, err := NewWithRunner(ctx, 2, stPath, fr)
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := mgr.Add("https://playlist")
	<-probeEntered // expansion goroutine is now inside Probe

	closed := make(chan struct{})
	go func() { mgr.Close(); close(closed) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a probe was still in flight (expand goroutine untracked)")
	case <-time.After(120 * time.Millisecond):
	}

	cancel() // shutdown begins before Close returns, as both TUI and daemon do
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never returned after probe finished")
	}
	if _, ok := mgr.Get(parent); ok {
		t.Fatal("parent row should be gone after expansion")
	}
	if len(mgr.List()) != 2 {
		t.Fatalf("children = %d, want 2", len(mgr.List()))
	}
	snap, err := (&Store{path: stPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("persisted items = %d, want 2 (expansion results must be saved)", len(snap.Items))
	}
}

func TestAddRejectedDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	mgr, err := NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	mgr.Close()
	if _, err := mgr.Add("https://v/late"); err == nil {
		t.Fatal("Add must fail once the manager is closing")
	}
}

func TestValidURLRejectsControlsAndFlags(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"magnet:?xt=urn:x", false}, // not http(s): see TestValidURL
		{"https://example.com/v", true},
		{"-o/etc/passwd", false},          // flag-like, even with ://
		{"-https://example.com/v", false}, // leading dash is never a URL
		{"https://x\x1b[2J/v", false},     // escape byte
		{"https://x\t/v", false},          // tab
		{"https://x\r/v", false},          // CR
	}
	for _, c := range cases {
		if got := ValidURL(c.url); got != c.ok {
			t.Errorf("ValidURL(%q) = %v, want %v", c.url, got, c.ok)
		}
	}
}

// --- event delivery policy ---

func TestPublishRecoversWithSnapshotWhenSubscriberStalled(t *testing.T) {
	m, _ := newTestManager(t, 1, newFakeRunner())
	ch, unsub := m.Subscribe()
	defer unsub()
	m.mu.Lock()
	m.items["done"] = &item{Item: ipc.Item{ID: "done", URL: "https://v/done", State: ipc.StateCompleted}}
	m.mu.Unlock()
	// Fill the subscriber buffer completely with progress noise.
	for i := 0; i < 1024; i++ {
		it := ipc.Item{ID: "p", State: ipc.StateDownloading, Progress: float64(i)}
		m.publish(0, ipc.Event{Event: "update", Item: &it})
	}
	// A removal must recover the subscriber to an authoritative snapshot,
	// rather than being lost or evicting another structural event blindly.
	m.publish(0, ipc.Event{Event: "removed", ID: "gone"})
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Event == "snapshot" {
				for _, it := range ev.Items {
					if it.ID == "done" && it.State == ipc.StateCompleted {
						return
					}
				}
			}
		case <-deadline:
			t.Fatal("subscriber did not receive an authoritative recovery snapshot")
		}
	}
}

var _ sync.Locker = (*sync.Mutex)(nil)
