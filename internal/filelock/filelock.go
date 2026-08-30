// Package filelock provides process-lifetime advisory locks for files owned by
// yt-dlp-manager. The lock file is deliberately persistent: unlinking a locked
// file would let a second process create and lock a different inode at the same
// path while the first process still owns the original one.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// ErrLocked means another process already holds the requested lock.
var ErrLocked = errors.New("lock is held by another process")

type Lock struct {
	file *os.File
	once sync.Once
}

// Acquire opens path without following symlinks and takes an exclusive,
// non-blocking advisory lock. Callers must keep the returned Lock alive for the
// entire ownership period and Close it when ownership ends.
func Acquire(path string) (*Lock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lock %s: invalid file descriptor", path)
	}
	closeOnError := func(err error) (*Lock, error) {
		_ = f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("stat lock %s: %w", path, err))
	}
	if !fi.Mode().IsRegular() {
		return closeOnError(fmt.Errorf("lock %s is not a regular file", path))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return closeOnError(fmt.Errorf("stat lock %s: %w", path, err))
	}
	if st.Nlink != 1 {
		return closeOnError(fmt.Errorf("lock %s has %d hard links; refusing it", path, st.Nlink))
	}
	if err := f.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("chmod lock %s: %w", path, err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return closeOnError(fmt.Errorf("%w: %s", ErrLocked, path))
		}
		return closeOnError(fmt.Errorf("lock %s: %w", path, err))
	}
	return &Lock{file: f}, nil
}

// Close releases the lock. It is safe to call more than once.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	var closeErr error
	l.once.Do(func() {
		if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
			closeErr = err
		}
		if err := l.file.Close(); closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}
