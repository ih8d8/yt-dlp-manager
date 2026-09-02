package ipcserver

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

// dlFake is a Runner whose downloads instantly "complete" by writing a file
// captured from the @g line, mimicking yt-dlp's merged-output report.
type dlFake struct{ dir string }

func (f dlFake) Probe(ctx context.Context, job manager.Job) ([]manager.Entry, error) {
	url := job.URL
	return []manager.Entry{{URL: url, Title: "t"}}, nil
}

func (f dlFake) Run(ctx context.Context, job manager.Job, onLine func(string)) (string, error) {
	target := filepath.Join(f.dir, "done.bin")
	onLine(manager.PrintLine("@g|", target))
	if err := os.WriteFile(target, []byte("video"), 0o600); err != nil {
		return "", err
	}
	return "", nil
}

func newTestMgr(t *testing.T, dir string) *manager.Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr, err := manager.NewWithRunner(ctx, 1, filepath.Join(dir, "state.json"), dlFake{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return mgr
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

// TestDispatchRemoveKeepsDownloadedFile pins that the control socket cannot be
// used to destroy media: "delete" is gone, and "remove" is a list operation.
func TestDispatchRemoveKeepsDownloadedFile(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(newTestMgr(t, dir))

	resp := srv.dispatch(ipc.Request{Cmd: "add", URL: "https://v/1"})
	if !resp.OK || resp.ID == "" {
		t.Fatalf("add resp = %+v", resp)
	}
	waitFor(t, time.Second, func() bool {
		it, ok := srv.mgr.Get(resp.ID)
		return ok && it.State == ipc.StateCompleted
	}, "download completed")

	if del := srv.dispatch(ipc.Request{Cmd: "delete", ID: resp.ID}); del.OK {
		t.Fatal("delete must no longer be a supported command")
	}

	rm := srv.dispatch(ipc.Request{Cmd: "remove", ID: resp.ID})
	if !rm.OK {
		t.Fatalf("remove resp = %+v", rm)
	}
	if _, err := os.Stat(filepath.Join(dir, "done.bin")); err != nil {
		t.Errorf("finished file must survive a remove: %v", err)
	}
	if _, ok := srv.mgr.Get(resp.ID); ok {
		t.Error("row must be gone")
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	srv := NewServer(newTestMgr(t, t.TempDir()))
	resp := srv.dispatch(ipc.Request{Cmd: "self_destruct"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("resp = %+v, want failure with message", resp)
	}
}

func TestListenRefusesNonSocketAndSymlink(t *testing.T) {
	dir := t.TempDir()

	regPath := filepath.Join(dir, "regular.sock")
	if err := os.WriteFile(regPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(regPath); err == nil {
		ln.Close()
		t.Fatal("Listen must refuse to clobber a regular file")
	} else if _, statErr := os.Stat(regPath); statErr != nil {
		t.Error("the regular file must not be removed")
	}

	linkPath := filepath.Join(dir, "link.sock")
	target := filepath.Join(dir, "victim")
	os.WriteFile(target, []byte("v"), 0o600) //nolint:errcheck
	os.Symlink(target, linkPath)             //nolint:errcheck
	if ln, err := Listen(linkPath); err == nil {
		ln.Close()
		t.Fatal("Listen must refuse a symlink at the socket path")
	} else if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Error("symlink must not be removed")
	}
}

func TestListenReplacesOwnStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // leaves the socket file behind, like a crashed process

	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("own stale socket must be replaceable: %v", err)
	}
	defer func() { ln2.Close(); os.Remove(path) }()
}

func TestListenRefusesLivePeerSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.sock")

	// A real live listener: accept and hold connections.
	ln1, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln1.Accept()
			if err != nil {
				close(done)
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { <-done })

	if _, err := Listen(path); !errors.Is(err, ErrOwnerAlive) {
		t.Fatalf("Listen on a live socket = %v, want ErrOwnerAlive", err)
	}
	// The live owner's socket file must be untouched and still dialable.
	if _, derr := net.DialTimeout("unix", path, time.Second); derr != nil {
		t.Fatalf("live peer socket was disturbed: %v", derr)
	}
}

func TestListenRebindsAfterStaleProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.sock")

	// Create a stale socket: bind, do NOT accept, close without unlink.
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.(*net.UnixListener).SetUnlinkOnClose(false)

	// A pending connection in the backlog makes a plain dial "succeed";
	// Listen must not treat backlog as an alive owner... but with no
	// accept loop, the handshake completes at connect time. Close raw
	// first so we exercise the true stale case.
	raw.Close()

	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("stale socket must be replaceable: %v", err)
	}
	ln2.Close()
}

func TestListenSerializesConcurrentStaleSocketReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.sock")
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.(*net.UnixListener).SetUnlinkOnClose(false)
	raw.Close()

	const starters = 32
	start := make(chan struct{})
	results := make(chan struct {
		ln  net.Listener
		err error
	}, starters)
	var wg sync.WaitGroup
	for range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ln, err := Listen(path)
			results <- struct {
				ln  net.Listener
				err error
			}{ln, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner net.Listener
	for result := range results {
		if result.err == nil {
			if winner != nil {
				result.ln.Close()
				winner.Close()
				t.Fatal("more than one concurrent listener acquired ownership")
			}
			winner = result.ln
			continue
		}
		if !errors.Is(result.err, ErrOwnerAlive) {
			t.Fatalf("losing Listen returned %v, want ErrOwnerAlive", result.err)
		}
	}
	if winner == nil {
		t.Fatal("no concurrent listener acquired ownership")
	}
	winner.Close()
}

func TestCloseWaitsForAcceptedHandlers(t *testing.T) {
	srv := NewServer(newTestMgr(t, t.TempDir()))
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	if !srv.track(serverConn) {
		t.Fatal("server unexpectedly closed")
	}
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		defer srv.untrack(serverConn)
		srv.handle(serverConn)
	}()

	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join an accepted handler")
	}
	select {
	case <-handled:
	default:
		t.Fatal("Close returned before the accepted handler exited")
	}
}
