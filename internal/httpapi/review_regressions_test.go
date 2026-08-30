package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupServer builds a server that still needs its first-run password, bound
// to a wildcard address the way the container default does.
func setupServer(t *testing.T, trusted ...string) *Server {
	t.Helper()
	d := testDeps(t)
	d.Unauthenticated = false
	d.Listen = "0.0.0.0:8080"
	d.TrustedHosts = trusted
	d.Auth = &Auth{
		sessions:      NewSessions([]byte("0123456789abcdef0123456789abcdef")),
		limiter:       newLoginLimiter(),
		setupRequired: true,
	}
	return testServer(t, d)
}

func reqTo(s *Server, method, path, host, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// A page that rebinds its own hostname to 127.0.0.1 is same-origin as far as
// the browser is concerned, so neither SameSite nor the origin guard stops it.
// Only the host check does: it must get no setup token and no account.
func TestFirstRunSetupRejectsReboundHost(t *testing.T) {
	s := setupServer(t)

	rec := reqTo(s, "GET", "/api/v1/session", "evil.example.com:8080", "")
	var got sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SetupRequired {
		t.Fatalf("expected setup_required, got %+v", got)
	}
	if got.CSRFToken != "" {
		t.Fatal("a rebound host was handed a usable setup CSRF token")
	}

	// Even holding a token minted over loopback, the mutation must be refused.
	loop := reqTo(s, "GET", "/api/v1/session", "127.0.0.1:8080", "")
	var ok sessionResponse
	if err := json.Unmarshal(loop.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.CSRFToken == "" {
		t.Fatal("loopback should receive a setup token")
	}
	r := httptest.NewRequest("PUT", "/api/v1/session/password",
		strings.NewReader(`{"password":"correct horse","confirmation":"correct horse"}`))
	r.Host = "evil.example.com:8080"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", ok.CSRFToken)
	for _, c := range loop.Result().Cookies() {
		r.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, r)
	if rec2.Code != 403 {
		t.Fatalf("rebound setup returned %d, want 403", rec2.Code)
	}
	if !s.deps.Auth.SetupRequired() {
		t.Fatal("the administrator account was claimed through a rebound host")
	}
}

func TestFirstRunSessionDistinguishesLoopbackAndTrustedHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		s := setupServer(t)
		rec := reqTo(s, "GET", "/api/v1/session", host, "")
		var got sessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.CSRFToken == "" {
			t.Fatalf("%s should pass the anti-rebinding host gate", host)
		}
		if !got.SetupTokenRequired {
			// reqTo deliberately retains httptest's non-loopback RemoteAddr.
			t.Fatalf("%s must still require a setup token from a non-loopback peer", host)
		}
	}
	s := setupServer(t, "nas.lan:8080")
	rec := reqTo(s, "GET", "/api/v1/session", "nas.lan:8080", "")
	var got sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CSRFToken == "" {
		t.Fatal("an explicitly trusted host should pass the anti-rebinding host gate")
	}
	if !got.SetupTokenRequired {
		t.Fatal("a trusted Host must not authorize setup from a non-loopback peer")
	}
}

// A source-rotating attacker must not be able to lock the administrator out
// for the whole failure window. The global delay has to count down.
func TestGlobalLoginBackoffDecaysAndIsBounded(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 500; i++ {
		l.fail(strings.Repeat("a", i%5) + string(rune('a'+i%26)) + "." + time.Now().Format("150405.000000000"))
	}
	_, retry := l.allow("victim")
	if retry > globalMaxDelay {
		t.Fatalf("global delay %v exceeds the %v cap", retry, globalMaxDelay)
	}
	if retry == 0 {
		t.Fatal("expected some global slowdown after 500 failures")
	}

	// It must be a countdown, not a latch until the window expires.
	l.mu.Lock()
	l.globalNextOK = time.Now().Add(-time.Second)
	l.mu.Unlock()
	if ok, _ := l.allow("victim"); !ok {
		t.Fatal("global backoff latched instead of expiring")
	}
}

func TestSuccessfulLoginClearsGlobalBackoff(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 200; i++ {
		l.fail(time.Now().Format("150405.000000000") + string(rune(i)))
	}
	if ok, _ := l.allow("admin"); ok {
		t.Skip("no global delay accrued; nothing to clear")
	}
	l.success("admin")
	if ok, _ := l.allow("admin"); !ok {
		t.Fatal("a correct password did not clear the service-wide slowdown")
	}
}

func TestSessionEvictionRespectsCapAndDropsOldest(t *testing.T) {
	s := NewSessions([]byte("0123456789abcdef0123456789abcdef"))
	s.maxSessions = 8
	var keep string
	for i := 0; i < 40; i++ {
		v, _ := s.Create()
		if i == 39 {
			keep = v
		}
	}
	s.mu.Lock()
	n := len(s.byID)
	s.mu.Unlock()
	if n > 8 {
		t.Fatalf("session table holds %d entries, cap is 8", n)
	}
	if _, err := s.Verify(keep); err != nil {
		t.Fatal("the newest session was evicted instead of the oldest")
	}
}

func TestPasswordMinimumCountsCharactersNotBytes(t *testing.T) {
	a := &Auth{sessions: NewSessions([]byte("0123456789abcdef0123456789abcdef")), limiter: newLoginLimiter()}
	// Four emoji are 16 bytes but only 4 characters.
	if err := a.changePassword("🔑🔑🔑🔑", "🔑🔑🔑🔑"); err == nil {
		t.Fatal("a 4-character password was accepted because it was 16 bytes")
	}
}

// occupyDerivationSlots fills every slot and returns a release func. The
// holders signal from inside fn, so the caller knows the slots are genuinely
// taken rather than inferring it from a sleep.
func occupyDerivationSlots(t *testing.T) func() {
	t.Helper()
	release := make(chan struct{})
	running := make(chan struct{}, derivationConcurrency)
	var held sync.WaitGroup
	for i := 0; i < derivationConcurrency; i++ {
		held.Add(1)
		go func() {
			defer held.Done()
			_, _ = withDerivationSlot(context.Background(), func() bool {
				running <- struct{}{}
				<-release
				return false
			})
		}()
	}
	for i := 0; i < derivationConcurrency; i++ {
		select {
		case <-running:
		case <-time.After(10 * time.Second):
			close(release)
			held.Wait()
			t.Fatal("derivation slots never filled")
		}
	}
	var once sync.Once
	return func() { once.Do(func() { close(release); held.Wait() }) }
}

// A simultaneous burst all passes allow() before any of it has failed, so the
// only thing standing between it and thousands of queued 170ms derivations is
// the queue bound.
func TestPasswordVerifierShedsLoadInsteadOfQueueing(t *testing.T) {
	releaseSlots := occupyDerivationSlots(t)

	// Fill the waiting queue to its cap.
	var queued sync.WaitGroup
	for i := 0; i < derivationQueueDepth; i++ {
		queued.Add(1)
		go func() {
			defer queued.Done()
			_, _ = withDerivationSlot(context.Background(), func() bool { return false })
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for derivationWaiting.Load() < int64(derivationQueueDepth) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := derivationWaiting.Load(); got < int64(derivationQueueDepth) {
		releaseSlots()
		queued.Wait()
		t.Fatalf("precondition: only %d of %d waiters registered", got, derivationQueueDepth)
	}

	// One more must be refused rather than parked.
	_, err := withDerivationSlot(context.Background(), func() bool { return false })

	releaseSlots()
	queued.Wait()

	if !errors.Is(err, ErrVerifierBusy) {
		t.Fatalf("expected ErrVerifierBusy past the queue cap, got %v", err)
	}
}

// A client that gives up must not still cost a key derivation.
func TestPasswordVerifierHonoursCancellation(t *testing.T) {
	releaseSlots := occupyDerivationSlots(t)
	defer releaseSlots()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	_, err := withDerivationSlot(ctx, func() bool { ran = true; return false })
	if ran {
		t.Fatal("derived a key for a caller that had already gone away")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
