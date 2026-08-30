package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/manager"
)

// VersionInfo carries build metadata for /api/v1/system and About.
type VersionInfo struct {
	AppVersion string `json:"app_version"`
	Commit     string `json:"commit"`
	BuildDate  string `json:"build_date"`
	GoVersion  string `json:"go_version"`
}

// ReadinessCheck is one named readiness probe result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Readiness aggregates probe results; Detail strings may contain paths and
// are only exposed through authenticated endpoints.
type Readiness struct {
	OK     bool
	Checks []ReadinessCheck
}

// Deps wires the HTTP adapter to its collaborators.
type Deps struct {
	Manager              *manager.Manager
	Config               *config.File
	Sources              map[string]config.Source
	Store                *config.Store // nil disables settings persistence (tests)
	YtDlpConfigPath      string
	AllowYtDlpConfigEdit bool
	Auth                 *Auth         // always present; AuthMode controls enforcement
	AuthMode             config.Source // informational: where allow-unauthenticated came from
	Unauthenticated      bool
	SecureCookie         bool
	Listen               string
	TrustedHosts         []string
	// SetupToken is the one-time first-run bootstrap secret, printed to the
	// server log at startup when the bind can be reached from off-box. Empty
	// means no remote first-run setup is possible at all (a loopback bind, or
	// a server whose administrator password already exists).
	SetupToken string
	StatePath  string
	StateDir   string
	Web        http.Handler
	Version    VersionInfo
	Ready      func() Readiness
	Logger     *slog.Logger
	StartTime  time.Time
}

// Server is the HTTP adapter over one manager.
type Server struct {
	deps Deps
	http *http.Server
	mux  *http.ServeMux

	// settingsMu guards the mutable *config.File pointed to by deps.Config.
	// Every read and write of that config inside this package must hold it;
	// it is per-Server so independent Server instances never serialize each
	// other.
	settingsMu sync.Mutex

	// thumbFlight coalesces concurrent cache misses for the same thumbnail.
	// A grid of fresh rows renders many <img> tags at once, so without this
	// one download is fetched from the remote host once per in-flight
	// request rather than once in total.
	thumbFlightMu sync.Mutex
	thumbFlight   map[string]*thumbFetch
	// thumbFlightWaiters counts callers currently parked on someone else's
	// in-flight fetch. It is the coalescing hit count, and tests use it to
	// synchronize without sleeping.
	thumbFlightWaiters atomic.Int64
}

func New(d Deps) *Server {
	s := &Server{deps: d}
	s.mux = s.routes()
	return s
}

// ServeHTTP exposes the fully wrapped handler (used directly by tests).
// Header/recovery layers run outside the host guard so even rejected
// requests carry security headers; accessLog sits inside withRequestID so
// log lines carry the generated request id.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := s.deps.Logger
	hg := &hostGuard{
		listen:       s.deps.Listen,
		trustedHosts: s.deps.TrustedHosts,
		loopbackOnly: s.deps.Unauthenticated,
	}
	handler := recoverPanic(log,
		securityHeaders(
			withRequestID(
				hg.middleware(originGuard(accessLog(log, s.mux))))))
	handler.ServeHTTP(w, r)
}

// Run serves until ctx is cancelled or the listener fails, then drains
// connections for up to ten seconds before force-closing. Manager shutdown
// remains the caller's job through service.Close().
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	// Bound the thumbnail cache: eviction-on-removal only runs while an event
	// stream is attached, so restarts are where orphans would otherwise pile up.
	s.sweepThumbnails()

	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// SSE responses extend their own write deadlines per write via
		// ResponseController; ordinary requests stay bounded by WriteTimeout.
	}

	errCh := make(chan error, 1)
	go func() {
		if serr := s.http.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		_ = s.http.Close()
		return err
	}
	return nil
}
