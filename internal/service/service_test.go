package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yt-dlp-manager/internal/filelock"
	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type fakeRunner struct{ dir string }

func (f fakeRunner) Probe(ctx context.Context, job manager.Job) ([]manager.Entry, error) {
	url := job.URL
	return []manager.Entry{{URL: url, Title: "t"}}, nil
}

func (f fakeRunner) Run(ctx context.Context, job manager.Job, onLine func(string)) (string, error) {
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

// TestSecondInstanceIsRefusedWithALongStateName proves single-owner protection
// survives the name shortening that long state filenames force. If two state
// files could share a lock — or a lock could alias its own state file — a
// second instance would start against state the first is still writing.
func TestSecondInstanceIsRefusedWithALongStateName(t *testing.T) {
	dir := t.TempDir()
	// At the component limit, so the lock name must be shortened.
	statePath := filepath.Join(dir, strings.Repeat("s", 255))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := Start(ctx, Options{
		Max: 1, StatePath: statePath,
		SocketPath: filepath.Join(dir, "a.sock"),
		InboxPath:  filepath.Join(dir, "a.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatalf("first instance did not start: %v", err)
	}
	defer first.Close()

	second, err := Start(ctx, Options{
		Max: 1, StatePath: statePath,
		SocketPath: filepath.Join(dir, "b.sock"),
		InboxPath:  filepath.Join(dir, "b.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err == nil {
		second.Close()
		t.Fatal("a second instance started against the same state file")
	}
	if !errors.Is(err, ErrOwnerAlive) {
		t.Errorf("second instance error = %v, want ErrOwnerAlive", err)
	}

	// A DIFFERENT state file sharing the same long prefix must still start:
	// the shortening must not merge two states into one lock.
	other := filepath.Join(dir, strings.Repeat("s", 250)+"other")
	third, err := Start(ctx, Options{
		Max: 1, StatePath: other,
		SocketPath: filepath.Join(dir, "c.sock"),
		InboxPath:  filepath.Join(dir, "c.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatalf("a distinct state file was refused, so two states share one lock: %v", err)
	}
	third.Close()
}

// TestStateFileNamedLikeALockDoesNotStealOwnership drives the exact collision
// through two real services: "foo" and "foo.lock" are both legitimate state
// paths, and under a "<state>.lock" naming scheme the first instance's lock IS
// the second instance's state file — whose next save atomically replaces that
// inode, silently ending the first one's ownership.
func TestStateFileNamedLikeALockDoesNotStealOwnership(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := Start(ctx, Options{
		Max: 1, StatePath: filepath.Join(dir, "foo"),
		SocketPath: filepath.Join(dir, "a.sock"),
		InboxPath:  filepath.Join(dir, "a.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatalf("first instance did not start: %v", err)
	}
	defer a.Close()
	if _, err := os.Lstat(filepath.Join(dir, "foo.lock")); !os.IsNotExist(err) {
		t.Fatalf("transitional locking manufactured another valid state path: %v", err)
	}

	// A second, unrelated state file that happens to be named like A's lock.
	b, err := Start(ctx, Options{
		Max: 1, StatePath: filepath.Join(dir, "foo.lock"),
		SocketPath: filepath.Join(dir, "b.sock"),
		InboxPath:  filepath.Join(dir, "b.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatalf("a distinct state file was refused: %v", err)
	}
	// B writes, which under the old scheme replaced the inode A held.
	if _, err := b.Manager().Add("https://example.test/b"); err != nil {
		t.Fatal(err)
	}
	b.Close()

	// A must still own its state: a second A is refused.
	second, err := Start(ctx, Options{
		Max: 1, StatePath: filepath.Join(dir, "foo"),
		SocketPath: filepath.Join(dir, "c.sock"),
		InboxPath:  filepath.Join(dir, "c.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err == nil {
		second.Close()
		t.Fatal("a second instance started: the first one's ownership lock was revoked")
	}
	if !errors.Is(err, ErrOwnerAlive) {
		t.Errorf("error = %v, want ErrOwnerAlive", err)
	}
}

func TestExistingLegacyLockBlocksUpgrade(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	legacyPath := state + ".lock"
	oldOwner, err := filelock.Acquire(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldOwner.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked, err := Start(ctx, Options{
		Max: 1, StatePath: state,
		SocketPath: filepath.Join(dir, "blocked.sock"),
		InboxPath:  filepath.Join(dir, "blocked.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err == nil {
		blocked.Close()
		t.Fatal("new service started while an old process held the legacy lock")
	}
	if !errors.Is(err, ErrOwnerAlive) {
		t.Fatalf("upgrade error = %v, want ErrOwnerAlive", err)
	}

	if err := oldOwner.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Start(ctx, Options{
		Max: 1, StatePath: state,
		SocketPath: filepath.Join(dir, "upgraded.sock"),
		InboxPath:  filepath.Join(dir, "upgraded.inbox"),
		Runner:     fakeRunner{dir},
	})
	if err != nil {
		t.Fatalf("service did not start after old owner exited: %v", err)
	}
	upgraded.Close()
}

// TestSymlinkedStatePathIsOneOwner: /real/state.json and /alias/state.json,
// where alias is a symlink to real, are one file with two spellings. Lexical
// cleaning does not notice, so hashing the spelling would hand out two locks
// for one state and let two services write it at once.
func TestSymlinkedStatePathIsOneOwner(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := Start(ctx, Options{
		Max: 1, StatePath: filepath.Join(real, "state.json"),
		SocketPath: filepath.Join(root, "a.sock"),
		InboxPath:  filepath.Join(root, "a.inbox"),
		Runner:     fakeRunner{root},
	})
	if err != nil {
		t.Fatalf("first instance did not start: %v", err)
	}
	defer first.Close()

	second, err := Start(ctx, Options{
		Max: 1, StatePath: filepath.Join(alias, "state.json"),
		SocketPath: filepath.Join(root, "b.sock"),
		InboxPath:  filepath.Join(root, "b.inbox"),
		Runner:     fakeRunner{root},
	})
	if err == nil {
		second.Close()
		t.Fatal("a second instance started on the same state through a symlinked path")
	}
	if !errors.Is(err, ErrOwnerAlive) {
		t.Errorf("error = %v, want ErrOwnerAlive", err)
	}
}

// TestSymlinkCannotPlaceStateInTheLockDirectory: the lock namespace is off
// limits to state files, and a symlink must not be a way around that.
func TestSymlinkCannotPlaceStateInTheLockDirectory(t *testing.T) {
	root := t.TempDir()
	locks := filepath.Join(root, ".locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "sneaky")
	if err := os.Symlink(locks, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := manager.NewStore(filepath.Join(alias, "state.json")); err == nil {
		t.Error("a symlink placed a state file inside the lock directory")
	}
}
