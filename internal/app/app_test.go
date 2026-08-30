package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureDirWritableDoesNotFollowPredictableSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".write-probe")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureDirWritable(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me" {
		t.Fatalf("writeability probe changed symlink target: %q", got)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("writeability probe removed pre-existing entry: %v", err)
	}
}

// captureFile redirects a std stream into a pipe and returns a finalize func
// that stops capturing and returns everything written meanwhile.
func captureFile(t *testing.T, target **os.File) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *target
	*target = w
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, r)
		close(done)
	}()
	return func() string {
		*target = old
		w.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		r.Close()
		return buf.String()
	}
}

func TestUsageMentionsAllCommands(t *testing.T) {
	u := Usage()
	for _, want := range []string{
		"tui", "server", "daemon", "add", "list", "pause", "resume",
		"start-now", "remove", "clear-finished", "clear-all",
		"healthcheck", "version", "YTDLP_MANAGER_MAX_CONCURRENT",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func TestRunHelpAndVersionExitZero(t *testing.T) {
	finishOut := captureFile(t, &os.Stdout)
	defer finishOut()

	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"version"}} {
		if code := Run(args); code != 0 {
			t.Errorf("Run(%q) = %d, want 0", args, code)
		}
	}
	got := finishOut()
	if !strings.Contains(got, productName) {
		t.Errorf("output missing product name: %q", got)
	}
}

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	finishErr := captureFile(t, &os.Stderr)
	code := Run([]string{"definitely-not-a-command"})
	out := finishErr()
	if code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(out, "unknown command") {
		t.Errorf("stderr missing explanation: %q", out)
	}
}

func TestClientCommandsRouteWithoutManagerFailCleanly(t *testing.T) {
	finishErr := captureFile(t, &os.Stderr)
	code := Run([]string{"list"})
	out := finishErr()
	if code == 0 {
		t.Skip("a manager appears to be running in the environment")
	}
	if code != 1 {
		t.Errorf("client command without manager = %d, want 1", code)
	}
	if !strings.Contains(out, "manager not running") {
		t.Errorf("stderr must explain the failure: %q", out)
	}
}
