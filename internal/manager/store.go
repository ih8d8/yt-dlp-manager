package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"yt-dlp-manager/internal/ipc"
)

const stateVersion = 1
const maxStateBytes = 64 << 20

type Store struct {
	path string
	mu   sync.Mutex
}

// Path returns the file path this store persists to.
func (s *Store) Path() string { return s.path }

type snapshotFile struct {
	Version int        `json:"version"`
	SavedAt time.Time  `json:"saved_at"`
	Items   []ipc.Item `json:"items"`
	Order   []string   `json:"order"`
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		path = DefaultStorePath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{path: path}, nil
}

func DefaultStorePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "yt-dlp-manager-state.json"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "yt-dlp-manager", "state.json")
}

// LegacyStorePath is the pre-rename state location. It is only ever read for
// one-time migration; it is never deleted or renamed by this program.
func LegacyStorePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "yt-dlp-tui-state.json"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "yt-dlp-tui", "state.json")
}

// Exists reports whether the state file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// MigrateLegacyState copies valid legacy state to the store's path when the
// new state file does not exist yet. Corrupt or unsupported legacy state is
// left untouched (never quarantined or deleted here — the legacy file belongs
// to the previous installation). Migration completion is marked purely by the
// presence of valid new state. It reports whether a copy was made.
func MigrateLegacyState(st *Store, legacyPath string) bool {
	if st.Exists() {
		return false
	}
	snap, err := loadSnapshotFile(legacyPath)
	if err != nil {
		return false
	}
	// Re-marshal from the parsed snapshot so the copied file is canonical and
	// already validated. The legacy file itself stays exactly where it was.
	return st.Save(snap.Items, snap.Order) == nil
}

// loadSnapshotFile reads and validates a snapshot from an arbitrary path
// using the same rules as Store.Load.
func loadSnapshotFile(path string) (*snapshotFile, error) {
	data, err := readFileLimited(path, maxStateBytes)
	if err != nil {
		return nil, err
	}
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if snap.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", snap.Version)
	}
	if err := validateSnapshot(&snap); err != nil {
		return nil, fmt.Errorf("invalid state: %w", err)
	}
	return &snap, nil
}

func (s *Store) Save(items []ipc.Item, order []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := snapshotFile{
		Version: stateVersion,
		SavedAt: time.Now(),
		Items:   items,
		Order:   order,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	// Load refuses anything larger, so writing it would produce a file that
	// the next start quarantines as corrupt — silently losing the history.
	// Failing the save keeps the last readable snapshot on disk instead.
	if len(data) > maxStateBytes {
		return fmt.Errorf("state is %d bytes, over the %d byte limit: "+
			"clear finished downloads to shrink it", len(data), maxStateBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	// The rename is atomic, but the directory entry it creates is not durable
	// until the parent directory itself is synced: a power loss in that window
	// can leave the state file missing entirely even though every write
	// succeeded. Syncing the directory closes it.
	//
	// The snapshot itself is already in place by now, so a failure here costs
	// only durability. It is still reported: the caller retries the save (which
	// is harmless — the write is idempotent) and /readyz stops claiming the
	// queue is safely persisted while the filesystem is misbehaving.
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return fmt.Errorf("open state dir for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state dir: %w", err)
	}
	return nil
}

var ErrNoState = errors.New("no saved state")

func (s *Store) Quarantine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UnixNano())
	if err := os.Rename(s.path, target); err != nil {
		return ""
	}
	return target
}

// Load parses the state file. Unknown fields are deliberately tolerated (no
// DisallowUnknownFields) so a newer version that adds optional fields to the
// same schema version still loads; anything unparseable fails and gets
// quarantined by the caller instead of being overwritten.
func (s *Store) Load() (*snapshotFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := readFileLimited(s.path, maxStateBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoState
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if snap.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", snap.Version)
	}
	if err := validateSnapshot(&snap); err != nil {
		return nil, fmt.Errorf("invalid state: %w", err)
	}
	return &snap, nil
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func validateSnapshot(snap *snapshotFile) error {
	seen := make(map[string]ipc.State, len(snap.Items))
	for _, it := range snap.Items {
		if it.ID == "" {
			return errors.New("item has empty id")
		}
		if _, duplicate := seen[it.ID]; duplicate {
			return fmt.Errorf("duplicate item id %q", it.ID)
		}
		if !ipc.ValidURL(it.URL) {
			return fmt.Errorf("item %q has invalid url", it.ID)
		}
		switch it.State {
		case ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused,
			ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
		default:
			return fmt.Errorf("item %q has invalid state %q", it.ID, it.State)
		}
		seen[it.ID] = it.State
	}
	ordered := make(map[string]struct{}, len(snap.Order))
	for _, id := range snap.Order {
		if _, duplicate := ordered[id]; duplicate {
			return fmt.Errorf("duplicate queue id %q", id)
		}
		_, ok := seen[id]
		if !ok {
			return fmt.Errorf("queue references unknown item %q", id)
		}
		ordered[id] = struct{}{}
	}
	return nil
}
