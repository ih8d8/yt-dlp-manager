package httpapi

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Auth implements single-admin authentication. It is deliberately not
// multi-user: there is exactly one persisted, salted password verifier.
type Auth struct {
	changeMu        sync.Mutex
	mu              sync.RWMutex
	passwordHash    []byte
	salt            []byte
	iterations      int
	setupRequired   bool
	credentialsPath string

	sessions *Sessions
	limiter  *loginLimiter
}

const (
	passwordIterations = 210_000
	passwordSaltBytes  = 16
	passwordHashBytes  = 32
)

var (
	ErrWeakSecret       = errors.New("authentication password must be at least 8 characters")
	ErrPasswordMismatch = errors.New("passwords do not match")
	ErrPasswordTooLong  = errors.New("password must be at most 1024 characters")
	ErrSetupComplete    = errors.New("administrator password setup is already complete")
)

type storedCredential struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

// NewAuth loads the persisted password verifier at path. On first run no
// verifier exists, so every protected application endpoint stays locked until
// the administrator creates a password through the one-time setup flow.
//
// The session signing key is ephemeral. Server mode instead pairs
// LoadOrCreateKey with NewAuthWithKey so its signing key is stable. The
// server-side session table itself is intentionally in memory, so restarting
// the process still invalidates every existing session.
func NewAuth(credentialsPath string) (*Auth, error) {
	return newAuth(credentialsPath, ephemeralSessionKey())
}

// NewSetupToken returns a fresh first-run bootstrap secret: 128 bits of
// randomness, hex encoded so it survives a copy out of a log line.
func NewSetupToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetSessions overrides the session store (tests inject isolated instances).
func (a *Auth) SetSessions(s *Sessions) { a.sessions = s }

// checkPassword compares in constant time regardless of input length.
func (a *Auth) checkPassword(ctx context.Context, pw string) (bool, error) {
	a.mu.RLock()
	if a.setupRequired {
		a.mu.RUnlock()
		return false, nil
	}
	salt := append([]byte(nil), a.salt...)
	want := append([]byte(nil), a.passwordHash...)
	iterations := a.iterations
	a.mu.RUnlock()
	return withDerivationSlot(ctx, func() bool {
		got, err := pbkdf2.Key(sha256.New, pw, salt, iterations, len(want))
		return err == nil && subtle.ConstantTimeCompare(got, want) == 1
	})
}

func (a *Auth) SetupRequired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.setupRequired
}

// ChangePassword validates, derives, and atomically persists a new password
// verifier. Neither the plaintext nor a reversible representation is stored.
func (a *Auth) ChangePassword(password, confirmation string) error {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	return a.changePassword(password, confirmation)
}

// SetupPassword completes first-run setup exactly once. Serializing the
// check and write prevents two open setup tabs from racing to choose the
// persisted administrator password.
func (a *Auth) SetupPassword(password, confirmation string) error {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if !a.SetupRequired() {
		return ErrSetupComplete
	}
	return a.changePassword(password, confirmation)
}

func (a *Auth) changePassword(password, confirmation string) error {
	if password != confirmation {
		return ErrPasswordMismatch
	}
	// Count characters, not bytes: "🔑🔑" is 8 bytes and would otherwise
	// satisfy an advertised eight-character minimum. The upper bound stays in
	// bytes because it exists to cap PBKDF2 input size.
	if utf8.RuneCountInString(password) < minPasswordRunes {
		return ErrWeakSecret
	}
	if len(password) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate password salt: %w", err)
	}
	// Deliberately not behind derivationSlots: both callers hold changeMu, so
	// this derivation is already serialized to one at a time, and it is
	// reachable only with an administrator session or, during first-run
	// setup, from a loopback host.
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordHashBytes)
	if err != nil {
		return fmt.Errorf("derive password verifier: %w", err)
	}
	if err := saveCredential(a.credentialsPath, salt, hash, passwordIterations); err != nil {
		return err
	}
	a.mu.Lock()
	a.salt = salt
	a.passwordHash = hash
	a.iterations = passwordIterations
	a.setupRequired = false
	a.mu.Unlock()
	return nil
}

func newAuth(credentialsPath string, key []byte) (*Auth, error) {
	a := &Auth{
		credentialsPath: credentialsPath,
		sessions:        NewSessions(key),
		limiter:         newLoginLimiter(),
	}
	info, statErr := os.Lstat(credentialsPath)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("admin credential path must be a regular file")
		}
		if info.Size() > 64<<10 {
			return nil, errors.New("admin credential file is too large")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect admin credentials: %w", statErr)
	}
	data, err := os.ReadFile(credentialsPath)
	if errors.Is(err, os.ErrNotExist) {
		a.setupRequired = true
		return a, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read admin credentials: %w", err)
	}
	var stored storedCredential
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("decode admin credentials: %w", err)
	}
	if stored.Version != 1 || stored.Algorithm != "pbkdf2-sha256" ||
		stored.Iterations < 100_000 || stored.Iterations > 2_000_000 {
		return nil, errors.New("unsupported or unsafe admin credential format")
	}
	a.salt, err = base64.RawStdEncoding.DecodeString(stored.Salt)
	if err != nil || len(a.salt) < passwordSaltBytes {
		return nil, errors.New("invalid admin credential salt")
	}
	a.passwordHash, err = base64.RawStdEncoding.DecodeString(stored.Hash)
	if err != nil || len(a.passwordHash) != passwordHashBytes {
		return nil, errors.New("invalid admin credential hash")
	}
	a.iterations = stored.Iterations
	return a, nil
}

func saveCredential(path string, salt, hash []byte, iterations int) error {
	if path == "" {
		return errors.New("admin credential path is not configured")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("admin credential path must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect admin credentials: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create admin credential directory: %w", err)
	}
	record := storedCredential{1, "pbkdf2-sha256", iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".admin-credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create admin credential temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("persist admin credentials: %w", err)
	}
	if err := syncParentDir(path); err != nil {
		return fmt.Errorf("persist admin credential directory: %w", err)
	}
	return nil
}

// ephemeralSessionKey returns a signing key that lives only as long as this
// process, so sessions minted with it do not survive a restart.
//
// It takes no path on purpose: persisting the key is LoadOrCreateKey's job,
// and server mode passes that result to NewAuthWithKey. This used to accept a
// path it silently ignored, which read as though the caller could choose where
// the key was stored.
func ephemeralSessionKey() []byte {
	b := make([]byte, sessionKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}

const sessionKeyBytes = 32

// A valid key file is 65 bytes including its newline. Keep corrupt files from
// turning startup into an unbounded allocation before they are rotated.
const maxSessionKeyFileBytes = 4 << 10

var errSessionKeyFileTooLarge = errors.New("session key file is too large")

func readSessionKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSessionKeyFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSessionKeyFileBytes {
		return nil, errSessionKeyFileTooLarge
	}
	return data, nil
}

// LoadOrCreateKey reads the hex-encoded session signing key at path or
// generates and persists one atomically with 0600 permissions.
func LoadOrCreateKey(path string) ([]byte, error) {
	// Same check the credential file beside it already gets: a symlink or a
	// non-regular file here would let another process choose where the signing
	// key is read from or written to.
	var existing os.FileInfo
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("session key path must be a regular file")
		}
		existing = info
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect session key: %w", err)
	}
	if existing != nil && existing.Size() <= maxSessionKeyFileBytes {
		data, err := readSessionKeyFile(path)
		if err != nil {
			// A file replaced with an oversized one between Lstat and Open is
			// still bounded by readSessionKeyFile and handled like any other
			// corrupt key: atomically rotate it below.
			if !errors.Is(err, errSessionKeyFileTooLarge) {
				return nil, fmt.Errorf("read session key: %w", err)
			}
		}
		key, derr := hex.DecodeString(strings.TrimSpace(string(data)))
		if derr == nil && len(key) == sessionKeyBytes {
			return key, nil
		}
		// Fall through and rotate an unreadable/short key rather than fail.
	}
	b := make([]byte, sessionKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate session key: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".session-key-*.tmp")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(hex.EncodeToString(b) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(name, path); err != nil {
		return nil, err
	}
	if err := syncParentDir(path); err != nil {
		return nil, fmt.Errorf("persist session key directory: %w", err)
	}
	return b, nil
}

// syncParentDir makes an already-fsynced temp-file rename durable across a
// sudden power loss. Syncing only the file does not guarantee that the new
// directory entry itself reaches stable storage.
func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// NewAuthWithKey builds Auth with an explicit signing key.
func NewAuthWithKey(credentialsPath string, key []byte) (*Auth, error) {
	return newAuth(credentialsPath, key)
}

// --- login rate limiting ---

const (
	minPasswordRunes = 8
	maxPasswordBytes = 1024

	loginBaseDelay = 500 * time.Millisecond
	// loginMaxDelay caps the per-client backoff. It is deliberately short.
	//
	// This is a single-admin, self-hosted application, and a long lockout
	// punishes the wrong person. The operator who half-remembers their own
	// password and tries a dozen variants is far more common than an attacker,
	// and at the previous fifteen-minute ceiling that operator was shut out of
	// their own server for a quarter of an hour. It also made the shared
	// bucket behind a reverse proxy (see clientip.go) an outright denial of
	// service rather than a nuisance.
	//
	// What actually makes guessing infeasible is the password itself and the
	// 210k-iteration PBKDF2 verification behind it, not the length of the
	// pause: a minute between attempts already reduces an attacker to ~1440
	// guesses a day, which no password worth accepting falls to. Stretching
	// that to fifteen minutes buys almost nothing and costs the operator a
	// great deal.
	loginMaxDelay    = 60 * time.Second
	maxTrackedClient = 1024

	// Service-wide backoff kicks in past this many failures inside the window,
	// which is far above anything a person mistyping a password produces.
	globalFailureThreshold = 50
	globalFailureWindow    = 15 * time.Minute
	// Ceiling on the service-wide delay. Reached only under a sustained
	// source-rotating attack, and it decays as soon as the attack stops.
	globalMaxDelay = 30 * time.Second
)

// derivationSlots bounds concurrent PBKDF2 work. At 210k iterations a single
// derivation costs ~170ms of CPU, so without a bound a handful of concurrent
// unauthenticated login attempts saturate the machine and starve the download
// scheduler sharing this process. Attempts queue instead of piling up.
const (
	derivationConcurrency = 4
	// Cap on how many requests may be *waiting* for a slot. The rate limiter
	// only knows about failures it has already recorded, so a simultaneous
	// burst all passes allow() before any of it fails. Without a queue bound
	// each of those requests parks a goroutine that will still eventually
	// spend ~170ms of CPU: the work is serialized but never shed. Past this
	// depth the server refuses immediately instead.
	derivationQueueDepth = 32
)

var (
	derivationSlots   = make(chan struct{}, derivationConcurrency)
	derivationWaiting atomic.Int64
)

// ErrVerifierBusy reports that password verification is saturated and the
// caller should retry rather than be queued behind unbounded work.
var ErrVerifierBusy = errors.New("password verification is saturated")

func withDerivationSlot(ctx context.Context, fn func() bool) (bool, error) {
	if derivationWaiting.Add(1) > derivationQueueDepth {
		derivationWaiting.Add(-1)
		return false, ErrVerifierBusy
	}
	defer derivationWaiting.Add(-1)
	select {
	case derivationSlots <- struct{}{}:
	case <-ctx.Done():
		// The client gave up (or the server is shutting down); do not spend
		// CPU deriving a key nobody is waiting for.
		return false, ctx.Err()
	}
	defer func() { <-derivationSlots }()
	return fn(), nil
}

type attempt struct {
	failures int
	nextOK   time.Time
}

// loginLimiter tracks failures per client address with an exponential,
// capped delay. Forwarded headers are never trusted; the socket's remote
// address is the only identity used.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attempt
	lastEvit time.Time

	// Global backoff. Per-client state cannot bound an attacker who rotates
	// addresses, so recent failures are also counted service-wide over a
	// sliding window.
	globalFailures int
	globalSince    time.Time
	// globalNextOK is a service-wide "not before" stamp. Each failure past
	// the threshold pushes it a little further out, so the wait actually
	// counts down instead of latching for the whole window — an attacker
	// rotating source addresses slows everyone down briefly rather than
	// locking the administrator out for fifteen minutes.
	globalNextOK time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]*attempt), lastEvit: time.Now()}
}

func delayFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := loginBaseDelay << (failures - 1)
	if d > loginMaxDelay || d <= 0 {
		return loginMaxDelay
	}
	return d
}

// allow reports whether a login attempt may proceed now.
func (l *loginLimiter) allow(client string) (ok bool, retryIn time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeEvict()
	if g := l.globalDelayLocked(); g > 0 {
		return false, g
	}
	a, tracked := l.attempts[client]
	if !tracked {
		return true, 0
	}
	if wait := time.Until(a.nextOK); wait > 0 {
		return false, wait
	}
	return true, 0
}

// fail records a failed attempt and schedules the next allowed try. It also
// advances a global counter: per-client backoff alone does nothing against an
// attacker who rotates source addresses, which is the cheap attack when a
// single /64 supplies effectively unlimited ones.
func (l *loginLimiter) fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, tracked := l.attempts[client]
	if !tracked {
		if len(l.attempts) >= maxTrackedClient {
			// Table full of live penalties: still count this globally rather
			// than evicting somebody else's backoff to make room.
			l.noteGlobalFailureLocked()
			return
		}
		a = &attempt{}
		l.attempts[client] = a
	}
	a.failures++
	a.nextOK = time.Now().Add(delayFor(a.failures))
	l.noteGlobalFailureLocked()
}

// noteGlobalFailureLocked maintains a decaying count of recent failures across
// all clients. Caller must hold l.mu.
func (l *loginLimiter) noteGlobalFailureLocked() {
	now := time.Now()
	if !l.globalSince.IsZero() && now.Sub(l.globalSince) > globalFailureWindow {
		l.globalFailures = 0
	}
	if l.globalFailures == 0 {
		l.globalSince = now
		l.globalNextOK = time.Time{}
	}
	l.globalFailures++
	if l.globalFailures <= globalFailureThreshold {
		return
	}
	base := now
	if l.globalNextOK.After(base) {
		base = l.globalNextOK
	}
	next := base.Add(loginBaseDelay)
	// Cap how far ahead the queue can be pushed. Brute force is already
	// bounded by per-client exponential backoff and a 210k-iteration PBKDF2
	// verification; this only has to stop address rotation from making
	// guessing free, and a hard cap keeps it from becoming a denial of
	// service against the legitimate administrator. Same reasoning as
	// loginMaxDelay, which is why the two ceilings are the same order.
	if limit := now.Add(globalMaxDelay); next.After(limit) {
		next = limit
	}
	l.globalNextOK = next
}

// globalDelay reports how long every client must wait because the service as a
// whole is under a guessing attempt. Caller must hold l.mu.
func (l *loginLimiter) globalDelayLocked() time.Duration {
	if !l.globalSince.IsZero() && time.Since(l.globalSince) > globalFailureWindow {
		l.globalFailures = 0
		l.globalNextOK = time.Time{}
		return 0
	}
	if wait := time.Until(l.globalNextOK); wait > 0 {
		return wait
	}
	return 0
}

// success clears failure state after a valid login.
func (l *loginLimiter) success(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, client)
	l.globalFailures = 0
	l.globalNextOK = time.Time{}
	l.globalSince = time.Time{}
}

// maybeEvict bounds memory. Only entries whose penalty has already expired are
// evicted: the previous version fell back to deleting in map-iteration order,
// which is randomized, so an attacker with more than maxTrackedClient source
// addresses (any IPv6 /64) could flush live penalties — including their own —
// simply by filling the table. When every entry is still live the table is
// left alone and new clients go untracked instead, which is bounded and cannot
// erase an existing penalty.
func (l *loginLimiter) maybeEvict() {
	if len(l.attempts) < maxTrackedClient {
		return
	}
	now := time.Now()
	for k, a := range l.attempts {
		if now.After(a.nextOK) {
			delete(l.attempts, k)
		}
	}
}
