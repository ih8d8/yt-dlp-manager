// Package service wires one manager to its Unix control socket with a single
// ownership contract and an idempotent shutdown. Server, daemon, and
// standalone-TUI modes all bootstrap through Start so their lifecycle
// behavior cannot drift apart.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"yt-dlp-manager/internal/filelock"
	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/ipcserver"
	"yt-dlp-manager/internal/manager"
)

// ErrOwnerAlive is returned when another live process already owns the
// requested state file and control socket.
var ErrOwnerAlive = errors.New("another yt-dlp-manager instance is already running")

type Options struct {
	Max int // maximum concurrent downloads (<=0 means manager default)
	// ExtraArgs are the global yt-dlp arguments from configuration. They are
	// installed on the manager BEFORE the offline inbox is drained: inbox URLs
	// are scheduled the moment they are added, so setting them afterwards
	// would let the first downloads after a restart run without the proxy,
	// headers or rate limit everything else gets.
	ExtraArgs  string
	StatePath  string // "" uses the XDG default and enables legacy migration
	SocketPath string // "" uses the XDG default
	InboxPath  string // "" uses the XDG default
	Runner     manager.Runner
}

// Service owns exactly one manager and its IPC listener.
type Service struct {
	mgr *manager.Manager
	ln  net.Listener
	srv *ipcserver.Server
	// stateLock prevents a second process with a different control socket from
	// loading and writing the same state file concurrently.
	stateLock *filelock.Lock
	// legacyLock is the pre-lock-directory lock name, held only so an older
	// process still using it blocks this one. Best effort; may be nil.
	legacyLock *filelock.Lock

	// Interrupted counts unfinished downloads recovered from the previous
	// process. They are restored paused and stay that way; this is only how
	// many are waiting. InboxDrained counts offline adds consumed from the inbox.
	Interrupted  int
	InboxDrained int

	closeOnce sync.Once
	done      chan struct{}
}

// Start performs the shared startup sequence:
//
//  1. lock the state file against all other process instances,
//  2. migrate valid legacy state when using the default path,
//  3. bind and lock the control socket,
//  4. construct the manager,
//  5. serve IPC in the background,
//  6. drain the offline inbox and count recovered unfinished work (left paused).
//
// The state and socket locks serialize concurrent starters across processes.
// If any step fails, everything started so far is closed again before the
// error returns.
func Start(ctx context.Context, opt Options) (*Service, error) {
	socket := opt.SocketPath
	if socket == "" {
		socket = ipc.DefaultSocketPath()
	}
	statePath := opt.StatePath
	migrateLegacy := opt.StatePath == ""
	st, err := manager.NewStore(statePath)
	if err != nil {
		return nil, err
	}
	lockPath, err := st.LockPath()
	if err != nil {
		return nil, err
	}
	stateOwner, err := filelock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("%w: state %s", ErrOwnerAlive, st.Path())
		}
		return nil, err
	}
	fail := true
	defer func() {
		if fail {
			_ = stateOwner.Close()
		}
	}()

	// Transitional: a process from before the lock directory holds the OLD
	// lock name and knows nothing about the new one, so an upgrade in place
	// could otherwise run two writers against one state file, each holding a
	// lock the other never looks at. Only an EXISTING EMPTY legacy lock is
	// eligible: old releases left exactly that behind. Never creating the old
	// name, and refusing a non-empty file, avoids manufacturing or locking a
	// different instance's state (for example "foo" and "foo.lock"). Only a
	// contended legacy lock is fatal; other failures mean the transitional
	// guarantee is unavailable and the documented stop-before-start rule wins.
	legacyOwner, legacyErr := filelock.AcquireExistingEmpty(st.LegacyLockPath())
	if errors.Is(legacyErr, filelock.ErrLocked) {
		return nil, fmt.Errorf("%w: state %s", ErrOwnerAlive, st.Path())
	}
	if legacyOwner != nil {
		defer func() {
			if fail {
				_ = legacyOwner.Close()
			}
		}()
	}
	if migrateLegacy {
		manager.MigrateLegacyState(st, manager.LegacyStorePath())
	}

	// Fast-fail ownership probe: a dialable socket means somebody else owns
	// this state already. The authoritative check is Listen's live-peer
	// refusal below.
	if c, err := ipc.Dial(socket); err == nil {
		c.Close()
		return nil, fmt.Errorf("%w: %s", ErrOwnerAlive, socket)
	}

	// Bind first: from here on we own the socket path or we fail cleanly.
	ln, err := ipcserver.Listen(socket)
	if err != nil {
		if errors.Is(err, ipcserver.ErrOwnerAlive) {
			err = fmt.Errorf("%w: %s", ErrOwnerAlive, socket)
		}
		return nil, err
	}

	var mgr *manager.Manager
	if opt.Runner != nil {
		mgr, err = manager.NewWithRunner(ctx, opt.Max, st.Path(), opt.Runner)
	} else {
		mgr, err = manager.New(ctx, opt.Max, st.Path())
	}
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	// Before anything can be scheduled: the inbox drain below adds URLs, and
	// an added URL is scheduled immediately.
	if err := mgr.SetExtraArgs(opt.ExtraArgs); err != nil {
		mgr.Close()
		_ = ln.Close()
		return nil, fmt.Errorf("downloads.extra_args: %w", err)
	}

	svc := &Service{
		mgr: mgr, ln: ln, stateLock: stateOwner, legacyLock: legacyOwner,
		done: make(chan struct{}),
	}
	fail = false

	svc.srv = ipcserver.NewServer(mgr)
	go func() {
		_ = svc.srv.Serve(ln)
	}()

	svc.InboxDrained = ipcserver.DrainInbox(mgr, opt.InboxPath)
	svc.Interrupted = mgr.InterruptedCount()

	// A cancelled parent context must tear the whole service down; Close's
	// sync.Once makes this race-free with an explicit Close.
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				svc.Close()
			case <-svc.done:
			}
		}()
	}
	return svc, nil
}

// Manager exposes the owned manager (used by the TUI local backend and the
// HTTP adapter).
func (s *Service) Manager() *manager.Manager { return s.mgr }

// SocketPath reports the control socket this service listens on.
func (s *Service) SocketPath() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Done closes after Close has fully quiesced the service.
func (s *Service) Done() <-chan struct{} { return s.done }

// Close stops accepting new IPC connections, terminates live clients, waits
// for child processes and persistence inside the manager, and removes only
// the socket it created. It is safe to call any number of times.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		defer close(s.done)
		if s.ln != nil {
			_ = s.ln.Close() // UnixListener unlinks the socket file it created
		}
		if s.srv != nil {
			s.srv.Close() // joins every accepted IPC handler
		}
		if s.mgr != nil {
			s.mgr.Close()
		}
		if s.legacyLock != nil {
			_ = s.legacyLock.Close()
		}
		if s.stateLock != nil {
			_ = s.stateLock.Close()
		}
	})
}

// OwnsSocketFile is a small helper for tests: it reports whether path looks
// like a socket file currently present on disk.
func OwnsSocketFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
