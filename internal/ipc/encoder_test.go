package ipc

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// failAfter accepts n writes and then fails every later one, which is what
// makes a shared json.Encoder write its sticky error field while other
// goroutines are reading it.
type failAfter struct {
	mu    sync.Mutex
	n     int
	lines []string
}

func (w *failAfter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n--
	if w.n < 0 {
		return 0, errors.New("writer closed")
	}
	w.lines = append(w.lines, string(p))
	return len(p), nil
}

// The IPC server writes command responses from its request loop while a
// subscription goroutine writes events over the same connection. Locking the
// underlying writer alone left json.Encoder's own state shared; run under
// -race, this is the check that the whole Encode is serialized.
func TestEncoderIsSafeForConcurrentUse(t *testing.T) {
	w := &failAfter{n: 40}
	enc := NewEncoder(w)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = enc.Encode(Response{OK: true, ID: "x"})
			}
		}(g)
	}
	wg.Wait()

	// Every accepted write must be exactly one complete message: interleaved
	// fragments would be unparseable at the other end.
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") ||
			!strings.HasPrefix(line, "{") {
			t.Fatalf("interleaved write: %q", line)
		}
	}
}
