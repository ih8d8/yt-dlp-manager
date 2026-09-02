package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireExcludesASecondOwnerAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		first.Close()
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	second.Close()
}

func TestAcquireRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "symlink.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := Acquire(filepath.Join(dir, "symlink.lock")); err == nil {
		lock.Close()
		t.Fatal("Acquire followed a symlink")
	}
	if err := os.Link(target, filepath.Join(dir, "hardlink.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := Acquire(filepath.Join(dir, "hardlink.lock")); err == nil {
		lock.Close()
		t.Fatal("Acquire accepted a multiply-linked inode")
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("rejected link changed target mode to %o", fi.Mode().Perm())
	}
}

func TestAcquireExistingEmptyDoesNotCreateOrLockState(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.lock")
	if lock, err := AcquireExistingEmpty(missing); err == nil {
		lock.Close()
		t.Fatal("AcquireExistingEmpty created a missing lock")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing legacy lock was created: %v", err)
	}

	state := filepath.Join(dir, "another-state.lock")
	const contents = `{"version":1}`
	if err := os.WriteFile(state, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireExistingEmpty(state); err == nil {
		lock.Close()
		t.Fatal("AcquireExistingEmpty accepted a non-empty state file")
	}
	got, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != contents {
		t.Fatalf("state file changed to %q", got)
	}

	legacy := filepath.Join(dir, "legacy.lock")
	if err := os.WriteFile(legacy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireExistingEmpty(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireExistingEmpty(legacy); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second legacy acquisition = %v, want ErrLocked", err)
	}
}
