package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	added := time.Now()
	items := []ipc.Item{
		{ID: "id1", URL: "https://a", Title: "A", State: ipc.StateQueued, AddedAt: added},
		{ID: "id2", URL: "https://b", State: ipc.StateCompleted, AddedAt: added, DoneAt: &added},
	}
	order := []string{"id1"}
	if err := st.Save(items, order); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	if got.Items[0].ID != "id1" || got.Items[0].Title != "A" {
		t.Errorf("item0 = %+v", got.Items[0])
	}
	if len(got.Order) != 1 || got.Order[0] != "id1" {
		t.Errorf("order = %v", got.Order)
	}
}

func TestStoreFilePerms(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "state.json"))
	if err := st.Save(nil, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestLoadMissingReturnsErr(t *testing.T) {
	st, _ := NewStore(filepath.Join(t.TempDir(), "none.json"))
	if _, err := st.Load(); err != ErrNoState {
		t.Fatalf("err = %v, want ErrNoState", err)
	}
}

func TestLoadCorruptFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	st, _ := NewStore(p)
	if _, err := st.Load(); err == nil {
		t.Fatal("expected error for corrupt state")
	}
}

func TestLoadRejectsSemanticallyInvalidSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	data := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","items":[{"id":"dup","url":"https://a","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"},{"id":"dup","url":"https://b","state":"queued","progress":0,"added_at":"2026-01-01T00:00:00Z"}],"order":["dup","dup"]}`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := NewStore(p)
	if _, err := st.Load(); err == nil {
		t.Fatal("duplicate IDs/order entries must reject the snapshot")
	}
}

func TestReadFileLimitedRejectsOversizedInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(p, []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileLimited(p, 8); err == nil {
		t.Fatal("oversized state input must be rejected during the bounded read")
	}
}

func TestParseNum(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", -1},
		{"NA", -1},
		{"None", -1},
		{"0", 0},
		{"42", 42},
		{"1048576.5", 1048576},
		{"abc", -1},
		{" 7 ", 7},
	}
	for _, c := range cases {
		if got := ParseNum(c.in); got != c.want {
			t.Errorf("ParseNum(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
