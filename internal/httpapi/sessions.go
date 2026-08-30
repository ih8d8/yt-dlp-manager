package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session lifetime; sliding: each authenticated request extends it.
const sessionTTL = 24 * time.Hour

const sessionCookieName = "ytdlp_session"

type session struct {
	id      string
	csrf    string
	expires time.Time
}

var errNoSession = errors.New("no valid session")

// Sessions stores random server-side session IDs and signs cookie values
// with a persisted HMAC key so a stolen key alone cannot mint sessions and a
// forged ID cannot pass verification.
type Sessions struct {
	mu          sync.Mutex
	byID        map[string]*session
	key         []byte
	maxSessions int
}

func NewSessions(key []byte) *Sessions {
	return &Sessions{
		byID:        make(map[string]*session),
		key:         key,
		maxSessions: 1024,
	}
}

func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *Sessions) sign(id string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Create registers a fresh session and returns its cookie value plus the
// per-session CSRF token.
func (s *Sessions) Create() (cookieValue, csrf string) {
	sess := &session{id: newSecret(32), csrf: newSecret(32), expires: time.Now().Add(sessionTTL)}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound memory: drop expired entries first, then oldest. Evict to one
	// below the cap so that inserting this session lands exactly at it —
	// evicting to the cap and then inserting allowed maxSessions+1.
	if len(s.byID) >= s.maxSessions {
		s.evictLocked(s.maxSessions - 1)
	}
	s.byID[sess.id] = sess
	return sess.id + "." + s.sign(sess.id), sess.csrf
}

func (s *Sessions) evictLocked(target int) {
	now := time.Now()
	for id, sess := range s.byID {
		if now.After(sess.expires) {
			delete(s.byID, id)
		}
	}
	if len(s.byID) <= target {
		return
	}
	// Genuinely oldest first, as documented. Map iteration order is random,
	// so the previous loop dropped an arbitrary live session — potentially
	// the caller's own, while a staler one survived. Expiry slides on use,
	// so ordering by it is least-recently-used.
	type entry struct {
		id      string
		expires time.Time
	}
	rest := make([]entry, 0, len(s.byID))
	for id, sess := range s.byID {
		rest = append(rest, entry{id: id, expires: sess.expires})
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].expires.Before(rest[j].expires) })
	for _, e := range rest {
		if len(s.byID) <= target {
			return
		}
		delete(s.byID, e.id)
	}
}

// Verify validates a cookie value (signature then existence then expiry) and
// slides the expiry forward. It returns the session's CSRF token.
func (s *Sessions) Verify(cookieValue string) (*session, error) {
	id, sig, ok := strings.Cut(cookieValue, ".")
	if !ok || id == "" || sig == "" || len(id) > 128 {
		return nil, errNoSession
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(id))
	expect := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expect), []byte(sig)) {
		return nil, errNoSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, errNoSession
	}
	if time.Now().After(sess.expires) {
		delete(s.byID, id)
		return nil, errNoSession
	}
	sess.expires = time.Now().Add(sessionTTL)
	return sess, nil
}

// Destroy removes a session by raw id extracted from its signed value.
func (s *Sessions) Destroy(cookieValue string) bool {
	id, sig, ok := strings.Cut(cookieValue, ".")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(id))
	expect := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expect), []byte(sig)) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, had := s.byID[id]
	delete(s.byID, id)
	return had
}

// LogoutAll drops every session (Security settings action).
func (s *Sessions) LogoutAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.byID)
	s.byID = make(map[string]*session)
	return n
}

// sessionFromRequest resolves the caller's session, if any.
func (a *Auth) sessionFromRequest(r *http.Request) (*session, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, errNoSession
	}
	return a.sessions.Verify(c.Value)
}

// setSessionCookie writes the cookie with hardened attributes.
func setSessionCookie(w http.ResponseWriter, secure bool, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   maxAge,
	})
}
