package manager

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// "Start now" jumps the queue on purpose, but it must not be an unlimited
// process spawner: the HTTP API accepts 500 ids in one batch, and every forced
// item used to start immediately regardless of max_concurrent.
func TestForcedStartsStopAtTheHardCeiling(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)

	const extra = 6
	ids := make([]string, 0, MaxForcedExtra+extra)
	for i := 0; i < MaxForcedExtra+extra; i++ {
		id, err := m.Add(fmt.Sprintf("https://v/%d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		// An item the scheduler already started is not queued any more, so it
		// refuses a force-start; that item is running either way.
		if err := m.StartNow(id); err != nil && err != ErrInvalidState {
			t.Fatalf("StartNow(%s): %v", id, err)
		}
	}

	want := 1 + MaxForcedExtra // max + the forced headroom
	waitFor(t, 2*time.Second, func() bool {
		running, _ := m.Stats()
		return running == want
	}, "forced starts reach the ceiling")

	// And stay there: the remaining forced items wait for a slot.
	time.Sleep(50 * time.Millisecond)
	if running, _ := m.Stats(); running != want {
		t.Fatalf("running = %d, want %d (max 1 + %d forced headroom)", running, want, MaxForcedExtra)
	}
}

// A queue that grows without limit eventually produces a snapshot too large to
// persist, at which point every save fails and the whole history is lost.
// Refusing one addition is the better failure.
func TestAddRefusesWhenTheQueueIsFull(t *testing.T) {
	fr := newFakeRunner()
	m, _ := newTestManager(t, 1, fr)

	m.mu.Lock()
	for i := 0; i < MaxQueueItems; i++ {
		id := fmt.Sprintf("full-%d", i)
		// Completed rows keep the scheduler from trying to run any of this.
		m.items[id] = &item{probed: true, Item: ipc.Item{
			ID: id, URL: "https://filler/" + id, State: ipc.StateCompleted,
		}}
	}
	m.mu.Unlock()

	if _, err := m.Add("https://v/one-too-many"); err != ErrQueueFull {
		t.Fatalf("Add at the ceiling = %v, want ErrQueueFull", err)
	}
}

// yt-dlp's flat-playlist output is capped at 64 MiB, which is not a bound on
// entries: at ~200 bytes a line that is still hundreds of thousands of items.
func TestPlaylistProbeOutputIsCappedByEntryCount(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxPlaylistEntries+50; i++ {
		fmt.Fprintf(&b, `@e|{"url":"https://v/%d","title":"t%d","thumbnail":""}`+"\n", i, i)
	}
	entries := parseProbeOutput(b.String(), "https://playlist")
	if len(entries) != MaxPlaylistEntries {
		t.Fatalf("parsed %d entries, want the %d cap", len(entries), MaxPlaylistEntries)
	}
}

// Children bypass Add, so the queue ceiling has to be enforced during playlist
// expansion too.
func TestPlaylistExpansionStopsAtTheQueueCeiling(t *testing.T) {
	fr := newFakeRunner()
	fr.probeFn = func(url string) ([]Entry, error) {
		entries := make([]Entry, 0, 20)
		for i := 0; i < 20; i++ {
			entries = append(entries, Entry{URL: fmt.Sprintf("https://child/%d", i), Title: "child"})
		}
		return entries, nil
	}
	m, _ := newTestManager(t, 1, fr)

	// Two slots left under the ceiling, one of which the parent placeholder
	// occupies until it is replaced.
	m.mu.Lock()
	for i := 0; i < MaxQueueItems-2; i++ {
		id := fmt.Sprintf("full-%d", i)
		m.items[id] = &item{probed: true, Item: ipc.Item{
			ID: id, URL: "https://filler/" + id, State: ipc.StateCompleted,
		}}
	}
	m.mu.Unlock()

	parent, err := m.Add("https://playlist/big")
	if err != nil {
		t.Fatal(err)
	}
	// Expansion replaces the parent placeholder with its children.
	waitFor(t, 2*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, parentLive := m.items[parent]
		return !parentLive
	}, "playlist expanded")

	m.mu.Lock()
	total := len(m.items)
	m.mu.Unlock()
	if total > MaxQueueItems {
		t.Fatalf("queue grew to %d items, past the %d ceiling", total, MaxQueueItems)
	}
}
