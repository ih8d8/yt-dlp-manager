package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/ipc"
)

var (
	ErrNotFound     = errors.New("no such item")
	ErrInvalidState = errors.New("invalid state for this operation")
	ErrShuttingDown = errors.New("manager is shutting down")
	// ErrDuplicate is the sentinel behind DuplicateError; match on it with
	// errors.Is when the concrete details are not needed.
	ErrDuplicate = errors.New("url is already tracked")
	// ErrQueueFull rejects an addition that would push the queue past
	// MaxQueueItems.
	ErrQueueFull = fmt.Errorf("the queue already holds its maximum of %d entries; "+
		"remove finished downloads to make room", MaxQueueItems)
)

// MaxQueueItems bounds how many entries the manager tracks at once. Without a
// ceiling, a handful of large playlists can grow the queue until the snapshot
// no longer fits maxStateBytes — at which point every save fails and the whole
// history is lost, rather than one addition being refused here.
const MaxQueueItems = 20000

// DuplicateError reports an Add rejected because a live entry already holds
// the same URL. It names that entry so callers can point at it.
type DuplicateError struct {
	ID    string
	State ipc.State
}

func (e *DuplicateError) Error() string {
	if e.State == ipc.StateCompleted {
		return "this URL has already been downloaded and is in your library; remove that entry to download it again"
	}
	return fmt.Sprintf("this URL is already in the queue (%s)", e.State)
}

// Is makes errors.Is(err, ErrDuplicate) work for the typed error.
func (e *DuplicateError) Is(target error) bool { return target == ErrDuplicate }

type item struct {
	ipc.Item
	expanding bool
	probed    bool
	cancel    context.CancelFunc
	stop      string
	remove    bool
	lastPush  time.Time
}

type Manager struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	max       int
	store     *Store
	runner    Runner

	mu sync.Mutex
	// publishMu serializes overflow recovery, where a stale subscriber
	// backlog is replaced by one authoritative snapshot.
	publishMu sync.Mutex
	items     map[string]*item
	order     []string
	nrun      int
	nexp      int
	subs      map[chan ipc.Event]struct{}

	// Event ordering: every mutation stamps a sequence number while holding
	// mu; a queued "update" whose seq predates a recorded removal of the same
	// item is suppressed on publish. This makes update-after-removed races
	// (which would resurrect ghost rows in the UI) impossible.
	seq       uint64
	removedAt map[string]uint64
	closing   bool

	dirty     chan struct{}
	saverStop chan struct{}
	saverDone chan struct{}

	// Last persistence outcome, read by SaveError (and so by /readyz) from
	// goroutines other than the saver.
	saveErrMu    sync.Mutex
	saveErr      error
	shutdownOnce sync.Once
	closeDone    chan struct{}
	wg           sync.WaitGroup
}

func New(ctx context.Context, max int, storePath string) (*Manager, error) {
	return newManager(ctx, max, storePath, YtdlpRunner{})
}

// NewWithRunner is like New but lets tests inject a fake yt-dlp runner.
func NewWithRunner(ctx context.Context, max int, storePath string, r Runner) (*Manager, error) {
	return newManager(ctx, max, storePath, r)
}

func newManager(ctx context.Context, max int, storePath string, r Runner) (*Manager, error) {
	max = ClampMax(max)
	st, err := NewStore(storePath)
	if err != nil {
		return nil, err
	}
	managerCtx, cancelCtx := context.WithCancel(ctx)
	m := &Manager{
		ctx:       managerCtx,
		cancelCtx: cancelCtx,
		max:       max,
		store:     st,
		runner:    r,
		items:     make(map[string]*item),
		subs:      make(map[chan ipc.Event]struct{}),
		dirty:     make(chan struct{}, 1),
		closeDone: make(chan struct{}),
	}
	m.restore()
	m.saverStop = make(chan struct{})
	m.saverDone = make(chan struct{})
	go m.saver()
	m.schedule()
	return m, nil
}

func (m *Manager) restore() {
	snap, err := m.store.Load()
	if err != nil {
		if !errors.Is(err, ErrNoState) {
			if target := m.store.Quarantine(); target != "" {
				fmt.Fprintf(os.Stderr, "yt-dlp-manager: unreadable state moved to %s\n", target)
			}
		}
		return
	}
	now := time.Now()
	for _, it := range snap.Items {
		originalState := it.State
		// Neither work that was active nor work that was merely waiting may
		// start by itself after a process/container restart. The operator has
		// to resume it explicitly once the service is back. UserPaused stays
		// false so clients can distinguish recovery pauses from deliberate
		// user pauses.
		if originalState == ipc.StateDownloading || originalState == ipc.StateQueued {
			it.State = ipc.StatePaused
			it.UserPaused = false
		}
		switch it.State {
		case ipc.StateQueued, ipc.StatePaused, ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
		default:
			continue
		}
		if it.AddedAt.IsZero() || it.AddedAt.After(now) {
			it.AddedAt = now
		}
		it.Speed, it.ETA = 0, 0
		it.Forced = false
		switch it.State {
		case ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
			// Terminal rows are history: the failure reason and the
			// completion time are the whole point of keeping them. Wiping
			// these made Library sorting fall back to added_at and made
			// every past failure look reasonless after a restart.
		default:
			it.StartedAt, it.DoneAt, it.Error = nil, nil, ""
		}
		if it.State != ipc.StateCompleted {
			hydrateFromDisk(&it)
		} else {
			it.Progress = 100
		}
		if (it.State == ipc.StateCompleted || it.State == ipc.StatePaused) &&
			len(it.Files) > 0 && !filesExist(it.Files) {
			it.State = ipc.StateDeleted
			it.Got, it.Total, it.Progress = 0, 0, 0
		}
		// A bare queued placeholder may not have completed metadata probing
		// before shutdown. Although it is now represented as paused, preserve
		// that fact so an explicit resume probes it before trying to download.
		probed := originalState != ipc.StateQueued || it.Title != "" || len(it.Files) > 0
		m.items[it.ID] = &item{Item: it, probed: probed}
	}
	ordered := make(map[string]struct{}, len(snap.Order))
	for _, id := range snap.Order {
		if it, ok := m.items[id]; ok && it.State == ipc.StateQueued {
			m.order = append(m.order, id)
			ordered[id] = struct{}{}
		}
	}
	for id, it := range m.items {
		if _, ok := ordered[id]; it.State == ipc.StateQueued && !ok {
			m.order = append(m.order, id)
			ordered[id] = struct{}{}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Save retry pacing. A failed save leaves the queue's only durable copy
// stale, so it is retried on its own rather than waiting for the next
// mutation to happen along — a disk that filled up while the user was adding
// downloads would otherwise silently drop everything since the last good
// snapshot at restart.
const (
	saveDebounce   = 400 * time.Millisecond
	saveRetryFirst = 2 * time.Second
	saveRetryMax   = 60 * time.Second
)

func (m *Manager) saver() {
	defer close(m.saverDone)
	// Zero while saves are succeeding; the current backoff after a failure.
	var retryIn time.Duration
	for {
		select {
		case <-m.dirty:
			// Debounce: coalesce bursts of touches into one save. The wait
			// must be interruptible so Close can quiesce all writers.
			wait := saveDebounce
			if retryIn > 0 {
				wait = retryIn
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
				if m.flush() {
					retryIn = 0
					break
				}
				// Keep the pending state marked dirty so the loop comes back
				// to it, and slow down so a persistent failure (full disk,
				// read-only mount) does not spin or flood the log.
				if retryIn *= 2; retryIn == 0 {
					retryIn = saveRetryFirst
				} else if retryIn > saveRetryMax {
					retryIn = saveRetryMax
				}
				m.touch()
			case <-m.saverStop:
				timer.Stop()
				return
			case <-m.ctx.Done():
				timer.Stop()
				m.flush()
				return
			}
		case <-m.saverStop:
			return
		case <-m.ctx.Done():
			m.flush()
			return
		}
	}
}

// flush writes the current queue to disk and reports whether it succeeded.
// The failure is recorded rather than only printed: an acknowledged change
// that never reached disk has to be visible to /readyz, not just to whoever
// happens to be reading stderr.
func (m *Manager) flush() bool {
	items, order := m.snapshotRaw()
	err := m.store.Save(items, order)
	m.saveErrMu.Lock()
	prev := m.saveErr
	m.saveErr = err
	m.saveErrMu.Unlock()
	if err != nil {
		// One line per distinct failure, not per retry.
		if prev == nil || prev.Error() != err.Error() {
			fmt.Fprintf(os.Stderr, "yt-dlp-manager: save state: %v\n", err)
		}
		return false
	}
	if prev != nil {
		fmt.Fprintf(os.Stderr, "yt-dlp-manager: save state: recovered, queue persisted\n")
	}
	return true
}

// SaveError returns the error from the most recent attempt to persist the
// queue, or nil when the on-disk snapshot is current.
func (m *Manager) SaveError() error {
	m.saveErrMu.Lock()
	defer m.saveErrMu.Unlock()
	return m.saveErr
}

func (m *Manager) touch() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

func (m *Manager) Subscribe() (<-chan ipc.Event, func()) {
	ch := make(chan ipc.Event, 1024)
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		// Late subscriber during shutdown gets a closed stream immediately;
		// consumers already treat a closed channel as "stream ended".
		m.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	items := make([]ipc.Item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, it.Item)
	}
	// Queue the snapshot while holding the same lock used to register the
	// subscriber. Every later mutation is therefore delivered after it.
	ch <- ipc.Event{Event: "snapshot", Items: items}
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}
}

func (m *Manager) broadcast(ev ipc.Event) {
	m.publish(0, ev)
}

// stampLocked allocates the next sequence number. Caller must hold m.mu.
func (m *Manager) stampLocked() uint64 {
	m.seq++
	return m.seq
}

// noteRemovedLocked records when an item was removed. Caller must hold m.mu.
func (m *Manager) noteRemovedLocked(id string) {
	if len(m.removedAt) > 16384 {
		// IDs are never reused, so old entries can only be superseded by
		// newer ones; a wholesale reset merely weakens suppression for
		// ancient IDs and keeps the map bounded.
		m.removedAt = nil
	}
	if m.removedAt == nil {
		m.removedAt = make(map[string]uint64)
	}
	m.removedAt[id] = m.seq
}

// publish delivers ev to all subscribers. seq is the mutation's sequence
// stamp (0 for removal events, which are never suppressed). Progress updates
// may be dropped under load — the next one supersedes them — but a full
// subscriber that misses a structural/state event is recovered with a fresh
// snapshot instead of being left permanently stale.
func (m *Manager) publish(seq uint64, ev ipc.Event) {
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	m.mu.Lock()
	if seq != 0 && ev.Event == "update" && ev.Item != nil {
		if rs, ok := m.removedAt[ev.Item.ID]; ok && rs >= seq {
			m.mu.Unlock()
			return // update predates this item's removal: never resurrect it
		}
	}
	subs := make([]chan ipc.Event, 0, len(m.subs))
	for ch := range m.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()

	droppable := ev.IsProgress()
	var recovery *ipc.Event
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			if droppable {
				continue
			}
			// A terminal/structural event must never disappear behind stale
			// progress. Drain this subscriber's backlog and replace it with a
			// current snapshot, which subsumes every pending update/removal.
			for {
				select {
				case <-ch:
					continue
				default:
				}
				break
			}
			if recovery == nil {
				items, _ := m.snapshotRaw()
				snap := ipc.Event{Event: "snapshot", Items: items}
				recovery = &snap
			}
			select {
			case ch <- *recovery:
			default:
			}
		}
	}
}

func (m *Manager) push(it *item) {
	m.mu.Lock()
	// Liveness re-check: the item may have been removed between the mutation
	// that produced it and this publish (e.g. Add racing ClearAll, or a
	// playlist child cancelled during expansion). Stamping a fresh sequence
	// would defeat tombstone suppression and resurrect a ghost row.
	if _, live := m.items[it.ID]; !live {
		m.mu.Unlock()
		return
	}
	cp := it.Item
	seq := m.stampLocked()
	m.mu.Unlock()
	m.publish(seq, ipc.Event{Event: "update", Item: &cp})
}

var randomRead = rand.Read

func newID() (string, error) {
	var b [4]byte
	if _, err := randomRead(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// freshIDLocked returns an ID guaranteed not to collide with any live item.
// Caller must hold m.mu.
func (m *Manager) freshIDLocked() (string, error) {
	for {
		id, err := newID()
		if err != nil {
			return "", err
		}
		if _, taken := m.items[id]; !taken {
			return id, nil
		}
	}
}

func ValidURL(u string) bool { return ipc.ValidURL(u) }

// MaxForcedExtra is how many force-started ("start now") downloads may run
// beyond max_concurrent. Forced items jump the queue by design, but without a
// ceiling one batch request — the HTTP API accepts 500 ids — could launch
// hundreds of yt-dlp processes at once and exhaust memory, file descriptors or
// bandwidth. Beyond the ceiling a forced item simply stays queued and starts
// as soon as a slot frees, which is what the user asked for anyway.
const MaxForcedExtra = 8

// hasCapacityLocked reports whether another run or probe may start now.
// Caller must hold m.mu.
func (m *Manager) hasCapacityLocked(forced bool) bool {
	limit := m.max
	if forced {
		limit += MaxForcedExtra
	}
	return m.nrun+m.nexp < limit
}

// ClampMax confines a concurrency setting to the supported range. Callers
// that can report a bad value to the user should validate first; this is the
// last line of defence so no path can start an unbounded number of children.
func ClampMax(n int) int {
	if n < config.MinConcurrent {
		return config.MinConcurrent
	}
	if n > config.MaxConcurrent {
		return config.MaxConcurrent
	}
	return n
}

func DefaultMax() int {
	// YTDLP_MANAGER_MAX_CONCURRENT is the supported variable. The legacy
	// YTDLPTUI_ name is honored only as a silent fallback for pre-rename
	// deployments; it is intentionally not announced on every start.
	if v := os.Getenv("YTDLP_MANAGER_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return ClampMax(n)
		}
	}
	if v := os.Getenv("YTDLPTUI_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return ClampMax(n)
		}
	}
	return 20
}

// blocksDuplicate reports whether an entry in this state still stands for its
// URL, so re-adding the same URL would download the same thing twice.
//
// Failed and deleted entries deliberately do NOT block: pasting the URL again
// is a normal way to retry one, and an entry the user already cleared is gone.
func blocksDuplicate(s ipc.State) bool {
	switch s {
	case ipc.StateQueued, ipc.StateDownloading, ipc.StatePaused, ipc.StateCompleted:
		return true
	}
	return false
}

// findDuplicateLocked returns the error for an existing live entry holding
// rawURL, or nil when the URL is free. `exclude` skips one id, so an item can
// ask whether anyone ELSE holds its URL. Comparison is exact on the trimmed
// URL: the case this guards is an accidental re-paste, and guessing that two
// differently-spelled URLs name the same video is the extractor's job, not
// ours. Caller must hold m.mu.
func (m *Manager) findDuplicateLocked(rawURL, exclude string) *DuplicateError {
	for id, it := range m.items {
		if id == exclude {
			continue
		}
		if it.URL == rawURL && blocksDuplicate(it.State) {
			return &DuplicateError{ID: id, State: it.State}
		}
	}
	return nil
}

// blockingURLsLocked is findDuplicateLocked's bulk form: the set of URLs that
// would refuse a duplicate, gathered once so checking a whole playlist stays
// linear instead of scanning the queue per entry. Caller must hold m.mu.
func (m *Manager) blockingURLsLocked(exclude string) map[string]struct{} {
	urls := make(map[string]struct{}, len(m.items))
	for id, it := range m.items {
		if id == exclude || !blocksDuplicate(it.State) {
			continue
		}
		urls[it.URL] = struct{}{}
	}
	return urls
}

func (m *Manager) Add(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if !ValidURL(rawURL) {
		return "", errors.New("invalid url")
	}
	it := &item{Item: ipc.Item{State: ipc.StateQueued, AddedAt: time.Now()}}
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return "", ErrShuttingDown
	}
	if dup := m.findDuplicateLocked(rawURL, ""); dup != nil {
		m.mu.Unlock()
		return "", dup
	}
	if len(m.items) >= MaxQueueItems {
		m.mu.Unlock()
		return "", ErrQueueFull
	}
	id, err := m.freshIDLocked()
	if err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("generate item id: %w", err)
	}
	it.ID = id
	it.URL = rawURL
	m.items[it.ID] = it
	m.order = append(m.order, it.ID)
	m.mu.Unlock()
	m.push(it)
	m.touch()
	m.schedule()
	return it.ID, nil
}

func (m *Manager) expand(it *item, ctx context.Context, cancel context.CancelFunc) {
	defer m.wg.Done()
	url := it.URL

	entries, perr := m.runner.Probe(ctx, url)

	m.mu.Lock()
	if _, live := m.items[it.ID]; !live {
		it.expanding = false
		it.cancel = nil
		m.nexp--
		m.mu.Unlock()
		cancel()
		return
	}
	it.expanding = false
	it.cancel = nil
	m.nexp--

	removed := it.remove
	var removedID string
	if removed {
		removedID = it.ID
		m.removeItemLocked(it)
	}
	stale := it.stop != "" || it.State != ipc.StateQueued
	if stale && it.State == ipc.StatePaused {
		// A user pause during metadata probing must retry the probe on resume;
		// otherwise a playlist placeholder could be run as one download.
		it.probed = false
	}

	switch {
	case removed:
		m.mu.Unlock()
		m.publish(0, ipc.Event{Event: "removed", ID: removedID})
		m.touch()
	case stale:
		m.mu.Unlock()
		m.touch()
	case m.ctx.Err() != nil && perr != nil:
		// Keep a shutdown-interrupted probe recoverable. The persisted queued
		// row is restored paused on the next launch and probed again only after
		// the operator explicitly resumes it; cancellation is not a failure.
		it.probed = false
		m.mu.Unlock()
		m.touch()
	case perr != nil:
		it.State = ipc.StateFailed
		it.Error = truncate(sanitize(strings.TrimSpace(perr.Error())), 300)
		done := time.Now()
		it.DoneAt = &done
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		m.touch()
	case len(entries) == 0:
		it.State = ipc.StateFailed
		it.Error = "no downloadable entries found"
		done := time.Now()
		it.DoneAt = &done
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		m.touch()
	case len(entries) == 1:
		it.Title = sanitize(entries[0].Title)
		it.ThumbURL = sanitize(entries[0].Thumbnail)
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		m.touch()
	default:
		parentID := it.ID
		added := time.Now()
		children := make([]*item, 0, len(entries))
		// A playlist routinely lists a video the queue already holds, and can
		// list the same video twice itself. Either would point two yt-dlp runs
		// at one output file and its .part, so entries are deduplicated
		// against the live queue AND against each other before any child is
		// inserted — the same guarantee Add gives, which children would
		// otherwise bypass by never going through it.
		//
		// The parent placeholder is excluded: it is about to be replaced by
		// exactly these children.
		taken := m.blockingURLsLocked(parentID)
		skipped := 0
		// Entries dropped because the queue is already at MaxQueueItems. The
		// probe is capped at MaxPlaylistEntries, but several playlists can
		// still add up, and children bypass Add's own ceiling.
		dropped := 0
		for i, e := range entries {
			// The parent placeholder is about to be deleted, so it does not
			// count against the ceiling the children have to fit under.
			if len(m.items)-1 >= MaxQueueItems {
				dropped = len(entries) - i
				break
			}
			// Child URLs come from the remote page's metadata, so they get the
			// same validation an operator-supplied URL does. An invalid one
			// would otherwise enter the queue and later make the whole
			// persisted snapshot fail validateSnapshot on restart, costing
			// the entire history rather than one row.
			//
			// The trimmed form is what gets stored, so validation, the
			// duplicate check and the persisted row all agree on one spelling.
			childURL := strings.TrimSpace(e.URL)
			if !ipc.ValidURL(childURL) {
				continue
			}
			if _, dup := taken[childURL]; dup {
				skipped++
				continue
			}
			taken[childURL] = struct{}{}
			id, err := m.freshIDLocked()
			if err != nil {
				for _, child := range children {
					delete(m.items, child.ID)
				}
				it.State = ipc.StateFailed
				it.Error = truncate(sanitize("generate playlist item id: "+err.Error()), 300)
				done := time.Now()
				it.DoneAt = &done
				cp := it.Item
				seq := m.stampLocked()
				m.mu.Unlock()
				m.publish(seq, ipc.Event{Event: "update", Item: &cp})
				m.touch()
				cancel()
				m.schedule()
				return
			}
			child := &item{probed: true, Item: ipc.Item{
				ID:       id,
				URL:      childURL,
				Title:    sanitize(e.Title),
				ThumbURL: sanitize(e.Thumbnail),
				State:    ipc.StateQueued,
				AddedAt:  added,
			}}
			m.items[child.ID] = child
			children = append(children, child)
		}
		if len(children) == 0 {
			it.State = ipc.StateFailed
			if skipped > 0 {
				// Deliberately not "every entry": invalid entries are dropped
				// on a separate path, so `skipped` is only the duplicates.
				it.Error = fmt.Sprintf(
					"nothing new to queue: %d entries are already queued or downloaded", skipped)
			} else {
				it.Error = "no valid entries found in playlist"
			}
			done := time.Now()
			it.DoneAt = &done
			cp := it.Item
			seq := m.stampLocked()
			m.mu.Unlock()
			m.publish(seq, ipc.Event{Event: "update", Item: &cp})
			m.touch()
			cancel()
			m.schedule()
			return
		}
		m.noteRemovedLocked(parentID)
		delete(m.items, parentID)
		order := make([]string, 0, len(m.order)+len(children))
		for _, id := range m.order {
			if id != parentID {
				order = append(order, id)
			}
		}
		for _, c := range children {
			order = append(order, c.ID)
		}
		m.order = order
		m.mu.Unlock()
		if dropped > 0 {
			fmt.Fprintf(os.Stderr,
				"yt-dlp-manager: playlist expansion queued %d entries and dropped %d: "+
					"the queue is at its %d entry limit\n", len(children), dropped, MaxQueueItems)
		}
		m.publish(0, ipc.Event{Event: "removed", ID: parentID})
		for _, c := range children {
			m.push(c)
		}
		m.touch()
	}
	cancel()
	m.schedule()
}

type startJob struct {
	it     *item
	ctx    context.Context
	cancel context.CancelFunc
}

type probeJob struct {
	it     *item
	ctx    context.Context
	cancel context.CancelFunc
}

func (m *Manager) schedule() {
	// Shutdown discipline: never spawn new work once the manager's context
	// is cancelled. Otherwise every finishing run during Close() would
	// re-enter here and cascade-start the whole queue against a dead
	// context (and race wg.Add against Close's wg.Wait).
	if m.ctx.Err() != nil {
		return
	}
	type snap struct {
		item ipc.Item
		seq  uint64
	}
	var probes []probeJob
	var jobs []startJob
	var updates []snap
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	kept := m.order[:0]
	for _, id := range m.order {
		it, ok := m.items[id]
		if !ok || it.remove {
			continue
		}
		if it.expanding || it.State != ipc.StateQueued {
			kept = append(kept, id)
			continue
		}
		if !it.probed {
			if m.hasCapacityLocked(it.Forced) {
				m.nexp++
				ctx, cancel := context.WithCancel(m.ctx)
				it.cancel = cancel
				it.expanding = true
				it.probed = true
				probes = append(probes, probeJob{it: it, ctx: ctx, cancel: cancel})
			}
			kept = append(kept, id)
			continue
		}
		if m.hasCapacityLocked(it.Forced) {
			m.nrun++
			ctx, cancel := context.WithCancel(m.ctx)
			it.cancel = cancel
			it.State = ipc.StateDownloading
			jobs = append(jobs, startJob{it: it, ctx: ctx, cancel: cancel})
			updates = append(updates, snap{item: it.Item, seq: m.stampLocked()})
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
	m.wg.Add(len(probes) + len(jobs))
	m.mu.Unlock()
	for _, p := range probes {
		go m.expand(p.it, p.ctx, p.cancel)
	}
	for _, j := range jobs {
		go m.run(j.it, j.ctx, j.cancel)
	}
	for _, u := range updates {
		m.publish(u.seq, ipc.Event{Event: "update", Item: &u.item})
	}
}

func (m *Manager) run(it *item, ctx context.Context, cancel context.CancelFunc) {
	defer m.wg.Done()

	started := time.Now()

	m.mu.Lock()
	it.StartedAt = &started
	it.Error = ""
	url := it.URL
	cp := it.Item
	seq := m.stampLocked()
	m.mu.Unlock()
	m.publish(seq, ipc.Event{Event: "update", Item: &cp})

	tail, exitErr := m.runner.Run(ctx, url, func(line string) { m.onLine(it, line) })
	cancel()

	m.mu.Lock()
	it.cancel = nil
	reason := it.stop
	it.stop = ""
	done := time.Now()
	it.DoneAt = &done
	it.Speed = 0
	it.ETA = 0
	it.Forced = false
	m.nrun--

	switch {
	case it.remove || reason == "cancel":
		id := it.ID
		// Only this item's own scratch, and only where no surviving item
		// records the same base — another row may still be downloading it.
		files := filterSharedFiles(it.Files, m.filesHeldByOthersLocked(it))
		m.mu.Unlock()
		// Filesystem work stays outside the lock, but the row must not
		// disappear before its data does: observers (tests, UI removal
		// events) rely on partials being gone once the row is gone.
		cleanupErr := cleanupPartials(files)
		m.mu.Lock()
		_, stillLive := m.items[id]
		if cleanupErr != nil && stillLive {
			it.remove = false
			it.State = ipc.StateFailed
			it.Error = truncate(sanitize("cleanup: "+cleanupErr.Error()), 300)
			cp := it.Item
			seq := m.stampLocked()
			m.mu.Unlock()
			m.publish(seq, ipc.Event{Event: "update", Item: &cp})
			m.touch()
			m.schedule()
			return
		}
		if stillLive {
			it.Files = nil
			m.removeItemLocked(it)
		}
		m.mu.Unlock()
		if stillLive {
			m.publish(0, ipc.Event{Event: "removed", ID: id})
		}
		m.touch()
	case exitErr == nil:
		it.State = ipc.StateCompleted
		it.Progress = 100
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		m.touch()
	default:
		switch {
		case m.ctx.Err() != nil:
			it.State = ipc.StatePaused
			it.UserPaused = false
			it.DoneAt = nil
		case reason == "pause":
			it.State = ipc.StatePaused
			// Pausing is not finishing. DoneAt is surfaced as completed_at,
			// so leaving it set made paused rows look complete.
			it.DoneAt = nil
		default:
			it.State = ipc.StateFailed
			if t := strings.TrimSpace(tail); t != "" {
				it.Error = truncate(sanitize(t), 300)
			} else {
				it.Error = truncate(sanitize(exitErr.Error()), 300)
			}
		}
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		m.touch()
	}
	m.schedule()
}

func (m *Manager) onLine(it *item, line string) {
	switch {
	case strings.HasPrefix(line, "@p|"):
		f := strings.SplitN(strings.TrimPrefix(line, "@p|"), "|", 5)
		if len(f) < 4 {
			return
		}
		got := ParseNum(f[0])
		total := ParseNum(f[1])
		speed := ParseNum(f[2])
		eta := ParseNum(f[3])
		reportedProgress := -1.0
		if len(f) == 5 {
			reportedProgress = parsePercent(f[4])
		}

		m.mu.Lock()
		if got >= 0 {
			it.Got = got
		}
		if total >= 0 {
			it.Total = total
		}
		if speed >= 0 {
			it.Speed = speed
		}
		if eta >= 0 {
			it.ETA = eta
		}
		if reportedProgress >= 0 {
			it.Progress = min(reportedProgress, 100)
		} else if it.Total > 0 {
			p := float64(it.Got) / float64(it.Total) * 100
			if p > 100 {
				p = 100
			}
			it.Progress = p
		}
		now := time.Now()
		var cp ipc.Item
		var seq uint64
		due := now.Sub(it.lastPush) >= 100*time.Millisecond
		if due {
			it.lastPush = now
			cp = it.Item
			seq = m.stampLocked()
		}
		m.mu.Unlock()

		if due {
			m.publish(seq, ipc.Event{Event: "update", Item: &cp})
		}
	case strings.HasPrefix(line, "@t|"):
		title, ok := decodePrintedString(strings.TrimPrefix(line, "@t|"), true)
		if !ok {
			return
		}
		title = strings.TrimSpace(title)
		if title == "" {
			return
		}
		title = sanitize(title)
		m.mu.Lock()
		if it.Title == title {
			m.mu.Unlock()
			return
		}
		it.Title = title
		cp := it.Item
		seq := m.stampLocked()
		m.mu.Unlock()
		m.publish(seq, ipc.Event{Event: "update", Item: &cp})
	case strings.HasPrefix(line, "@f|"), strings.HasPrefix(line, "@g|"):
		tag := "@f|"
		if strings.HasPrefix(line, "@g|") {
			tag = "@g|"
		}
		// Paths must arrive JSON-encoded. Unlike the title, a bare literal is
		// NOT accepted here: these values decide which scratch files get
		// removed on cancel, so anything that did not come from our own "j"
		// template is discarded rather than trusted.
		path, ok := decodePrintedString(strings.TrimPrefix(line, tag), false)
		if !ok {
			return
		}
		path = strings.TrimSpace(path)
		if path == "" || !isSafeBase(path) {
			return
		}
		m.mu.Lock()
		if !contains(it.Files, path) {
			next := make([]string, 0, len(it.Files)+1)
			next = append(next, it.Files...)
			next = append(next, path)
			it.Files = next
		}
		m.mu.Unlock()
	}
}

// PrintLine renders one line of the stdout protocol the runner asks yt-dlp to
// emit ("@t|", "@f|", "@g|"), with the value JSON-encoded exactly as the
// "%(field)j" template does. It exists so the framing has a single definition
// that adapters' fake runners share instead of hand-rolling it.
func PrintLine(tag, value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return tag
	}
	return tag + string(encoded)
}

// decodePrintedString decodes one value emitted by a "%(field)j" template.
// allowLiteral controls the fallback for a payload that is not a JSON string:
// display-only fields tolerate it (an older yt-dlp still shows a title), while
// security-relevant fields such as recorded file paths do not.
func decodePrintedString(raw string, allowLiteral bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, `"`) {
		var v string
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return "", false
		}
		if naValue(v) {
			return "", false
		}
		return v, true
	}
	if naValue(raw) || !allowLiteral {
		return "", false
	}
	return raw, true
}

func parsePercent(raw string) float64 {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	p, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(p) || p < 0 {
		return -1
	}
	return p
}

func (m *Manager) Pause(id string) error {
	var (
		cp        *ipc.Item
		seq       uint64
		cancelNow context.CancelFunc
		removed   bool
	)
	err := m.with(id, func(it *item) error {
		switch it.State {
		case ipc.StateQueued:
			it.State = ipc.StatePaused
			it.UserPaused = true
			if it.expanding {
				it.stop = "pause"
				cancelNow = it.cancel
				return nil
			}
			c := it.Item
			cp = &c
			seq = m.stampLocked()
			return nil
		case ipc.StateDownloading:
			if it.cancel == nil {
				m.removeItemLocked(it)
				removed = true
				return nil
			}
			it.stop = "pause"
			it.UserPaused = true
			cancelNow = it.cancel
			return nil
		default:
			return ErrInvalidState
		}
	})
	if err != nil {
		return err
	}
	if cancelNow != nil {
		cancelNow()
	}
	switch {
	case removed:
		m.publish(0, ipc.Event{Event: "removed", ID: id})
		m.touch()
	case cp != nil:
		m.publish(seq, ipc.Event{Event: "update", Item: cp})
		m.touch()
	}
	m.schedule()
	return nil
}

func (m *Manager) Resume(id string) error {
	var cp *ipc.Item
	var seq uint64
	err := m.with(id, func(it *item) error {
		if it.State != ipc.StatePaused && it.State != ipc.StateFailed && it.State != ipc.StateDeleted {
			return ErrInvalidState
		}
		// Failed and deleted rows deliberately do NOT reserve their URL — that
		// is what lets a user re-add one — so between the failure and this
		// retry another entry may have claimed it. Re-queueing regardless
		// would aim two yt-dlp runs at the same output file, which is exactly
		// what Add refuses; "Retry all failed" makes that easy to trigger in
		// bulk. Paused rows still hold their URL and are not re-checked: a
		// duplicate pair restored from an older state file must stay resumable
		// rather than becoming permanently stuck.
		if it.State == ipc.StateFailed || it.State == ipc.StateDeleted {
			if dup := m.findDuplicateLocked(it.URL, id); dup != nil {
				return dup
			}
		}
		it.State = ipc.StateQueued
		it.Error = ""
		it.stop = ""
		it.UserPaused = false
		// The row is about to run again: its previous outcome no longer
		// applies, and a stale DoneAt would render as completed_at.
		it.StartedAt, it.DoneAt = nil, nil
		if !contains(m.order, id) {
			m.order = append(m.order, id)
		}
		c := it.Item
		cp = &c
		seq = m.stampLocked()
		return nil
	})
	if err != nil {
		return err
	}
	m.publish(seq, ipc.Event{Event: "update", Item: cp})
	m.touch()
	m.schedule()
	return nil
}

func (m *Manager) StartNow(id string) error {
	var cp *ipc.Item
	var seq uint64
	err := m.with(id, func(it *item) error {
		if it.State != ipc.StateQueued && it.State != ipc.StatePaused && it.State != ipc.StateFailed && it.State != ipc.StateDeleted {
			return ErrInvalidState
		}
		// Failed and deleted rows deliberately do NOT reserve their URL — that
		// is what lets a user re-add one — so between the failure and this
		// retry another entry may have claimed it. Re-queueing regardless
		// would aim two yt-dlp runs at the same output file, which is exactly
		// what Add refuses; "Retry all failed" makes that easy to trigger in
		// bulk. Paused rows still hold their URL and are not re-checked: a
		// duplicate pair restored from an older state file must stay resumable
		// rather than becoming permanently stuck.
		if it.State == ipc.StateFailed || it.State == ipc.StateDeleted {
			if dup := m.findDuplicateLocked(it.URL, id); dup != nil {
				return dup
			}
		}
		it.State = ipc.StateQueued
		it.Forced = true
		it.stop = ""
		it.UserPaused = false
		// Same reset as Resume: the row is about to run again, so its previous
		// outcome no longer applies. Leaving these made a force-started retry
		// report an old error and a completed_at while it was downloading.
		it.Error = ""
		it.StartedAt, it.DoneAt = nil, nil
		if !contains(m.order, id) {
			m.order = append(m.order, id)
		}
		c := it.Item
		cp = &c
		seq = m.stampLocked()
		return nil
	})
	if err != nil {
		return err
	}
	m.publish(seq, ipc.Event{Event: "update", Item: cp})
	m.touch()
	m.schedule()
	return nil
}

func (m *Manager) Cancel(id string) error { return m.stopOrRemove(id) }

func (m *Manager) Remove(id string) error { return m.stopOrRemove(id) }

func (m *Manager) stopOrRemove(id string) error {
	var (
		removed   bool
		cancelNow context.CancelFunc
		scratch   []string
	)
	err := m.with(id, func(it *item) error {
		// Capture the scratch to clear while still holding the lock, so the
		// shared-file check sees a consistent view of the other rows.
		drop := func() {
			scratch = filterSharedFiles(it.Files, m.filesHeldByOthersLocked(it))
			m.removeItemLocked(it)
			removed = true
		}
		switch it.State {
		case ipc.StateQueued:
			if it.expanding {
				it.remove = true
				cancelNow = it.cancel
				return nil
			}
			drop()
			return nil
		case ipc.StateDownloading:
			if it.cancel == nil {
				drop()
				return nil
			}
			// A live child cleans up in run() once it has actually exited.
			it.remove = true
			cancelNow = it.cancel
			return nil
		case ipc.StatePaused, ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
			drop()
			return nil
		default:
			return ErrInvalidState
		}
	})
	if err != nil {
		return err
	}
	if cancelNow != nil {
		cancelNow()
		return nil
	}
	if removed {
		// Removing a row that is not running still has to clear its leftovers.
		// yt-dlp abandons ".part"/".ytdl" (and per-format intermediates) when a
		// download is interrupted, and nothing else will ever come back for
		// them once the row is gone.
		cleanupScratch(scratch)
		m.broadcast(ipc.Event{Event: "removed", ID: id})
		m.touch()
	}
	return nil
}

// cleanupScratch clears yt-dlp working files best-effort. A failure here is
// deliberately not surfaced: the row is already gone, the finished media is
// untouched either way, and a leftover temp file is not worth an error the
// user cannot act on.
func cleanupScratch(files []string) {
	if len(files) == 0 {
		return
	}
	if err := cleanupPartials(files); err != nil {
		fmt.Fprintf(os.Stderr, "yt-dlp-manager: clear partial data: %v\n", err)
	}
}

// filesHeldByOthersLocked returns the set of file paths recorded by any
// other live item. Caller must hold m.mu.
func (m *Manager) filesHeldByOthersLocked(self *item) map[string]struct{} {
	held := make(map[string]struct{})
	for _, other := range m.items {
		if other == self {
			continue
		}
		for _, f := range other.Files {
			if isSafeBase(f) {
				held[f] = struct{}{}
			}
		}
	}
	return held
}

// filterSharedFiles drops bases that another live item also records. yt-dlp can
// reuse the same output name across attempts recorded on different rows, and the
// surviving owner may still be mid-download — cleaning up "our" partials would
// then delete its working file.
func filterSharedFiles(files []string, held map[string]struct{}) []string {
	if len(held) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, shared := held[f]; !shared {
			out = append(out, f)
		}
	}
	return out
}

func (m *Manager) ClearAll() (int, error) {
	type kill struct {
		cancel context.CancelFunc
	}
	var (
		kills      []kill
		removedIDs []string
		scratch    []string
	)
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return 0, ErrShuttingDown
	}
	refs := m.fileReferenceCountsLocked()
	for id, it := range m.items {
		switch {
		case it.State == ipc.StateDownloading && it.cancel != nil:
			it.remove = true
			kills = append(kills, kill{cancel: it.cancel})
		case it.expanding && it.cancel != nil:
			it.remove = true
			kills = append(kills, kill{cancel: it.cancel})
		default:
			scratch = append(scratch, releaseFiles(it.Files, refs)...)
			m.noteRemovedLocked(it.ID)
			delete(m.items, it.ID)
			removedIDs = append(removedIDs, id)
		}
	}
	m.order = nil
	m.mu.Unlock()
	cleanupScratch(scratch)

	for _, id := range removedIDs {
		m.broadcast(ipc.Event{Event: "removed", ID: id})
	}
	for _, k := range kills {
		k.cancel()
	}
	if len(removedIDs)+len(kills) > 0 {
		m.touch()
	}
	// Both halves are reported: rows already gone, plus rows whose download was
	// asked to stop and which disappear when the child exits.
	return len(removedIDs) + len(kills), nil
}

// ClearAllDetailed is ClearAll with the two halves separated, so an adapter can
// say "removed N, stopping M" instead of implying M rows are already gone.
func (m *Manager) ClearAllDetailed() (removed, stopping int, err error) {
	before := m.countRunningCancellable()
	total, err := m.ClearAll()
	if err != nil {
		return 0, 0, err
	}
	if before > total {
		before = total
	}
	return total - before, before, nil
}

func (m *Manager) countRunningCancellable() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, it := range m.items {
		if (it.State == ipc.StateDownloading || it.expanding) && it.cancel != nil {
			n++
		}
	}
	return n
}

func (m *Manager) ClearFinished() (int, error) {
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return 0, ErrShuttingDown
	}
	var removed []string
	var scratch []string
	refs := m.fileReferenceCountsLocked()
	for id, it := range m.items {
		switch it.State {
		case ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted:
			scratch = append(scratch, releaseFiles(it.Files, refs)...)
			m.noteRemovedLocked(it.ID)
			delete(m.items, it.ID)
			removed = append(removed, id)
		}
	}
	order := m.order[:0]
	for _, id := range m.order {
		if _, ok := m.items[id]; ok {
			order = append(order, id)
		}
	}
	m.order = order
	m.mu.Unlock()
	cleanupScratch(scratch)
	for _, id := range removed {
		m.broadcast(ipc.Event{Event: "removed", ID: id})
	}
	if len(removed) > 0 {
		m.touch()
	}
	return len(removed), nil
}

// fileReferenceCountsLocked gathers all safe output bases once for bulk
// removal. Recomputing the owners for every row made ClearAll/ClearFinished
// quadratic at the supported 20,000-item queue limit.
func (m *Manager) fileReferenceCountsLocked() map[string]int {
	refs := make(map[string]int)
	for _, it := range m.items {
		for _, f := range it.Files {
			if isSafeBase(f) {
				refs[f]++
			}
		}
	}
	return refs
}

// releaseFiles removes one item's references and returns bases whose final
// owner is being removed. Each such base is cleaned exactly once after unlock.
func releaseFiles(files []string, refs map[string]int) []string {
	var released []string
	for _, f := range files {
		if !isSafeBase(f) {
			continue
		}
		refs[f]--
		if refs[f] == 0 {
			released = append(released, f)
		}
	}
	return released
}

func (m *Manager) removeItemLocked(it *item) {
	m.noteRemovedLocked(it.ID)
	delete(m.items, it.ID)
	order := m.order[:0]
	for _, id := range m.order {
		if id != it.ID {
			order = append(order, id)
		}
	}
	m.order = order
}

func (m *Manager) with(id string, fn func(*item) error) error {
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	err := fn(it)
	m.mu.Unlock()
	return err
}

func stateRank(s ipc.State) int {
	switch s {
	case ipc.StateDownloading:
		return 0
	case ipc.StateQueued:
		return 1
	case ipc.StatePaused:
		return 2
	case ipc.StateFailed:
		return 3
	default:
		return 4
	}
}

func (m *Manager) List() []ipc.Item {
	items, _ := m.snapshotRaw()
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := stateRank(items[i].State), stateRank(items[j].State)
		if ri != rj {
			return ri < rj
		}
		return items[i].AddedAt.Before(items[j].AddedAt)
	})
	return items
}

func (m *Manager) Get(id string) (ipc.Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return ipc.Item{}, false
	}
	return it.Item, true
}

func (m *Manager) snapshotRaw() ([]ipc.Item, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]ipc.Item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, it.Item)
	}
	order := make([]string, len(m.order))
	copy(order, m.order)
	return items, order
}

func (m *Manager) Stats() (running, queued int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		if it, ok := m.items[id]; ok && it.State == ipc.StateQueued && !it.expanding {
			queued++
		}
	}
	return m.nrun, queued
}

// StatsSnapshot is a read-only aggregate for adapters that would otherwise
// recompute state counts and total speed per request. Values are copied under
// the lock; formatting stays with callers.
type StatsSnapshot struct {
	Running                  int   `json:"running"`
	Queued                   int   `json:"queued"`
	Paused                   int   `json:"paused"`
	Completed                int   `json:"completed"`
	Failed                   int   `json:"failed"`
	Deleted                  int   `json:"deleted"`
	TotalSpeedBytesPerSecond int64 `json:"total_speed_bytes_per_second"`
}

func (m *Manager) StatsSnapshot() StatsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st StatsSnapshot
	st.Running = m.nrun
	for _, it := range m.items {
		switch it.State {
		case ipc.StateQueued:
			if !it.expanding {
				st.Queued++
			}
		case ipc.StatePaused:
			st.Paused++
		case ipc.StateCompleted:
			st.Completed++
		case ipc.StateFailed:
			st.Failed++
		case ipc.StateDeleted:
			st.Deleted++
		}
		if it.State == ipc.StateDownloading && it.Speed > 0 {
			st.TotalSpeedBytesPerSecond += it.Speed
		}
	}
	return st
}

// SetMaxConcurrent updates the concurrency cap at runtime. Reducing the cap
// never kills active jobs; it only delays future starts. The lock must be
// released before scheduling because schedule acquires it itself.
func (m *Manager) SetMaxConcurrent(n int) error {
	if n < 1 || n > 100 {
		return fmt.Errorf("max concurrent downloads must be between 1 and 100, got %d", n)
	}
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	m.max = n
	m.mu.Unlock()
	m.schedule()
	return nil
}

func (m *Manager) Close() {
	m.shutdownOnce.Do(func() {
		defer close(m.closeDone)
		m.mu.Lock()
		m.closing = true
		cancel := m.cancelCtx
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}

		// Wake every stream consumer (SSE handlers, IPC pumps, TUI backends) so
		// graceful shutdown never waits on an idle subscriber. Serialized behind
		// publishMu so no in-flight publish can send on a channel we are about
		// to close; late subscribers already get a pre-closed stream.
		m.publishMu.Lock()
		m.mu.Lock()
		subs := make([]chan ipc.Event, 0, len(m.subs))
		for ch := range m.subs {
			subs = append(subs, ch)
		}
		m.subs = make(map[chan ipc.Event]struct{})
		m.mu.Unlock()
		m.publishMu.Unlock()
		for _, ch := range subs {
			close(ch)
		}

		if m.saverStop != nil {
			close(m.saverStop)
			// Join the saver so no state write can happen after Close returns.
			<-m.saverDone
		}
		m.wg.Wait()
		m.flush()
	})
	<-m.closeDone
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

// ansiSeq matches CSI sequences and OSC strings. yt-dlp runs with
// --color no_color, but titles and error text come from remote pages and may
// embed escape sequences; rendering them raw would let a page repaint or spoof
// the terminal.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// sanitize strips terminal escape sequences and other control characters from
// remote-influenced display strings (titles, error tails).
func sanitize(s string) string {
	unsafe := func(r rune) bool {
		return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			r == 0x061c || r == 0x200e || r == 0x200f ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
	}
	if !strings.Contains(s, "\x1b") && !strings.ContainsFunc(s, unsafe) {
		return s
	}
	s = ansiSeq.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if unsafe(r) {
			return -1
		}
		return r
	}, s)
}

// InterruptedCount reports how many unfinished downloads (running or queued)
// were recovered from the previous process. restore() brings all of them back
// paused, and they stay that way: nothing auto-resumes.
//
// Startup used to resume them automatically. It no longer does, because a
// restart is the one moment the operator has not asked for anything — a
// container that comes back up after a reboot should not immediately start
// pulling bandwidth on its own. The count exists so the TUI and the daemon log
// can say that paused work is waiting; pauseOrigin already distinguishes these
// rows ("shutdown") from ones the user paused ("user").
func (m *Manager) InterruptedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, it := range m.items {
		if it.State == ipc.StatePaused && !it.UserPaused {
			n++
		}
	}
	return n
}

func fileSize(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, false
	}
	return fi.Size(), true
}

func filesExist(files []string) bool {
	for _, f := range files {
		if !isSafeBase(f) {
			continue
		}
		if _, ok := fileSize(f); ok {
			return true
		}
		if _, ok := fileSize(f + ".part"); ok {
			return true
		}
		if _, ok := fileSize(f + ".ytdl"); ok {
			return true
		}
		// A video+audio download — the default on YouTube — never writes
		// "<final>.part" at all: yt-dlp reports only the FINAL merged name
		// through --print, while the bytes land in per-format scratch files
		// like "<stem>.f137.mp4.part". Checking only the two names above
		// therefore declared an interrupted merge "gone", and restore()
		// turned it into a deleted row: absent from Queue, absent from
		// Library, and still counted by Clear finished.
		if _, ok := scratchSiblingBytes(f); ok {
			return true
		}
	}
	return false
}

// scratchSiblingBytes totals the bytes yt-dlp has written into its working
// files for the output at base, and reports whether any exist. It recognises
// the same two shapes removeScratchSiblings cleans up: "<base>.part-*"
// fragments (HLS/DASH) and "<stem>.f<id>.*" per-format intermediates.
func scratchSiblingBytes(base string) (int64, bool) {
	if !isSafeBase(base) {
		return 0, false
	}
	dir := filepath.Dir(base)
	name := filepath.Base(base)
	fragPrefix := name + ".part-"
	stem := strings.TrimSuffix(name, filepath.Ext(name))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var total int64
	var found bool
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if !strings.HasPrefix(n, fragPrefix) && !formatIntermediate(n, stem) {
			continue
		}
		if sz, ok := fileSize(filepath.Join(dir, n)); ok {
			total += sz
			found = true
		}
	}
	return total, found
}

func pctOf(got, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(got) / float64(total) * 100
	if p > 100 {
		p = 100
	}
	return p
}

func hydrateFromDisk(it *ipc.Item) {
	if it.Total <= 0 || len(it.Files) == 0 {
		return
	}
	// Same guard filesExist and cleanupPartials apply: one rule governs every
	// use of Files, including the read-only ones.
	for _, f := range it.Files {
		if !isSafeBase(f) {
			continue
		}
		if sz, ok := fileSize(f + ".part"); ok && sz > 0 {
			it.Got = sz
			it.Progress = pctOf(sz, it.Total)
			return
		}
	}
	for _, f := range it.Files {
		if !isSafeBase(f) {
			continue
		}
		if sz, ok := fileSize(f); ok && sz > 0 {
			it.Got = sz
			it.Progress = pctOf(sz, it.Total)
			return
		}
	}
	// Same reason filesExist looks here: an interrupted merge has written no
	// "<final>" and no "<final>.part", only per-format scratch. Summing it is
	// what lets a restored row show the progress it actually has instead of
	// reporting 0% for a download that is half finished on disk.
	for _, f := range it.Files {
		if sz, ok := scratchSiblingBytes(f); ok && sz > 0 {
			it.Got = sz
			it.Progress = pctOf(sz, it.Total)
			return
		}
	}
}
