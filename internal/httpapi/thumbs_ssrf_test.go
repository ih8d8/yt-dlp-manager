package httpapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

func TestIsPublicIPBlocksSpecialUse(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254",               // cloud metadata
		"100.64.0.1", "100.100.100.100", // CGNAT / Tailscale
		"240.0.0.1", "255.255.255.255", "0.0.0.0",
		"198.18.0.1", "192.0.2.1", "203.0.113.9",
		"::1", "fc00::1", "fe80::1", "::",
		"::ffff:10.0.0.1", "::ffff:169.254.169.254", // v4-mapped
		"2001:db8::1", "64:ff9b::a00:1",
	}
	for _, a := range blocked {
		if isPublicIP(net.ParseIP(a)) {
			t.Errorf("%s should be rejected", a)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700::1111"}
	for _, a := range allowed {
		if !isPublicIP(net.ParseIP(a)) {
			t.Errorf("%s should be allowed", a)
		}
	}
}

// Concurrent cache misses for one download must produce exactly one remote
// fetch, not one per in-flight request.
func TestThumbnailFetchIsCoalesced(t *testing.T) {
	var hits int32
	var once sync.Once
	entered := make(chan struct{})
	gate := make(chan struct{})
	orig := fetchThumb
	t.Cleanup(func() { fetchThumb = orig })
	fetchThumb = func(ctx context.Context, url string) ([]byte, string, error) {
		atomic.AddInt32(&hits, 1)
		// Closed, never sent to: if the coalescing is broken every caller
		// runs this, and a channel send would deadlock them all instead of
		// letting the test report the real hit count.
		once.Do(func() { close(entered) })
		<-gate
		return []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)), "image/png", nil
	}

	s := &Server{}
	const callers = 12
	var wg sync.WaitGroup

	// The first caller registers the in-flight entry, then blocks in fetchThumb.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = s.fetchThumbnailOnce(context.Background(), "abcd1234", "https://example.com/t.png")
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(gate)
		wg.Wait()
		t.Fatal("first fetch never started")
	}

	// Everyone arriving now must find that entry rather than start their own.
	// No sleep is involved: the map entry is registered before fetchThumb runs.
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.fetchThumbnailOnce(context.Background(), "abcd1234", "https://example.com/t.png")
		}()
	}
	// Wait until all of them are genuinely parked on the in-flight fetch.
	// The first fetch cannot finish while the gate is closed, so the entry
	// cannot be deleted out from under a late arrival.
	deadline := time.Now().Add(10 * time.Second)
	for s.thumbFlightWaiters.Load() < int64(callers-1) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	parked := s.thumbFlightWaiters.Load()

	close(gate)
	wg.Wait()

	if parked < int64(callers-1) {
		t.Fatalf("precondition: only %d of %d callers parked on the in-flight fetch", parked, callers-1)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("origin was hit %d times for %d concurrent callers, want 1", n, callers)
	}
}

// The cache path is a pure function of (id, thumbnail URL), so an item that
// gets re-probed to a DIFFERENT thumbnail cannot be served the image the
// previous URL produced. Keying on the id alone made that stale image
// permanent — reachable now that a retry re-probes.
func TestThumbnailCacheIsKeyedByItsSourceURL(t *testing.T) {
	s := &Server{}
	s.deps.StateDir = t.TempDir()

	const id = "abcd1234"
	_, first, ok := s.thumbCachePath(id, "https://img.example/old.jpg")
	if !ok {
		t.Fatal("no cache path for a valid id and url")
	}
	_, second, ok := s.thumbCachePath(id, "https://img.example/new.jpg")
	if !ok {
		t.Fatal("no cache path after re-probe")
	}
	if first == second {
		t.Fatalf("two different thumbnail urls share one cache file: %s", first)
	}
	// Same URL must still be one stable path, or nothing would ever be cached.
	_, again, _ := s.thumbCachePath(id, "https://img.example/old.jpg")
	if again != first {
		t.Fatalf("cache path is not stable for one url: %s vs %s", first, again)
	}
	// Both live under the item's own directory, which is what makes eviction
	// proportional to the item rather than to the whole cache.
	itemDir, ok := s.thumbItemDir(id)
	if !ok {
		t.Fatal("no item directory for a valid id")
	}
	for _, p := range []string{first, second} {
		if filepath.Dir(p) != itemDir {
			t.Errorf("%s is not inside the item directory %s", p, itemDir)
		}
	}
	// An item with no probed thumbnail has no cache entry at all.
	if _, _, ok := s.thumbCachePath(id, ""); ok {
		t.Error("an item with no thumbnail url was given a cache path")
	}
	// An id that could walk out of the cache root is refused outright.
	for _, bad := range []string{"../../etc", "a/b", "", "NOTHEX", strings.Repeat("a", 65)} {
		if _, ok := s.thumbItemDir(bad); ok {
			t.Errorf("thumbItemDir(%q) was accepted", bad)
		}
	}
}

// Eviction has to take every image an item ever cached, including one left
// under a thumbnail URL it no longer has — and must not touch anything else.
func TestEvictThumbnailDropsEveryEntryForTheItem(t *testing.T) {
	s := &Server{}
	s.deps.StateDir = t.TempDir()
	const id = "abcd1234"

	var written []string
	for _, u := range []string{"https://img.example/old.jpg", "https://img.example/new.jpg"} {
		dir, p, _ := s.thumbCachePath(id, u)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("img"), 0o600); err != nil {
			t.Fatal(err)
		}
		written = append(written, p)
	}
	// Another item's entry, and a file this cache did not write, must survive.
	otherDir, other, _ := s.thumbCachePath("beef5678", "https://img.example/other.jpg")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("img"), 0o600); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(s.deps.StateDir, "thumbs", "please-keep.txt")
	if err := os.WriteFile(stranger, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.EvictThumbnail(id)

	for _, p := range written {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived eviction", p)
		}
	}
	for _, p := range []string{other, stranger} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive another item's eviction: %v", filepath.Base(p), err)
		}
	}
	// Evicting again, and evicting an item that never cached anything, are
	// both no-ops: every connected event-stream client runs this for the same
	// removal, so it has to be idempotent.
	s.EvictThumbnail(id)
	s.EvictThumbnail("00000000")
}

// Upgrade path. Releases before the per-item layout wrote "<id>.img" flat
// under thumbs/. Those files can never be served again, so the startup sweep
// has to reclaim them — otherwise a large library's cache sits on disk for the
// life of the install with nothing that will ever delete it.
func TestSweepReclaimsLegacyAndStaleEntries(t *testing.T) {
	s, _ := newClearTestServer(t)
	s.deps.StateDir = t.TempDir()
	mgr := s.deps.Manager

	const liveURL = "https://example.com/done"
	live, _ := mgr.Add(liveURL)
	waitFor(t, 5*time.Second, func() bool {
		it, _ := mgr.Get(live)
		return it.State == ipc.StateCompleted && it.ThumbURL != ""
	}, "download to finish and record its thumbnail")
	thumbURL := thumbFor(liveURL)

	root := filepath.Join(s.deps.StateDir, "thumbs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(p string) string {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("img"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Pre-v2 entries, for a live item and a dead one. Both are unservable.
	legacyLive := write(filepath.Join(root, live+".img"))
	legacyDead := write(filepath.Join(root, "00000000.img"))
	// A dead item's directory, and a live item's entry from a URL it no
	// longer has (the re-probe case).
	deadDir := filepath.Join(root, "11111111")
	deadEntry := write(filepath.Join(deadDir, thumbSourceTag("https://img.example/gone.jpg")+".img"))
	staleEntry := write(filepath.Join(root, live, thumbSourceTag("https://img.example/old.jpg")+".img"))
	// The one entry the live item is entitled to, and a file we never wrote.
	_, current, _ := s.thumbCachePath(live, thumbURL)
	write(current)
	stranger := write(filepath.Join(root, "README.txt"))

	s.sweepThumbnails()

	for _, p := range []string{legacyLive, legacyDead, deadEntry, staleEntry} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", strings.TrimPrefix(p, root))
		}
	}
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Error("a dead item's directory survived the sweep")
	}
	for _, p := range []string{current, stranger} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive the sweep: %v", strings.TrimPrefix(p, root), err)
		}
	}
}
