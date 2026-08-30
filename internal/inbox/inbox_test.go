package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendDrainRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "inbox.jsonl")
	if err := Append(p, "https://a/1"); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, "https://a/2"); err != nil {
		t.Fatal(err)
	}
	urls, err := Drain(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 || urls[0] != "https://a/1" || urls[1] != "https://a/2" {
		t.Fatalf("urls = %v", urls)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("inbox file should be removed after drain")
	}
}

func TestDrainMissingFile(t *testing.T) {
	urls, err := Drain(filepath.Join(t.TempDir(), "none.jsonl"))
	if err != nil || urls != nil {
		t.Fatalf("urls=%v err=%v", urls, err)
	}
}

func TestDrainKeepsFileWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "inbox.jsonl")
	os.WriteFile(p, []byte("\n"), 0o600)
	urls, err := Drain(p)
	if err != nil || len(urls) != 0 {
		t.Fatalf("urls=%v err=%v", urls, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("empty inbox file should survive")
	}
}

func TestAppendPerms(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "inbox.jsonl")
	if err := Append(p, "https://x"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestAppendEmptyRejected(t *testing.T) {
	if err := Append(filepath.Join(t.TempDir(), "inbox.jsonl"), "   "); err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestAppendInvalidURLRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inbox.jsonl")
	for _, u := range []string{"--exec=bad", "https://x\nsecond"} {
		if err := Append(p, u); err == nil {
			t.Errorf("Append(%q) accepted an invalid URL", u)
		}
	}
}

func TestDrainRejectsMalformedLineWithoutDuplicatingPrefix(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "inbox.jsonl")
	good := `{"url":"https://a/1","added_at":"2026-01-01T00:00:00Z"}`
	bad := `{"url":"https://a/2","added_at":"2026-01-01T00:00:00Z","unknown":true}`
	if err := os.WriteFile(p, []byte(good+"\n"+bad+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	urls, err := Drain(p)
	if err == nil || urls != nil {
		t.Fatalf("urls=%v err=%v, want no returned prefix and a strict JSON error", urls, err)
	}
	data, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), good) || !strings.Contains(string(data), bad) {
		t.Fatalf("failed drain must preserve every line, got %q", data)
	}
}

func TestUniqueClaimsAndRestorePreserveConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "inbox.jsonl")
	if err := Append(p, "https://a/old"); err != nil {
		t.Fatal(err)
	}
	first, err := claim(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(p, "https://a/new"); err != nil {
		t.Fatal(err)
	}
	second, err := claim(p)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("concurrent claims must use distinct paths")
	}
	if err := restore(first, p); err != nil {
		t.Fatal(err)
	}
	if err := restore(second, p); err != nil {
		t.Fatal(err)
	}
	urls, err := Drain(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 || !containsURL(urls, "https://a/old") || !containsURL(urls, "https://a/new") {
		t.Fatalf("restored urls = %v", urls)
	}
}

func containsURL(urls []string, want string) bool {
	for _, u := range urls {
		if u == want {
			return true
		}
	}
	return false
}
