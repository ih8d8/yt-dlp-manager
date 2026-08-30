package httpapi

import (
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
