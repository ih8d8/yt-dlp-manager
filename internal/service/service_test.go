package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type fakeRunner struct{ dir string }

func (f fakeRunner) Probe(ctx context.Context, url string) ([]manager.Entry, error) {
	return []manager.Entry{{URL: url, Title: "t"}}, nil
}

func (f fakeRunner) Run(ctx context.Context, url string, onLine func(string)) (string, error) {
	target := filepath.Join(f.dir, "done.bin")
	onLine(manager.PrintLine("@g|", target))
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	return "", nil
}

func optsFor(dir string) Options {
	return Options{
		StatePath:  filepath.Join(dir, "state.json"),
		SocketPath: filepath.Join(dir, "test.sock"),
		InboxPath:  filepath.Join(dir, "inbox.jsonl"),
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}

func TestStartRefusesSecondOwner(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := Start(ctx, optsFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	o := optsFor(dir)
	o.Runner = fakeRunner{dir}
	if _, err := Start(ctx, o); err == nil {
		t.Fatal("second owner must be refused while the first is live")
	}
}

func TestStartRefusesSameStateThroughDifferentSockets(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := optsFor(dir)
	svc, err := Start(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	second := first
	second.SocketPath = filepath.Join(dir, "other.sock")
	if _, err := Start(ctx, second); !errors.Is(err, ErrOwnerAlive) {
		t.Fatalf("same state with another socket = %v, want ErrOwnerAlive", err)
	}
	if OwnsSocketFile(second.SocketPath) {
		t.Fatal("rejected owner must not create its control socket")
	}
}

func TestStartWithDefaultStateMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	legacyStore, err := manager.NewStore(manager.LegacyStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyStore.Save([]ipc.Item{{
		ID: "abc123", URL: "https://v/legacy", State: ipc.StatePaused, AddedAt: time.Now(),
	}}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := Start(ctx, Options{
		SocketPath: filepath.Join(dir, "migration.sock"),
		InboxPath:  filepath.Join(dir, "inbox.jsonl"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	if item, ok := svc.Manager().Get("abc123"); !ok || item.URL != "https://v/legacy" {
		t.Fatalf("default-path migration item = %+v ok=%v", item, ok)
	}
}

func TestFailedListenerClosesManager(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := Start(ctx, optsFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	// Occupy a second socket path with a regular file so Listen fails.
	blocked := optsFor(dir)
	sock2 := filepath.Join(dir, "blocked.sock")
	if err := os.WriteFile(sock2, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.SocketPath = sock2
	blocked.Runner = fakeRunner{dir}

	if _, err := Start(ctx, blocked); err == nil {
		t.Fatal("start must fail when the listener cannot bind")
	}
}

func TestCloseIsIdempotentAndWaitsForChildren(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	o := optsFor(dir)
	o.Runner = fakeRunner{dir}
	svc, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}

	id, err := svc.Manager().Add("https://v/x")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		it, _ := svc.Manager().Get(id)
		return it.State == ipc.StateCompleted
	}, "download completed")

	cancel() // parent ctx cancelled: service must close itself
	select {
	case <-svc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not close the service")
	}

	// Idempotent: repeated Close must not panic or block.
	done := make(chan struct{})
	go func() { svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second Close blocked")
	}

	if OwnsSocketFile(o.SocketPath) {
		t.Error("socket file must be removed after close")
	}
}

func TestDrainInboxAndCountInterrupted(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	st, err := manager.NewStore(stPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]ipc.Item{
		{ID: "aaa111", URL: "https://v/interrupted", State: ipc.StateDownloading},
	}, nil); err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(dir, "inbox.jsonl")
	if err := os.WriteFile(inboxPath, []byte(`{"url":"https://v/offline","added_at":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := optsFor(dir)
	o.StatePath = stPath
	o.InboxPath = inboxPath
	o.Runner = fakeRunner{dir}
	svc, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	if svc.Interrupted != 1 {
		t.Fatalf("interrupted = %d, want 1", svc.Interrupted)
	}
	if svc.InboxDrained != 1 {
		t.Fatalf("inbox drained = %d, want 1", svc.InboxDrained)
	}
	// The interrupted row is reported, not restarted: startup must not begin
	// downloading on its own. A URL drained from the inbox is a fresh add and
	// still runs normally.
	time.Sleep(300 * time.Millisecond)
	if it, ok := svc.Manager().Get("aaa111"); !ok || it.State != ipc.StatePaused {
		t.Fatalf("interrupted item = %+v ok=%v, want it left paused", it, ok)
	}
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Error("inbox must be consumed after drain")
	}
}

func TestUserPausedNeverAutoResumes(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	st, err := manager.NewStore(stPath)
	if err != nil {
		t.Fatal(err)
	}
	userPausedAt := time.Now()
	if err := st.Save([]ipc.Item{
		{ID: "bbb222", URL: "https://v/user-paused", State: ipc.StatePaused, UserPaused: true, AddedAt: userPausedAt},
	}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := optsFor(dir)
	o.StatePath = stPath
	o.Runner = fakeRunner{dir}
	svc, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	if svc.Interrupted != 0 {
		t.Fatalf("a user pause is not an interruption; interrupted = %d", svc.Interrupted)
	}
	it, ok := svc.Manager().Get("bbb222")
	if !ok || it.State != ipc.StatePaused || !it.UserPaused {
		t.Fatalf("item = %+v ok=%v, want paused user_paused", it, ok)
	}
}

func writeLegacyState(t *testing.T, dir string, items []ipc.Item, order []string) string {
	t.Helper()
	legacyDir := filepath.Join(dir, "legacy-state-root", "yt-dlp-tui")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(legacyDir, "state.json")
	data, err := json.Marshal(map[string]any{
		"version":  1,
		"saved_at": time.Now(),
		"items":    items,
		"order":    order,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLegacyStateMigrationCopiesValidState(t *testing.T) {
	dir := t.TempDir()
	newStore, err := manager.NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := writeLegacyState(t, dir, []ipc.Item{
		{ID: "ccc333", URL: "https://v/old", State: ipc.StateCompleted, Progress: 100},
	}, nil)

	if !manager.MigrateLegacyState(newStore, legacy) {
		t.Fatal("migration should copy valid legacy state")
	}
	// Marked by presence of new state; legacy untouched.
	if !newStore.Exists() {
		t.Error("new state must exist after migration")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Error("legacy state must not be deleted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := manager.NewWithRunner(ctx, 1, newStore.Path(), fakeRunner{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	it, ok := mgr.Get("ccc333")
	if !ok || it.URL != "https://v/old" {
		t.Fatalf("migrated item = %+v ok=%v", it, ok)
	}
}

func TestLegacyMigrationSkipsCorruptAndExisting(t *testing.T) {
	dir := t.TempDir()

	// Corrupt legacy input must be ignored, not copied or destroyed.
	corruptDir := filepath.Join(dir, "c1")
	st1, err := manager.NewStore(filepath.Join(corruptDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := writeLegacyState(t, dir, nil, nil)
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if manager.MigrateLegacyState(st1, corrupt) {
		t.Error("corrupt legacy state must not migrate")
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Error("corrupt legacy file must remain on disk")
	}
	if st1.Exists() {
		t.Error("no new state may appear from invalid input")
	}

	// Existing new state wins: never overwritten by legacy.
	existingDir := filepath.Join(dir, "c2")
	st2, err := manager.NewStore(filepath.Join(existingDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Save([]ipc.Item{
		{ID: "ddd444", URL: "https://v/current", State: ipc.StateQueued},
	}, []string{"ddd444"}); err != nil {
		t.Fatal(err)
	}
	legacy := writeLegacyState(t, dir, []ipc.Item{
		{ID: "eee555", URL: "https://v/older", State: ipc.StateQueued},
	}, nil)
	if manager.MigrateLegacyState(st2, legacy) {
		t.Error("migration must not overwrite existing new state")
	}
}

func TestServiceServesIPCClients(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := optsFor(dir)
	o.Runner = fakeRunner{dir}
	svc, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	c, err := ipc.Dial(o.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := c.Call(ipc.Request{Cmd: "add", URL: "https://v/cli"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID == "" {
		t.Fatalf("resp = %+v", resp)
	}
	waitFor(t, time.Second, func() bool {
		it, _ := svc.Manager().Get(resp.ID)
		return it.State == ipc.StateCompleted
	}, "download completed via IPC client")

	// The listener must refuse a second bind while alive.
	if ln, err := net.Listen("unix", o.SocketPath); err == nil {
		ln.Close()
		t.Error("live socket path must not be rebindable")
	}
}
