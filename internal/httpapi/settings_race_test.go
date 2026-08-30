package httpapi

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// Regression: GET readers and PUT writers of the shared config previously
// synchronized through a package-global mutex that only PUT took, so a
// concurrent GET could observe a half-updated config (data race). Both sides
// now take the per-Server settingsMu; this test fails under -race without it.
func TestSettingsGetPutConcurrentNoRace(t *testing.T) {
	d := testDeps(t)
	s := New(d)

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if g%2 == 0 {
					rec := do(t, s, http.MethodPut, "/api/v1/settings",
						map[string]any{"downloads": map[string]any{"max_concurrent": 1 + (i%4)*3}}, nil)
					if rec.Code != http.StatusOK {
						errs <- fmt.Errorf("put status %d: %s", rec.Code, rec.Body.String())
					}
				} else {
					rec := do(t, s, http.MethodGet, "/api/v1/settings", nil, nil)
					if rec.Code != http.StatusOK {
						errs <- fmt.Errorf("get status %d: %s", rec.Code, rec.Body.String())
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Two independent Server instances must not serialize each other through any
// shared settings lock (the lock is per-Server, never package-global).
func TestSettingsLockIsPerServer(t *testing.T) {
	d1 := testDeps(t)
	d2 := testDeps(t)
	s1, s2 := New(d1), New(d2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if rec := do(t, s1, http.MethodPut, "/api/v1/settings",
				map[string]any{"ui": map[string]any{"compact": i%2 == 0}}, nil); rec.Code != http.StatusOK {
				t.Errorf("s1 put status %d", rec.Code)
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if rec := do(t, s2, http.MethodPut, "/api/v1/settings",
			map[string]any{"ui": map[string]any{"theme": "dark"}}, nil); rec.Code != http.StatusOK {
			t.Fatalf("s2 put status %d (servers must not block each other)", rec.Code)
		}
	}
	<-done
}
