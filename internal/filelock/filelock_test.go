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
