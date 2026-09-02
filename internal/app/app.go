// Package app routes yt-dlp-manager subcommands to their adapters and owns
// exit codes. It contains no business logic: every mode is a thin shell over
// internal/service, internal/tui, internal/cli, or internal/httpapi.
package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"yt-dlp-manager/internal/cli"
	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/httpapi"
	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
	"yt-dlp-manager/internal/service"
	"yt-dlp-manager/internal/tui"
	"yt-dlp-manager/internal/webui"
)

// Build information, overridable through -ldflags at link time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

const productName = "yt-dlp-manager"

// Run dispatches args (already without argv[0]) and returns a process exit
// code. With no arguments it opens the TUI for local compatibility.
func Run(args []string) int {
	if len(args) == 0 {
		return tui.Run(nil)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(Usage())
		return 0
	case "version":
		fmt.Printf("%s %s (commit %s, built %s, %s)\n", productName, Version, Commit, Date, goVersion())
		return 0
	case "tui":
		return tui.Run(args[1:])
	case "server":
		return runServer(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "healthcheck":
		return runHealthcheck(args[1:])
	default:
		if cli.IsClientCommand(args[0]) {
			return cli.Run(args)
		}
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", productName, args[0])
		fmt.Fprint(os.Stderr, Usage())
		return 2
	}
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	max := fs.Int("max", manager.DefaultMax(), "maximum concurrent downloads")
	socket := fs.String("socket", "", "unix socket path")
	state := fs.String("state", "", "state file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s daemon: unexpected argument %q\n", productName, fs.Arg(0))
		return 2
	}
	if *max < config.MinConcurrent || *max > config.MaxConcurrent {
		fmt.Fprintf(os.Stderr, "%s daemon: --max must be between %d and %d, got %d\n",
			productName, config.MinConcurrent, config.MaxConcurrent, *max)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := service.Start(ctx, service.Options{
		Max:        *max,
		StatePath:  *state,
		SocketPath: *socket,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s daemon: %v\n", productName, err)
		return 1
	}
	if svc.Interrupted > 0 {
		fmt.Fprintf(os.Stderr,
			"%s daemon: %d unfinished download(s) restored paused; resume them to continue\n",
			productName, svc.Interrupted)
	}

	<-ctx.Done()
	svc.Close()
	return 0
}

// serverFlags mirrors the documented `server` contract. Flags override
// environment variables which override the config file.
type serverFlags struct {
	listen       string
	max          int
	state        string
	socket       string
	configPath   string
	allowUnauth  bool
	secureCookie bool
	// set records which flags actually appeared on the command line. A bool
	// flag's value alone cannot distinguish "not given" from "given as
	// false", so without this "--allow-unauthenticated=false" could never
	// switch off a true coming from the config file or the environment.
	set map[string]bool
}

func parseServerFlags(args []string) (*serverFlags, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	sf := &serverFlags{}
	fs.StringVar(&sf.listen, "listen", "", "HTTP listen address (default from config/env)")
	fs.IntVar(&sf.max, "max", 0, "maximum concurrent downloads")
	fs.StringVar(&sf.state, "state", "", "state file path")
	fs.StringVar(&sf.socket, "socket", "", "unix control socket path")
	fs.StringVar(&sf.configPath, "config", config.DefaultPath(), "manager config file path")
	fs.BoolVar(&sf.allowUnauth, "allow-unauthenticated", false, "serve without authentication (loopback only by policy)")
	fs.BoolVar(&sf.secureCookie, "secure-cookie", false, "mark session cookies Secure (behind HTTPS proxy)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if fs.NArg() > 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	sf.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { sf.set[f.Name] = true })
	return sf, fs, nil
}

// boolFromEnv reports the value of a boolean environment variable and whether
// it was set to anything recognizable at all.
func boolFromEnv(key string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// applyBool resolves one boolean setting across the documented precedence:
// flag beats environment beats file. Each layer records where the value came
// from so `settings` can report it honestly.
func applyBool(dst *bool, src map[string]config.Source, key, flagName, envKey string, sf *serverFlags, flagValue bool) {
	switch {
	case sf.set[flagName]:
		*dst = flagValue
		src[key] = config.SourceFlag
	default:
		if v, ok := boolFromEnv(envKey); ok {
			*dst = v
			src[key] = config.SourceEnv
		}
	}
}

func runServer(args []string) int {
	sf, _, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s server: %v\n", productName, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolved, err := config.Resolve(sf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s server: configuration error: %v\n", productName, err)
		return 1
	}
	cfg := &resolved.File

	// Flags override environment which overrides file.
	if sf.listen != "" {
		cfg.Server.Listen = sf.listen
		resolved.Sources["server.listen"] = config.SourceFlag
	}
	if sf.max != 0 {
		if sf.max < config.MinConcurrent || sf.max > config.MaxConcurrent {
			fmt.Fprintf(os.Stderr, "%s server: --max must be between %d and %d, got %d\n",
				productName, config.MinConcurrent, config.MaxConcurrent, sf.max)
			return 2
		}
		cfg.Downloads.MaxConcurrent = sf.max
		resolved.Sources["downloads.max_concurrent"] = config.SourceFlag
	}
	applyBool(&cfg.Server.AllowUnauthenticated, resolved.Sources,
		"server.allow_unauthenticated", "allow-unauthenticated",
		"YTDLP_MANAGER_ALLOW_UNAUTHENTICATED", sf, sf.allowUnauth)
	applyBool(&cfg.Server.SecureCookie, resolved.Sources,
		"server.secure_cookie", "secure-cookie",
		"YTDLP_MANAGER_SECURE_COOKIE", sf, sf.secureCookie)

	loopback := isLoopbackListen(cfg.Server.Listen)

	// Parsed before anything binds or starts: a typo here would otherwise
	// silently trust nothing, leaving the login limiter collapsed onto one
	// shared bucket exactly where the operator meant to fix it.
	trustedProxies, err := trustedProxiesFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"%s server: YTDLP_MANAGER_TRUSTED_PROXIES: %v\n\n"+
				"Expected a comma-separated list of IP addresses or CIDR blocks,\n"+
				"for example \"127.0.0.1\" or \"10.0.0.0/8,::1\".\n",
			productName, err)
		return 1
	}

	// Preflight: the state directory must be writable before anything tries
	// to create the session key or open state. Container bind mounts are the
	// common failure (Docker creates missing host dirs as root while we run
	// as a non-root uid), so fail here with an actionable message instead of
	// a bare EACCES deep in key creation.
	statePath := stateDirFor(sf.state)
	if err := ensureDirWritable(filepath.Dir(statePath)); err != nil {
		fmt.Fprintf(os.Stderr,
			"%s server: %v\n\n"+
				"The state directory must be writable by the process user\n"+
				"(uid %d). For Docker bind mounts, run from the host:\n"+
				"  sudo chown -R %d:%d <state-volume-dir>\n"+
				"or start the container with --user matching your own uid.\n",
			productName, err, os.Getuid(), os.Getuid(), os.Getgid())
		return 1
	}

	var auth *httpapi.Auth
	unauthenticated := cfg.Server.AllowUnauthenticated
	if unauthenticated && !loopback {
		// The README promises this is "restricted to explicit loopback use",
		// and it now is. A warning on stderr is not a restriction: in a
		// container nobody reads it, and the combination exposes every route —
		// including the yt-dlp config writer and delete-all — to the network
		// with no credential at all.
		fmt.Fprintf(os.Stderr,
			"%s server: refusing to serve without authentication on non-loopback address %q.\n\n"+
				"--allow-unauthenticated (YTDLP_MANAGER_ALLOW_UNAUTHENTICATED) is only\n"+
				"honored when the listen address is loopback. Either bind loopback:\n"+
				"  --listen 127.0.0.1:%s\n"+
				"or drop --allow-unauthenticated and sign in normally.\n",
			productName, cfg.Server.Listen, portOrDefault(cfg.Server.Listen))
		return 1
	}

	if !unauthenticated {
		keyPath := filepath.Join(filepath.Dir(stateDirFor(sf.state)), "sessions.key")
		key, kerr := httpapi.LoadOrCreateKey(keyPath)
		if kerr != nil {
			fmt.Fprintf(os.Stderr, "%s server: session key: %v\n", productName, kerr)
			return 1
		}
		credentialsPath := filepath.Join(filepath.Dir(stateDirFor(sf.state)), "admin.json")
		auth, err = httpapi.NewAuthWithKey(credentialsPath, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s server: %v\n", productName, err)
			return 1
		}
	}

	// Reserve the public listener before starting the manager. Constructing a
	// manager restores state and may schedule queued downloads immediately; an
	// invalid or occupied HTTP address must therefore fail before any child
	// process can start or queue state can change.
	addr, lerr := net.ResolveTCPAddr("tcp", cfg.Server.Listen)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "%s server: listen address %q invalid\n", productName, cfg.Server.Listen)
		return 1
	}
	ln, lerr := net.ListenTCP("tcp", addr)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "%s server: HTTP listener failed: %v\n", productName, lerr)
		return 1
	}
	defer ln.Close()

	// First-run bootstrap secret. Claiming the administrator account is the one
	// mutation available before any credential exists, so it is restricted to
	// callers that are demonstrably local — and a Host header is not proof of
	// that, since the client chooses it. A bind that off-box clients (or a
	// container's port proxy) can reach therefore needs an out-of-band secret,
	// and the server log is that channel.
	//
	// A loopback bind needs none: every peer that can connect is already local.
	setupToken := ""
	if auth != nil && auth.SetupRequired() && !loopback {
		setupToken, err = httpapi.NewSetupToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s server: generate setup token: %v\n", productName, err)
			return 1
		}
		fmt.Fprintf(os.Stderr,
			"\n%s: first-run setup token: %s\n"+
				"Needed only when setting the administrator password from somewhere other\n"+
				"than this machine (a browser on another host, or through Docker's port\n"+
				"proxy). The setup screen asks for it; the token changes on every restart\n"+
				"and stops working once the password is set.\n\n",
			productName, setupToken)
	}

	// Manager + IPC control socket.
	// HTTP shutdown drains accepted requests before this service is closed.
	// Giving the manager the signal context directly would close and flush it
	// concurrently with those still-running handlers.
	svc, err := service.Start(context.Background(), service.Options{
		Max:        cfg.Downloads.MaxConcurrent,
		ExtraArgs:  cfg.Downloads.ExtraArgs,
		StatePath:  sf.state,
		SocketPath: sf.socket,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s server: %v\n", productName, err)
		return 1
	}
	defer svc.Close()

	deps := httpapi.Deps{
		Manager:              svc.Manager(),
		Config:               cfg,
		Sources:              resolved.Sources,
		Store:                config.NewStore(sf.configPath),
		YtDlpConfigPath:      ytDlpConfigPath(),
		AllowYtDlpConfigEdit: cfg.Advanced.AllowYtDlpConfigEdit,
		Auth:                 auth,
		Unauthenticated:      unauthenticated,
		SecureCookie:         cfg.Server.SecureCookie,
		Listen:               cfg.Server.Listen,
		TrustedHosts:         trustedHostsFromEnv(),
		TrustedProxies:       trustedProxies,
		SetupToken:           setupToken,
		StatePath:            stateDirFor(sf.state),
		StateDir:             filepath.Dir(stateDirFor(sf.state)),
		Web:                  webui.Handler(),
		Version: httpapi.VersionInfo{
			AppVersion: Version,
			Commit:     Commit,
			BuildDate:  Date,
			GoVersion:  runtime.Version(),
		},
		Ready:     readinessCheck(svc, filepath.Dir(stateDirFor(sf.state))),
		Logger:    logger(),
		StartTime: time.Now(),
	}

	fmt.Fprintf(os.Stderr, "%s %s: listening on http://%s (web UI + API)\n", productName, Version, ln.Addr())

	hs := httpapi.New(deps)
	if serr := hs.Run(ctx, ln); serr != nil {
		fmt.Fprintf(os.Stderr, "%s server: %v\n", productName, serr)
		return 1
	}
	return 0
}

// readinessCheck returns the /readyz probe set: executables present and the
// writable directories usable.
//
// stateDir is the directory actually in use — passing it in matters because
// the previous implementation called stateDirFor("") and so probed (and
// created) the XDG default even when --state pointed somewhere else.
//
// /readyz is unauthenticated, so the probe must also be cheap: it stats rather
// than writing, and the write test that proves a mount is usable happens once
// during startup preflight instead of on every request.
func readinessCheck(svc *service.Service, stateDir string) func() httpapi.Readiness {
	return func() httpapi.Readiness {
		checks := []httpapi.ReadinessCheck{}
		ok := true

		for _, name := range []string{"yt-dlp", "ffmpeg"} {
			p, err := exec.LookPath(name)
			c := httpapi.ReadinessCheck{Name: name, OK: err == nil}
			if err != nil {
				c.Detail = "not found on PATH"
				ok = false
			} else {
				c.Detail = p
			}
			checks = append(checks, c)
		}

		err := dirUsable(stateDir)
		c := httpapi.ReadinessCheck{Name: "dir:" + stateDir, OK: err == nil}
		if err != nil {
			c.Detail = "not writable"
			ok = false
		}
		checks = append(checks, c)

		// The download directory belongs to yt-dlp's configuration, which may
		// point anywhere. /downloads is the container convention, so report it
		// when it exists and stay silent otherwise rather than holding the
		// whole service "not ready" on a desktop install that has no such path.
		if _, statErr := os.Stat("/downloads"); statErr == nil {
			derr := dirUsable("/downloads")
			dc := httpapi.ReadinessCheck{Name: "dir:/downloads", OK: derr == nil}
			if derr != nil {
				dc.Detail = "not writable"
				ok = false
			}
			checks = append(checks, dc)
		}

		if svc.Manager() == nil {
			ok = false
			checks = append(checks, httpapi.ReadinessCheck{Name: "manager", OK: false})
		} else {
			checks = append(checks, httpapi.ReadinessCheck{Name: "manager", OK: true})
			// A queue that cannot be written to disk still serves requests,
			// but every change acknowledged since the failure is lost on
			// restart. That is exactly what a readiness probe is for: say so
			// instead of reporting healthy while the state file goes stale.
			if serr := svc.Manager().SaveError(); serr != nil {
				ok = false
				checks = append(checks, httpapi.ReadinessCheck{
					Name:   "state-persistence",
					OK:     false,
					Detail: "queue not persisted: " + serr.Error(),
				})
			} else {
				checks = append(checks, httpapi.ReadinessCheck{Name: "state-persistence", OK: true})
			}
		}
		return httpapi.Readiness{OK: ok, Checks: checks}
	}
}

// dirUsable reports whether dir exists and is a writable directory, without
// creating anything or touching the filesystem beyond a stat + access check.
func dirUsable(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := syscall.Access(dir, unixWriteOK|unixExecOK); err != nil {
		return fmt.Errorf("%s is not writable", dir)
	}
	return nil
}

const (
	unixWriteOK = 0x2 // W_OK
	unixExecOK  = 0x1 // X_OK
)

// portOrDefault extracts the port from a listen address for use in advice text.
func portOrDefault(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" {
		return port
	}
	return "8080"
}

// ensureDirWritable verifies dir exists (creating it if needed) and accepts
// writes, returning a descriptive error when it does not.
func ensureDirWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state directory %s is not usable: %v", dir, err)
	}
	// A fixed probe name can already be a symlink. os.WriteFile follows it,
	// truncating an unrelated target before Remove only deletes the link. A
	// unique O_EXCL temp file proves the same thing without touching any
	// pre-existing directory entry.
	probe, err := os.CreateTemp(dir, ".write-probe-*.tmp")
	if err != nil {
		return fmt.Errorf("state directory %s is not writable: %v", dir, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("state directory %s is not writable: %v", dir, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("state directory %s cannot remove temporary files: %v", dir, err)
	}
	return nil
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" || host == "::" || host == "[::]" {
		// Wildcard binds are NOT loopback-only.
		if host == "localhost" {
			return true
		}
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stateDirFor resolves the effective state path: explicit flag wins, else
// XDG default. The directory is also where the session signing key lives.
func stateDirFor(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return manager.DefaultStorePath()
}

// ytDlpConfigPath mirrors the container layout while staying natural on a
// desktop: $XDG_CONFIG_HOME/yt-dlp/config.
func ytDlpConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "yt-dlp-config"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "yt-dlp", "config")
}

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// runHealthcheck is the container health probe: no curl dependency. It first
// probes HTTP /healthz on the effective listen address; if that is closed it
// accepts a live IPC socket as proof of a healthy manager-only deployment.
func runHealthcheck(args []string) int {
	sf, _, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resolved, err := config.Resolve(sf.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: config:", err)
		return 1
	}
	listen := resolved.File.Server.Listen
	if sf.listen != "" {
		listen = sf.listen
	}
	if probeHTTPHealth(listen) {
		return 0
	}
	// Manager-only (daemon) deployments are still healthy when the control
	// socket answers.
	socket := sf.socket
	if socket == "" {
		socket = ipc.DefaultSocketPath()
	}
	if c, derr := ipc.Dial(socket); derr == nil {
		c.Close()
		fmt.Println("ok (ipc)")
		return 0
	}
	fmt.Fprintln(os.Stderr, "healthcheck failed")
	return 1
}

func probeHTTPHealth(listen string) bool {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	url := net.JoinHostPort(host, port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + url + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// goVersion reports the Go runtime version for the version banner.
func goVersion() string { return runtime.Version() }

// trustedHostsFromEnv reads YTDLP_MANAGER_TRUSTED_HOSTS, a comma-separated
// list of host[:port] values that the anti-rebinding Host guard accepts. A
// trusted Host does not establish client locality or authorize first-run
// setup; non-loopback peers still need the one-time setup token. The list is
// useful when a reverse proxy uses a name the app cannot infer from its bind.
// trustedProxiesFromEnv reads YTDLP_MANAGER_TRUSTED_PROXIES, a comma-separated
// list of IP addresses and CIDR blocks naming the reverse proxies in front of
// this server. It exists because login rate limiting keys on the socket peer:
// behind a proxy that is the proxy's address for everyone, so one attacker's
// failures drive the shared backoff to its ceiling and lock the administrator
// out. Naming the proxy lets the limiter read the forwarded chain instead.
//
// It does not relax first-run setup authorization, which keeps reading the
// accepted socket — see peerIsLoopback.
func trustedProxiesFromEnv() ([]netip.Prefix, error) {
	return httpapi.ParseTrustedProxies(os.Getenv("YTDLP_MANAGER_TRUSTED_PROXIES"))
}

func trustedHostsFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("YTDLP_MANAGER_TRUSTED_HOSTS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if h := strings.TrimSpace(part); h != "" {
			out = append(out, h)
		}
	}
	return out
}
