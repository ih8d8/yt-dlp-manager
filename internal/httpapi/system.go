package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// handleHealthz reports liveness with zero private detail.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz reports coarse readiness. Failure reasons stay at code level
// here; path-level detail belongs to the authenticated /api/v1/system.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ready == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	rep := s.deps.Ready()
	if rep.OK {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	reason := "not ready"
	for _, c := range rep.Checks {
		if !c.OK {
			reason = c.Name + " not ready"
			break
		}
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"status": "not_ready", "reason": reason,
	})
}

type systemResponse struct {
	Version       VersionInfo      `json:"version"`
	Runtime       runtimeInfo      `json:"runtime"`
	Tools         toolsInfo        `json:"tools"`
	Paths         pathsInfo        `json:"paths"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	Ready         []ReadinessCheck `json:"readiness_checks,omitempty"`
	ReadyOK       bool             `json:"ready"`
}

type runtimeInfo struct {
	Go          string `json:"go"`
	UptimeHuman string `json:"uptime_human"`
}

type toolsInfo struct {
	YtDlp  toolVersion `json:"yt_dlp"`
	Ffmpeg toolVersion `json:"ffmpeg"`
	Js     toolVersion `json:"js_runtime"`
}

type toolVersion struct {
	Present bool `json:"present"`
	// Name is the binary that answered the probe. It matters for js_runtime,
	// which is whichever of several interpreters was found first: "v24.18.1"
	// alone does not tell a reader that node, rather than deno or QuickJS, is
	// what yt-dlp will drive. Deliberately left empty on the not-found path so
	// firstPresent cannot report the name of a probe that failed.
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
	Error   string `json:"error,omitempty"`
}

type pathsInfo struct {
	StatePath       string `json:"state_path"`
	ConfigPath      string `json:"config_path"`
	YtDlpConfigPath string `json:"yt_dlp_config_path"`
	Listen          string `json:"listen"`
}

var (
	toolsMu    sync.Mutex // guards cachedTool/toolsGood/toolsAt
	toolsProbe sync.Mutex // serializes probes without holding toolsMu during exec
	cachedTool toolsInfo
	toolsGood  bool      // a probe where every tool answered
	toolsAt    time.Time // when cachedTool was produced (zero = never)
)

// failedProbeTTL bounds how often an incomplete probe is retried. A successful
// probe is cached for the life of the process; a failed one used to be retried
// on EVERY request, so a host missing (say) node spawned four subprocesses per
// /api/v1/system call, serialized behind toolsProbe with a 5s timeout each.
// Recovery is still detected, just on a sane cadence.
const failedProbeTTL = 60 * time.Second

// probeTools runs fixed argv version probes. Locks are never held while the
// external commands run, so a slow probe cannot stall concurrent requests
// behind a mutex.
func probeTools(ctx context.Context) toolsInfo {
	if t, ok := cachedTools(); ok {
		return t
	}

	toolsProbe.Lock()
	defer toolsProbe.Unlock()

	// A probe that completed while we waited for the lock wins over ours.
	if t, ok := cachedTools(); ok {
		return t
	}

	// Deliberately NOT derived from the request context: a client that
	// disconnects mid-probe would otherwise discard work every other caller is
	// waiting on, and poison the result for them too.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	probed := toolsInfo{
		YtDlp:  probeBinary(probeCtx, "yt-dlp", "--version"),
		Ffmpeg: probeBinary(probeCtx, "ffmpeg", "-version"),
		// Every runtime yt-dlp can drive, in its own priority order. The
		// container ships qjs (QuickJS) rather than deno or node because it is
		// ~2 MB instead of ~90 MB; a desktop install may well have any of them.
		Js: firstPresent(probeCtx, [][]string{
			{"deno", "--version"},
			{"node", "--version"},
			{"qjs", "--help"},
			{"bun", "--version"},
		}),
	}
	good := probed.YtDlp.Present && probed.Ffmpeg.Present &&
		probed.Js.Present && probed.YtDlp.Error == "" &&
		probed.Ffmpeg.Error == "" && probed.Js.Error == ""

	toolsMu.Lock()
	if !toolsGood {
		cachedTool, toolsGood, toolsAt = probed, good, time.Now()
	}
	t := cachedTool
	toolsMu.Unlock()
	return t
}

// cachedTools returns the cached probe when it is still authoritative: forever
// once every tool answered, otherwise until failedProbeTTL elapses.
func cachedTools() (toolsInfo, bool) {
	toolsMu.Lock()
	defer toolsMu.Unlock()
	if toolsAt.IsZero() {
		return toolsInfo{}, false
	}
	if toolsGood || time.Since(toolsAt) < failedProbeTTL {
		return cachedTool, true
	}
	return toolsInfo{}, false
}

// cleanVersion reduces a tool's self-reported first line to the version alone.
// Each binary prints something different, and several bury the version in prose:
//
//	ffmpeg  "ffmpeg version n9.0.1 Copyright (c) 2000-2026 the FFmpeg developers"
//	qjs     "QuickJS version 2025-09-13"
//	deno    "deno 2.9.6 (stable, release, x86_64-unknown-linux-gnu)"
//	node    "v26.8.1"
//	yt-dlp  "2026.08.19"
//
// Rendered verbatim in the About table that puts a copyright notice and a build
// triple on screen. An unrecognised shape falls back to the whole line: a noisy
// version beats an empty one.
func cleanVersion(line string) string {
	fields := strings.Fields(line)
	// "<tool> version X" / "QuickJS version X" — the token after the word.
	for i, f := range fields {
		if strings.EqualFold(f, "version") && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	// Otherwise the first token that reads like one, which also drops a leading
	// tool name ("deno 2.9.6 (...)") and anything trailing it.
	for _, f := range fields {
		if looksLikeVersion(f) {
			return f
		}
	}
	return strings.TrimSpace(line)
}

// looksLikeVersion accepts "2026.08.19", node's "v26.8.1" and ffmpeg's "n9.0.1".
func looksLikeVersion(f string) bool {
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	switch {
	case f == "":
		return false
	case digit(f[0]):
		return true
	case (f[0] == 'v' || f[0] == 'n') && len(f) > 1 && digit(f[1]):
		return true
	}
	return false
}

func probeBinary(ctx context.Context, name string, args ...string) toolVersion {
	path, err := exec.LookPath(name)
	if err != nil {
		return toolVersion{Present: false, Error: "not found on PATH"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Exit status is deliberately not the success signal. Plenty of tools print
	// their version and then exit non-zero — QuickJS's "qjs --help" is exactly
	// that (version on stdout, status 1) — and some print to stderr. What makes
	// a probe successful is getting a line back, not the code it exited with.
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run() // see above: the exit status carries no signal we act on

	line := firstLine(stdout.String())
	if line == "" {
		line = firstLine(stderr.String())
	}
	if line == "" {
		return toolVersion{Present: true, Name: name, Path: path, Error: "version probe failed"}
	}
	if len(line) > 120 {
		line = line[:120]
	}
	return toolVersion{Present: true, Name: name, Version: cleanVersion(line), Path: path}
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

// firstPresent probes candidates in priority order and stops at the first one
// that is present. Probing lazily matters: the arguments to a variadic call
// are all evaluated first, so the previous form spawned deno, node, qjs and
// bun on every cold probe even when the first candidate answered.
func firstPresent(ctx context.Context, candidates [][]string) toolVersion {
	var first toolVersion
	for i, c := range candidates {
		t := probeBinary(ctx, c[0], c[1:]...)
		if t.Present {
			return t
		}
		if i == 0 {
			first = t
		}
	}
	return first
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	var rep Readiness
	if s.deps.Ready != nil {
		rep = s.deps.Ready()
	} else {
		rep = Readiness{OK: true}
	}
	resp := systemResponse{
		Version: s.deps.Version,
		Runtime: runtimeInfo{Go: goRuntimeVersion()},
		Tools:   probeTools(r.Context()),
		Paths: pathsInfo{
			StatePath:       s.deps.StatePath,
			ConfigPath:      "",
			YtDlpConfigPath: s.deps.YtDlpConfigPath,
			Listen:          s.deps.Listen,
		},
		UptimeSeconds: int64(time.Since(s.deps.StartTime).Seconds()),
		ReadyOK:       rep.OK,
	}
	if s.deps.Store != nil {
		resp.Paths.ConfigPath = s.deps.Store.Path()
	}
	if s.deps.Ready != nil {
		resp.Ready = rep.Checks
	}
	if d := time.Since(s.deps.StartTime); d >= 0 {
		resp.Runtime.UptimeHuman = d.Truncate(time.Second).String()
	}
	writeJSON(w, http.StatusOK, resp)
}
