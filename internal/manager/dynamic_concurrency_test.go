package manager

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

type blockingRunner struct {
	mu       sync.Mutex
	release  chan struct{}
	released int
}

func (b *blockingRunner) Probe(ctx context.Context, url string) ([]Entry, error) {
	return []Entry{{URL: url, Title: "t"}}, nil
}

func (b *blockingRunner) Run(ctx context.Context, url string, onLine func(string)) (string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	b.mu.Lock()
	b.released++
	b.mu.Unlock()
	return "", nil
}

func TestSetMaxConcurrentValidates(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := NewWithRunner(ctx, 2, filepath.Join(dir, "state.json"), &blockingRunner{release: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)

	for _, n := range []int{0, -3, 101} {
		if err := mgr.SetMaxConcurrent(n); err == nil {
			t.Errorf("SetMaxConcurrent(%d) must fail", n)
		}
	}
	if err := mgr.SetMaxConcurrent(50); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
}

func TestIncreaseSchedulesWorkDecreaseKeepsJobsAlive(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rel := &blockingRunner{release: make(chan struct{})}
	mgr, err := NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), rel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(rel.release); mgr.Close() })

	id1, _ := mgr.Add("https://v/1")
	waitForState(t, mgr, id1, ipc.StateDownloading, "first job starts under cap 1")

	id2, _ := mgr.Add("https://v/2")
	time.Sleep(50 * time.Millisecond)
	if it, _ := mgr.Get(id2); it.State != ipc.StateQueued {
		t.Fatalf("second item must stay queued under cap 1, got %s", it.State)
	}

	// Raising the cap must schedule the queued work without any new Add.
	if err := mgr.SetMaxConcurrent(2); err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, id2, ipc.StateDownloading, "second job starts after raise")

	// Lowering the cap must not kill active jobs.
	if err := mgr.SetMaxConcurrent(1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	st1, _ := mgr.Get(id1)
	st2, _ := mgr.Get(id2)
	if st1.State != ipc.StateDownloading || st2.State != ipc.StateDownloading {
		t.Fatalf("reduction killed active jobs: %s / %s", st1.State, st2.State)
	}
}

func TestStatsSnapshotCountsAndSpeed(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rel := &blockingRunner{release: make(chan struct{})}
	mgr, err := NewWithRunner(ctx, 4, filepath.Join(dir, "state.json"), rel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)

	a, _ := mgr.Add("https://v/a")
	b, _ := mgr.Add("https://v/b")
	c, _ := mgr.Add("https://v/c")
	d, _ := mgr.Add("https://v/d")
	e, _ := mgr.Add("https://v/e")

	mgr.Pause(e) // queued item: user-paused before it can start

	waitFor(t, 2*time.Second, func() bool {
		st := mgr.StatsSnapshot()
		return st.Running == 4 && st.Queued == 0
	}, "four running")

	if err := mgr.Pause(a); err != nil {
		t.Fatal(err)
	}
	// Pausing a running item completes asynchronously (SIGTERM + wait).
	waitFor(t, 2*time.Second, func() bool { return mgr.StatsSnapshot().Running == 3 }, "pause settles")
	st := mgr.StatsSnapshot()
	if st.Paused < 1 {
		t.Fatalf("stats = %+v", st)
	}

	close(rel.release)
	// a stays paused, e stays user-paused: exactly b,c,d complete.
	waitFor(t, 2*time.Second, func() bool { return mgr.StatsSnapshot().Completed == 3 }, "released jobs complete")
	final := mgr.StatsSnapshot()
	if final.Completed != 3 || final.Running != 0 {
		t.Fatalf("final stats = %+v", final)
	}
	_ = b
	_ = c
	_ = d
}

func waitForState(t *testing.T, m *Manager, id string, want ipc.State, msg string) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		it, ok := m.Get(id)
		return ok && it.State == want
	}, msg)
}
