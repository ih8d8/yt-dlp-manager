package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"yt-dlp-manager/internal/ipc"
)

const stateVersion = 1

// maxStateBytes bounds any state file read or written.
//
// Rows are bounded at admission (see MaxItemBytes) and the queue is bounded at
// MaxQueueItems, so a queue this build accepts always fits here — that is the
// guarantee, pinned by TestQueueAlwaysFitsTheStateLimit. Trimming the oldest
// completed history is a safety net for state written by a build with looser
// limits, not the mechanism: live work is never dropped to make room.
//
// This is the size of the FILE. Reading it allocates more: the bytes, plus the
// decoded structures, plus JSON's own scratch.
const maxStateBytes = 64 << 20

type Store struct {
	path string
	mu   sync.Mutex
}

// Path returns the file path this store persists to.
func (s *Store) Path() string { return s.path }

// LegacyLockPath is where releases before the lock directory kept the owner
// lock. It is acquired as a second, transitional lock so an old process still
// holding it blocks a new one; without that, an upgrade in place could briefly
// run two writers against one state file, each holding a lock the other never
// looks at.
//
// Best effort by design: callers only acquire an existing empty file left by
// an older release. They never create this path or lock a non-empty file,
// because the old name can itself be another instance's state file.
func (s *Store) LegacyLockPath() string { return s.path + ".lock" }

// LockPath is this state file's owner lock: inside the lock directory, named
// from the state file's canonical path.
func (s *Store) LockPath() (string, error) {
	dir := filepath.Join(filepath.Dir(s.path), lockDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create lock dir: %w", err)
	}
	// s.path is already canonical (see NewStore), so every spelling of one
	// state file produces one lock.
	sum := sha256.Sum256([]byte(s.path))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:32]+".lock"), nil
}

type snapshotFile struct {
	Version int        `json:"version"`
	SavedAt time.Time  `json:"saved_at"`
	Items   []ipc.Item `json:"items"`
	Order   []string   `json:"order"`

	// Dropped carries rows this build could not restore, set during validation
	// and never persisted. They are written to a sidecar file and reported
	// rather than vanishing: an upgrade that silently discards pending
	// downloads is not a migration anyone can trust.
	Dropped []DroppedItem `json:"-"`
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		path = DefaultStorePath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve state path: %w", err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	// Resolve symlinks now that the directory exists. Everything downstream —
	// the lock name, the sibling names, the writes — then works from one
	// spelling of one file, so two paths that name the same state cannot be
	// treated as two states.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve state dir: %w", err)
	}
	canonical := filepath.Join(resolvedDir, filepath.Base(abs))
	// The state file itself may be a symlink; follow it so the same file
	// reached by another name is still the same file here. Only a genuinely
	// absent path is a new store. A dangling/looping symlink or another
	// resolution failure is a configuration error, not permission to fall back
	// to a second spelling and a second owner lock.
	info, statErr := os.Lstat(canonical)
	switch {
	case statErr == nil:
		target, err := filepath.EvalSymlinks(canonical)
		if err != nil {
			return nil, fmt.Errorf("resolve state file: %w", err)
		}
		canonical = target
		info, err = os.Stat(canonical)
		if err != nil {
			return nil, fmt.Errorf("stat state file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("state path %q is not a regular file", canonical)
		}
	case errors.Is(statErr, os.ErrNotExist):
		// A new state file is created by the first successful save.
	default:
		return nil, fmt.Errorf("inspect state file: %w", statErr)
	}
	// The lock namespace is off limits to state files, checked AFTER
	// resolution so a symlink cannot be used to step into it.
	if filepath.Base(filepath.Dir(canonical)) == lockDirName {
		return nil, fmt.Errorf("state file may not live in the %s directory", lockDirName)
	}
	return &Store{path: canonical}, nil
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
	// Rows the new limits cannot hold are written aside here too. Migration is
	// exactly where they appear — the legacy file was written under different
	// rules — and the legacy file, while left in place, is never read again,
	// so saying nothing would lose them for good.
	if len(snap.Dropped) > 0 {
		where, derr := st.SaveDropped(snap.Dropped)
		if derr != nil {
			// The legacy file is left exactly where it is and is never deleted
			// by this program, so it remains the durable copy; say so rather
			// than printing URLs, which can carry tokens.
			// The legacy file is never deleted by this program, so it stays
			// the copy of record.
			fmt.Fprintf(os.Stderr,
				"yt-dlp-manager: %d legacy entries not migrated (%v); still in %s\n",
				len(snap.Dropped), derr, legacyPath)
		} else {
			fmt.Fprintf(os.Stderr,
				"yt-dlp-manager: %d legacy entries not migrated; kept in %s\n",
				len(snap.Dropped), where)
		}
	}
	// Re-marshal from the parsed snapshot so the copied file is canonical and
	// already validated. The legacy file itself stays exactly where it was.
	return st.Save(snap.Items, snap.Order) == nil
}

// fitToStateLimit drops the oldest completed history until the snapshot fits.
//
// With per-item budgets enforced at admission this is a safety net, not the
// guarantee: it exists for state written by an older build with different
// limits. It can only ever help when there is expendable history to drop —
// which is exactly why admission control, not trimming, is what keeps a live
// queue persistable.
//
// Only COMPLETED rows are expendable. Failed and deleted rows are pending
// work — the API offers Retry and Start now for both — and silently discarding
// a row's URL and saved options would lose a download the user still intends
// to make. A completed row is the only one whose record is purely historical.
//
// Sizing walks the rows and adds them up rather than marshalling the whole
// snapshot: a pathological queue would otherwise have to be materialised in
// full — hundreds of megabytes — before the limit that exists to prevent that
// could even be checked.
func fitToStateLimit(items []ipc.Item, order []string, measure bool) ([]ipc.Item, []string, []string, error) {
	// Fast path, and the one every ordinary save takes: admitted rows are
	// bounded by MaxItemBytes, so if the worst case fits there is nothing to
	// measure. Without it every save marshalled each row separately on top of
	// marshalling the snapshot — 24ms per save on a 2000-row queue.
	//
	// The caller measures the result anyway and asks again with measure=true
	// if it did not fit, so a row that somehow escaped its budget costs a
	// second pass rather than a failed save.
	if !measure && int64(len(items))*MaxItemBytes+snapshotOverhead <= maxStateBytes {
		return items, order, nil, nil
	}

	sizes := make([]int, len(items))
	total := int64(snapshotOverhead)
	for i, it := range items {
		b, err := ipc.MarshalNoHTMLEscape(it)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("marshal state: %w", err)
		}
		sizes[i] = len(b) + len(it.ID) + 4 // the row, its id in order, separators
		total += int64(sizes[i])
	}
	if total <= maxStateBytes {
		return items, order, nil, nil
	}

	// Oldest expendable first. AddedAt is the stable ordering here: DoneAt is
	// nil for rows that never ran.
	victims := make([]int, 0, len(items))
	for i, it := range items {
		if it.State == ipc.StateCompleted {
			victims = append(victims, i)
		}
	}
	sort.SliceStable(victims, func(a, b int) bool {
		return items[victims[a]].AddedAt.Before(items[victims[b]].AddedAt)
	})

	dropped := make(map[string]bool, len(victims))
	var trimmed []string
	for _, idx := range victims {
		if total <= maxStateBytes {
			break
		}
		total -= int64(sizes[idx])
		dropped[items[idx].ID] = true
		trimmed = append(trimmed, items[idx].ID)
	}
	keptItems, keptOrder := without(items, order, dropped)
	return keptItems, keptOrder, trimmed, nil
}

// snapshotOverhead is the wrapper around the items: version, timestamp, field
// names and brackets. A generous constant; the arithmetic only needs an upper
// bound that does not vary with the number of rows.
const snapshotOverhead = 512

// marshalSnapshot encodes one snapshot of the queue.
func marshalSnapshot(items []ipc.Item, order []string) ([]byte, error) {
	data, err := ipc.MarshalNoHTMLEscape(snapshotFile{
		Version: stateVersion, SavedAt: time.Now(), Items: items, Order: order,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal state: %w", err)
	}
	return data, nil
}

// snapshotSize reports the serialized size of a snapshot.
func snapshotSize(items []ipc.Item, order []string) (int64, error) {
	data, err := ipc.MarshalNoHTMLEscape(snapshotFile{
		Version: stateVersion, SavedAt: time.Now(), Items: items, Order: order,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal state: %w", err)
	}
	return int64(len(data)), nil
}

// without returns the items and order with the dropped ids removed.
func without(items []ipc.Item, order []string, dropped map[string]bool) ([]ipc.Item, []string) {
	keptItems := make([]ipc.Item, 0, len(items)-len(dropped))
	for _, it := range items {
		if !dropped[it.ID] {
			keptItems = append(keptItems, it)
		}
	}
	keptOrder := make([]string, 0, len(order))
	for _, id := range order {
		if !dropped[id] {
			keptOrder = append(keptOrder, id)
		}
	}
	return keptItems, keptOrder
}

// DroppedItem is a row that could not be restored, with why. The reason is
// recorded per row because they are not all dropped for the same cause, and
// "exceeds the size limit" is unhelpful advice for a row dropped as a
// duplicate.
type DroppedItem struct {
	Item   ipc.Item `json:"item"`
	Reason string   `json:"reason"`
}

// Reasons a row is not restored.
const (
	DropTooLarge  = "exceeds this build's per-entry storage limits"
	DropQueueFull = "the saved queue holds more entries than this build allows"
	DropDuplicate = "another restored entry already holds this URL"
)

// SaveDropped writes rows this build could not restore to a sidecar file next
// to the state, so an upgrade never destroys pending work without a trace. It
// is written with the same durability as the state itself — temp file, fsync,
// atomic rename, directory fsync — and every failure is reported rather than
// swallowed, because the caller has already decided to drop these rows and
// this file is the only remaining copy.
func (s *Store) SaveDropped(items []DroppedItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	data, err := ipc.MarshalNoHTMLEscape(struct {
		SavedAt time.Time     `json:"saved_at"`
		Note    string        `json:"note"`
		Items   []DroppedItem `json:"items"`
	}{
		SavedAt: time.Now(),
		Note: "these entries could not be restored; each carries its own reason, " +
			"and their URLs are preserved here so they can be re-added",
		Items: items,
	})
	if err != nil {
		return "", fmt.Errorf("marshal dropped entries: %w", err)
	}
	target, err := siblingPath(s.path, fmt.Sprintf(".dropped-%d.json", time.Now().UnixNano()))
	if err != nil {
		return "", err
	}
	if err := writeFileDurable(target, data); err != nil {
		return "", err
	}
	return target, nil
}

// Sibling files (the owner lock, quarantined state, preserved backups, records
// of unrestorable rows) are named after the state file. Two constraints govern
// that naming, and both have teeth:
//
//   - The result must fit one path component. A state file may already sit at
//     the filesystem's limit, so a suffix cannot simply be appended.
//   - The result must be UNIQUE to the state file it belongs to. Truncating a
//     long name to make room breaks that: two state files sharing a prefix
//     would get one lock between them, and a name ending in ".lock" could
//     shorten to the state file itself — a process would then hold a lock on
//     the very inode the next save atomically replaces, and a second instance
//     could start against the same state.
//
// So when shortening is needed the name is rebuilt from a short readable
// prefix plus a digest of the WHOLE original name, which keeps distinct state
// files distinct.
const (
	// maxNameLen is the length at which a sibling name is rebuilt in the short
	// digested form. It is deliberately far below the usual 255-byte POSIX
	// limit: filesystems with smaller limits exist (eCryptfs is ~143, some
	// network mounts less), and a threshold of 255 would happily produce a
	// 145-byte name that such a filesystem rejects. Shortening early costs
	// nothing — ordinary names are far below it and stay readable.
	maxNameLen = 96
	// shortPrefixLen is how much of the original name a shortened sibling
	// keeps, for readability.
	shortPrefixLen = 24
	// digestLen is the hex length of the name digest: 48 bits over the names
	// in one directory.
	digestLen = 12
)

// lockDirName is the directory locks live in, beside the state file.
//
// Locks do NOT live next to the state as "<state>.lock". That naming can put
// one instance's lock exactly on another instance's state file — a state file
// literally named "foo.lock" is the lock for a state file named "foo" — and
// then the second instance's ordinary save atomically replaces the inode the
// first is holding, quietly ending its ownership. A separate directory makes
// that impossible rather than unlikely, and NewStore refuses a state path
// inside it so the two namespaces cannot be crossed from the other side.
//
// The lock is named from the state file's CANONICAL path, symlinks resolved.
// Lexical cleaning is not enough: /real/state.json and /alias/state.json,
// where alias is a symlink to real, are one file with two spellings, and two
// hashes of those spellings would hand out two locks for it.
const lockDirName = ".locks"

// siblingPath returns path with suffix appended, shortening the base name —
// with a digest of the original — when the result would not fit.
func siblingPath(path, suffix string) (string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	if len(suffix)+shortPrefixLen+digestLen+1 > maxNameLen {
		return "", fmt.Errorf("suffix %q is too long to name a sibling file", suffix)
	}
	name := base + suffix
	if len(name) > maxNameLen {
		sum := sha256.Sum256([]byte(base))
		prefix := base
		if len(prefix) > shortPrefixLen {
			prefix = prefix[:shortPrefixLen]
		}
		name = prefix + "-" + hex.EncodeToString(sum[:])[:digestLen] + suffix
	}
	if name == base {
		// Unreachable once a digest is in the name, but the cost of being
		// wrong is two processes writing one state file.
		return "", fmt.Errorf("sibling name for %q collides with the file itself", base)
	}
	return filepath.Join(dir, name), nil
}

// PreserveCurrent renames the current state file aside, returning the new
// path. It is used before writing a reduced snapshot, so the pre-migration
// state survives as a durable copy no matter what happens afterwards.
func (s *Store) PreserveCurrent() (string, error) {
	if !s.Exists() {
		return "", nil
	}
	target, err := siblingPath(s.path, fmt.Sprintf(".before-prune-%d", time.Now().UnixNano()))
	if err != nil {
		return "", err
	}
	if err := os.Rename(s.path, target); err != nil {
		return "", fmt.Errorf("preserve state: %w", err)
	}
	// A rename is atomic but its directory entry is not durable until the
	// parent is synced. Without this the "preserved" copy can vanish in a
	// power loss — precisely the situation it exists for.
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return "", fmt.Errorf("open state dir for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", fmt.Errorf("sync state dir: %w", err)
	}
	return target, nil
}

// writeFileDurable writes data to path so that a crash cannot leave a partial
// or missing file: temp file, fsync, atomic rename, then fsync the directory.
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dropped-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %q: %w", path, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
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

// Save writes the snapshot, trimming the oldest finished rows if it would not
// otherwise fit. It reports how many were trimmed so the caller can drop them
// from memory too and stay consistent with what is on disk.
func (s *Store) Save(items []ipc.Item, order []string) error {
	_, err := s.SaveTrimmed(items, order)
	return err
}

// SaveTrimmed is Save with the trimmed ids reported.
func (s *Store) SaveTrimmed(items []ipc.Item, order []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, order, trimmed, err := fitToStateLimit(items, order, false)
	if err != nil {
		return nil, err
	}
	data, err := marshalSnapshot(items, order)
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateBytes {
		// The cheap estimate was wrong for this queue — a row larger than the
		// budget got in somehow. Measure properly and trim for real.
		items, order, trimmed, err = fitToStateLimit(items, order, true)
		if err != nil {
			return nil, err
		}
		if data, err = marshalSnapshot(items, order); err != nil {
			return nil, err
		}
	}
	if len(data) > maxStateBytes {
		// Only reachable when live rows alone exceed the limit, which trimming
		// deliberately will not touch: losing a running download's identity is
		// worse than a failed save the saver will retry.
		return nil, fmt.Errorf("state is %d bytes, over the %d byte limit even after "+
			"trimming history: remove some active downloads", len(data), maxStateBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return nil, fmt.Errorf("rename state: %w", err)
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
		return nil, fmt.Errorf("open state dir for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return nil, fmt.Errorf("sync state dir: %w", err)
	}
	return trimmed, nil
}

var ErrNoState = errors.New("no saved state")

// Quarantine renames unreadable state aside so a fresh one can be written. It
// reports failure rather than swallowing it: if the file cannot be moved, the
// caller must not carry on and overwrite it — that file is the only copy of
// whatever it holds.
func (s *Store) Quarantine() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.path)
	if err != nil {
		return "", fmt.Errorf("inspect state before quarantine: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to quarantine non-regular state path %q", s.path)
	}
	target, err := siblingPath(s.path, fmt.Sprintf(".corrupt-%d", time.Now().UnixNano()))
	if err != nil {
		return "", err
	}
	if err := os.Rename(s.path, target); err != nil {
		return "", fmt.Errorf("quarantine state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return "", fmt.Errorf("open state dir for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", fmt.Errorf("sync state dir: %w", err)
	}
	return target, nil
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
	// O_NONBLOCK prevents a path swapped to a FIFO after NewStore's validation
	// from hanging startup. O_NOFOLLOW and the descriptor-level type check keep
	// the validation attached to the inode that is actually read.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %q: invalid file descriptor", path)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state path %q is not a regular file", path)
	}
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
	kept := make([]ipc.Item, 0, len(snap.Items))
	dropped := make(map[string]bool)
	var dropRows []DroppedItem
	for i := range snap.Items {
		it := &snap.Items[i]
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

		// The same byte bounds the live path enforces. A snapshot written by
		// an older build — or edited by hand — must not reintroduce a field
		// this build would never have accepted.
		it.Title = truncateEncoded(it.Title, MaxTitleBytes)
		it.ThumbURL = truncateEncoded(it.ThumbURL, MaxThumbURLBytes)
		it.Error = truncateEncoded(it.Error, MaxErrorBytes)
		// Filter first, then cap. Capping first would discard a valid path
		// simply because unusable ones happened to precede it.
		paths := it.Files[:0]
		for _, f := range it.Files {
			if encodedLen(f) <= MaxFilePathBytes {
				paths = append(paths, f)
			}
		}
		if len(paths) > MaxFilesPerItem {
			paths = paths[:MaxFilesPerItem]
		}
		it.Files = paths

		// Per-download options are re-validated on the way in exactly as they
		// were on the way out, because they become yt-dlp arguments either
		// way. What happens to a row that fails depends on whether it would
		// run again:
		//
		//   - a row that could still run is FAILED and marked, not silently
		//     repaired. Clearing its options would restart it with today's
		//     defaults rather than the quality, container or arguments that
		//     were actually asked for — a different download wearing the same
		//     row.
		//   - a finished row keeps its history and just loses the overrides,
		//     since nothing will re-read them.
		//
		// Neither case rejects the whole snapshot: losing an entire download
		// history to one unreadable field is worse than either outcome.
		it.Options = it.Options.Normalize()
		if err := it.Options.Validate(); err != nil {
			it.Options = ipc.Options{}
			// Every state that can be run again — which includes failed and
			// deleted, since both offer Retry and Start now — must carry the
			// marker. Only a completed row is safe to leave alone: nothing
			// will re-read its options.
			switch it.State {
			case ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused,
				ipc.StateFailed, ipc.StateDeleted:
				it.State = ipc.StateFailed
				it.OptionsInvalid = true
				it.Error = truncateEncoded("saved download options are no longer valid ("+
					err.Error()+"); re-add this URL with the options you want", MaxErrorBytes)
			}
		}

		// The same sub-budgets a new row must satisfy, not merely the total:
		// a row under 12 KiB today can still exceed it once its bounded title,
		// paths and error arrive, unless its URL and options were themselves
		// within their limits.
		if encodedLen(it.URL) > MaxURLBytes ||
			encodedLen(it.Options.Key()) > MaxOptionsBytes ||
			itemBytes(*it) > MaxItemBytes {
			dropped[it.ID] = true
			dropRows = append(dropRows, DroppedItem{Item: *it, Reason: DropTooLarge})
			continue
		}

		seen[it.ID] = it.State
		kept = append(kept, *it)
	}
	// Strict identity has to hold for restored state too. A snapshot written
	// before the rule — or by a build that allowed one URL at several
	// qualities — can carry duplicates, and resuming a paused row deliberately
	// skips the duplicate check, so nothing downstream would catch them. Two
	// rows for one URL means two downloads writing one file.
	byURL := make(map[string]bool, len(kept))
	deduped := make([]ipc.Item, 0, len(kept))
	for _, it := range kept {
		if blocksDuplicate(it.State) {
			if byURL[it.URL] {
				dropped[it.ID] = true
				dropRows = append(dropRows, DroppedItem{Item: it, Reason: DropDuplicate})
				continue
			}
			byURL[it.URL] = true
		}
		deduped = append(deduped, it)
	}
	kept = deduped

	// A snapshot from an older build can hold more rows than this one admits,
	// and every restored row is live after a restart (running work comes back
	// paused). Left alone they would grow as titles and paths arrive until the
	// snapshot no longer fits — the wedge the ceiling exists to prevent. The
	// excess is dropped here, oldest history first, so what survives is what
	// the queue would accept today.
	if len(kept) > MaxQueueItems {
		expendable := make([]int, 0, len(kept))
		other := make([]int, 0, len(kept))
		for i, it := range kept {
			if it.State == ipc.StateCompleted {
				expendable = append(expendable, i)
			} else {
				other = append(other, i)
			}
		}
		sort.SliceStable(expendable, func(a, b int) bool {
			return kept[expendable[a]].AddedAt.Before(kept[expendable[b]].AddedAt)
		})
		sort.SliceStable(other, func(a, b int) bool {
			return kept[other[a]].AddedAt.Before(kept[other[b]].AddedAt)
		})
		// History first, then the oldest of everything else if that is still
		// not enough.
		excess := len(kept) - MaxQueueItems
		for _, group := range [][]int{expendable, other} {
			for _, idx := range group {
				if excess == 0 {
					break
				}
				dropped[kept[idx].ID] = true
				dropRows = append(dropRows, DroppedItem{Item: kept[idx], Reason: DropQueueFull})
				excess--
			}
		}
		survivors := make([]ipc.Item, 0, MaxQueueItems)
		for _, it := range kept {
			if !dropped[it.ID] {
				survivors = append(survivors, it)
			}
		}
		kept = survivors
	}

	snap.Items = kept
	snap.Dropped = dropRows
	if len(dropped) > 0 {
		order := make([]string, 0, len(snap.Order))
		for _, id := range snap.Order {
			if !dropped[id] {
				order = append(order, id)
			}
		}
		snap.Order = order
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
