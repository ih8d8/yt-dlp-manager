// Package ipcserver serves the hardened Unix-socket control protocol that the
// TUI and CLI use to observe and drive a manager they do not own.
package ipcserver

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"yt-dlp-manager/internal/filelock"
	"yt-dlp-manager/internal/inbox"
	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

// ErrOwnerAlive reports that another live process already owns the socket.
var ErrOwnerAlive = errors.New("socket is owned by a live process")

// Listen creates the control socket with the same hardening rules the daemon
// always used: never follow symlinks or clobber non-sockets, refuse paths
// owned by another user, chmod the socket 0600 so only the same user can
// talk to the manager, and — critically — never steal a socket that still
// has a live owner. A stale leftover (no listener behind it) is replaced;
// a dialable one makes us fail instead of racing a second manager into
// existence on the same state file.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	owner, err := filelock.Acquire(path + ".lock")
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrOwnerAlive, path)
		}
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = owner.Close()
		}
	}()
	if fi, err := os.Lstat(path); err == nil {
		// Never follow or unlink symlinks, regular files, or another user's
		// socket — those indicate either a mistake or interference.
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a stale yt-dlp-manager socket", path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Getuid()) {
			return nil, fmt.Errorf("%s is owned by another user; refusing to replace it", path)
		}
		// Same-user socket: only replace it when nothing is listening.
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			c.Close()
			return nil, fmt.Errorf("%w: %s", ErrOwnerAlive, path)
		} else if !isConnRefused(derr) {
			return nil, fmt.Errorf("probe %s: %w", path, derr)
		}
		_ = os.Remove(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		ln, err := net.Listen("unix", path)
		if err == nil {
			if cerr := os.Chmod(path, 0o600); cerr != nil {
				ln.Close()
				return nil, cerr
			}
			failed = false
			return &lockedListener{Listener: ln, owner: owner}, nil
		}
		if errors.Is(err, syscall.EADDRINUSE) && attempt == 0 {
			// Someone rebound between our probe and bind: re-check liveness
			// once rather than deleting a possibly-live socket.
			if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
				c.Close()
				return nil, fmt.Errorf("%w: %s", ErrOwnerAlive, path)
			} else if isConnRefused(derr) {
				_ = os.Remove(path)
				continue
			}
		}
		return nil, err
	}
}

// lockedListener keeps the ownership lock until the socket has been closed and
// unlinked. Releasing it earlier would reopen the stale-socket race during
// shutdown.
type lockedListener struct {
	net.Listener
	owner *filelock.Lock
	once  sync.Once
	err   error
}

func (l *lockedListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
		if err := l.owner.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}

// isConnRefused reports whether err is the expected "nothing behind this
// socket file" answer.
func isConnRefused(err error) bool {
	var sysErr *os.SyscallError
	return errors.As(err, &sysErr) && errors.Is(sysErr.Err, syscall.ECONNREFUSED)
}

// Server dispatches IPC requests against one manager. It tracks live client
// connections so Close can quiesce every handler goroutine.
type Server struct {
	mgr *manager.Manager

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
	close  sync.Once
	done   chan struct{}
}

func NewServer(mgr *manager.Manager) *Server {
	return &Server{mgr: mgr, conns: make(map[net.Conn]struct{}), done: make(chan struct{})}
}

// Serve accepts connections until the listener closes. It returns
// net.ErrClosed once Close has been called.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return net.ErrClosed
			}
			time.Sleep(5 * time.Millisecond) // avoid hot-spinning on transient fd errors
			continue
		}
		if !s.track(c) {
			c.Close()
			continue
		}
		go func() {
			defer s.untrack(c)
			s.handle(c)
		}()
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

// Close stops accepting and terminates every live client connection. Handler
// goroutines exit once their conn writes fail, and each removes itself before
// handle returns.
func (s *Server) Close() {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		conns := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.conns = make(map[net.Conn]struct{})
		s.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		s.wg.Wait()
		close(s.done)
	})
	<-s.done
}

// DrainInbox consumes offline add requests parked by the CLI while no manager
// was running and enqueues them now.
func DrainInbox(mgr *manager.Manager, path string) int {
	if path == "" {
		path = inbox.DefaultPath()
	}
	urls, err := inbox.Drain(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yt-dlp-manager: inbox: %v\n", err)
	}
	n := 0
	for _, u := range urls {
		if _, err := mgr.Add(u); err == nil {
			n++
		}
	}
	return n
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	enc := ipc.NewEncoder(c)

	var (
		stopSub func()
		done    = make(chan struct{})
	)
	defer func() {
		close(done)
		if stopSub != nil {
			stopSub()
		}
	}()

	for {
		var req ipc.Request
		if err := ipc.ReadLineJSON(br, &req); err != nil {
			return
		}
		if req.Cmd == "subscribe" {
			if stopSub != nil {
				// One event stream per connection; a second subscribe would
				// leak the first subscription and duplicate events.
				_ = enc.Encode(ipc.Response{OK: false, Error: "already subscribed"})
				continue
			}
			ch, cancel := s.mgr.Subscribe()
			stopSub = cancel
			// Subscribe atomically queues the snapshot before registering any
			// later updates, so an update can never be replayed after a newer
			// separately-read snapshot.
			snapshot := <-ch
			if err := enc.Encode(snapshot); err != nil {
				return
			}
			go func() {
				for {
					select {
					case ev, ok := <-ch:
						if !ok {
							return
						}
						if err := enc.Encode(ev); err != nil {
							return
						}
					case <-done:
						return
					}
				}
			}()
			continue
		}
		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(req ipc.Request) ipc.Response {
	fail := func(err error) ipc.Response {
		return ipc.Response{OK: false, Error: err.Error()}
	}
	count := func(n int) ipc.Response {
		return ipc.Response{OK: true, Count: &n}
	}
	switch req.Cmd {
	case "add":
		id, err := s.mgr.Add(req.URL)
		if err != nil {
			return fail(err)
		}
		return ipc.Response{OK: true, ID: id}
	case "list":
		return ipc.Response{OK: true, Items: s.mgr.List()}
	case "pause":
		if err := s.mgr.Pause(req.ID); err != nil {
			return fail(err)
		}
		return ipc.Response{OK: true}
	case "resume":
		if err := s.mgr.Resume(req.ID); err != nil {
			return fail(err)
		}
		return ipc.Response{OK: true}
	case "cancel", "remove":
		if err := s.mgr.Cancel(req.ID); err != nil {
			return fail(err)
		}
		return ipc.Response{OK: true}
	case "start_now":
		if err := s.mgr.StartNow(req.ID); err != nil {
			return fail(err)
		}
		return ipc.Response{OK: true}
	case "clear_finished":
		n, err := s.mgr.ClearFinished()
		if err != nil {
			return fail(err)
		}
		return count(n)
	case "clear_all":
		n, err := s.mgr.ClearAll()
		if err != nil {
			return fail(err)
		}
		return count(n)
	default:
		return fail(fmt.Errorf("unknown command %q", strings.TrimSpace(req.Cmd)))
	}
}
